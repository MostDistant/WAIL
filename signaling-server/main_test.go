package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Token bucket unit tests
// ---------------------------------------------------------------------------

func TestTokenBucketBasic(t *testing.T) {
	b := newTokenBucket(10, 20) // 10 tokens/sec, burst of 20
	// Should allow 20 rapid calls (full burst).
	for i := 0; i < 20; i++ {
		if !b.allow() {
			t.Fatalf("expected allow at call %d", i)
		}
	}
	// 21st should fail — bucket empty.
	if b.allow() {
		t.Fatal("expected deny after burst exhausted")
	}
	// After 100ms, ~1 token refilled (10/sec * 0.1s = 1).
	time.Sleep(110 * time.Millisecond)
	if !b.allow() {
		t.Fatal("expected allow after refill")
	}
}

func TestTokenBucketRefill(t *testing.T) {
	b := newTokenBucket(100, 100)
	// Drain all tokens.
	for i := 0; i < 100; i++ {
		b.allow()
	}
	if b.allow() {
		t.Fatal("expected deny after drain")
	}
	// Wait 500ms → ~50 tokens should refill.
	time.Sleep(500 * time.Millisecond)
	allowed := 0
	for i := 0; i < 60; i++ {
		if b.allow() {
			allowed++
		}
	}
	// Allow some jitter: 40-60 tokens.
	if allowed < 40 || allowed > 60 {
		t.Fatalf("expected ~50 tokens after 500ms, got %d", allowed)
	}
}

func TestTokenBucketStreamScaling(t *testing.T) {
	streams := 3
	b := newTokenBucket(baseBinaryRate*float64(streams), baseBinaryBurst*float64(streams))
	b.refillRate = 0 // freeze refill so the count is exact regardless of loop duration
	expectedBurst := int(baseBinaryBurst * float64(streams))
	allowed := 0
	for i := 0; i < expectedBurst+50; i++ {
		if b.allow() {
			allowed++
		}
	}
	if allowed != expectedBurst {
		t.Fatalf("expected %d burst for %d streams, got %d", expectedBurst, streams, allowed)
	}
}

func TestStreamCountCap(t *testing.T) {
	// Even if a peer claims 100 streams, the rate bucket should be capped at maxStreamsPerPeer.
	cap := maxStreamsPerPeer
	b := newTokenBucket(baseBinaryRate*float64(cap), baseBinaryBurst*float64(cap))
	b.refillRate = 0 // freeze refill so the count is exact regardless of loop duration
	expectedBurst := int(baseBinaryBurst * float64(cap))
	allowed := 0
	for i := 0; i < expectedBurst+50; i++ {
		if b.allow() {
			allowed++
		}
	}
	if allowed != expectedBurst {
		t.Fatalf("expected %d burst for capped streams, got %d", expectedBurst, allowed)
	}
}

// ---------------------------------------------------------------------------
// Integration test helpers
// ---------------------------------------------------------------------------

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS peers (
			room TEXT NOT NULL, peer_id TEXT NOT NULL, display_name TEXT,
			stream_count INTEGER DEFAULT 1, last_seen INTEGER NOT NULL,
			PRIMARY KEY (room, peer_id))`,
		`CREATE TABLE IF NOT EXISTS rooms (
			room TEXT PRIMARY KEY, password_hash TEXT,
			created_at INTEGER NOT NULL DEFAULT 0)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func testServer(t *testing.T) (*hub, *httptest.Server) {
	t.Helper()
	h := newHub(testDB(t))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleWS(h, w, r)
	}))
	t.Cleanup(func() { srv.Close() })
	return h, srv
}

func dialWS(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}

func joinRoom(t *testing.T, ws *websocket.Conn, room, peerID string, streamCount int) {
	t.Helper()
	msg := map[string]any{
		"type":           "join",
		"room":           room,
		"peer_id":        peerID,
		"stream_count":   streamCount,
		"client_version": "99.0.0",
	}
	if err := ws.WriteJSON(msg); err != nil {
		t.Fatal(err)
	}
	// Read join_ok or join_error.
	var resp map[string]any
	if err := ws.ReadJSON(&resp); err != nil {
		t.Fatal(err)
	}
	if resp["type"] != "join_ok" {
		t.Fatalf("expected join_ok, got %v", resp)
	}
}

// ---------------------------------------------------------------------------
// Integration tests
// ---------------------------------------------------------------------------

