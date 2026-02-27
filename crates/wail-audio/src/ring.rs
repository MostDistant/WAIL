/// NINJAM-style interval ring buffer for simultaneous record and playback.
///
/// The core concept from NINJAM:
/// - Two slots: one is being recorded into, the other is being played back
/// - At each interval boundary, slots swap: the just-recorded interval becomes
///   available for transmission, and received remote audio starts playing back
/// - Multiple remote peers' audio is mixed together in the playback slot
///
/// This implementation is designed for use on an audio thread:
/// - `process()` is the main method called per audio buffer
/// - It writes input to the record slot and reads from the playback slot
/// - At interval boundaries (driven by beat position), it swaps slots
///
/// # Interval Timing
///
/// Intervals are defined by: `bars * quantum` beats.
/// Beat position comes from the DAW transport (Ableton Link beat grid).
/// Example: 4 bars of 4/4 = 16 beats per interval.
pub struct IntervalRing {
    /// The slot currently being recorded into (local audio capture)
    record_slot: Vec<f32>,
    /// The slot currently being played back (mixed remote audio)
    playback_slot: Vec<f32>,
    /// Write position in the record slot (in interleaved samples)
    record_pos: usize,
    /// Read position in the playback slot (in interleaved samples)
    playback_pos: usize,
    /// Current interval index
    current_interval: Option<i64>,
    /// Completed intervals ready for encoding and transmission
    completed: Vec<CompletedInterval>,
    /// Remote peer intervals waiting to be mixed into next playback slot
    pending_remote: Vec<RemoteInterval>,
    /// Audio parameters
    sample_rate: u32,
    channels: u16,
    /// Interval parameters
    bars: u32,
    quantum: f64,
}

/// A completed local recording ready for encoding.
pub struct CompletedInterval {
    pub index: i64,
    pub samples: Vec<f32>,
}

/// A received remote interval to mix into playback.
struct RemoteInterval {
    pub index: i64,
    pub peer_id: String,
    pub samples: Vec<f32>,
}

impl IntervalRing {
    /// Create a new interval ring buffer.
    pub fn new(sample_rate: u32, channels: u16, bars: u32, quantum: f64) -> Self {
        let bars = bars.max(1);
        let quantum = quantum.max(f64::EPSILON);
        let beats_per_interval = bars as f64 * quantum;
        // Pre-allocate for max expected interval size at 200 BPM (fast tempo = short intervals)
        // At 60 BPM, 16 beats = 16 seconds. At 200 BPM, 16 beats = 4.8 seconds.
        let max_seconds = beats_per_interval / 60.0; // at 60 BPM
        let slot_capacity = (sample_rate as f64 * max_seconds * channels as f64) as usize;

        Self {
            record_slot: Vec::with_capacity(slot_capacity),
            playback_slot: Vec::new(),
            record_pos: 0,
            playback_pos: 0,
            current_interval: None,
            completed: Vec::new(),
            pending_remote: Vec::new(),
            sample_rate,
            channels,
            bars,
            quantum,
        }
    }

    /// Process one audio buffer: record input and produce output.
    ///
    /// Called once per audio callback from the DAW/plugin.
    ///
    /// - `input`: interleaved f32 samples from DAW (captured audio)
    /// - `output`: interleaved f32 buffer to fill with playback audio
    /// - `beat_position`: current beat position from DAW transport / Link
    ///
    /// Returns `Some(interval_index)` if an interval boundary was crossed.
    pub fn process(
        &mut self,
        input: &[f32],
        output: &mut [f32],
        beat_position: f64,
    ) -> Option<i64> {
        let interval_index = self.beat_to_interval(beat_position);
        let mut boundary_crossed = None;

        // Check for interval boundary
        match self.current_interval {
            Some(prev) if prev != interval_index => {
                boundary_crossed = Some(prev);
                self.swap_intervals(prev);
            }
            None => {
                // First process call — start recording
            }
            _ => {}
        }
        self.current_interval = Some(interval_index);

        // Record: append input to record slot
        self.record_slot.extend_from_slice(input);
        self.record_pos += input.len();

        // Playback: read from playback slot
        let available = self.playback_slot.len().saturating_sub(self.playback_pos);
        let to_read = available.min(output.len());

        if to_read > 0 {
            output[..to_read]
                .copy_from_slice(&self.playback_slot[self.playback_pos..self.playback_pos + to_read]);
            self.playback_pos += to_read;
        }

        // Fill remainder with silence
        for sample in &mut output[to_read..] {
            *sample = 0.0;
        }

        boundary_crossed
    }

