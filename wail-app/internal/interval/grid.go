package interval

// What remains of the ADR-0006 grid-alignment math after ADR-0009 retired it:
// the slew and its δ machinery are gone (playout re-quantizes every round onto
// the local grid, so cross-LAN phase never reaches the ear), but the audibility
// bound they were built around survives as the tempo detector's de-noising bar.

const (
	// SlewMaxFraction is the largest tempo deviation WAIL ever considered
	// inaudible: 0.05% = 0.86 cents, below the pitch JND even for trained ears
	// on isolated sustained tones (~1–3 cents). The old 0.3% (5.2 cents) was
	// NOT inaudible — the 2026-07-25 field session heard every slew episode.
	// The grid slew that used this as its nudge cap is retired (ADR-0009); the
	// figure remains as the boundary between "observation noise a room need
	// not hear about" and "a tempo change worth declaring".
	SlewMaxFraction = 0.0005
)

// SlewAuthorityBPM expresses SlewMaxFraction in BPM at a given tempo (0.06 at
// 120). The tempo detector's reporting bar keys off it: an observed divergence
// inside this band is inaudible by the bound above, so it is de-noised rather
// than declared to the room.
func SlewAuthorityBPM(bpm float64) float64 {
	if bpm <= 0 {
		return 0
	}
	return SlewMaxFraction * bpm
}
