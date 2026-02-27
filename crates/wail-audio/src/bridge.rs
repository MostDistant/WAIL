use crate::codec::{AudioDecoder, AudioEncoder};
use crate::interval::AudioInterval;
use crate::ring::IntervalRing;
use crate::wire::AudioWire;

/// AudioBridge: connects the IntervalRing to Opus encoding and IPC framing.
///
/// This replaces the original AudioBridge from wail-plugin with one backed
/// by the NINJAM-style IntervalRing. It lives in wail-audio so both the
/// plugin and the app can use it.
///
/// Responsibilities:
/// 1. Wrap IntervalRing's process() for the audio thread
/// 2. Opus-encode completed intervals into wire-ready bytes
/// 3. Opus-decode incoming wire bytes and feed them to the ring
pub struct AudioBridge {
    ring: IntervalRing,
    encoder: Option<AudioEncoder>,
    decoder: Option<AudioDecoder>,
    sample_rate: u32,
    channels: u16,
    bitrate_kbps: u32,
    bpm: f64,
    bars: u32,
    quantum: f64,
}

impl AudioBridge {
    pub fn new(sample_rate: u32, channels: u16, bars: u32, quantum: f64, bitrate_kbps: u32) -> Self {
        let encoder = AudioEncoder::new(sample_rate, channels, bitrate_kbps).ok();
        let decoder = AudioDecoder::new(sample_rate, channels).ok();

        Self {
            ring: IntervalRing::new(sample_rate, channels, bars, quantum),
            encoder,
            decoder,
            sample_rate,
            channels,
            bitrate_kbps,
            bpm: 120.0,
            bars,
            quantum,
        }
    }

    /// Process one audio buffer from the DAW. Records input, outputs playback.
    /// Returns wire-encoded bytes for any interval that just completed.
    pub fn process(
        &mut self,
        input: &[f32],
        output: &mut [f32],
        beat_position: f64,
    ) -> Vec<Vec<u8>> {
        // Drive the ring buffer
        self.ring.process(input, output, beat_position);

        // Check for completed intervals and encode them
        let completed = self.ring.take_completed();
        let mut wire_messages = Vec::new();

        for interval in completed {
            if let Some(ref mut encoder) = self.encoder {
                match encoder.encode_interval(&interval.samples) {
                    Ok(opus_data) => {
                        let num_frames =
                            (interval.samples.len() / self.channels as usize) as u32;
                        let audio_interval = AudioInterval {
                            index: interval.index,
                            opus_data,
                            sample_rate: self.sample_rate,
                            channels: self.channels,
                            num_frames,
                            bpm: self.bpm,
                            quantum: self.quantum,
                            bars: self.bars,
                        };
                        wire_messages.push(AudioWire::encode(&audio_interval));
                    }
                    Err(e) => {
                        tracing::warn!(error = %e, "Failed to encode completed interval");
                    }
                }
            }
        }

        wire_messages
    }

    /// Receive a wire-encoded audio interval from a remote peer.
    /// Decodes Opus and feeds to the ring for playback.
    pub fn receive_wire(&mut self, peer_id: &str, wire_data: &[u8]) {
        let interval = match AudioWire::decode(wire_data) {
            Ok(i) => i,
            Err(e) => {
                tracing::warn!(error = %e, "Failed to decode wire data");
                return;
            }
        };

        if let Some(ref mut decoder) = self.decoder {
            match decoder.decode_interval(&interval.opus_data) {
                Ok(samples) => {
                    self.ring.feed_remote(peer_id, interval.index, samples);
                }
                Err(e) => {
                    tracing::warn!(error = %e, "Failed to decode Opus audio");
                }
            }
        }
    }

    /// Update tempo/config from DAW transport.
    pub fn update_config(&mut self, bars: u32, quantum: f64, bpm: f64) {
        self.bars = bars;
        self.quantum = quantum;
        self.bpm = bpm;
        self.ring.set_config(bars, quantum);
    }

