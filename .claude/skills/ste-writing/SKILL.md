---
name: ste-writing
description: Rewrite prose (docs, READMEs, ADRs, PR descriptions, error messages, release notes, comments — never code) into ASD-STE100 Simplified Technical English to remove "AI slop". Use when asked to make writing not sound like AI, make docs clear or plain, enforce a controlled writing style, or write technical documentation that reads human. Two modes — strict (procedures/safety) and STE-flavored (general prose).
---

# ste-writing

Write prose in ASD-STE100 Simplified Technical English. This applies to
documentation, READMEs, ADRs, pull-request text, error messages, release notes,
and comments. It does not apply to code, identifiers, or command syntax. It is
not for marketing copy, essays, or anything that needs a voice — STE strips
voice on purpose.

## Rules

WORDS
- Use one name for one thing. Do not call the same item by two different names.
- Use the short common word: start (not begin/commence/initiate), use (not utilize/leverage), help (not facilitate), make sure (not ensure), before (not prior to), after (not subsequent to), about (not regarding/concerning), get (not obtain/acquire), show (not demonstrate), also (not additionally/furthermore/moreover).
- Give each word one meaning. "fall" means to move down, not to decrease.
- No marketing adjectives: seamless, robust, powerful, cutting-edge, effortless, world-class, next-generation, revolutionary.
- American spelling.

VERBS
- Active voice. "the parser reads the file", not "the file is read by the parser".
- Use a verb for an action. "analyze the log", not "perform an analysis of the log".
- No stacked auxiliaries. Not "it is important to note that this may help to improve". Write "this improves X".
- No "-ing" main verb where a simple tense works.

SENTENCES
- One instruction per sentence. Max 20 words (instruction), max 25 (descriptive).
- No contractions. Use articles: a, an, the, this, these.

PUNCTUATION
- No semicolons. Write two sentences. (Note: the em dash is not banned by STE, only the semicolon is — add "no em dash" yourself if you want it gone.)

STRUCTURE
- One topic per paragraph, max six sentences. For steps, use a numbered vertical list, one action per item, imperative form. Put a condition before its command.

Write only the requested text. No preamble, no summary, no closing remarks.

## Modes

- **strict** — procedures, runbooks, safety text, error messages: apply every rule and both length caps.
- **STE-flavored** — general prose (READMEs, PR descriptions, docs): apply the sentence, paragraph, active-voice, and no-phrasal-verb discipline; relax the ~900-word dictionary lockdown so the text keeps enough range to read naturally.

## Self-lint (run before returning text)

1. Any sentence over 20 words? Split it.
2. Any semicolon? Replace with a period.
3. Any contraction? Expand it.
4. Any passive voice with a known actor? Make it active.
5. Any "-ing" main verb, nominalization ("perform an analysis"), or phrasal verb ("spin up")? Replace with a plain verb.
6. Same thing named two ways? Pick one name.

The mechanical rules above are lintable and are what removes slop. Full STE also
needs human judgment (the right technical noun, whether a sentence "makes good
sense") — a checker cannot certify that, and slop is not about that. This skill
fixes the FORM of slop. It cannot make a hollow paragraph true.

Free official standard (do not paste it in full; it is copyrighted): https://asd-ste100.org

## WAIL vocabulary — one name for one thing

WAIL breaks the first WORDS rule more than any other. Use these names, and only
these names:

| Use | Not |
|---|---|
| relay | server, signaling server, backend (reserve `signaling-server` for the Go module and its directory) |
| room | session (in WAIL, a session is the local state machine in `session.go`) |
| interval | chunk, block, slice, buffer |
| peer | user, client, participant, node |
| stream | a peer's numbered audio stream, and nothing else |
| Link Audio channel | track, output, bus |
| WAIF frame | packet, message (a *sync message* is the JSON kind, over the same relay) |
| capture | send side, record, input path |
| emit | playback side, output path, render |
| interval offset D | delay, latency setting, lookahead |

Keep the spelling of proper names exactly: Ableton Link, Link Audio, Opus,
NINJAM, WAIF, CLAP, knope, Wails.

## Never rewrite

- Code, identifiers, command syntax, file paths, and anything inside backticks or
  a fenced block. The linter already skips these. Do the same by hand.
- `CHANGELOG.md` — knope generates it.
- Commit subject lines. The conventional-commit prefix (`feat:`, `fix:`, `feat!:`)
  controls the version bump, per `CLAUDE.md`. Rewrite the body, never the prefix.
- Quoted log lines, error strings copied from a real run, and test fixtures.

## Measure, do not assert

Lint the draft, rewrite it, then lint it again. Report the two scores and the
delta. Do not claim the writing improved without the numbers.

```sh
bin/ste-lint docs/architecture.md        # before
bin/ste-lint -v docs/architecture.md     # per-line violations, to fix them
```

The score is violations per 100 words. Lower is cleaner. The delta between two
lints is the signal, not the absolute number.

## Provenance

This skill and `wail-app/cmd/ste-lint` come from the episode kit
[`woosal1337/blog` → `videos/ep01-the-cure-for-ai-slop`](https://github.com/woosal1337/blog/tree/main/videos/ep01-the-cure-for-ai-slop)
by Ege Çelebi, MIT licensed. See `LICENSE-upstream` in this directory. The
`WAIL vocabulary`, `Never rewrite`, and `Measure, do not assert` sections above
are ours. The linter is a Go port of the upstream `ste-lint.py`.

Their cross-model result, over 6 writing tasks and 4 conditions, scored in
violations per 100 words:

| Condition | Claude sonnet | gpt-5.5 |
|---|---|---|
| baseline | 4.36 | 3.54 |
| banned-words list | 4.21 (−3%) | 2.14 (−40%) |
| Orwell's 6 rules | 2.48 (−43%) | 1.69 (−52%) |
| STE skill | **1.12 (−74%)** | 1.76 (−50%) |

Their full data — `experiment-results.md`, `experiment-results-openai.md`,
`before-after-samples.md`, and the reproduction script — stays upstream. It is a
heuristic linter over 6 tasks and 2 models: directional, not proof.
