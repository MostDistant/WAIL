// ste-lint is a heuristic anti-slop linter for WAIL's prose. It scores a
// document in violations per 100 words: the machine-checkable subset of
// ASD-STE100 Simplified Technical English. Lower is cleaner, and the delta
// between two lints is the signal — the absolute number is not.
//
// This is a Go port of ste-lint.py from the episode kit at
// https://github.com/woosal1337/blog/tree/main/videos/ep01-the-cure-for-ai-slop
// (MIT, (c) 2026 Ege Celebi). The full notice is in
// .claude/skills/ste-writing/LICENSE-upstream. The rule set, the word lists and
// the table output match upstream so the two tools can be diffed; the -v mode
// (per-line violations) is ours.
//
// It is not a certified STE checker. The judgment rules of ASD-STE100 need a
// human. This covers the mechanical subset, which is where the slop lives.
package main

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Violation categories, in the order the upstream tool emits them.
const (
	catLongSentence   = "long_sentence(>20w)"
	catSemicolon      = "semicolon"
	catContraction    = "contraction"
	catPassive        = "passive_voice"
	catIngMainVerb    = "ing_main_verb"
	catNominalization = "nominalization"
	catPhrasalVerb    = "phrasal_verb"
	catBannedWord     = "banned_word"
	catMarketing      = "marketing_adjective"
	catModalHedge     = "modal_hedge"
	catLongParagraph  = "long_paragraph(>6s)"
)

var categories = []string{
	catLongSentence, catSemicolon, catContraction, catPassive, catIngMainVerb,
	catNominalization, catPhrasalVerb, catBannedWord, catMarketing,
	catModalHedge, catLongParagraph,
}

var marketingWords = []string{
	"seamless", "seamlessly", "robust", "powerful", "cutting-edge", "effortless", "effortlessly",
	"world-class", "next-generation", "revolutionary", "blazing", "lightning-fast", "elegant", "delightful",
	"turnkey", "best-in-class", "state-of-the-art", "game-changing", "first-class", "battle-tested",
	"enterprise-grade", "supercharge", "unlock", "unleash", "empower", "empowers",
}

var bannedWords = []string{
	"begin", "begins", "commence", "commences", "initiate", "initiates", "originate",
	"utilize", "utilizes", "utilizing", "leverage", "leverages", "leveraging", "facilitate", "facilitates",
	"ensure", "ensures", "ensuring", "prior to", "subsequent to", "obtain", "obtains", "acquire", "acquires",
	"demonstrate", "demonstrates", "additionally", "furthermore", "moreover", "comprehensive", "comprehensively",
	"utilization", "aforementioned", "henceforth", "therein", "whilst", "amongst", "numerous", "myriad", "plethora",
	"in order to", "a variety of", "in the event that", "due to the fact that", "it is important to note",
}

var phrasalVerbs = []string{
	"spin up", "spin down", "reach out", "dive into", "dives into", "diving into", "kick off", "kicks off",
	"roll out", "rolls out", "tear down", "ramp up", "circle back", "drill down", "spun up", "reaching out",
}

var modalHedges = []string{
	"it is important to note", "it should be noted", "it is worth noting", "please note that",
	"as mentioned", "as noted above",
}

const (
	beVerbs = `(?:am|is|are|was|were|be|been|being)`
	ppIrreg = `(?:done|made|sent|read|built|kept|held|set|put|run|written|shown|given|taken|found|got|gotten|seen|known|thrown|drawn)`
)

var (
	codeFenceRe = regexp.MustCompile("(?s)```.*?```")
	inlineCode  = regexp.MustCompile("`[^`]*`")
	headingRe   = regexp.MustCompile(`^\s*#{1,6}\s*`)
	listRe      = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+`)
	wordRe      = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9'\-/]*`)
	paraSepRe   = regexp.MustCompile(`\n\s*\n`)

	// Go's \w and \b are ASCII-only, where Python's are Unicode-aware. For
	// English technical prose the two agree; the curly apostrophe is spelled
	// out because it is not an ASCII word character either way.
	contractionRe = regexp.MustCompile(`\b\w+['\x{2019}](?:t|re|ve|ll|d|s|m)\b`)
	passiveRe     = regexp.MustCompile(`(?i)\b` + beVerbs + `\s+(?:\w+ed|` + ppIrreg + `)\b`)
	ingRe         = regexp.MustCompile(`(?i)\b` + beVerbs + `\s+\w+ing\b`)
	nominalVerbRe = regexp.MustCompile(`(?i)\b(?:perform(?:s|ed)?|conduct(?:s|ed)?|provide(?:s|d)?|carry out|carries out|make use of|makes use of)\b`)
	nominalNounRe = regexp.MustCompile(`(?i)\b\w{4,}(?:tion|ment|ance|ence)\s+of\b`)
)

