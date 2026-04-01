//! Shared test tone generation and audio validation utilities.
//!
//! Used by both `wail-e2e` (two-machine tests) and `wail-tauri` (test mode)
//! to generate synthetic audio and validate received audio without a DAW.

use anyhow::{bail, Result};

use crate::wire::AudioFrameWire;

/// Result of validating received audio data.
pub struct AudioValidation {
    /// Wire format: "WAIF" (frame)
    pub format: String,
    /// Total wire size in bytes
    pub size_bytes: usize,
    /// RMS energy of decoded PCM (0.0 = silence)
    pub rms: f32,
    /// Human-readable detail string
    pub detail: String,
}

/// Generate a single 20ms stereo sine frame (960 samples/channel at 48kHz).
///
/// `phase` is updated in-place for continuous waveform across calls.
/// Returns interleaved stereo f32 samples.
pub fn generate_sine_frame(freq: f32, phase: &mut f64, sample_rate: u32, channels: u16) -> Vec<f32> {
    let samples_per_frame = (sample_rate as f64 * 0.020) as usize; // 20ms
    let num_samples = samples_per_frame * channels as usize;
    let phase_inc = freq as f64 / sample_rate as f64;
    let mut samples = vec![0.0f32; num_samples];

    for i in 0..samples_per_frame {
        let val = (2.0 * std::f64::consts::PI * *phase).sin() as f32 * 0.5;
        for ch in 0..channels as usize {
            samples[i * channels as usize + ch] = val;
        }
        *phase += phase_inc;
    }
    samples
}

/// Compute the expected number of 20ms frames in one interval.
pub fn frames_per_interval(bpm: f64, bars: u32, quantum: f64) -> u32 {
    let beats = bars as f64 * quantum;
    let seconds = beats / (bpm / 60.0);
    (seconds / 0.020).round().max(1.0) as u32
}

/// Validate received audio wire data: decode, check format, return details.
pub fn validate_audio(data: &[u8]) -> Result<AudioValidation> {
    if data.len() < 4 {
        bail!("audio data too short ({} bytes)", data.len());
    }

    if &data[0..4] == b"WAIF" {
        let frame = AudioFrameWire::decode(data)?;
        let detail = format!(
            "WAIF frame: {} bytes, frame #{}, interval {}, final={}",
            data.len(),
            frame.frame_number,
            frame.interval_index,
            frame.is_final,
        );

        Ok(AudioValidation {
            format: "WAIF".into(),
            size_bytes: data.len(),
            rms: 0.0,
            detail,
        })
    } else {
        bail!(
            "unknown wire format: magic={:?}",
            &data[..data.len().min(4)]
        );
    }
}

/// RMS (root mean square) energy of an audio buffer.
pub fn rms(samples: &[f32]) -> f32 {
    if samples.is_empty() {
        return 0.0;
    }
    let sum: f64 = samples.iter().map(|&s| (s as f64) * (s as f64)).sum();
    (sum / samples.len() as f64).sqrt() as f32
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn generate_sine_frame_correct_length() {
        let mut phase = 0.0;
        let samples = generate_sine_frame(440.0, &mut phase, 48000, 2);
        assert_eq!(samples.len(), 960 * 2); // 20ms at 48kHz stereo
        assert!(phase > 0.0);
    }

    #[test]
    fn generate_sine_frame_nonzero_energy() {
        let mut phase = 0.0;
        let samples = generate_sine_frame(440.0, &mut phase, 48000, 2);
        assert!(rms(&samples) > 0.1);
    }

    #[test]
    fn frames_per_interval_120bpm_4bars() {
        assert_eq!(frames_per_interval(120.0, 4, 4.0), 400);
    }

    #[test]
    fn rms_of_silence_is_zero() {
        let silence = vec![0.0f32; 1920];
        assert_eq!(rms(&silence), 0.0);
    }

    #[test]
    fn validate_rejects_garbage() {
        let garbage = vec![0u8; 10];
        assert!(validate_audio(&garbage).is_err());
    }

    #[test]
    fn validate_rejects_short_data() {
        let short = vec![0u8; 2];
        assert!(validate_audio(&short).is_err());
    }
}
