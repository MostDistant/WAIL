//! FFT-based audio analysis for validating received audio in cross-network e2e tests.
//!
//! Provides frequency detection, dropout analysis, and seam (click/pop) checking
//! on a per-bar and per-interval basis.

use rustfft::{num_complex::Complex, FftPlanner};
use serde::Serialize;

use crate::test_tone::rms;

const FFT_SIZE: usize = 8192;
const DEFAULT_SEAM_THRESHOLD: f32 = 0.3;
const SILENCE_RMS_THRESHOLD: f32 = 0.001;
const FREQ_TOLERANCE_HZ: f32 = 10.0;

/// Analysis results for a single bar within an interval.
#[derive(Debug, Serialize)]
pub struct BarAnalysis {
    pub bar_index: u32,
    /// Expected frequency in Hz, or None if silence is expected.
    pub expected_freq: Option<f32>,
    /// Dominant detected frequency in Hz.
    pub detected_freq: f32,
    /// Whether the detected frequency matches the expected (within +/-10 Hz), or silence confirmed.
    pub freq_match: bool,
    /// RMS energy of the bar's audio.
    pub rms: f32,
    /// Whether silence was expected for this bar.
    pub is_silence_expected: bool,
    /// Number of silent 20ms frames after audio starts (dropout detection).
    pub dropout_frames: u32,
    /// Maximum sample-to-sample delta in the first 256 samples of the bar.
    pub seam_max_delta: f32,
    /// Whether the seam check passed (no pop/click).
    pub seam_ok: bool,
}

/// Analysis results for an entire interval (multiple bars).
#[derive(Debug, Serialize)]
pub struct IntervalAnalysis {
    pub interval_index: i64,
    pub bars: Vec<BarAnalysis>,
    pub overall_rms: f32,
    pub frames_expected: u32,
    pub frames_received: u32,
    pub pass: bool,
}

/// Detect the dominant frequency in a mono audio buffer using FFT.
///
/// Uses an 8192-point FFT (~5.9 Hz resolution at 48 kHz). Samples shorter than
/// 8192 are zero-padded; longer buffers use only the first 8192 samples.
pub fn dominant_frequency(samples: &[f32], sample_rate: u32) -> f32 {
    let mut planner = FftPlanner::<f64>::new();
    let fft = planner.plan_fft_forward(FFT_SIZE);

    let mut buffer: Vec<Complex<f64>> = Vec::with_capacity(FFT_SIZE);
    let take = samples.len().min(FFT_SIZE);
    for &s in &samples[..take] {
        buffer.push(Complex {
            re: s as f64,
            im: 0.0,
        });
    }
    // Zero-pad if needed.
    buffer.resize(FFT_SIZE, Complex { re: 0.0, im: 0.0 });

    fft.process(&mut buffer);

    // Find bin with max magnitude, skipping DC (bin 0).
    // Only search up to Nyquist (FFT_SIZE / 2).
    let nyquist = FFT_SIZE / 2;
    let (max_bin, _) = buffer[1..=nyquist]
        .iter()
        .enumerate()
        .max_by(|(_, a), (_, b)| {
            a.norm_sqr()
                .partial_cmp(&b.norm_sqr())
                .unwrap_or(std::cmp::Ordering::Equal)
        })
        .unwrap();

    let bin_index = max_bin + 1; // offset by 1 since we skipped bin 0
    bin_index as f32 * sample_rate as f32 / FFT_SIZE as f32
}

/// Check the seam (first `samples.len()` samples) for clicks/pops.
///
/// Returns `(max_delta, max_delta < threshold)`.
pub fn check_seam(samples: &[f32], threshold: f32) -> (f32, bool) {
    if samples.len() < 2 {
        return (0.0, true);
    }
    let max_delta = samples
        .windows(2)
        .map(|w| (w[1] - w[0]).abs())
        .fold(0.0f32, f32::max);
    (max_delta, max_delta < threshold)
}

/// Downmix interleaved multi-channel audio to mono by averaging channels.
pub fn downmix_to_mono(interleaved: &[f32], channels: u16) -> Vec<f32> {
    if channels <= 1 {
        return interleaved.to_vec();
    }
    let ch = channels as usize;
    let num_frames = interleaved.len() / ch;
    let mut mono = Vec::with_capacity(num_frames);
    for f in 0..num_frames {
        let sum: f32 = (0..ch).map(|c| interleaved[f * ch + c]).sum();
        mono.push(sum / ch as f32);
    }
    mono
}