// Finding is one violation, located at a byte offset in the original text.
// Offset is -1 when the location could not be recovered.
type Finding struct {
	Category string
	Offset   int
	Text     string
}

// Result is the score for one document.
type Result struct {
	Words                int
	Sentences            int
	Counts               map[string]int
	Total                int
	Per100               float64
	EmDash               int
	LongestSentenceWords int
	SampleMarketing      []string
	SampleBanned         []string
	Findings             []Finding
}

// Lint scores raw and returns every violation it found.
func Lint(raw string) Result {
	// Code never counts as prose. Fenced blocks go first, then inline spans, so
	// that the two passes compose exactly as they do upstream.
	t1, fenceSegs := replaceSpans(raw, codeFenceRe, " ")
	text, inlineSegs := replaceSpans(t1, inlineCode, " ")
	toRaw := func(off int) int { return mapOffset(fenceSegs, mapOffset(inlineSegs, off)) }

	sents := splitSentences(text)
	words, longest := 0, 0
	var findings []Finding
	for _, s := range sents {
		n := countWords(s.text)
		words += n
		if n > longest {
			longest = n
		}
		if n > 20 {
			findings = append(findings, Finding{catLongSentence, toRaw(s.off), s.text})
		}
	}
	if words == 0 {
		words = 1
	}

	for i := 0; i < len(text); i++ {
		if text[i] == ';' {
			findings = append(findings, Finding{catSemicolon, toRaw(i), ";"})
		}
	}

	addMatches := func(cat string, re *regexp.Regexp) {
		for _, loc := range re.FindAllStringIndex(text, -1) {
			findings = append(findings, Finding{cat, toRaw(loc[0]), text[loc[0]:loc[1]]})
		}
	}
	addMatches(catContraction, contractionRe)
	addMatches(catPassive, passiveRe)
	addMatches(catIngMainVerb, ingRe)
	addMatches(catNominalization, nominalVerbRe)
	addMatches(catNominalization, nominalNounRe)

	// Phrase lists are matched case-insensitively against the lowercased text.
	// Lowercasing can change byte length for a few non-ASCII runes; when it
	// does, the counts still hold but the offsets no longer map back.
	low := strings.ToLower(text)
	offsetsUsable := len(low) == len(text)
	addPhrases := func(cat string, list []string) []string {
		var hits []string
		for _, phrase := range list {
			for _, at := range findPhrase(low, phrase) {
				off := -1
				if offsetsUsable {
					off = toRaw(at)
				}
				findings = append(findings, Finding{cat, off, phrase})
				hits = append(hits, phrase)
			}
		}
		return hits
	}
	addPhrases(catPhrasalVerb, phrasalVerbs)
	bannedHits := addPhrases(catBannedWord, bannedWords)
	marketingHits := addPhrases(catMarketing, marketingWords)
	addPhrases(catModalHedge, modalHedges)

	// Paragraphs are split from the raw text, matching upstream.
	for _, p := range splitParagraphs(raw) {
		if strings.TrimSpace(p.text) == "" {
			continue
		}
		stripped, _ := replaceSpans(p.text, codeFenceRe, " ")
		stripped, _ = replaceSpans(stripped, inlineCode, " ")
		if len(splitSentences(stripped)) > 6 {
			findings = append(findings, Finding{catLongParagraph, p.off, firstLine(p.text)})
		}
	}

	counts := make(map[string]int, len(categories))
	for _, c := range categories {
		counts[c] = 0
	}
	for _, f := range findings {
		counts[f.Category]++
	}
	total := len(findings)

	return Result{
		Words:                words,
		Sentences:            len(sents),
		Counts:               counts,
		Total:                total,
		Per100:               round2(float64(total) * 100.0 / float64(words)),
		EmDash:               strings.Count(raw, "—") + strings.Count(raw, "–"),
		LongestSentenceWords: longest,
		SampleMarketing:      firstUnique(marketingHits, 6),
		SampleBanned:         firstUnique(bannedHits, 6),
		Findings:             findings,
	}
}

// segment maps a byte range of the output text back to the input text. A
// segment is either copied through (offsets shift by a constant) or a
// replacement, in which case every offset maps to the start of what it replaced.
type segment struct {
	outStart, outEnd int
	srcStart         int
	copied           bool
}

