package main

import (
	"strings"
	"testing"
)

// Every field on this path is unauthenticated client input, and it is written
// into the operator's log. The newline case is the one that matters: it lets a
// client forge whole log lines and assert anything about any room.
func TestSanitizeForLog(t *testing.T) {
	long := strings.Repeat("a", maxServerLoggedMessage+50)

	cases := []struct {
		name, in string
		check    func(*testing.T, string)
	}{
		{"newline cannot forge a line", "grid jumped\nroom evil peer x error [align] owned", func(t *testing.T, got string) {
			if strings.Contains(got, "\n") {
				t.Fatalf("newline survived: %q", got)
			}
		}},
		{"carriage return and tab go too", "a\rb\tc", func(t *testing.T, got string) {
			if strings.ContainsAny(got, "\r\t") {
				t.Fatalf("control char survived: %q", got)
			}
		}},
		{"other control chars go", "a\x00b\x1bc", func(t *testing.T, got string) {
			for _, r := range got {
				if r < 0x20 {
					t.Fatalf("control char survived: %q", got)
				}
			}
		}},
		{"over-length is truncated", long, func(t *testing.T, got string) {
			if len(got) > maxServerLoggedMessage+len("…(truncated)") {
				t.Fatalf("not truncated: %d bytes", len(got))
			}
			if !strings.HasSuffix(got, "(truncated)") {
				t.Fatalf("truncation not marked: %q", got)
			}
		}},
		// One ASCII byte then two-byte runes, so the byte cap lands INSIDE a
		// rune. Without the odd prefix the cut is rune-aligned by luck and the
		// case proves nothing — verified by mutation.
		{"truncation keeps valid utf-8", "a" + strings.Repeat("é", maxServerLoggedMessage), func(t *testing.T, got string) {
			// NOT ValidString: range-over-string yields U+FFFD for a broken
			// rune and WriteRune re-encodes it, so the output is always valid
			// UTF-8 and that assertion can never fail. The observable symptom
			// is the replacement character itself.
			if strings.ContainsRune(got, '\uFFFD') {
				t.Fatal("truncation cut a multi-byte rune (U+FFFD in output)")
			}
		}},
		{"ordinary text is untouched", "grid jumped +5.22 beats — cause: Link session merge", func(t *testing.T, got string) {
			if got != "grid jumped +5.22 beats — cause: Link session merge" {
				t.Fatalf("mangled: %q", got)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.check(t, sanitizeForLog(c.in)) })
	}
}

// The gate exists to stay narrow as the code around it moves: peers already
// echo each other's logs, so mirroring anything broader multiplies a room's
// chatter into the operator's log.
func TestShouldMirrorToServerLog(t *testing.T) {
	cases := []struct {
		level, target string
		want          bool
	}{
		{"warn", "align", true},
		{"error", "align", true},
		{"info", "align", false},
		{"debug", "align", false},
		{"warn", "", false},
		{"warn", "session", false},
		{"error", "audio", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := shouldMirrorToServerLog(c.level, c.target); got != c.want {
			t.Errorf("shouldMirrorToServerLog(%q, %q) = %v, want %v", c.level, c.target, got, c.want)
		}
	}
}