func TestRateLimitDisconnectBinary(t *testing.T) {
	_, srv := testServer(t)
	ws := dialWS(t, srv)
	joinRoom(t, ws, "test-room", "flood-peer", 1)

	// Flood binary messages well beyond the burst limit (120 for 1 stream).
	// After burst + rateLimitWarnMax violations, the server should close the connection.
	fakeAudio := make([]byte, 100)
	sent := 0
	for i := 0; i < 500; i++ {
		if err := ws.WriteMessage(websocket.BinaryMessage, fakeAudio); err != nil {
			break
		}
		sent++
	}

	// The server should eventually close the connection.
	// Try reading — expect an error.
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, err := ws.ReadMessage()
		if err != nil {
			break // connection closed as expected
		}
	}

	// If we got here without error within the deadline, the connection was closed.
	t.Logf("sent %d binary messages before connection closed", sent)
}

func TestRateLimitDisconnectText(t *testing.T) {
	_, srv := testServer(t)
	ws := dialWS(t, srv)
	joinRoom(t, ws, "test-room", "flood-peer", 1)

	// Flood text messages (sync type).
	syncMsg := map[string]any{
		"type":    "sync",
		"payload": map[string]any{"type": "Ping", "sent_at": 12345},
	}
	raw, _ := json.Marshal(syncMsg)
	sent := 0
	for i := 0; i < 500; i++ {
		if err := ws.WriteMessage(websocket.TextMessage, raw); err != nil {
			break
		}
		sent++
	}

	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, err := ws.ReadMessage()
		if err != nil {
			break
		}
	}
	t.Logf("sent %d text messages before connection closed", sent)
}

func TestLegitTrafficPasses(t *testing.T) {
	_, srv := testServer(t)

	// Two peers: sender with 3 streams, receiver.
	wsSend := dialWS(t, srv)
	wsRecv := dialWS(t, srv)
	joinRoom(t, wsSend, "legit-room", "sender", 3)
	joinRoom(t, wsRecv, "legit-room", "receiver", 1)

	// Drain peer_joined notification on receiver.
	wsRecv.SetReadDeadline(time.Now().Add(1 * time.Second))
	wsRecv.ReadMessage() // peer_joined for sender (may already be read in join)

	// Send at 150/sec (50fps * 3 streams) for 1 second.
	// With rate = 60*3 = 180 tokens/sec and burst = 360, this should pass.
	fakeAudio := make([]byte, 100)
	ticker := time.NewTicker(time.Second / 150)
	defer ticker.Stop()

	deadline := time.After(1 * time.Second)
	sent := 0
	sendErr := false
loop:
	for {
		select {
		case <-deadline:
			break loop
		case <-ticker.C:
			if err := wsSend.WriteMessage(websocket.BinaryMessage, fakeAudio); err != nil {
				sendErr = true
				break loop
			}
			sent++
		}
	}

	if sendErr {
		t.Fatalf("send error after %d messages — connection was closed prematurely", sent)
	}

	// Verify sender is still connected by sending one more message.
	if err := wsSend.WriteMessage(websocket.BinaryMessage, fakeAudio); err != nil {
		t.Fatal("sender connection was closed despite legitimate traffic")
	}
	t.Logf("sent %d messages at ~150/sec with 3 streams — connection stayed open", sent)
}

func TestJoinExemptFromTextRateLimit(t *testing.T) {
	_, srv := testServer(t)
	ws := dialWS(t, srv)

	// Exhaust the text bucket by sending sync messages to use up the burst,
	// plus a few more to accumulate some violations (but not enough to disconnect).
	syncMsg, _ := json.Marshal(map[string]any{
		"type":    "sync",
		"payload": map[string]any{"type": "Ping", "sent_at": 12345},
	})
	for i := 0; i < 230; i++ {
		ws.WriteMessage(websocket.TextMessage, syncMsg)
	}

	// Join should still work (it's exempted from text rate limiting).
	joinMsg := map[string]any{
		"type":           "join",
		"room":           "join-test",
		"peer_id":        "late-joiner",
		"stream_count":   1,
		"client_version": "99.0.0",
	}
	if err := ws.WriteJSON(joinMsg); err != nil {
		t.Fatal(err)
	}

	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		var resp map[string]any
		if err := ws.ReadJSON(&resp); err != nil {
			t.Fatalf("expected join_ok after rate limit, got error: %v", err)
		}
		if resp["type"] == "join_ok" {
			break // success
		}
		if resp["type"] == "rate_limit_warning" {
			continue // skip warnings, keep reading
		}
		t.Fatalf("unexpected message type: %v", resp)
	}
}

// ---------------------------------------------------------------------------
// Loopback (server echo of own audio) tests
// ---------------------------------------------------------------------------

// readBinaryFrame reads until a binary frame arrives (skipping text messages),
// returning its sender-prefix and payload; ok=false on read timeout. The
// connection must not be reused after a timeout.
func readBinaryFrame(t *testing.T, ws *websocket.Conn, timeout time.Duration) (string, []byte, bool) {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(timeout))
	for {
		msgType, data, err := ws.ReadMessage()
		if err != nil {
			return "", nil, false
		}
		if msgType != websocket.BinaryMessage {
			continue
		}
		if len(data) < 1 || len(data) < 1+int(data[0]) {
			t.Fatalf("malformed binary frame: %v", data)
		}
		pidLen := int(data[0])
		return string(data[1 : 1+pidLen]), data[1+pidLen:], true
	}
}

func setLoopback(t *testing.T, ws *websocket.Conn, enabled bool) {
	t.Helper()
	if err := ws.WriteJSON(map[string]any{"type": "set_loopback", "enabled": enabled}); err != nil {
		t.Fatal(err)
	}
}

func TestLoopbackEchoesOwnAudio(t *testing.T) {
	_, srv := testServer(t)
	wsA := dialWS(t, srv)
	wsB := dialWS(t, srv)
	joinRoom(t, wsA, "loop-room", "looper", 1)
	joinRoom(t, wsB, "loop-room", "listener", 1)

	setLoopback(t, wsA, true)
	payload := []byte("WAIF-test-payload")
	if err := wsA.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatal(err)
	}

	// The sender gets its own frame back, prefixed with its own peer ID.
	from, data, ok := readBinaryFrame(t, wsA, 2*time.Second)
	if !ok {
		t.Fatal("expected loopback echo, got none")
	}
	if from != "looper" {
		t.Fatalf("echo from = %q, want %q", from, "looper")
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("echo payload = %q, want %q", data, payload)
	}

	// Normal broadcast to the other peer is unaffected.
	from, data, ok = readBinaryFrame(t, wsB, 2*time.Second)
	if !ok || from != "looper" || !bytes.Equal(data, payload) {
		t.Fatalf("listener frame = (%q, %q, %v), want (looper, payload, true)", from, data, ok)
	}
}

func TestNoLoopbackByDefault(t *testing.T) {
	_, srv := testServer(t)
	wsA := dialWS(t, srv)
	wsB := dialWS(t, srv)
	joinRoom(t, wsA, "noloop-room", "sender", 1)
	joinRoom(t, wsB, "noloop-room", "receiver", 1)

	payload := []byte("no-echo-please")
	if err := wsA.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatal(err)
	}

	// Receiver gets it — proves the frame was relayed.
	from, data, ok := readBinaryFrame(t, wsB, 2*time.Second)
	if !ok || from != "sender" || !bytes.Equal(data, payload) {
		t.Fatalf("receiver frame = (%q, %q, %v), want (sender, payload, true)", from, data, ok)
	}

	// Sender must NOT get an echo.
	if from, _, ok := readBinaryFrame(t, wsA, 500*time.Millisecond); ok {
		t.Fatalf("unexpected echo to sender from %q with loopback off", from)
	}
}

func TestLoopbackDisable(t *testing.T) {
	_, srv := testServer(t)
	ws := dialWS(t, srv)
	joinRoom(t, ws, "toggle-room", "toggler", 1)

	setLoopback(t, ws, true)
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("on")); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := readBinaryFrame(t, ws, 2*time.Second); !ok {
		t.Fatal("expected echo while enabled")
	}

	setLoopback(t, ws, false)
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("off")); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := readBinaryFrame(t, ws, 500*time.Millisecond); ok {
		t.Fatal("unexpected echo after disable")
	}
}

func TestLoopbackResetsOnRejoin(t *testing.T) {
	_, srv := testServer(t)
	ws := dialWS(t, srv)
	joinRoom(t, ws, "rejoin-room", "rejoiner", 1)

	setLoopback(t, ws, true)
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("pre")); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := readBinaryFrame(t, ws, 2*time.Second); !ok {
		t.Fatal("expected echo while enabled")
	}

	// Re-joining resets loopback to off (clients must re-send after reconnect).
	joinRoom(t, ws, "rejoin-room", "rejoiner", 1)
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("post")); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := readBinaryFrame(t, ws, 500*time.Millisecond); ok {
		t.Fatal("unexpected echo after rejoin — loopback should reset")
	}
}

// ---------------------------------------------------------------------------
// Room password hashing tests
// ---------------------------------------------------------------------------

func TestHashPasswordIsSaltedBcrypt(t *testing.T) {
	a, err := hashPassword("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	b, err := hashPassword("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two hashes of the same password are identical — hash is not salted")
	}
	if !strings.HasPrefix(a, "$2a$") {
		t.Fatalf("expected a bcrypt hash, got %q", a)
	}
	cost, err := bcrypt.Cost([]byte(a))
	if err != nil {
		t.Fatal(err)
	}
	if cost < 12 {
		t.Fatalf("bcrypt cost %d is below the minimum of 12", cost)
	}
}