// replaceSpans replaces every match of re with repl and returns the offset map.
func replaceSpans(s string, re *regexp.Regexp, repl string) (string, []segment) {
	locs := re.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s, []segment{{0, len(s), 0, true}}
	}
	var b strings.Builder
	var segs []segment
	prev := 0
	for _, loc := range locs {
		if loc[0] > prev {
			segs = append(segs, segment{b.Len(), b.Len() + loc[0] - prev, prev, true})
			b.WriteString(s[prev:loc[0]])
		}
		segs = append(segs, segment{b.Len(), b.Len() + len(repl), loc[0], false})
		b.WriteString(repl)
		prev = loc[1]
	}
	if prev < len(s) {
		segs = append(segs, segment{b.Len(), b.Len() + len(s) - prev, prev, true})
		b.WriteString(s[prev:])
	}
	return b.String(), segs
}

func mapOffset(segs []segment, off int) int {
	if len(segs) == 0 {
		return off
	}
	i := sort.Search(len(segs), func(i int) bool { return segs[i].outEnd > off })
	if i == len(segs) {
		i = len(segs) - 1
	}
	s := segs[i]
	if !s.copied {
		return s.srcStart
	}
	if off < s.outStart {
		return s.srcStart
	}
	return s.srcStart + (off - s.outStart)
}

type located struct {
	text string
	off  int
}

// splitSentences reproduces the upstream sentence splitter: one pass per line,
// heading and list markers removed, then split after .!?: when the next
// non-space character starts something new.
func splitSentences(text string) []located {
	var out []located
	for lineStart := 0; lineStart <= len(text); {
		var line string
		next := len(text) + 1
		if nl := strings.IndexByte(text[lineStart:], '\n'); nl >= 0 {
			line = text[lineStart : lineStart+nl]
			next = lineStart + nl + 1
		} else {
			line = text[lineStart:]
		}
		base := lineStart
		lineStart = next

		left := strings.TrimLeftFunc(line, unicode.IsSpace)
		base += len(line) - len(left)
		s := strings.TrimRightFunc(left, unicode.IsSpace)
		if s == "" {
			continue
		}
		if m := headingRe.FindStringIndex(s); m != nil {
			base += m[1]
			s = s[m[1]:]
		}
		if m := listRe.FindStringIndex(s); m != nil {
			base += m[1]
			s = s[m[1]:]
		}
		if s == "" {
			continue
		}
		for _, part := range splitAfterStops(s) {
			t := strings.TrimLeftFunc(part.text, unicode.IsSpace)
			lead := len(part.text) - len(t)
			t = strings.TrimRightFunc(t, unicode.IsSpace)
			if t == "" {
				continue
			}
			out = append(out, located{t, base + part.off + lead})
		}
	}
	return out
}

func splitAfterStops(s string) []located {
	var parts []located
	start := 0
	for i := 1; i < len(s); i++ {
		if !isSpaceByte(s[i]) || strings.IndexByte(".!?:", s[i-1]) < 0 {
			continue
		}
		j := i
		for j < len(s) && isSpaceByte(s[j]) {
			j++
		}
		if j >= len(s) || !startsSentence(s[j]) {
			continue
		}
		parts = append(parts, located{s[start:i], start})
		start = j
		i = j
	}
	return append(parts, located{s[start:], start})
}

func splitParagraphs(raw string) []located {
	var out []located
	prev := 0
	for _, loc := range paraSepRe.FindAllStringIndex(raw, -1) {
		out = append(out, located{raw[prev:loc[0]], prev})
		prev = loc[1]
	}
	return append(out, located{raw[prev:], prev})
}

// findPhrase returns every offset in low (already lowercased) where phrase
// appears with no lowercase letter touching either end.
func findPhrase(low, phrase string) []int {
	var hits []int
	for i := 0; i+len(phrase) <= len(low); {
		j := strings.Index(low[i:], phrase)
		if j < 0 {
			break
		}
		at := i + j
		end := at + len(phrase)
		before := at == 0 || !isLowerAlpha(low[at-1])
		after := end == len(low) || !isLowerAlpha(low[end])
		if before && after {
			hits = append(hits, at)
			i = end
		} else {
			i = at + 1
		}
	}
	return hits
}

func countWords(s string) int { return len(wordRe.FindAllString(s, -1)) }

func isLowerAlpha(c byte) bool { return c >= 'a' && c <= 'z' }

func isSpaceByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}

func startsSentence(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '"' || c == '\'' || c == '-'
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func firstUnique(hits []string, n int) []string {
	out := []string{}
	seen := make(map[string]bool, len(hits))
	for _, h := range hits {
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
		if len(out) == n {
			break
		}
	}
	return out
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }
