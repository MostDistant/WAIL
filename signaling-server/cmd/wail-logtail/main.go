// wail-logtail tails a WAIL room's shared logs over the relay: joins the room
// as a quiet observer (stream_count 0) and prints LogBroadcast messages and
// peer join/leave events as they arrive. Peers emit logs when peer log sharing
// is armed (the debug room, or the app's -log-sharing flag).
//
// Usage:
//
//	wail-logtail -server wss://wail-relay.fly.dev -room wail-debug
//	wail-logtail -server ws://localhost:8897 -room test-room -password secret -events
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	server := flag.String("server", "wss://wail-relay.fly.dev", "relay WebSocket URL")
	room := flag.String("room", "wail-debug", "room to observe")
	password := flag.String("password", "", "room password (if any)")
	name := flag.String("name", "logtail", "display name shown to room peers")
	version := flag.String("version", "3.14.3", "client_version sent at join (must meet relay min_version)")
	events := flag.Bool("events", false, "also print non-log sync message types")
	flag.Parse()

	if *room == "" {
		log.Fatal("-room is required")
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	for {
		err := tail(*server, *room, *password, *name, *version, *events, sig)
		if err == errStopped {
			return
		}
		log.Printf("[logtail] disconnected: %v — reconnecting in 2s", err)
		select {
		case <-sig:
			return
		case <-time.After(2 * time.Second):
		}
	}
}

var errStopped = fmt.Errorf("stopped")

// envelope is the relay wire shape: client sync messages are wrapped as
// {type:"sync", from, payload:<SyncMessage JSON>}; LogBroadcast rides in the
// payload. peer_joined/peer_left are relay-originated, snake_case.
type envelope struct {
	Type        string          `json:"type"`
	From        string          `json:"from,omitempty"`
	PeerID      string          `json:"peer_id,omitempty"`
	DisplayName *string         `json:"display_name,omitempty"`
	Code        string          `json:"code,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Level       string          `json:"level,omitempty"`
	Message     string          `json:"message,omitempty"`
}

type logPayload struct {
	Type    string `json:"type"`
	From    string `json:"from,omitempty"`
	Level   string `json:"level,omitempty"`
	Message string `json:"message,omitempty"`
}

func tail(server, room, password, name, version string, events bool, sig chan os.Signal) error {
	// The relay serves WebSocket joins at /ws; append it if the URL has no path.
	u := server
	if !strings.Contains(strings.TrimPrefix(strings.TrimPrefix(u, "wss://"), "ws://"), "/") {
		u = strings.TrimSuffix(u, "/") + "/ws"
	}
	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		return err
	}
	defer c.Close()

	join := map[string]any{
		"type":           "join",
		"room":           room,
		"peer_id":        fmt.Sprintf("logtail-%d", time.Now().UnixNano()),
		"password":       password,
		"stream_count":   0,
		"display_name":   "[" + name + "]",
		"client_version": version,
	}
	if err := c.WriteJSON(join); err != nil {
		return fmt.Errorf("join write: %w", err)
	}
	fmt.Printf("[logtail] joined %q on %s — tailing (Ctrl-C to stop)\n", room, server)

	stop := make(chan struct{})
	go func() {
		<-sig
		_ = c.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		close(stop)
	}()

	for {
		select {
		case <-stop:
			return errStopped
		default:
		}
		mt, data, err := c.ReadMessage()
		if err != nil {
			select {
			case <-stop:
				return errStopped
			default:
				return err
			}
		}
		if mt == websocket.BinaryMessage {
			continue // audio frames: not our business
		}
		var m envelope
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		ts := time.Now().Format("15:04:05")
		switch m.Type {
		case "log":
			level := m.Level
			if level == "" {
				level = "info"
			}
			fmt.Printf("%s [%s/%s] %s\n", ts, m.From, level, m.Message)
		case "sync":
			var p logPayload
			if err := json.Unmarshal(m.Payload, &p); err != nil {
				continue
			}
			if p.Type == "LogBroadcast" {
				level := p.Level
				if level == "" {
					level = "info"
				}
				fmt.Printf("%s [%s/%s] %s\n", ts, p.From, level, p.Message)
			} else if events {
				fmt.Printf("%s [event] sync %s: %s\n", ts, p.Type, strings.TrimSpace(string(m.Payload)))
			}
		case "peer_joined":
			fmt.Printf("%s [room] + %s joined\n", ts, display(m))
		case "peer_left":
			fmt.Printf("%s [room] − %s left\n", ts, display(m))
		case "join_error":
			return fmt.Errorf("join refused: %s", m.Code)
		default:
			if events && m.Type != "" && !strings.HasPrefix(m.Type, "interval") {
				fmt.Printf("%s [event] %s: %s\n", ts, m.Type, strings.TrimSpace(string(data)))
			}
		}
	}
}

func display(m envelope) string {
	if m.DisplayName != nil && *m.DisplayName != "" {
		return *m.DisplayName
	}
	if m.PeerID != "" {
		return m.PeerID
	}
	return "?"
}
