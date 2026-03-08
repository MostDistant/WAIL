package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

const (
	minVersion     = "0.0.0" // bump when a breaking client change ships
	stalePeerSec   = 30
	pingInterval   = 15 * time.Second
	pongWait       = 20 * time.Second
	writeWait      = 10 * time.Second
	maxMessageSize = 64 * 1024
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// clientMsg is the envelope for all client→server messages.
type clientMsg struct {
	Type          string          `json:"type"`
	Room          string          `json:"room,omitempty"`
	PeerID        string          `json:"peer_id,omitempty"`
	Password      string          `json:"password,omitempty"`
	StreamCount   int             `json:"stream_count,omitempty"`
	DisplayName   *string         `json:"display_name,omitempty"`
	ClientVersion string          `json:"client_version,omitempty"`
	To            string          `json:"to,omitempty"`
	From          string          `json:"from,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	// Log broadcast fields
	Level       string `json:"level,omitempty"`
	Target      string `json:"target,omitempty"`
	Message     string `json:"message,omitempty"`
	TimestampUs int64  `json:"timestamp_us,omitempty"`
}

// conn wraps a single WebSocket connection that has joined a room.
type conn struct {
	ws     *websocket.Conn
	room   string
	peerID string
	send   chan []byte
}

// hub tracks all active connections, keyed by room → peer_id → conn.
type hub struct {
	mu    sync.Mutex
	rooms map[string]map[string]*conn
	db    *sql.DB
}

// ---------------------------------------------------------------------------
// Database
// ---------------------------------------------------------------------------

func openDB() *sql.DB {
	path := os.Getenv("DB_PATH")
	if path == "" {
		path = "/data/wail.db"
	}
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS peers (
			room TEXT NOT NULL,
			peer_id TEXT NOT NULL,
			display_name TEXT,
			stream_count INTEGER DEFAULT 1,
			last_seen INTEGER NOT NULL,
			PRIMARY KEY (room, peer_id)
		)`,
		`CREATE TABLE IF NOT EXISTS rooms (
			room TEXT PRIMARY KEY,
			password_hash TEXT,
			created_at INTEGER NOT NULL DEFAULT 0
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			log.Fatalf("migrate: %v", err)
		}
	}
	// Clean stale peers from previous run
	cutoff := time.Now().Unix() - stalePeerSec
	db.Exec("DELETE FROM peers WHERE last_seen < ?", cutoff)
	// Remove rooms whose peers were all stale/crashed — prevents a public ghost room
	// from persisting across a restart and blocking private re-creation of the same name.
	db.Exec("DELETE FROM rooms WHERE room NOT IN (SELECT DISTINCT room FROM peers)")
	return db
}

func hashPassword(pw string) string {
	h := sha256.Sum256([]byte(pw))
	return hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------------------
// Hub methods
// ---------------------------------------------------------------------------

func newHub(db *sql.DB) *hub {
	return &hub{rooms: make(map[string]map[string]*conn), db: db}
}

func (h *hub) join(c *conn, msg clientMsg) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Leave previous room if already joined (prevents stale references on double-join)
	if c.room != "" {
		h.leaveUnlocked(c)
	}

	room := msg.Room
	peerID := msg.PeerID
	streamCount := msg.StreamCount
	if streamCount < 1 {
		streamCount = 1
	}

	// Version check
	if semverLess(msg.ClientVersion, minVersion) {
		c.sendJSON(map[string]any{
			"type":        "join_error",
			"code":        "version_outdated",
			"min_version": minVersion,
		})
		return
	}

	// Password check
	var storedHash sql.NullString
	var roomExists bool
	err := h.db.QueryRow("SELECT password_hash FROM rooms WHERE room = ?", room).Scan(&storedHash)
	if err == nil {
		roomExists = true
	}

	if roomExists && storedHash.Valid && storedHash.String != "" {
		if hashPassword(msg.Password) != storedHash.String {
			c.sendJSON(map[string]any{"type": "join_error", "code": "unauthorized"})
			return
		}
	}

	// Capacity check (32 stream slots per room)
	const roomCapacity = 32
	var usedSlots int
	h.db.QueryRow("SELECT COALESCE(SUM(stream_count), 0) FROM peers WHERE room = ?", room).Scan(&usedSlots)
	if usedSlots+streamCount > roomCapacity {
		c.sendJSON(map[string]any{
			"type":            "join_error",
			"code":            "room_full",
			"slots_available": roomCapacity - usedSlots,
		})
		return
	}

	// Create room if needed
	if !roomExists {
		pwHash := ""
		if msg.Password != "" {
			pwHash = hashPassword(msg.Password)
		}
		h.db.Exec("INSERT OR IGNORE INTO rooms (room, password_hash, created_at) VALUES (?, ?, ?)",
			room, pwHash, time.Now().Unix())
	}

	// Upsert peer in DB
	displayName := ""
	if msg.DisplayName != nil {
		displayName = *msg.DisplayName
	}
	h.db.Exec(`INSERT INTO peers (room, peer_id, display_name, stream_count, last_seen)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(room, peer_id) DO UPDATE SET display_name=excluded.display_name, stream_count=excluded.stream_count, last_seen=excluded.last_seen`,
		room, peerID, displayName, streamCount, time.Now().Unix())

	// Build peer list + display names from in-memory connections
	peers := []string{}
	peerDisplayNames := map[string]*string{}
	if roomConns, ok := h.rooms[room]; ok {
		for id, rc := range roomConns {
			if id != peerID {
				peers = append(peers, id)
				// Look up display name from DB
				var dn sql.NullString
				h.db.QueryRow("SELECT display_name FROM peers WHERE room = ? AND peer_id = ?", room, id).Scan(&dn)
				if dn.Valid && dn.String != "" {
					name := dn.String
					peerDisplayNames[id] = &name
				} else {
					peerDisplayNames[id] = nil
				}
				// Notify existing peer
				rc.sendJSON(map[string]any{
					"type":         "peer_joined",
					"peer_id":      peerID,
					"display_name": msg.DisplayName,
				})
			}
		}
	}

	// Register connection
	if h.rooms[room] == nil {
		h.rooms[room] = make(map[string]*conn)
	}
	h.rooms[room][peerID] = c
	c.room = room
	c.peerID = peerID

	// Send join_ok
	c.sendJSON(map[string]any{
		"type":               "join_ok",
		"peers":              peers,
		"peer_display_names": peerDisplayNames,
	})
}

func (h *hub) signal(c *conn, msg clientMsg) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if roomConns, ok := h.rooms[c.room]; ok {
		if target, ok := roomConns[msg.To]; ok {
			// Forward as-is
			raw, _ := json.Marshal(map[string]any{
				"type":    "signal",
				"to":      msg.To,
				"from":    c.peerID,
				"payload": msg.Payload,
			})
			select {
			case target.send <- raw:
			default:
				log.Printf("warn: dropped signal from %s to %s (send buffer full)", c.peerID, msg.To)
			}
		}
	}
}

func (h *hub) broadcastLog(c *conn, msg clientMsg) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if c.room == "" {
		return
	}
	if roomConns, ok := h.rooms[c.room]; ok {
		raw, _ := json.Marshal(map[string]any{
			"type":         "log",
			"from":         c.peerID,
			"level":        msg.Level,
			"target":       msg.Target,
			"message":      msg.Message,
			"timestamp_us": msg.TimestampUs,
		})
		for pid, rc := range roomConns {
			if pid != c.peerID {
				select {
				case rc.send <- raw:
				default:
					log.Printf("warn: dropped log from %s to %s (send buffer full)", c.peerID, pid)
				}
			}
		}
	}
}

func (h *hub) leave(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.leaveUnlocked(c)
}

// leaveUnlocked removes c from its current room. Caller must hold h.mu.
func (h *hub) leaveUnlocked(c *conn) {
	if c.room == "" {
		return
	}

	room := c.room
	peerID := c.peerID

	// Remove from in-memory map
	if roomConns, ok := h.rooms[room]; ok {
		delete(roomConns, peerID)

		// Notify remaining peers
		for _, rc := range roomConns {
			rc.sendJSON(map[string]any{
				"type":    "peer_left",
				"peer_id": peerID,
			})
		}

		// If room is empty, clean up
		if len(roomConns) == 0 {
			delete(h.rooms, room)
			h.db.Exec("DELETE FROM rooms WHERE room = ?", room)
		}
	}

	// Remove from DB
	h.db.Exec("DELETE FROM peers WHERE room = ? AND peer_id = ?", room, peerID)

	c.room = ""
	c.peerID = ""
}

// ---------------------------------------------------------------------------
// Connection helpers
// ---------------------------------------------------------------------------

func (c *conn) sendJSON(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		log.Printf("warn: sendJSON marshal error for peer %s: %v", c.peerID, err)
		return
	}
	select {
	case c.send <- raw:
	default:
		log.Printf("warn: dropped message to peer %s (send buffer full)", c.peerID)
	}
}

func (c *conn) writePump() {
	ticker := time.NewTicker(pingInterval)
	defer func() {
		ticker.Stop()
		c.ws.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.ws.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.ws.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *conn) readPump(h *hub) {
	defer func() {
		h.leave(c)
		close(c.send)
		c.ws.Close()
	}()

	c.ws.SetReadLimit(maxMessageSize)
	c.ws.SetReadDeadline(time.Now().Add(pongWait))
	c.ws.SetPongHandler(func(string) error {
		c.ws.SetReadDeadline(time.Now().Add(pongWait))
		// Update last_seen in DB
		if c.room != "" {
			h.db.Exec("UPDATE peers SET last_seen = ? WHERE room = ? AND peer_id = ?",
				time.Now().Unix(), c.room, c.peerID)
		}
		return nil
	})

	for {
		_, raw, err := c.ws.ReadMessage()
		if err != nil {
			return
		}

		var msg clientMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "join":
			h.join(c, msg)
		case "signal":
			h.signal(c, msg)
		case "log":
			h.broadcastLog(c, msg)
		case "leave":
			h.leave(c)
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func handleWS(h *hub, w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade: %v", err)
		return
	}

	c := &conn{
		ws:   ws,
		send: make(chan []byte, 64),
	}

	go c.writePump()
	c.readPump(h)
}

func handleRooms(h *hub, w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()

	type roomInfo struct {
		Room         string   `json:"room"`
		CreatedAt    int64    `json:"created_at"`
		PeerCount    int      `json:"peer_count"`
		DisplayNames []string `json:"display_names"`
	}

	var result []roomInfo
	for roomName, conns := range h.rooms {
		// Skip password-protected rooms (they are private)
		var pwHash sql.NullString
		h.db.QueryRow("SELECT password_hash FROM rooms WHERE room = ?", roomName).Scan(&pwHash)
		if pwHash.Valid && pwHash.String != "" {
			continue
		}

		var createdAt int64
		h.db.QueryRow("SELECT created_at FROM rooms WHERE room = ?", roomName).Scan(&createdAt)

		names := []string{}
		for _, c := range conns {
			var dn sql.NullString
			h.db.QueryRow("SELECT display_name FROM peers WHERE room = ? AND peer_id = ?", roomName, c.peerID).Scan(&dn)
			if dn.Valid && dn.String != "" {
				names = append(names, dn.String)
			}
		}

		result = append(result, roomInfo{
			Room:         roomName,
			CreatedAt:    createdAt,
			PeerCount:    len(conns),
			DisplayNames: names,
		})
	}

	if result == nil {
		result = []roomInfo{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"rooms": result})
}

// ---------------------------------------------------------------------------
// Semver comparison
// ---------------------------------------------------------------------------

func semverLess(a, b string) bool {
	var a1, a2, a3, b1, b2, b3 int
	fmt.Sscanf(a, "%d.%d.%d", &a1, &a2, &a3)
	fmt.Sscanf(b, "%d.%d.%d", &b1, &b2, &b3)
	if a1 != b1 {
		return a1 < b1
	}
	if a2 != b2 {
		return a2 < b2
	}
	return a3 < b3
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	db := openDB()
	defer db.Close()

	h := newHub(db)

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleWS(h, w, r)
	})
	http.HandleFunc("/rooms", func(w http.ResponseWriter, r *http.Request) {
		handleRooms(h, w, r)
	})
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("WAIL signaling server listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
