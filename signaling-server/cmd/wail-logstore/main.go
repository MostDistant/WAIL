// wail-logstore keeps a durable local copy of a WAIL session's logs in SQLite.
//
// Fly keeps app logs for about seven days and `flyctl logs --no-tail` returns
// only a short buffer, so by the time a jam is over the evidence is usually
// gone. This pulls the relay's logs from Fly's paginated logs API (which does
// reach back across the whole retention window) and, optionally, the room's
// peer-shared logs off the relay, into one table on one timeline — so
// correlating "a peer joined" against "capture drift micro-slew" is a query
// rather than an afternoon of lining up timestamps by hand.
//
// Usage:
//
//	wail-logstore -db wail-logs.db                       # backfill 7 days of relay logs
//	wail-logstore -since 24h -follow                     # backfill a day, then keep pulling
//	wail-logstore -room synthseeker -password drone      # relay logs + that room's peer logs
//
// The Fly token comes from -token, $FLY_API_TOKEN, or `flyctl auth token`.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
)

const (
	// flyLogsAPI reaches back across Fly's full retention window, unlike the
	// short buffer `flyctl logs --no-tail` returns.
	flyLogsAPI = "https://api.fly.io/api/v1/apps/%s/logs"
	// flyRetention is how far back Fly keeps app logs; the default -since.
	flyRetention = 7 * 24 * time.Hour
	// followPoll is the idle gap between pulls in -follow mode. The relay is
	// quiet for minutes at a time, so polling faster just burns API calls.
	followPoll = 15 * time.Second
)

func main() {
	var (
		dbPath   = flag.String("db", "wail-logs.db", "SQLite database to append into")
		app      = flag.String("app", "wail-relay", "Fly app whose logs to pull")
		token    = flag.String("token", "", "Fly API token (default $FLY_API_TOKEN, else `flyctl auth token`)")
		since    = flag.String("since", "168h", "how far back to backfill: a duration (24h) or an RFC3339 time")
		follow   = flag.Bool("follow", false, "keep pulling after the backfill completes")
		noRelay  = flag.Bool("no-relay", false, "skip Fly relay logs (peer logs only)")
		room     = flag.String("room", "", "also record this room's peer-shared logs (needs -server access)")
		password = flag.String("password", "", "room password, if any")
		server   = flag.String("server", "wss://wail-relay.fly.dev", "relay WebSocket URL for peer logs")
		version  = flag.String("version", "4.1.0", "client_version sent at join (must meet relay min_version)")
	)
	flag.Parse()

	start, err := parseSince(*since)
	if err != nil {
		log.Fatalf("-since: %v", err)
	}

	db, err := openDB(*dbPath)
	if err != nil {
		log.Fatalf("open %s: %v", *dbPath, err)
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	if !*noRelay {
		tok, err := flyToken(*token)
		if err != nil {
			log.Fatalf("fly token: %v", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := pullRelay(ctx, db, *app, tok, start, *follow); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[relay] stopped: %v", err)
			}
		}()
	}

	if *room != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tailPeers(ctx, db, *server, *room, *password, *version)
		}()
	}

	wg.Wait()
	report(db)
}

// --- storage ---------------------------------------------------------------

// openDB opens (creating if needed) the log store. ts_ns is the ordering key
// across both sources; the unique index makes re-runs idempotent, which
// matters because backfill windows are expected to overlap.
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	const schema = `
CREATE TABLE IF NOT EXISTS logs (
  ts_ns   INTEGER NOT NULL,
  ts      TEXT    NOT NULL,
  source  TEXT    NOT NULL,
  origin  TEXT    NOT NULL,
  level   TEXT    NOT NULL,
  region  TEXT    NOT NULL,
  message TEXT    NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS logs_dedup ON logs(ts_ns, source, origin, message);
CREATE INDEX IF NOT EXISTS logs_ts ON logs(ts_ns);
CREATE INDEX IF NOT EXISTS logs_source_ts ON logs(source, ts_ns);
`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// WAL keeps a reader (sqlite3 on the same file) working while we append.
	db.Exec("PRAGMA journal_mode=WAL")
	return db, nil
}

type entry struct {
	tsNS    int64
	ts      string
	source  string
	origin  string
	level   string
	region  string
	message string
}

// insert writes a batch, ignoring rows already present. Returns how many were
// new — the figure worth reporting, since overlap is normal.
func insert(db *sql.DB, rows []entry) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`INSERT OR IGNORE INTO logs
		(ts_ns, ts, source, origin, level, region, message) VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	added := 0
	for _, r := range rows {
		res, err := st.Exec(r.tsNS, r.ts, r.source, r.origin, r.level, r.region, r.message)
		if err != nil {
			return added, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	return added, tx.Commit()
}

func report(db *sql.DB) {
	var n int
	var lo, hi sql.NullString
	db.QueryRow(`SELECT COUNT(*), MIN(ts), MAX(ts) FROM logs`).Scan(&n, &lo, &hi)
	log.Printf("[store] %d rows, %s .. %s", n, lo.String, hi.String)
	rows, err := db.Query(`SELECT source, COUNT(*) FROM logs GROUP BY source ORDER BY source`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var c int
		if rows.Scan(&s, &c) == nil {
			log.Printf("[store]   %-6s %d", s, c)
		}
	}
}

// --- fly relay logs --------------------------------------------------------

// flyResponse is the JSON:API document the logs endpoint returns. next_token
// is the nanosecond timestamp of the newest entry in the page; passing it back
// asks for everything after it.
type flyResponse struct {
	Data []struct {
		Attributes struct {
			Timestamp string `json:"timestamp"`
			Message   string `json:"message"`
			Level     string `json:"level"`
			Instance  string `json:"instance"`
			Region    string `json:"region"`
		} `json:"attributes"`
	} `json:"data"`
	Meta struct {
		NextToken string `json:"next_token"`
	} `json:"meta"`
}

// pullRelay walks the logs API forward from start. Fly returns at most a page
// at a time, so this is a cursor walk, not a single fetch.
func pullRelay(ctx context.Context, db *sql.DB, app, token string, start time.Time, follow bool) error {
	client := &http.Client{Timeout: 30 * time.Second}
	cursor := fmt.Sprintf("%d", start.UnixNano())
	total, pages := 0, 0

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		resp, err := fetchPage(ctx, client, app, token, cursor)
		if err != nil {
			if !follow {
				return err
			}
			log.Printf("[relay] %v — retrying in %s", err, followPoll)
			if !sleepCtx(ctx, followPoll) {
				return ctx.Err()
			}
			continue
		}

		rows := make([]entry, 0, len(resp.Data))
		for _, d := range resp.Data {
			a := d.Attributes
			t, err := time.Parse(time.RFC3339Nano, a.Timestamp)
			if err != nil {
				continue
			}
			rows = append(rows, entry{
				tsNS: t.UnixNano(), ts: a.Timestamp, source: "relay",
				origin: a.Instance, level: a.Level, region: a.Region,
				message: strings.TrimRight(a.Message, "\n"),
			})
		}
		added, err := insert(db, rows)
		if err != nil {
			return fmt.Errorf("insert: %w", err)
		}
		total += added
		pages++
		if added > 0 {
			log.Printf("[relay] +%d new (%d rows this page, %s)", added, len(rows),
				rows[len(rows)-1].ts)
		}

		// An empty page means we have reached the live edge.
		if len(resp.Data) == 0 || resp.Meta.NextToken == "" || resp.Meta.NextToken == cursor {
			if !follow {
				log.Printf("[relay] backfill complete: %d new rows over %d pages", total, pages)
				return nil
			}
			if !sleepCtx(ctx, followPoll) {
				return ctx.Err()
			}
			continue
		}
		cursor = resp.Meta.NextToken
	}
}

func fetchPage(ctx context.Context, c *http.Client, app, token, cursor string) (*flyResponse, error) {
	url := fmt.Sprintf(flyLogsAPI, app)
	if cursor != "" {
		url += "?next_token=" + cursor
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// Fly macaroons authenticate with the FlyV1 scheme, not Bearer.
	req.Header.Set("Authorization", "FlyV1 "+token)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("logs API returned %d", resp.StatusCode)
	}
	var out flyResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &out, nil
}

// flyToken resolves the API token: explicit flag, environment, then flyctl.
func flyToken(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if t := os.Getenv("FLY_API_TOKEN"); t != "" {
		return t, nil
	}
	out, err := exec.Command("flyctl", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("no -token or $FLY_API_TOKEN, and `flyctl auth token` failed: %w", err)
	}
	t := strings.TrimSpace(string(out))
	if t == "" {
		return "", errors.New("`flyctl auth token` returned nothing — try `flyctl auth login`")
	}
	return t, nil
}

// --- room peer logs --------------------------------------------------------

// envelope mirrors wail-logtail: client sync messages arrive wrapped, and
// LogBroadcast rides in the payload.
type envelope struct {
	Type        string            `json:"type"`
	From        string            `json:"from,omitempty"`
	PeerID      string            `json:"peer_id,omitempty"`
	DisplayName *string           `json:"display_name,omitempty"`
	Code        string            `json:"code,omitempty"`
	Level       string            `json:"level,omitempty"`
	Message     string            `json:"message,omitempty"`
	PeerNames   map[string]string `json:"peer_display_names,omitempty"`
}

// tailPeers records the room's shared logs, reconnecting until cancelled. Peer
// logs only exist while someone is in the room — unlike the relay's, they
// cannot be backfilled, so this has to be running during the jam.
func tailPeers(ctx context.Context, db *sql.DB, server, room, password, version string) {
	for ctx.Err() == nil {
		if err := tailOnce(ctx, db, server, room, password, version); err != nil && ctx.Err() == nil {
			log.Printf("[peers] disconnected: %v — reconnecting in 2s", err)
			if !sleepCtx(ctx, 2*time.Second) {
				return
			}
		}
	}
}

func tailOnce(ctx context.Context, db *sql.DB, server, room, password, version string) error {
	u := server
	if !strings.Contains(strings.TrimPrefix(strings.TrimPrefix(u, "wss://"), "ws://"), "/") {
		u = strings.TrimSuffix(u, "/") + "/ws"
	}
	c, _, err := websocket.DefaultDialer.DialContext(ctx, u, nil)
	if err != nil {
		return err
	}
	defer c.Close()

	// stream_count 0 joins as an observer, claiming no audio slots.
	join := map[string]any{
		"type": "join", "room": room, "password": password,
		"peer_id":      fmt.Sprintf("logstore-%d", time.Now().UnixNano()),
		"stream_count": 0, "display_name": "[logstore]", "client_version": version,
	}
	if err := c.WriteJSON(join); err != nil {
		return fmt.Errorf("join: %w", err)
	}
	log.Printf("[peers] recording %q", room)

	go func() { <-ctx.Done(); c.Close() }()

	names := map[string]string{}
	for {
		var e envelope
		if err := c.ReadJSON(&e); err != nil {
			return err
		}
		switch e.Type {
		case "join_error":
			return fmt.Errorf("join refused: %s", e.Code)
		case "joined":
			for id, n := range e.PeerNames {
				names[id] = n
			}
		case "peer_joined":
			if e.DisplayName != nil {
				names[e.PeerID] = *e.DisplayName
			}
		case "log":
			origin := e.From
			if n, ok := names[e.From]; ok && n != "" {
				origin = n
			}
			now := time.Now().UTC()
			level := e.Level
			if level == "" {
				level = "info"
			}
			// Stamped on arrival: the relay does not carry a peer's wall clock,
			// and peer clocks disagree anyway — one receiver's arrival order is
			// the only timeline all sources can share.
			if _, err := insert(db, []entry{{
				tsNS: now.UnixNano(), ts: now.Format(time.RFC3339Nano),
				source: "peer", origin: origin, level: level, region: "",
				message: strings.TrimRight(e.Message, "\n"),
			}}); err != nil {
				log.Printf("[peers] insert: %v", err)
			}
		}
	}
}

// --- helpers ---------------------------------------------------------------

// parseSince accepts either a lookback duration or an absolute RFC3339 time,
// clamped to Fly's retention (asking for more just wastes empty pages).
func parseSince(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither a duration nor an RFC3339 time", s)
	}
	if d > flyRetention {
		d = flyRetention
	}
	return time.Now().Add(-d), nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
