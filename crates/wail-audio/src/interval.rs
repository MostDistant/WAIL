use std::collections::VecDeque;

/// A complete audio interval ready for transmission or playback.
#[derive(Debug, Clone)]
pub struct AudioInterval {
    /// Interval index (monotonically increasing per the Link beat grid)
    pub index: i64,
    /// Stream index within a peer (0 = default, supports multi-stream send)
    pub stream_id: u16,
    /// Opus-encoded audio data (length-prefixed frames)
    pub opus_data: Vec<u8>,
    /// Sample rate of the source audio
    pub sample_rate: u32,
    /// Number of channels (1=mono, 2=stereo)
    pub channels: u16,
    /// Total number of source samples per channel before encoding
    pub num_frames: u32,
    /// BPM at the time of recording
    pub bpm: f64,
    /// Quantum (beats per bar) at the time of recording
    pub quantum: f64,
    /// Bars per interval
    pub bars: u32,
}

/// Records audio samples into intervals, triggering encoding at interval boundaries.
///
/// Usage in an audio processing callback:
/// 1. Call `push_samples()` with each audio buffer from the DAW
/// 2. Call `finish_interval()` at interval boundaries to get the encoded interval
pub struct IntervalRecorder {
    /// Accumulated interleaved f32 samples for the current interval
    buffer: Vec<f32>,
    /// Current interval index being recorded
    current_index: Option<i64>,
    /// Audio parameters
    sample_rate: u32,
    channels: u16,
}

impl IntervalRecorder {
    pub fn new(sample_rate: u32, channels: u16) -> Self {
        // Pre-allocate for ~8 bars at 120bpm, 4/4 time = 16 beats = 8 seconds
        let estimated_capacity = sample_rate as usize * channels as usize * 8;
        Self {
            buffer: Vec::with_capacity(estimated_capacity),
            current_index: None,
            sample_rate,
            channels,
        }
    }

    /// Push interleaved f32 samples into the current interval buffer.
    pub fn push_samples(&mut self, samples: &[f32], interval_index: i64) {
        // If the interval changed, discard the buffer (caller should have called finish_interval)
        if self.current_index.is_some() && self.current_index != Some(interval_index) {
            tracing::warn!(
                old = ?self.current_index,
                new = interval_index,
                "Interval changed without finish_interval — discarding buffer"
            );
            self.buffer.clear();
        }
        self.current_index = Some(interval_index);
        self.buffer.extend_from_slice(samples);
    }

    /// Finish the current interval and return the raw samples for encoding.
    /// Resets the buffer for the next interval.
    pub fn finish_interval(&mut self) -> Option<(i64, Vec<f32>)> {
        let index = self.current_index.take()?;
        if self.buffer.is_empty() {
            return None;
        }
        let samples = std::mem::take(&mut self.buffer);
        Some((index, samples))
    }

    pub fn sample_rate(&self) -> u32 {
        self.sample_rate
    }

    pub fn channels(&self) -> u16 {
        self.channels
    }

    pub fn is_recording(&self) -> bool {
        self.current_index.is_some()
    }
}

/// Plays back received audio intervals with crossfade between consecutive intervals.
///
/// Maintains a queue of decoded intervals and reads samples sequentially.
pub struct IntervalPlayer {
    /// Queue of decoded interval audio (interleaved f32)
    queue: VecDeque<DecodedInterval>,
    /// Read position in the current interval
    read_pos: usize,
    /// Audio parameters
    sample_rate: u32,
    channels: u16,
    /// Crossfade length in samples per channel
    crossfade_samples: usize,
}

#[allow(dead_code)] // index retained for future interval ordering/debugging
struct DecodedInterval {
    index: i64,
    samples: Vec<f32>,
}

impl IntervalPlayer {
    /// Create a new interval player.
    ///
    /// `crossfade_ms`: Duration of crossfade between consecutive intervals (default 10ms)
    pub fn new(sample_rate: u32, channels: u16, crossfade_ms: u32) -> Self {
        let crossfade_samples = (sample_rate as usize * crossfade_ms as usize) / 1000;
        Self {
            queue: VecDeque::with_capacity(4),
            read_pos: 0,
            sample_rate,
            channels,
            crossfade_samples,
        }
    }

