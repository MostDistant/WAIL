use std::collections::HashMap;

use crate::interval::AudioFrame;

struct FrameCollection {
    frames: Vec<Option<Vec<u8>>>,
    channels: u16,
    sample_rate: u32,
    bpm: f64,
    quantum: f64,
    bars: u32,
}

/// A fully assembled audio interval ready for Opus decoding.
pub struct AssembledInterval {
    pub peer_id: String,
    pub stream_id: u16,
    pub interval_index: i64,
    pub channels: u16,
    pub sample_rate: u32,
    pub bpm: f64,
    pub quantum: f64,
    pub bars: u32,
    /// Length-prefixed Opus blob: `[u32 LE frame_count][u16 LE len][bytes]…`
    /// Matches the format returned by [`crate::AudioEncoder::encode_interval`]
    /// and consumed by [`crate::AudioDecoder::decode_interval`].
    pub opus_data: Vec<u8>,
    /// Total frames declared by the sender (from `total_frames` in the final frame).
    pub frames_expected: u32,
    /// Frames actually received (non-gap). `frames_expected - frames_received` = dropped.
    pub frames_received: u32,
}

/// Assembles WAIF streaming frames into complete Opus interval blobs.
///
/// Keyed by `(interval_index, stream_id, peer_id)`. Collects per-frame Opus
/// packets and, when the final frame arrives, assembles them into the
/// length-prefixed format that [`crate::AudioDecoder::decode_interval`] expects.
pub struct FrameAssembler {
    pending: HashMap<(i64, u16, String), FrameCollection>,
}

impl Default for FrameAssembler {
    fn default() -> Self {
        Self {
            pending: HashMap::new(),
        }
    }
}

impl FrameAssembler {
    pub fn new() -> Self {
        Self::default()
    }

    /// Insert a WAIF frame.
    ///
    /// Returns [`AssembledInterval`] when the final frame for an interval
    /// arrives, or `None` if more frames are still needed.
    pub fn insert(&mut self, peer_id: &str, frame: &AudioFrame) -> Option<AssembledInterval> {
        let key = (frame.interval_index, frame.stream_id, peer_id.to_string());

        let collection = self.pending.entry(key.clone()).or_insert_with(|| FrameCollection {
            frames: Vec::new(),
            channels: frame.channels,
            sample_rate: 0,
            bpm: 0.0,
            quantum: 0.0,
            bars: 0,
        });

        let idx = frame.frame_number as usize;
        const MAX_FRAMES_PER_INTERVAL: usize = 10_000;
        if idx >= MAX_FRAMES_PER_INTERVAL {
            tracing::warn!(
                frame_number = idx,
                "FrameAssembler: frame_number exceeds maximum, dropping"
            );
            return None;
        }
        if collection.frames.len() <= idx {
            collection.frames.resize(idx + 1, None);
        }
        collection.frames[idx] = Some(frame.opus_data.clone());

        if frame.is_final {
            collection.sample_rate = frame.sample_rate;
            collection.bpm = frame.bpm;
            collection.quantum = frame.quantum;
            collection.bars = frame.bars;

            let total = frame.total_frames as usize;
            let Some(coll) = self.pending.remove(&key) else {
                tracing::warn!("FrameAssembler: missing collection for key after insert");
                return None;
            };

            // Assemble into length-prefixed format:
            // [u32 LE frame_count][u16 LE len][opus bytes]...
            let mut opus_data = Vec::new();
            let mut received: u32 = 0;
            opus_data.extend_from_slice(&(total as u32).to_le_bytes());
            for i in 0..total {
                if let Some(Some(data)) = coll.frames.get(i) {
                    opus_data.extend_from_slice(&(data.len() as u16).to_le_bytes());
                    opus_data.extend_from_slice(data);
                    received += 1;
                } else {
                    // Missing frame — insert zero-length entry; decoder treats as gap
                    opus_data.extend_from_slice(&0u16.to_le_bytes());
                }
            }

            return Some(AssembledInterval {
                peer_id: peer_id.to_string(),
                stream_id: frame.stream_id,
                interval_index: frame.interval_index,
                channels: coll.channels,
                sample_rate: coll.sample_rate,
                bpm: coll.bpm,
                quantum: coll.quantum,
                bars: coll.bars,
                opus_data,
                frames_expected: total as u32,
                frames_received: received,
            });
        }

        None
    }

    /// Evict stale in-progress collections for intervals older than `current - 2`.
    pub fn evict_stale(&mut self, current_interval: i64) {
        self.pending
            .retain(|&(idx, _, _), _| idx >= current_interval - 2);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::interval::AudioFrame;

    #[test]
    fn missing_frames_produce_zero_length_entries() {
        let mut assembler = FrameAssembler::new();

        // Send frame 0
        let result = assembler.insert("peer-a", &AudioFrame {
            interval_index: 1,
            stream_id: 0,
            frame_number: 0,
            frame_seq: 0,
            channels: 2,
            opus_data: vec![1, 2, 3],
            is_final: false,
            sample_rate: 48000,
            total_frames: 0,
            bpm: 120.0,
            quantum: 4.0,
            bars: 4,
        });
        assert!(result.is_none());

        // Skip frame 1 (simulates network loss)

        // Send frame 2 as final, total_frames=3
        let result = assembler.insert("peer-a", &AudioFrame {
            interval_index: 1,
            stream_id: 0,
            frame_number: 2,
            frame_seq: 2,
            channels: 2,
            opus_data: vec![7, 8, 9],
            is_final: true,
            sample_rate: 48000,
            total_frames: 3,
            bpm: 120.0,
            quantum: 4.0,
            bars: 4,
        });
        let assembled = result.expect("should assemble on final frame");
        assert_eq!(assembled.frames_expected, 3);
        assert_eq!(assembled.frames_received, 2); // frame 1 was missing

        // Verify the blob: frame_count=3, frame0=real, frame1=zero-length, frame2=real
        let blob = &assembled.opus_data;
        let frame_count = u32::from_le_bytes([blob[0], blob[1], blob[2], blob[3]]);
        assert_eq!(frame_count, 3);

        // Frame 0: len=3, data=[1,2,3]
        let len0 = u16::from_le_bytes([blob[4], blob[5]]) as usize;
        assert_eq!(len0, 3);
        assert_eq!(&blob[6..9], &[1, 2, 3]);

        // Frame 1: len=0 (missing)
        let len1 = u16::from_le_bytes([blob[9], blob[10]]) as usize;
        assert_eq!(len1, 0, "missing frame should have zero-length entry");

        // Frame 2: len=3, data=[7,8,9]
        let len2 = u16::from_le_bytes([blob[11], blob[12]]) as usize;
        assert_eq!(len2, 3);
        assert_eq!(&blob[13..16], &[7, 8, 9]);
    }
}