    /// Feed a remote peer's decoded interval audio for playback.
    ///
    /// This will be mixed into the playback slot at the next interval boundary.
    /// Multiple peers' audio is summed together.
    pub fn feed_remote(&mut self, peer_id: &str, interval_index: i64, samples: Vec<f32>) {
        self.pending_remote.push(RemoteInterval {
            index: interval_index,
            peer_id: peer_id.to_string(),
            samples,
        });
    }

    /// Take completed intervals that are ready for encoding and transmission.
    pub fn take_completed(&mut self) -> Vec<CompletedInterval> {
        std::mem::take(&mut self.completed)
    }

    /// Update interval configuration (bars, quantum).
    pub fn set_config(&mut self, bars: u32, quantum: f64) {
        self.bars = bars.max(1);
        self.quantum = quantum.max(f64::EPSILON);
    }

    /// Reset all state.
    pub fn reset(&mut self) {
        self.record_slot.clear();
        self.playback_slot.clear();
        self.record_pos = 0;
        self.playback_pos = 0;
        self.current_interval = None;
        self.completed.clear();
        self.pending_remote.clear();
    }

    /// Current interval index, if any.
    pub fn current_interval(&self) -> Option<i64> {
        self.current_interval
    }

    /// Number of samples currently recorded in the record slot.
    pub fn record_position(&self) -> usize {
        self.record_pos
    }

    /// Number of samples remaining in the playback slot.
    pub fn playback_remaining(&self) -> usize {
        self.playback_slot.len().saturating_sub(self.playback_pos)
    }

    /// Number of remote intervals pending for next playback.
    pub fn pending_remote_count(&self) -> usize {
        self.pending_remote.len()
    }

    /// Convert a beat position to an interval index.
    fn beat_to_interval(&self, beat: f64) -> i64 {
        let beats_per_interval = self.bars as f64 * self.quantum;
        (beat / beats_per_interval).floor() as i64
    }