    /// Enqueue a decoded interval for playback.
    pub fn enqueue(&mut self, index: i64, samples: Vec<f32>) {
        // Drop intervals that are too old (keep max 2 ahead)
        while self.queue.len() >= 3 {
            self.queue.pop_front();
            self.read_pos = 0;
        }
        self.queue.push_back(DecodedInterval { index, samples });
    }

    /// Read interleaved f32 samples for playback.
    /// Fills the output buffer, returns the number of samples written.
    /// Outputs silence (zeros) if no intervals are queued.
    pub fn read_samples(&mut self, output: &mut [f32]) -> usize {
        let mut written = 0;
        let ch = self.channels as usize;

        while written < output.len() {
            let current = match self.queue.front() {
                Some(interval) => interval,
                None => {
                    // Silence
                    for sample in &mut output[written..] {
                        *sample = 0.0;
                    }
                    return output.len();
                }
            };

            let remaining_in_interval = current.samples.len().saturating_sub(self.read_pos);
            let remaining_in_output = output.len() - written;
            let to_copy = remaining_in_interval.min(remaining_in_output);

            if to_copy > 0 {
                // Apply crossfade at the beginning of a new interval
                let crossfade_total = self.crossfade_samples * ch;
                if self.read_pos < crossfade_total {
                    for i in 0..to_copy {
                        let pos = self.read_pos + i;
                        let fade = if pos < crossfade_total {
                            pos as f32 / crossfade_total as f32
                        } else {
                            1.0
                        };
                        output[written + i] = current.samples[pos] * fade;
                    }
                } else {
                    output[written..written + to_copy]
                        .copy_from_slice(&current.samples[self.read_pos..self.read_pos + to_copy]);
                }

                self.read_pos += to_copy;
                written += to_copy;
            }

            if self.read_pos >= current.samples.len() {
                self.queue.pop_front();
                self.read_pos = 0;
            }
        }

        written
    }

    /// Number of intervals currently queued.
    pub fn queued_intervals(&self) -> usize {
        self.queue.len()
    }

    /// Clear all queued intervals and reset playback state.
    pub fn clear(&mut self) {
        self.queue.clear();
        self.read_pos = 0;
    }

    pub fn sample_rate(&self) -> u32 {
        self.sample_rate
    }

    pub fn channels(&self) -> u16 {
        self.channels
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn recorder_basic_flow() {
        let mut rec = IntervalRecorder::new(48000, 2);

        // Push some samples for interval 0
        rec.push_samples(&[0.1, 0.2, 0.3, 0.4], 0);
        rec.push_samples(&[0.5, 0.6], 0);
        assert!(rec.is_recording());

        // Finish interval
        let (idx, samples) = rec.finish_interval().unwrap();
        assert_eq!(idx, 0);
        assert_eq!(samples.len(), 6);
        assert!(!rec.is_recording());
    }

    #[test]
    fn player_outputs_silence_when_empty() {
        let mut player = IntervalPlayer::new(48000, 2, 10);
        let mut output = vec![1.0f32; 100];
        player.read_samples(&mut output);
        assert!(output.iter().all(|&s| s == 0.0));
    }

    #[test]
    fn player_reads_enqueued_samples() {
        let mut player = IntervalPlayer::new(48000, 2, 0); // no crossfade for test
        let samples = vec![0.5f32; 200];
        player.enqueue(0, samples);

        let mut output = vec![0.0f32; 100];
        player.read_samples(&mut output);
        assert!(output.iter().all(|&s| s == 0.5));

        // Read rest
        player.read_samples(&mut output);
        assert!(output.iter().all(|&s| s == 0.5));

        // Now empty — should be silence
        let mut output = vec![1.0f32; 50];
        player.read_samples(&mut output);
        assert!(output.iter().all(|&s| s == 0.0));
    }
}
