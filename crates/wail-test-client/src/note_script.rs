use anyhow::{bail, Result};

/// A single step in a note script.
#[derive(Debug, Clone, PartialEq)]
pub struct NoteStep {
    pub freq: Option<f32>, // None = silence
    pub bars: u32,         // how many bars to hold this note
}

/// A scripted sequence of notes that loops when exhausted.
#[derive(Debug, Clone)]
pub struct NoteScript {
    steps: Vec<NoteStep>,
    total_bars: u32, // sum of all step.bars
}

impl NoteScript {
    /// Parse comma-separated entries: `"220:4,440:2,silence:1,330:4"`
    ///
    /// Each entry is either `<freq>:<bars>` or `silence:<bars>`.
    /// Frequency must be positive, bars must be >= 1.
    pub fn parse(script: &str) -> Result<Self> {
        let trimmed = script.trim();
        if trimmed.is_empty() {
            bail!("note script is empty");
        }

        let mut steps = Vec::new();
        for entry in trimmed.split(',') {
            let entry = entry.trim();
            let parts: Vec<&str> = entry.splitn(2, ':').collect();
            if parts.len() != 2 {
                bail!("malformed entry (expected freq:bars or silence:bars): {entry:?}");
            }

            let bars: u32 = parts[1]
                .trim()
                .parse()
                .map_err(|_| anyhow::anyhow!("invalid bars value in {entry:?}"))?;
            if bars == 0 {
                bail!("bars must be >= 1 in {entry:?}");
            }

            let freq_str = parts[0].trim();
            let freq = if freq_str.eq_ignore_ascii_case("silence") {
                None
            } else {
                let f: f32 = freq_str
                    .parse()
                    .map_err(|_| anyhow::anyhow!("invalid frequency in {entry:?}"))?;
                if f <= 0.0 {
                    bail!("frequency must be positive in {entry:?}");
                }
                Some(f)
            };

            steps.push(NoteStep { freq, bars });
        }

        if steps.is_empty() {
            bail!("note script has no steps");
        }

        let total_bars = steps.iter().map(|s| s.bars).sum();
        Ok(Self { steps, total_bars })
    }

    /// Given a global bar index, return the frequency (or None for silence).
    /// Wraps around when the pattern is exhausted.
    pub fn freq_at_bar(&self, global_bar: u64) -> Option<f32> {
        let bar_in_pattern = (global_bar % self.total_bars as u64) as u32;
        let mut accumulated = 0u32;
        for step in &self.steps {
            accumulated += step.bars;
            if bar_in_pattern < accumulated {
                return step.freq;
            }
        }
        // Should be unreachable, but return last step's freq as fallback.
        self.steps.last().and_then(|s| s.freq)
    }

    /// Returns the existing hardcoded pentatonic behavior:
    /// 220:1,261.63:1,293.66:1,329.63:1,392:1
    pub fn default_pentatonic() -> Self {
        Self {
            steps: vec![
                NoteStep { freq: Some(220.00), bars: 1 },
                NoteStep { freq: Some(261.63), bars: 1 },
                NoteStep { freq: Some(293.66), bars: 1 },
                NoteStep { freq: Some(329.63), bars: 1 },
                NoteStep { freq: Some(392.00), bars: 1 },
            ],
            total_bars: 5,
        }
    }

    /// Total bars in one cycle of the pattern.
    pub fn total_bars(&self) -> u32 {
        self.total_bars
    }

    /// Access the individual steps.
    pub fn steps(&self) -> &[NoteStep] {
        &self.steps
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const SCALE: [f32; 5] = [220.00, 261.63, 293.66, 329.63, 392.00];

    #[test]
    fn test_parse_simple() {
        let script = NoteScript::parse("220:4,440:2").unwrap();
        assert_eq!(script.steps().len(), 2);
        assert_eq!(script.total_bars(), 6);
        assert_eq!(script.steps()[0], NoteStep { freq: Some(220.0), bars: 4 });
        assert_eq!(script.steps()[1], NoteStep { freq: Some(440.0), bars: 2 });
    }

    #[test]
    fn test_parse_silence() {
        let script = NoteScript::parse("220:2,silence:1,330:2").unwrap();
        assert_eq!(script.steps().len(), 3);
        assert_eq!(script.steps()[1].freq, None);
        assert_eq!(script.steps()[1].bars, 1);
    }

    #[test]
    fn test_parse_error_empty() {
        assert!(NoteScript::parse("").is_err());
    }

    #[test]
    fn test_parse_error_bad_freq() {
        assert!(NoteScript::parse("abc:4").is_err());
    }

    #[test]
    fn test_parse_error_zero_bars() {
        assert!(NoteScript::parse("220:0").is_err());
    }

    #[test]
    fn test_parse_error_negative_freq() {
        assert!(NoteScript::parse("-100:4").is_err());
    }

    #[test]
    fn test_freq_at_bar_simple() {
        let script = NoteScript::parse("220:2,440:3").unwrap();
        assert_eq!(script.freq_at_bar(0), Some(220.0));
        assert_eq!(script.freq_at_bar(1), Some(220.0));
        assert_eq!(script.freq_at_bar(2), Some(440.0));
        assert_eq!(script.freq_at_bar(3), Some(440.0));
        assert_eq!(script.freq_at_bar(4), Some(440.0));
    }

    #[test]
    fn test_freq_at_bar_wraps() {
        let script = NoteScript::parse("220:2,440:3").unwrap();
        assert_eq!(script.total_bars(), 5);
        // bar 5 wraps to bar 0
        assert_eq!(script.freq_at_bar(5), Some(220.0));
        assert_eq!(script.freq_at_bar(6), Some(220.0));
        assert_eq!(script.freq_at_bar(7), Some(440.0));
    }

    #[test]
    fn test_freq_at_bar_silence() {
        let script = NoteScript::parse("220:1,silence:1,330:1").unwrap();
        assert_eq!(script.freq_at_bar(0), Some(220.0));
        assert_eq!(script.freq_at_bar(1), None);
        assert_eq!(script.freq_at_bar(2), Some(330.0));
    }

    #[test]
    fn test_default_pentatonic_matches_hardcoded() {
        let script = NoteScript::default_pentatonic();
        for i in 0u64..10 {
            let expected = SCALE[(i % 5) as usize];
            let actual = script.freq_at_bar(i);
            assert_eq!(
                actual,
                Some(expected),
                "mismatch at bar {i}: expected {expected}, got {actual:?}"
            );
        }
    }

    #[test]
    fn test_freq_at_bar_large_index() {
        let script = NoteScript::parse("220:2,440:3").unwrap();
        // bar 1_000_000 should work without overflow
        let bar = 1_000_000u64;
        let result = script.freq_at_bar(bar);
        // 1_000_000 % 5 = 0, so should be 220
        assert_eq!(result, Some(220.0));
    }
}
