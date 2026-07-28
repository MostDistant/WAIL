package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func main() {
	var (
		asJSON  = flag.Bool("json", false, "emit JSON instead of the summary table")
		verbose = flag.Bool("v", false, "list every violation as file:line:col")
		max     = flag.Float64("max", 0, "if above 0, exit 1 when a file scores more violations per 100 words than this")
	)
	flag.Usage = usage
	flag.Parse()

	files := expandGlobs(flag.Args())
	if len(files) == 0 {
		src, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ste-lint: read stdin:", err)
			os.Exit(2)
		}
		printJSON([]namedResult{{"<stdin>", Lint(string(src))}}, false)
		return
	}

	results := make([]namedResult, 0, len(files))
	readFailed := false
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ste-lint:", err)
			readFailed = true
			continue
		}
		results = append(results, namedResult{name, Lint(string(src))})
	}

	if *asJSON {
		printJSON(results, true)
	} else {
		for _, r := range results {
			fmt.Printf("%-32s words=%4d total=%3d per100w=%6.2f em_dash=%2d\n",
				filepath.Base(r.name), r.res.Words, r.res.Total, r.res.Per100, r.res.EmDash)
			if *verbose {
				printFindings(r)
			}
		}
	}

	if readFailed {
		os.Exit(2)
	}
	if *max > 0 {
		for _, r := range results {
			if r.res.Per100 > *max {
				fmt.Fprintf(os.Stderr, "ste-lint: %s scores %.2f, above the -max of %.2f\n", r.name, r.res.Per100, *max)
				os.Exit(1)
			}
		}
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ste-lint — score prose in violations per 100 words (lower is cleaner).

Usage:
  ste-lint [flags] <file>...     one summary row per file
  ste-lint < draft.md            score stdin, print JSON

The delta between two lints is the signal, not the absolute number. Lint a
draft, apply the ste-writing skill, then lint it again.

Flags:
`)
	flag.PrintDefaults()
}

type namedResult struct {
	name string
	res  Result
}

// printFindings lists each violation under its file, sorted by position.
func printFindings(r namedResult) {
	src, err := os.ReadFile(r.name)
	if err != nil {
		return
	}
	lineStarts := []int{0}
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}
	found := append([]Finding(nil), r.res.Findings...)
	sort.SliceStable(found, func(i, j int) bool { return found[i].Offset < found[j].Offset })
	for _, f := range found {
		if f.Offset < 0 {
			fmt.Printf("  %s: %s: %s\n", r.name, f.Category, snippet(f.Text))
			continue
		}
		line := sort.Search(len(lineStarts), func(i int) bool { return lineStarts[i] > f.Offset })
		col := f.Offset - lineStarts[line-1] + 1
		fmt.Printf("  %s:%d:%d: %s: %s\n", r.name, line, col, f.Category, snippet(f.Text))
	}
}

func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 90 {
		return s[:87] + "..."
	}
	return s
}

// score serializes like a Python float: always with a decimal point, so 40
// prints as 40.0 and the JSON stays byte-comparable with the upstream tool.
type score float64

func (s score) MarshalJSON() ([]byte, error) {
	out := strconv.FormatFloat(float64(s), 'g', -1, 64)
	if !strings.ContainsAny(out, ".eEnI") {
		out += ".0"
	}
	return []byte(out), nil
}

// jsonViolations keeps the upstream key names and their order.
type jsonViolations struct {
	LongSentence   int `json:"long_sentence(>20w)"`
	Semicolon      int `json:"semicolon"`
	Contraction    int `json:"contraction"`
	Passive        int `json:"passive_voice"`
	IngMainVerb    int `json:"ing_main_verb"`
	Nominalization int `json:"nominalization"`
	PhrasalVerb    int `json:"phrasal_verb"`
	BannedWord     int `json:"banned_word"`
	Marketing      int `json:"marketing_adjective"`
	ModalHedge     int `json:"modal_hedge"`
	LongParagraph  int `json:"long_paragraph(>6s)"`
}

type jsonResult struct {
	File                 string         `json:"file,omitempty"`
	Words                int            `json:"words"`
	Sentences            int            `json:"sentences"`
	Violations           jsonViolations `json:"violations"`
	Total                int            `json:"total"`
	TotalPer100w         score          `json:"total_per100w"`
	EmDash               int            `json:"em_dash(slop-marker)"`
	LongestSentenceWords int            `json:"longest_sentence_words"`
	SampleMarketing      []string       `json:"sample_marketing"`
	SampleBanned         []string       `json:"sample_banned"`
}

func toJSON(r namedResult, withFile bool) jsonResult {
	c := r.res.Counts
	j := jsonResult{
		Words:     r.res.Words,
		Sentences: r.res.Sentences,
		Violations: jsonViolations{
			LongSentence:   c[catLongSentence],
			Semicolon:      c[catSemicolon],
			Contraction:    c[catContraction],
			Passive:        c[catPassive],
			IngMainVerb:    c[catIngMainVerb],
			Nominalization: c[catNominalization],
			PhrasalVerb:    c[catPhrasalVerb],
			BannedWord:     c[catBannedWord],
			Marketing:      c[catMarketing],
			ModalHedge:     c[catModalHedge],
			LongParagraph:  c[catLongParagraph],
		},
		Total:                r.res.Total,
		TotalPer100w:         score(r.res.Per100),
		EmDash:               r.res.EmDash,
		LongestSentenceWords: r.res.LongestSentenceWords,
		SampleMarketing:      r.res.SampleMarketing,
		SampleBanned:         r.res.SampleBanned,
	}
	if withFile {
		j.File = r.name
	}
	return j
}

func printJSON(results []namedResult, withFile bool) {
	out := make([]jsonResult, 0, len(results))
	for _, r := range results {
		out = append(out, toJSON(r, withFile))
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	// The category keys contain ">", which Go escapes by default. Keep them
	// literal so the output stays diffable against the upstream tool.
	enc.SetEscapeHTML(false)
	var err error
	if len(out) == 1 && !withFile {
		err = enc.Encode(out[0])
	} else {
		err = enc.Encode(out)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ste-lint: encode:", err)
		os.Exit(2)
	}
}

// expandGlobs matches the upstream behavior: an argument with a wildcard is
// expanded and sorted, anything else is passed through untouched.
func expandGlobs(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.ContainsAny(a, "*?[") {
			out = append(out, a)
			continue
		}
		matches, err := filepath.Glob(a)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ste-lint: bad pattern %q: %v\n", a, err)
			continue
		}
		sort.Strings(matches)
		out = append(out, matches...)
	}
	return out
}
