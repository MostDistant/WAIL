package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// count returns how many violations of one category text produces.
func count(t *testing.T, text, category string) int {
	t.Helper()
	return Lint(text).Counts[category]
}

func TestLongSentence(t *testing.T) {
	short := "one two three four five six seven eight nine ten."
	if got := count(t, short, catLongSentence); got != 0 {
		t.Errorf("short sentence flagged %d times", got)
	}
	twenty := strings.TrimSpace(strings.Repeat("word ", 20))
	if got := count(t, twenty, catLongSentence); got != 0 {
		t.Errorf("20 words flagged %d times, the cap is >20", got)
	}
	if got := count(t, twenty+" word", catLongSentence); got != 1 {
		t.Errorf("21 words flagged %d times, want 1", got)
	}
}

func TestSemicolonAndContraction(t *testing.T) {
	if got := count(t, "The relay is up; the peer is not.", catSemicolon); got != 1 {
		t.Errorf("semicolon = %d, want 1", got)
	}
	// Both apostrophe forms count, and only the listed suffixes.
	for _, s := range []string{"don't", "It’s", "they're", "we've", "I'll", "he'd", "I'm", "WAIL's"} {
		if got := count(t, s, catContraction); got != 1 {
			t.Errorf("contraction %q = %d, want 1", s, got)
		}
	}
	if got := count(t, "the peers' rooms", catContraction); got != 0 {
		t.Errorf("plural possessive flagged %d times", got)
	}
}

func TestPassiveAndIng(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"The file is read by the parser.", 1}, // irregular participle
		{"The frame was encoded.", 1},          // -ed participle
		{"The parser reads the file.", 0},
		{"They are being run.", 1},
	}
	for _, c := range cases {
		if got := count(t, c.text, catPassive); got != c.want {
			t.Errorf("passive_voice %q = %d, want %d", c.text, got, c.want)
		}
	}
	if got := count(t, "The relay is holding the interval.", catIngMainVerb); got != 1 {
		t.Errorf("ing_main_verb = %d, want 1", got)
	}
	if got := count(t, "The relay holds the interval.", catIngMainVerb); got != 0 {
		t.Errorf("plain verb flagged as ing_main_verb")
	}
}

func TestNominalization(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"Perform an analysis of the log.", 1},          // verb form
		{"The implementation of the codec is done.", 1}, // -tion of
		{"Analyze the log.", 0},
		{"performance matters", 0}, // must not match perform + \b
	}
	for _, c := range cases {
		if got := count(t, c.text, catNominalization); got != c.want {
			t.Errorf("nominalization %q = %d, want %d", c.text, got, c.want)
		}
	}
}

func TestPhraseLists(t *testing.T) {
	cases := []struct {
		text, category string
		want           int
	}{
		{"spin up the relay", catPhrasalVerb, 1},
		{"utilize the relay", catBannedWord, 1},
		{"utilizes the relay", catBannedWord, 1},    // the -s form is its own entry
		{"utilized the relay", catBannedWord, 0},    // not in the list
		{"prior to the interval", catBannedWord, 1}, // multi-word phrase
		{"a robust and seamless relay", catMarketing, 2},
		{"robustness is fine", catMarketing, 0}, // must not match inside a longer word
		{"It should be noted that this fails.", catModalHedge, 1},
		{"robust robust robust", catMarketing, 3}, // adjacent repeats all count
	}
	for _, c := range cases {
		if got := count(t, c.text, c.category); got != c.want {
			t.Errorf("%s %q = %d, want %d", c.category, c.text, got, c.want)
		}
	}
	// "it is important to note" sits in both lists, so it scores twice. This
	// is upstream behavior, kept deliberately.
	r := Lint("It is important to note that this fails.")
	if r.Counts[catBannedWord] != 1 || r.Counts[catModalHedge] != 1 {
		t.Errorf("overlapping phrase: banned=%d hedge=%d, want 1 and 1",
			r.Counts[catBannedWord], r.Counts[catModalHedge])
	}
}

func TestCodeIsNotProse(t *testing.T) {
	fenced := "Text here.\n\n```\nutilize a robust seamless thing;\n```\n"
	r := Lint(fenced)
	if r.Total != 0 {
		t.Errorf("fenced block scored %d violations: %v", r.Total, r.Counts)
	}
	if got := count(t, "Run `utilize --robust` now.", catBannedWord); got != 0 {
		t.Errorf("inline code scored %d banned words", got)
	}
}

func TestLongParagraph(t *testing.T) {
	six := "S1. S2. S3. S4. S5. S6."
	if got := count(t, six, catLongParagraph); got != 0 {
		t.Errorf("six sentences flagged %d times, the cap is >6", got)
	}
	if got := count(t, six+" S7.", catLongParagraph); got != 1 {
		t.Errorf("seven sentences flagged %d times, want 1", got)
	}
}