/// Analyze a full interval of PCM audio, checking each bar against expected notes.
///
/// `expected_notes` has one entry per bar: `Some(freq)` for a tone, `None` for silence.
pub fn analyze_interval(
    pcm: &[f32],
    channels: u16,
    sample_rate: u32,
    bars: u32,
    bpm: f64,
    quantum: f64,
    interval_index: i64,
    expected_notes: &[Option<f32>],
) -> IntervalAnalysis {
    // Defensive clamping for wire-format values.
    let channels = channels.max(1);
    let sample_rate = sample_rate.max(1);

    let bar_duration_secs = 60.0 / bpm * quantum;
    let samples_per_bar_mono = (bar_duration_secs * sample_rate as f64) as usize;
    let samples_per_bar_interleaved = samples_per_bar_mono * channels as usize;

    let chunk_size_20ms = (sample_rate as usize * 20) / 1000; // 960 at 48kHz

    let overall_rms_val = rms(pcm);

    // Total expected mono frames across all bars.
    let frames_expected = (samples_per_bar_mono * bars as usize) as u32;
    let frames_received = (pcm.len() / channels as usize) as u32;

    let mut bar_analyses = Vec::with_capacity(bars as usize);
    let mut all_pass = true;

    for bar_idx in 0..bars {
        let start = bar_idx as usize * samples_per_bar_interleaved;
        let end = (start + samples_per_bar_interleaved).min(pcm.len());
        let bar_pcm = if start < pcm.len() {
            &pcm[start..end]
        } else {
            &[]
        };

        let mono = downmix_to_mono(bar_pcm, channels);

        let rms_val = rms(&mono);
        let is_silence_expected = expected_notes
            .get(bar_idx as usize)
            .map_or(true, |n| n.is_none());
        let expected_freq = expected_notes.get(bar_idx as usize).copied().flatten();

        let (detected_freq, freq_match) = if is_silence_expected {
            (0.0, rms_val < SILENCE_RMS_THRESHOLD)
        } else {
            let freq = if mono.is_empty() {
                0.0
            } else {
                dominant_frequency(&mono, sample_rate)
            };
            let matched = expected_freq
                .map(|ef| (freq - ef).abs() <= FREQ_TOLERANCE_HZ)
                .unwrap_or(false);
            (freq, matched)
        };

        // Dropout detection: split mono into 20ms chunks, find first non-silent,
        // then count silent chunks after it.
        let mut dropout_frames = 0u32;
        let chunks: Vec<&[f32]> = mono.chunks(chunk_size_20ms).collect();
        let mut found_audio = false;
        for chunk in &chunks {
            let chunk_rms = rms(chunk);
            if !found_audio {
                if chunk_rms >= SILENCE_RMS_THRESHOLD {
                    found_audio = true;
                }
            } else if chunk_rms < SILENCE_RMS_THRESHOLD {
                dropout_frames += 1;
            }
        }

        // Seam check on first 256 mono samples.
        let seam_samples = &mono[..mono.len().min(256)];
        let (seam_max_delta, seam_ok) = check_seam(seam_samples, DEFAULT_SEAM_THRESHOLD);

        let bar_pass = freq_match && seam_ok;
        if !bar_pass {
            all_pass = false;
        }

        bar_analyses.push(BarAnalysis {
            bar_index: bar_idx,
            expected_freq,
            detected_freq,
            freq_match,
            rms: rms_val,
            is_silence_expected,
            dropout_frames,
            seam_max_delta,
            seam_ok,
        });
    }

    IntervalAnalysis {
        interval_index,
        bars: bar_analyses,
        overall_rms: overall_rms_val,
        frames_expected,
        frames_received,
        pass: all_pass,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::f32::consts::PI;

    fn sine_wave(freq: f32, sample_rate: u32, num_samples: usize, amplitude: f32) -> Vec<f32> {
        (0..num_samples)
            .map(|i| (2.0 * PI * freq * i as f32 / sample_rate as f32).sin() * amplitude)
            .collect()
    }

    #[test]
    fn test_dominant_frequency_detects_440hz() {
        let samples = sine_wave(440.0, 48000, 8192, 0.5);
        let detected = dominant_frequency(&samples, 48000);
        let bin_resolution = 48000.0 / 8192.0; // ~5.86 Hz
        assert!(
            (detected - 440.0).abs() <= bin_resolution,
            "Expected ~440Hz, got {detected}Hz"
        );
    }

    #[test]
    fn test_dominant_frequency_detects_220hz() {
        let samples = sine_wave(220.0, 48000, 8192, 0.5);
        let detected = dominant_frequency(&samples, 48000);
        let bin_resolution = 48000.0 / 8192.0;
        assert!(
            (detected - 220.0).abs() <= bin_resolution,
            "Expected ~220Hz, got {detected}Hz"
        );
    }

    #[test]
    fn test_dominant_frequency_distinguishes_pentatonic() {
        let pentatonic = [220.0, 261.63, 293.66, 329.63, 392.0];
        for &freq in &pentatonic {
            let samples = sine_wave(freq, 48000, 8192, 0.5);
            let detected = dominant_frequency(&samples, 48000);
            assert!(
                (detected - freq).abs() <= FREQ_TOLERANCE_HZ,
                "Expected ~{freq}Hz, got {detected}Hz"
            );
        }
    }

    #[test]
    fn test_silence_detection() {
        let silence = vec![0.0f32; 8192];
        let rms_val = rms(&silence);
        assert!(
            rms_val < SILENCE_RMS_THRESHOLD,
            "Silence RMS should be < {SILENCE_RMS_THRESHOLD}, got {rms_val}"
        );
    }

    #[test]
    fn test_dropout_detection() {
        // 960 samples of tone, 960 of silence, 960 of tone (mono, 48kHz = 20ms chunks)
        let tone = sine_wave(440.0, 48000, 960, 0.5);
        let silence = vec![0.0f32; 960];
        let mut samples = Vec::with_capacity(2880);
        samples.extend_from_slice(&tone);
        samples.extend_from_slice(&silence);
        samples.extend_from_slice(&tone);

        // Count dropouts manually using the same logic as analyze_interval.
        let chunk_size = 960;
        let chunks: Vec<&[f32]> = samples.chunks(chunk_size).collect();
        let mut found_audio = false;
        let mut dropout_frames = 0u32;
        for chunk in &chunks {
            let chunk_rms = rms(chunk);
            if !found_audio {
                if chunk_rms >= SILENCE_RMS_THRESHOLD {
                    found_audio = true;
                }
            } else if chunk_rms < SILENCE_RMS_THRESHOLD {
                dropout_frames += 1;
            }
        }
        assert_eq!(dropout_frames, 1, "Should detect 1 dropout frame");
    }

    #[test]
    fn test_seam_ok_smooth_signal() {
        // Continuous sine — no discontinuity.
        let samples = sine_wave(440.0, 48000, 256, 0.2);
        let (max_delta, ok) = check_seam(&samples, DEFAULT_SEAM_THRESHOLD);
        assert!(ok, "Smooth sine should pass seam check, max_delta={max_delta}");
    }

    #[test]
    fn test_seam_violation_step() {
        let samples = [0.0f32, 0.0, 0.5, 0.5];
        let (max_delta, ok) = check_seam(&samples, DEFAULT_SEAM_THRESHOLD);
        assert!(
            (max_delta - 0.5).abs() < 1e-6,
            "Expected max_delta=0.5, got {max_delta}"
        );
        assert!(!ok, "Step discontinuity should fail seam check");
    }

    #[test]
    fn test_analyze_interval_pentatonic() {
        let sample_rate = 48000u32;
        let bpm = 120.0;
        let quantum = 4.0;
        let channels = 2u16;
        let bars = 4u32;

        let notes = [220.0f32, 261.63, 293.66, 329.63];
        let expected_notes: Vec<Option<f32>> = notes.iter().map(|&f| Some(f)).collect();

        // Bar duration = 60/120 * 4 = 2.0 seconds
        let samples_per_bar = (2.0 * sample_rate as f64) as usize;

        let mut pcm = Vec::new();
        for &freq in &notes {
            let mono = sine_wave(freq, sample_rate, samples_per_bar, 0.5);
            // Interleave to stereo.
            for &s in &mono {
                pcm.push(s);
                pcm.push(s);
            }
        }

        let result = analyze_interval(
            &pcm,
            channels,
            sample_rate,
            bars,
            bpm,
            quantum,
            0,
            &expected_notes,
        );

        assert!(
            result.pass,
            "All bars should pass. Details: {:#?}",
            result.bars
        );
        assert_eq!(result.bars.len(), 4);
        for (i, bar) in result.bars.iter().enumerate() {
            assert!(
                bar.freq_match,
                "Bar {i} freq mismatch: expected {:?}, got {}",
                bar.expected_freq, bar.detected_freq
            );
            assert!(bar.seam_ok, "Bar {i} seam check failed");
        }
    }
}