func TestCheckPasswordBcrypt(t *testing.T) {
	stored, err := hashPassword("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if ok, legacy := checkPassword(stored, "hunter2"); !ok || legacy {
		t.Fatalf("correct password: ok=%v legacy=%v, want true/false", ok, legacy)
	}
	if ok, _ := checkPassword(stored, "hunter3"); ok {
		t.Fatal("wrong password accepted")
	}
	if ok, _ := checkPassword(stored, ""); ok {
		t.Fatal("empty password accepted")
	}
}

// Passwords beyond bcrypt's 72-byte input limit must still round-trip rather
// than being silently truncated or rejected.
func TestCheckPasswordLongerThanBcryptLimit(t *testing.T) {
	long := strings.Repeat("a", 100)
	stored, err := hashPassword(long)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := checkPassword(stored, long); !ok {
		t.Fatal("long password rejected by its own hash")
	}
	// Differs only past byte 72 — a truncating implementation would accept it.
	if ok, _ := checkPassword(stored, strings.Repeat("a", 99)+"b"); ok {
		t.Fatal("password differing past byte 72 accepted — input is being truncated")
	}
}

func TestCheckPasswordLegacySHA256(t *testing.T) {
	sum := sha256.Sum256([]byte("hunter2"))
	stored := hex.EncodeToString(sum[:])
	ok, legacy := checkPassword(stored, "hunter2")
	if !ok || !legacy {
		t.Fatalf("legacy hash: ok=%v legacy=%v, want true/true", ok, legacy)
	}
	if ok, _ := checkPassword(stored, "hunter3"); ok {
		t.Fatal("wrong password accepted against legacy hash")
	}
}

func joinWithPassword(t *testing.T, ws *websocket.Conn, room, peerID, password string) map[string]any {
	t.Helper()
	msg := map[string]any{
		"type":           "join",
		"room":           room,
		"peer_id":        peerID,
		"password":       password,
		"stream_count":   1,
		"client_version": "99.0.0",
	}
	if err := ws.WriteJSON(msg); err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := ws.ReadJSON(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func roomHash(t *testing.T, h *hub, room string) string {
	t.Helper()
	var stored sql.NullString
	if err := h.db.QueryRow("SELECT password_hash FROM rooms WHERE room = ?", room).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	return stored.String
}

func TestJoinStoresBcryptHash(t *testing.T) {
	h, srv := testServer(t)
	if resp := joinWithPassword(t, dialWS(t, srv), "pw-room", "creator", "hunter2"); resp["type"] != "join_ok" {
		t.Fatalf("expected join_ok, got %v", resp)
	}

	stored := roomHash(t, h, "pw-room")
	if !strings.HasPrefix(stored, "$2") {
		t.Fatalf("expected a bcrypt hash in the rooms table, got %q", stored)
	}

	if resp := joinWithPassword(t, dialWS(t, srv), "pw-room", "guest", "hunter2"); resp["type"] != "join_ok" {
		t.Fatalf("correct password rejected: %v", resp)
	}
	resp := joinWithPassword(t, dialWS(t, srv), "pw-room", "intruder", "wrong")
	if resp["type"] != "join_error" || resp["code"] != "unauthorized" {
		t.Fatalf("expected unauthorized, got %v", resp)
	}
}

// Rooms created before the bcrypt migration keep working, and their stored
// digest is upgraded in place on the first successful join.
func TestJoinUpgradesLegacyHash(t *testing.T) {
	h, srv := testServer(t)
	sum := sha256.Sum256([]byte("hunter2"))
	legacy := hex.EncodeToString(sum[:])
	if _, err := h.db.Exec("INSERT INTO rooms (room, password_hash, created_at) VALUES (?, ?, ?)",
		"legacy-room", legacy, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	if resp := joinWithPassword(t, dialWS(t, srv), "legacy-room", "intruder", "wrong"); resp["code"] != "unauthorized" {
		t.Fatalf("expected unauthorized, got %v", resp)
	}
	if got := roomHash(t, h, "legacy-room"); got != legacy {
		t.Fatal("failed join rewrote the stored hash")
	}

	if resp := joinWithPassword(t, dialWS(t, srv), "legacy-room", "member", "hunter2"); resp["type"] != "join_ok" {
		t.Fatalf("legacy password rejected: %v", resp)
	}
	upgraded := roomHash(t, h, "legacy-room")
	if !strings.HasPrefix(upgraded, "$2") {
		t.Fatalf("expected the legacy hash to be upgraded to bcrypt, got %q", upgraded)
	}
	if resp := joinWithPassword(t, dialWS(t, srv), "legacy-room", "member2", "hunter2"); resp["type"] != "join_ok" {
		t.Fatalf("join failed after upgrade: %v", resp)
	}
}