func TestSentenceSplitting(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"One. Two! Three? Four: Five.", 5},
		{"No split here.lowercase follows.", 1}, // no space after the stop
		{"Stop. lowercase follows.", 1},         // next word is not a sentence start
		{`Split: "Quoted".`, 2},
		{"Split: 9 lives.", 2},
		{"# Heading text", 1},   // marker stripped, text kept
		{"- list item", 1},      // marker stripped, text kept
		{"1. numbered item", 1}, // marker stripped, text kept
	}
	for _, c := range cases {
		if got := Lint(c.text).Sentences; got != c.want {
			t.Errorf("sentences %q = %d, want %d", c.text, got, c.want)
		}
	}
}

func TestScoreIsPer100Words(t *testing.T) {
	// Ten words, one violation, so the score is 10 per 100 words.
	r := Lint("This robust thing has exactly ten words in it.")
	if r.Words != 9 {
		t.Fatalf("words = %d, want 9", r.Words)
	}
	if r.Total != 1 {
		t.Fatalf("total = %d, want 1", r.Total)
	}
	if r.Per100 != 11.11 {
		t.Errorf("per100 = %v, want 11.11", r.Per100)
	}
}

func TestEmptyInputDoesNotDivideByZero(t *testing.T) {
	for _, text := range []string{"", "\n\n\n", "   ", "```\ncode\n```"} {
		r := Lint(text)
		if r.Words < 1 {
			t.Errorf("Lint(%q).Words = %d, want at least 1 to keep the score finite", text, r.Words)
		}
		if r.Per100 != 0 {
			t.Errorf("Lint(%q).Per100 = %v, want 0", text, r.Per100)
		}
	}
}

func TestEmDashIsCountedButNotScored(t *testing.T) {
	r := Lint("A relay — and an en dash – too.")
	if r.EmDash != 2 {
		t.Errorf("em dash count = %d, want 2", r.EmDash)
	}
	if r.Counts[catBannedWord] != 0 || r.Total != 0 {
		t.Errorf("em dashes must not add to the score, got total=%d", r.Total)
	}
}

// TestFindingOffsets checks the -v path: every finding must point at the right
// line of the ORIGINAL text, even when a fenced block was stripped before it.
func TestFindingOffsets(t *testing.T) {
	src := "line one\n" + // 1
		"```\n" + // 2
		"robust code\n" + // 3
		"```\n" + // 4
		"a robust claim\n" + // 5
		"another line\n" // 6
	r := Lint(src)
	if len(r.Findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(r.Findings), r.Findings)
	}
	if got := lineOf(src, r.Findings[0].Offset); got != 5 {
		t.Errorf("finding on line %d, want 5 (offset %d)", got, r.Findings[0].Offset)
	}
}

func lineOf(src string, off int) int {
	if off < 0 || off > len(src) {
		return -1
	}
	return 1 + strings.Count(src[:off], "\n")
}

func TestFindingsMatchCounts(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "kitchen-sink.md"))
	if err != nil {
		t.Fatal(err)
	}
	r := Lint(string(src))
	sum := 0
	for _, c := range categories {
		sum += r.Counts[c]
	}
	if sum != r.Total || r.Total != len(r.Findings) {
		t.Errorf("counts sum to %d, total is %d, findings are %d — all three must agree",
			sum, r.Total, len(r.Findings))
	}
	for _, f := range r.Findings {
		if f.Offset < -1 || f.Offset > len(src) {
			t.Errorf("finding %q has out-of-range offset %d", f.Category, f.Offset)
		}
	}
}

// TestUpstreamParity pins the port to ste-lint.py. The golden files were
// produced by running the upstream Python on each fixture:
//
//	python3 ste-lint.py < testdata/<name>.md > testdata/<name>.golden.json
//
// A diff here means the Go port and the original tool no longer agree.
func TestUpstreamParity(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join("testdata", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures in testdata")
	}
	for _, path := range fixtures {
		t.Run(filepath.Base(path), func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(strings.TrimSuffix(path, ".md") + ".golden.json")
			if err != nil {
				t.Fatal(err)
			}

			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetIndent("", "  ")
			enc.SetEscapeHTML(false)
			if err := enc.Encode(toJSON(namedResult{path, Lint(string(src))}, false)); err != nil {
				t.Fatal(err)
			}
			if buf.String() != string(want) {
				t.Errorf("output differs from upstream ste-lint.py\n--- got ---\n%s\n--- want ---\n%s",
					buf.String(), want)
			}
		})
	}
}