    /// Swap intervals: move record → completed, mix pending remote → playback.
    fn swap_intervals(&mut self, completed_index: i64) {
        // Move the recorded audio to the completed queue
        if !self.record_slot.is_empty() {
            let samples = std::mem::take(&mut self.record_slot);
            self.completed.push(CompletedInterval {
                index: completed_index,
                samples,
            });
        }
        self.record_pos = 0;

        // Mix pending remote intervals into the new playback slot
        self.playback_slot.clear();
        self.playback_pos = 0;

        let pending = std::mem::take(&mut self.pending_remote);
        for remote in pending {
            if self.playback_slot.is_empty() {
                // First remote: just copy
                self.playback_slot = remote.samples;
            } else {
                // Mix (sum) subsequent remotes into the playback slot
                let mix_len = self.playback_slot.len().max(remote.samples.len());
                self.playback_slot.resize(mix_len, 0.0);
                for (i, sample) in remote.samples.iter().enumerate() {
                    self.playback_slot[i] += sample;
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const SR: u32 = 48000;
    const CH: u16 = 2;
    const BARS: u32 = 4;
    const QUANTUM: f64 = 4.0;
    // 4 bars * 4 beats = 16 beats per interval

    fn make_ring() -> IntervalRing {
        IntervalRing::new(SR, CH, BARS, QUANTUM)
    }

    // --- Test: Basic record and playback ---

    #[test]
    fn new_ring_starts_empty() {
        let ring = make_ring();
        assert_eq!(ring.current_interval(), None);
        assert_eq!(ring.record_position(), 0);
        assert_eq!(ring.playback_remaining(), 0);
        assert_eq!(ring.pending_remote_count(), 0);
    }

    #[test]
    fn process_records_input() {
        let mut ring = make_ring();
        let input = vec![0.5f32; 256];
        let mut output = vec![0.0f32; 256];

        // Beat 0.0 = interval 0
        let boundary = ring.process(&input, &mut output, 0.0);
        assert!(boundary.is_none()); // first call, no boundary
        assert_eq!(ring.current_interval(), Some(0));
        assert_eq!(ring.record_position(), 256);
    }

    #[test]
    fn process_outputs_silence_when_no_remote_audio() {
        let mut ring = make_ring();
        let input = vec![0.5f32; 256];
        let mut output = vec![1.0f32; 256]; // pre-fill with non-zero

        ring.process(&input, &mut output, 0.0);
        assert!(output.iter().all(|&s| s == 0.0), "Expected silence");
    }

    // --- Test: Interval boundary detection ---

    #[test]
    fn detects_interval_boundary() {
        let mut ring = make_ring();
        let input = vec![0.1f32; 128];
        let mut output = vec![0.0f32; 128];

        // Record in interval 0 (beat 0 to beat 15.9)
        ring.process(&input, &mut output, 0.0);
        ring.process(&input, &mut output, 8.0);
        ring.process(&input, &mut output, 15.0);

        // Cross into interval 1 (beat 16.0)
        let boundary = ring.process(&input, &mut output, 16.0);
        assert_eq!(boundary, Some(0), "Should detect boundary of interval 0");
        assert_eq!(ring.current_interval(), Some(1));
    }

    #[test]
    fn completed_interval_available_after_boundary() {
        let mut ring = make_ring();
        let input = vec![0.3f32; 128];
        let mut output = vec![0.0f32; 128];

        // Record in interval 0
        ring.process(&input, &mut output, 0.0);
        ring.process(&input, &mut output, 8.0);

        // No completed intervals yet
        assert!(ring.take_completed().is_empty());

        // Cross boundary
        ring.process(&input, &mut output, 16.0);

        // Now interval 0 should be completed
        let completed = ring.take_completed();
        assert_eq!(completed.len(), 1);
        assert_eq!(completed[0].index, 0);
        assert_eq!(completed[0].samples.len(), 256); // 2 calls * 128 samples
    }

    // --- Test: Remote audio playback ---

    #[test]
    fn plays_remote_audio_after_boundary() {
        let mut ring = make_ring();
        let input = vec![0.0f32; 128];
        let mut output = vec![0.0f32; 128];

        // Start in interval 0
        ring.process(&input, &mut output, 0.0);

        // Feed remote audio for next playback
        let remote_audio = vec![0.7f32; 128];
        ring.feed_remote("peer-a", 0, remote_audio);

        // Cross into interval 1 — remote audio should become playback
        ring.process(&input, &mut output, 16.0);

        // Output should now contain the remote audio
        assert!(output.iter().all(|&s| (s - 0.7).abs() < f32::EPSILON),
            "Output should be remote audio, got: {:?}", &output[..8]);
    }

    #[test]
    fn mixes_multiple_remote_peers() {
        let mut ring = make_ring();
        let input = vec![0.0f32; 128];
        let mut output = vec![0.0f32; 128];

        // Start in interval 0
        ring.process(&input, &mut output, 0.0);

        // Feed from two peers
        ring.feed_remote("peer-a", 0, vec![0.3f32; 128]);
        ring.feed_remote("peer-b", 0, vec![0.5f32; 128]);

        // Cross boundary — both should be mixed (summed)
        ring.process(&input, &mut output, 16.0);

        assert!(output.iter().all(|&s| (s - 0.8).abs() < 0.001),
            "Expected 0.3 + 0.5 = 0.8, got: {:?}", &output[..8]);
    }

    #[test]
    fn remote_audio_longer_than_buffer_spans_calls() {
        let mut ring = make_ring();
        let input = vec![0.0f32; 64];
        let mut output = vec![0.0f32; 64];

        // Start interval 0
        ring.process(&input, &mut output, 0.0);

        // Feed 256 samples of remote audio
        ring.feed_remote("peer-a", 0, vec![0.4f32; 256]);

        // Cross into interval 1
        ring.process(&input, &mut output, 16.0);
        assert!(output.iter().all(|&s| (s - 0.4).abs() < f32::EPSILON));
        assert_eq!(ring.playback_remaining(), 192); // 256 - 64

        // Second call still reads from the same playback slot
        ring.process(&input, &mut output, 16.5);
        assert!(output.iter().all(|&s| (s - 0.4).abs() < f32::EPSILON));
        assert_eq!(ring.playback_remaining(), 128); // 256 - 128
    }

    #[test]
    fn silence_after_playback_exhausted() {
        let mut ring = make_ring();
        let input = vec![0.0f32; 64];
        let mut output = vec![0.0f32; 64];

        ring.process(&input, &mut output, 0.0);
        ring.feed_remote("peer-a", 0, vec![0.5f32; 32]); // only 32 samples

        // Cross boundary
        ring.process(&input, &mut output, 16.0);

        // First 32 samples = remote audio, rest = silence
        assert!((output[0] - 0.5).abs() < f32::EPSILON);
        assert!((output[31] - 0.5).abs() < f32::EPSILON);
        assert_eq!(output[32], 0.0);
        assert_eq!(output[63], 0.0);
    }

    // --- Test: Multiple intervals ---

    #[test]
    fn multiple_interval_cycle() {
        let mut ring = make_ring();
        let ones = vec![1.0f32; 100];
        let twos = vec![2.0f32; 100];
        let mut output = vec![0.0f32; 100];

        // Interval 0: record ones
        ring.process(&ones, &mut output, 0.0);
        ring.process(&ones, &mut output, 8.0);

        // Feed remote for playback in interval 1
        ring.feed_remote("peer-a", 0, vec![0.9f32; 100]);

        // Interval 1: record twos, play remote
        ring.process(&twos, &mut output, 16.0);
        let completed = ring.take_completed();
        assert_eq!(completed.len(), 1);
        assert_eq!(completed[0].index, 0);
        assert!((output[0] - 0.9).abs() < f32::EPSILON);

        // Feed new remote for interval 2
        ring.feed_remote("peer-a", 1, vec![0.6f32; 100]);

        // Interval 2: record ones, play new remote
        ring.process(&ones, &mut output, 32.0);
        let completed = ring.take_completed();
        assert_eq!(completed.len(), 1);
        assert_eq!(completed[0].index, 1);
        // Completed interval 1 should contain twos
        assert!((completed[0].samples[0] - 2.0).abs() < f32::EPSILON);
        // Playback should be the new remote
        assert!((output[0] - 0.6).abs() < f32::EPSILON);
    }

    // --- Test: Configuration ---

    #[test]
    fn config_change_affects_interval_index() {
        let mut ring = make_ring(); // 4 bars * 4 quantum = 16 beats
        let input = vec![0.0f32; 64];
        let mut output = vec![0.0f32; 64];

        ring.process(&input, &mut output, 0.0);
        assert_eq!(ring.current_interval(), Some(0));

        // Beat 10 is still interval 0 (< 16)
        ring.process(&input, &mut output, 10.0);
        assert_eq!(ring.current_interval(), Some(0));

        // Change to 2 bars * 4 quantum = 8 beats per interval
        ring.set_config(2, 4.0);

        // Beat 10 is now interval 1 (10/8 = 1.25, floor = 1)
        let boundary = ring.process(&input, &mut output, 10.0);
        assert_eq!(boundary, Some(0)); // crossed from 0 to 1
        assert_eq!(ring.current_interval(), Some(1));
    }

    #[test]
    fn reset_clears_all_state() {
        let mut ring = make_ring();
        let input = vec![0.5f32; 128];
        let mut output = vec![0.0f32; 128];

        ring.process(&input, &mut output, 0.0);
        ring.feed_remote("peer-a", 0, vec![0.3f32; 64]);

        ring.reset();

        assert_eq!(ring.current_interval(), None);
        assert_eq!(ring.record_position(), 0);
        assert_eq!(ring.playback_remaining(), 0);
        assert_eq!(ring.pending_remote_count(), 0);
        assert!(ring.take_completed().is_empty());
    }

    // --- Test: Beat position edge cases ---

    #[test]
    fn negative_beat_position() {
        let mut ring = make_ring();
        let input = vec![0.0f32; 64];
        let mut output = vec![0.0f32; 64];

        // Negative beats (pre-roll)
        ring.process(&input, &mut output, -4.0);
        assert_eq!(ring.current_interval(), Some(-1));
    }

    #[test]
    fn fractional_beat_position() {
        let mut ring = make_ring();
        let input = vec![0.0f32; 64];
        let mut output = vec![0.0f32; 64];

        // Beat 15.999 is still interval 0
        ring.process(&input, &mut output, 15.999);
        assert_eq!(ring.current_interval(), Some(0));

        // Beat 16.001 is interval 1
        let boundary = ring.process(&input, &mut output, 16.001);
        assert_eq!(boundary, Some(0));
        assert_eq!(ring.current_interval(), Some(1));
    }

    #[test]
    fn zero_bars_clamped_to_one() {
        let mut ring = IntervalRing::new(SR, CH, 0, QUANTUM);
        let input = vec![0.0f32; 64];
        let mut output = vec![0.0f32; 64];
        // Must not panic — bars=0 is clamped to 1, so interval = 1*4 = 4 beats
        ring.process(&input, &mut output, 0.0);
        assert_eq!(ring.current_interval(), Some(0));
    }

    #[test]
    fn zero_quantum_clamped() {
        let mut ring = IntervalRing::new(SR, CH, BARS, 0.0);
        let input = vec![0.0f32; 64];
        let mut output = vec![0.0f32; 64];
        // Must not panic or produce NaN interval index
        ring.process(&input, &mut output, 10.0);
        assert!(ring.current_interval().is_some());
    }

    #[test]
    fn set_config_clamps_zero_values() {
        let mut ring = make_ring();
        let input = vec![0.0f32; 64];
        let mut output = vec![0.0f32; 64];

        ring.process(&input, &mut output, 0.0);
        ring.set_config(0, 0.0);
        // Must not panic
        ring.process(&input, &mut output, 10.0);
        assert!(ring.current_interval().is_some());
    }
}
