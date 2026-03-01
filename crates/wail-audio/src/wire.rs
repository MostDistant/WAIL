use anyhow::Result;

/// Binary wire format for audio intervals over WebRTC DataChannels.
///
/// Format (all integers are little-endian):
/// ```text
/// [4 bytes] magic: "WAIL"
/// [1 byte]  version: 1
/// [1 byte]  flags: bit 0 = stereo (0=mono, 1=stereo)
/// [2 bytes] reserved: 0
/// [8 bytes] interval_index: i64
/// [4 bytes] sample_rate: u32
/// [4 bytes] num_frames: u32 (source samples per channel)
/// [8 bytes] bpm: f64
/// [8 bytes] quantum: f64
/// [4 bytes] bars: u32
/// [4 bytes] opus_data_len: u32
/// [N bytes] opus_data
/// ```
///
/// Total header: 48 bytes + opus_data
pub struct AudioWire;

const MAGIC: &[u8; 4] = b"WAIL";
const VERSION: u8 = 1;
const HEADER_SIZE: usize = 48;

impl AudioWire {
    /// Serialize an AudioInterval into the binary wire format.
    pub fn encode(interval: &super::AudioInterval) -> Vec<u8> {
        let mut buf = Vec::with_capacity(HEADER_SIZE + interval.opus_data.len());

        // Magic
        buf.extend_from_slice(MAGIC);
        // Version
        buf.push(VERSION);
        // Flags: bit 0 = stereo
        buf.push(if interval.channels == 2 { 1 } else { 0 });
        // Reserved
        buf.extend_from_slice(&[0u8; 2]);
        // Interval index
        buf.extend_from_slice(&interval.index.to_le_bytes());
        // Sample rate
        buf.extend_from_slice(&interval.sample_rate.to_le_bytes());
        // Num frames
        buf.extend_from_slice(&interval.num_frames.to_le_bytes());
        // BPM
        buf.extend_from_slice(&interval.bpm.to_le_bytes());
        // Quantum
        buf.extend_from_slice(&interval.quantum.to_le_bytes());
        // Bars
        buf.extend_from_slice(&interval.bars.to_le_bytes());
        // Opus data length
        buf.extend_from_slice(&(interval.opus_data.len() as u32).to_le_bytes());
        // Opus data
        buf.extend_from_slice(&interval.opus_data);

        buf
    }

    /// Deserialize the binary wire format into an AudioInterval.
    pub fn decode(data: &[u8]) -> Result<super::AudioInterval> {
        if data.len() < HEADER_SIZE {
            anyhow::bail!(
                "Audio wire data too short: {} bytes, need at least {HEADER_SIZE}",
                data.len()
            );
        }

        // Magic
        if &data[0..4] != MAGIC {
            anyhow::bail!("Invalid audio wire magic: {:?}", &data[0..4]);
        }

        // Version
        let version = data[4];
        if version != VERSION {
            anyhow::bail!("Unsupported audio wire version: {version}");
        }

        // Flags
        let flags = data[5];
        let channels = if flags & 1 != 0 { 2 } else { 1 };

        // Interval index
        let index = i64::from_le_bytes(data[8..16].try_into()?);

        // Sample rate
        let sample_rate = u32::from_le_bytes(data[16..20].try_into()?);

        // Num frames
        let num_frames = u32::from_le_bytes(data[20..24].try_into()?);

        // BPM
        let bpm = f64::from_le_bytes(data[24..32].try_into()?);

        // Quantum
        let quantum = f64::from_le_bytes(data[32..40].try_into()?);

        // Bars
        let bars = u32::from_le_bytes(data[40..44].try_into()?);

        // Opus data length
        let opus_len = u32::from_le_bytes(data[44..48].try_into()?) as usize;

        if data.len() < HEADER_SIZE + opus_len {
            anyhow::bail!(
                "Audio wire data truncated: expected {} bytes of opus data, got {}",
                opus_len,
                data.len() - HEADER_SIZE
            );
        }

        let opus_data = data[HEADER_SIZE..HEADER_SIZE + opus_len].to_vec();

        Ok(super::AudioInterval {
            index,
            opus_data,
            sample_rate,
            channels,
            num_frames,
            bpm,
            quantum,
            bars,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::AudioInterval;

    #[test]
    fn wire_roundtrip() {
        let interval = AudioInterval {
            index: 42,
            opus_data: vec![1, 2, 3, 4, 5],
            sample_rate: 48000,
            channels: 2,
            num_frames: 96000,
            bpm: 120.0,
            quantum: 4.0,
            bars: 4,
        };

        let encoded = AudioWire::encode(&interval);
        assert_eq!(&encoded[0..4], b"WAIL");
        assert_eq!(encoded[4], 1); // version
        assert_eq!(encoded[5], 1); // stereo flag

        let decoded = AudioWire::decode(&encoded).unwrap();
        assert_eq!(decoded.index, 42);
        assert_eq!(decoded.sample_rate, 48000);
        assert_eq!(decoded.channels, 2);
        assert_eq!(decoded.num_frames, 96000);
        assert!((decoded.bpm - 120.0).abs() < f64::EPSILON);
        assert!((decoded.quantum - 4.0).abs() < f64::EPSILON);
        assert_eq!(decoded.bars, 4);
        assert_eq!(decoded.opus_data, vec![1, 2, 3, 4, 5]);
    }

    #[test]
    fn wire_mono() {
        let interval = AudioInterval {
            index: 0,
            opus_data: vec![],
            sample_rate: 48000,
            channels: 1,
            num_frames: 48000,
            bpm: 90.0,
            quantum: 3.0,
            bars: 2,
        };

        let encoded = AudioWire::encode(&interval);
        assert_eq!(encoded[5], 0); // mono flag

        let decoded = AudioWire::decode(&encoded).unwrap();
        assert_eq!(decoded.channels, 1);
    }

    #[test]
    fn wire_rejects_bad_magic() {
        let mut data = vec![0u8; 48];
        data[0..4].copy_from_slice(b"NOPE");
        assert!(AudioWire::decode(&data).is_err());
    }

    #[test]
    fn wire_rejects_truncated() {
        let data = vec![0u8; 10];
        assert!(AudioWire::decode(&data).is_err());
    }
}