    /// Reset all state (on transport stop, etc.)
    pub fn reset(&mut self) {
        self.ring.reset();
        self.encoder = AudioEncoder::new(self.sample_rate, self.channels, self.bitrate_kbps).ok();
        self.decoder = AudioDecoder::new(self.sample_rate, self.channels).ok();
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
    use crate::codec::AudioEncoder;
    use crate::interval::AudioInterval;

    fn make_bridge() -> AudioBridge {
        AudioBridge::new(48000, 2, 4, 4.0, 128)
    }

    // --- Construction ---

    #[test]
    fn creates_with_valid_params() {
        let bridge = make_bridge();
        assert_eq!(bridge.sample_rate(), 48000);
        assert_eq!(bridge.channels(), 2);
    }

    // --- Process: recording and encoding ---

    #[test]
    fn process_returns_empty_within_interval() {
        let mut bridge = make_bridge();
        let input = vec![0.5f32; 256];
        let mut output = vec![0.0f32; 256];

        let outgoing = bridge.process(&input, &mut output, 0.0);
        assert!(outgoing.is_empty());

        let outgoing = bridge.process(&input, &mut output, 8.0);
        assert!(outgoing.is_empty());
    }

    #[test]
    fn process_returns_wire_bytes_at_boundary() {
        let mut bridge = make_bridge();
        let input = vec![0.5f32; 256];
        let mut output = vec![0.0f32; 256];

        bridge.process(&input, &mut output, 0.0);
        bridge.process(&input, &mut output, 8.0);

        let outgoing = bridge.process(&input, &mut output, 16.0);
        assert_eq!(outgoing.len(), 1, "Should produce one encoded interval");

        let decoded = AudioWire::decode(&outgoing[0]).unwrap();
        assert_eq!(decoded.index, 0);
        assert_eq!(decoded.sample_rate, 48000);
        assert_eq!(decoded.channels, 2);
        assert!(!decoded.opus_data.is_empty(), "Opus data should not be empty");
    }

    #[test]
    fn wire_output_carries_correct_metadata() {
        let mut bridge = AudioBridge::new(48000, 2, 2, 3.0, 96);
        bridge.update_config(2, 3.0, 145.0);
        let input = vec![0.1f32; 128];
        let mut output = vec![0.0f32; 128];

        bridge.process(&input, &mut output, 0.0);
        let outgoing = bridge.process(&input, &mut output, 6.0);

        let decoded = AudioWire::decode(&outgoing[0]).unwrap();
        assert_eq!(decoded.bars, 2);
        assert!((decoded.quantum - 3.0).abs() < f64::EPSILON);
        assert!((decoded.bpm - 145.0).abs() < f64::EPSILON);
    }

    // --- Process: playback ---

    #[test]
    fn process_outputs_silence_with_no_remote() {
        let mut bridge = make_bridge();
        let input = vec![0.0f32; 128];
        let mut output = vec![1.0f32; 128];

        bridge.process(&input, &mut output, 0.0);
        assert!(output.iter().all(|&s| s == 0.0));
    }

    #[test]
    fn receive_wire_then_playback() {
        let mut bridge = make_bridge();
        let input = vec![0.0f32; 128];
        let mut output = vec![0.0f32; 128];

        bridge.process(&input, &mut output, 0.0);

        // Simulate receiving a remote interval
        let mut encoder = AudioEncoder::new(48000, 2, 128).unwrap();
        let remote_samples = vec![0.3f32; 1920];
        let opus_data = encoder.encode_interval(&remote_samples).unwrap();
        let wire = AudioWire::encode(&AudioInterval {
            index: 0,
            opus_data,
            sample_rate: 48000,
            channels: 2,
            num_frames: 960,
            bpm: 120.0,
            quantum: 4.0,
            bars: 4,
        });

        bridge.receive_wire("peer-a", &wire);

        // Cross boundary
        bridge.process(&input, &mut output, 16.0);

        let energy: f32 = output.iter().map(|s| s.abs()).sum();
        assert!(energy > 0.0, "Playback should contain decoded remote audio");
    }

    // --- Config updates ---

    #[test]
    fn update_config_changes_bpm_in_output() {
        let mut bridge = make_bridge();
        bridge.update_config(4, 4.0, 180.0);

        let input = vec![0.1f32; 64];
        let mut output = vec![0.0f32; 64];
        bridge.process(&input, &mut output, 0.0);
        let outgoing = bridge.process(&input, &mut output, 16.0);

        let decoded = AudioWire::decode(&outgoing[0]).unwrap();
        assert!((decoded.bpm - 180.0).abs() < f64::EPSILON);
    }

    // --- Reset ---

    #[test]
    fn reset_clears_state() {
        let mut bridge = make_bridge();
        let input = vec![0.5f32; 128];
        let mut output = vec![0.0f32; 128];

        bridge.process(&input, &mut output, 0.0);
        bridge.reset();

        let outgoing = bridge.process(&input, &mut output, 0.0);
        assert!(outgoing.is_empty());
    }
}
