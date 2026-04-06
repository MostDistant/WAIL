//! End-to-end tests for the WAIL Recv CLAP plugin.
//!
//! Loads the real `.clap` binary, verifies lifecycle and output behavior.
//!
//! All scenarios run in a single test to avoid loading the `.clap` dylib
//! on multiple threads — CLAP plugins have main-thread affinity for
//! `clap_entry.init()`.
//!
//! Requires: `cargo xtask build-plugin` before running.

use std::io::Write as _;
use std::time::Duration;

use clack_host::prelude::*;
use wail_audio::{AudioDecoder, AudioEncoder, IpcFramer, IpcMessage, IPC_ROLE_RECV};
use wail_plugin_test::*;

fn init_tracing() {
    let _ = tracing_subscriber::fmt()
        .with_env_filter("debug")
        .with_test_writer()
        .try_init();
}

const RECV_CLAP_ID: &str = "com.wail.recv";

fn load_recv_plugin() -> ClapTestHost {
    let path = find_plugin_bundle("wail-plugin-recv");
    assert!(
        path.exists(),
        "Plugin bundle not found at {}. Run `cargo xtask build-plugin` first.",
        path.display()
    );
    unsafe { ClapTestHost::load(&path, RECV_CLAP_ID).expect("Failed to load recv plugin") }
}

/// Number of output ports matching the recv plugin's default layout:
/// 1 main stereo + 15 aux stereo (per-peer/stream) = 16 total.
const NUM_OUTPUT_PORTS: usize = 16;

fn process_one_buffer(
    processor: &mut StartedPluginAudioProcessor<TestHost>,
    num_frames: u32,
    steady_time: u64,
) -> (ProcessStatus, Vec<f32>, Vec<f32>) {
    let n = num_frames as usize;
    let mut input_left = vec![0.0f32; n];
    let mut input_right = vec![0.0f32; n];

    // Pre-allocate all output channel buffers: [port_index] -> [left, right]
    // Port 0 is main output, ports 1-15 are aux (per-peer routing).
    // nih_plug requires the host to provide all ports declared by the
    // active audio layout, otherwise it silently skips process().
    let mut out_bufs: Vec<[Vec<f32>; 2]> = (0..NUM_OUTPUT_PORTS)
        .map(|_| [vec![0.0f32; n], vec![0.0f32; n]])
        .collect();

    let mut ports = AudioPorts::with_capacity(2, 1);
    let input_buffers = ports.with_input_buffers([AudioPortBuffer {
        latency: 0,
        channels: AudioPortBufferType::f32_input_only(
            [&mut input_left[..], &mut input_right[..]]
                .into_iter()
                .map(|b| InputChannel {
                    buffer: b,
                    is_constant: false,
                }),
        ),
    }]);

    let mut output_ports = AudioPorts::with_capacity(NUM_OUTPUT_PORTS * 2, NUM_OUTPUT_PORTS);
    let mut output_buffers = output_ports.with_output_buffers(
        out_bufs.iter_mut().map(|[left, right]| AudioPortBuffer {
            latency: 0,
            channels: AudioPortBufferType::f32_output_only(
                [left.as_mut_slice(), right.as_mut_slice()].into_iter(),
            ),
        }),
    );

    let input_events = InputEvents::empty();
    let mut output_events = OutputEvents::void();

    let status = processor
        .process(
            &input_buffers,
            &mut output_buffers,
            &input_events,
            &mut output_events,
            Some(steady_time),
            None,
        )
        .expect("process() failed");

    // Return main output (port 0) channels
    let output_left = out_bufs[0][0].clone();
    let output_right = out_bufs[0][1].clone();
    (status, output_left, output_right)
}

#[test]
fn recv_plugin_e2e() {
    init_tracing();
    let mut host = load_recv_plugin();

    // --- Scenario 1: plays back audio received via IPC ---
    {
        // Start TCP listener before activating (so the IPC thread can connect)
        let (listener, addr) = random_listener();
        unsafe {
            std::env::set_var("WAIL_IPC_ADDR", addr.to_string());
        }

        let stopped = host
            .activate(48000.0, 32, 4096)
            .expect("Failed to activate for IPC test");

        let mut processor = stopped
            .start_processing()
            .expect("Failed to start processing");

        // Accept the IPC connection from the plugin's background thread
        let (mut stream, role, _stream_index) = accept_ipc_connection(&listener, Duration::from_secs(5));
        assert_eq!(
            role, IPC_ROLE_RECV,
            "Expected RECV role byte (0x01), got 0x{role:02x}"
        );

        let buf_size: u32 = 4096;

        // Process one buffer to establish interval 0 in the ring
        process_one_buffer(&mut processor, buf_size, 0);

        // Send a pre-encoded test interval to the plugin via TCP.
        // The IPC thread will Opus-decode it and push to the audio thread's channel.
        let frame = make_test_interval_frame("test-peer", 0);

        // Self-test: verify the Opus encode→decode pipeline produces non-silent audio.
        // make_test_interval_frame sends WAIF streaming frames; we verify the underlying
        // codec independently here.
        {
            let sr = 48000u32;
            let channels = 2u16;
            let samples_per_channel = (4usize * 4 * 60 * sr as usize) / 120;
            let test_samples = sine_wave(440.0, samples_per_channel, channels, sr);
            let mut enc = AudioEncoder::new(sr, channels, 128).unwrap();
            let mut dec = AudioDecoder::new(sr, channels).unwrap();
            let opus = enc.encode_interval(&test_samples).unwrap();
            let decoded = dec.decode_interval(&opus).unwrap();
            let decoded_rms = rms(&decoded);
            eprintln!(
                "Self-test: decoded {} samples, RMS={decoded_rms}, index=0",
                decoded.len()
            );
            assert!(decoded_rms > 0.001, "Decoded audio should be non-silent");
        }

        stream.write_all(&frame).expect("Failed to write IPC frame");

        // Give the IPC thread time to read, decode, and send to channel
        std::thread::sleep(Duration::from_secs(1));

        // Drive a few callbacks so the ring establishes current_interval = Some(0)
        // from the drained interval-0 frames. This must happen BEFORE we send
        // interval 1, otherwise the drain loop jumps remote_interval straight
        // to Some(1) and the None → Some(1) path skips the swap.
        for i in 1..=10u64 {
            process_one_buffer(&mut processor, buf_size, i * buf_size as u64);
        }

        // Now send interval 1 so remote_interval advances 0 → 1, triggering
        // the boundary that flushes interval 0 into playback.
        let frame1 = make_test_interval_frame("test-peer", 1);
        stream.write_all(&frame1).expect("Failed to write interval 1");
        std::thread::sleep(Duration::from_millis(500));

        let num_callbacks: u64 = 100;

        let mut found_audio = false;
        for i in 1..=num_callbacks {
            let (_, out_l, _) =
                process_one_buffer(&mut processor, buf_size, i * buf_size as u64);
            let r = rms(&out_l);
            if r > 0.001 {
                found_audio = true;
            }
        }

        // Also check the final buffer
        let (_, output_left, _) = process_one_buffer(
            &mut processor,
            buf_size,
            (num_callbacks + 1) * buf_size as u64,
        );
        if rms(&output_left) > 0.001 {
            found_audio = true;
        }

        assert!(
            found_audio,
            "Recv plugin should output non-silent audio after receiving an interval via IPC \
             (checked {} buffers after boundary)",
            num_callbacks + 1
        );

        let stopped = processor.stop_processing();
        host.deactivate(stopped);
    }

    // --- Scenario 2: silence after PeerLeft ---
    //
    // Verifies that when a PeerLeft IPC message is received, the recv plugin
    // stops outputting that peer's audio after the next interval boundary.
    {
        let (listener, addr) = random_listener();
        unsafe { std::env::set_var("WAIL_IPC_ADDR", addr.to_string()); }

        let stopped = host
            .activate(48000.0, 32, 4096)
            .expect("Failed to activate for PeerLeft test");
        let mut processor = stopped
            .start_processing()
            .expect("Failed to start processing");

        let (mut stream, role, _) = accept_ipc_connection(&listener, Duration::from_secs(5));
        assert_eq!(role, IPC_ROLE_RECV);

        let buf_size: u32 = 4096;

        // Establish interval 0 in the ring
        process_one_buffer(&mut processor, buf_size, 0);

        // Send interval 0, drive callbacks to establish current_interval = Some(0)
        let frame0 = make_test_interval_frame("peer-disconnect", 0);
        stream.write_all(&frame0).expect("Failed to write audio frame 0");
        std::thread::sleep(Duration::from_secs(2));
        for i in 1..=20u64 {
            process_one_buffer(&mut processor, buf_size, i * buf_size as u64);
        }

        // Send interval 1 to trigger boundary (remote_interval 0 → 1)
        let frame1 = make_test_interval_frame("peer-disconnect", 1);
        stream.write_all(&frame1).expect("Failed to write audio frame 1");
        std::thread::sleep(Duration::from_millis(500));

        // Drive past interval boundary — should hear audio
        let mut found_audio = false;
        for i in 21..=120u64 {
            let (_, out_l, _) = process_one_buffer(&mut processor, buf_size, i * buf_size as u64);
            if rms(&out_l) > 0.001 {
                found_audio = true;
            }
        }
        assert!(found_audio, "Should have audio before PeerLeft");

        // Send PeerLeft IPC message
        let peer_left = IpcFramer::encode_frame(&IpcMessage::encode_peer_left("peer-disconnect"));
        stream.write_all(&peer_left).expect("Failed to write PeerLeft");
        std::thread::sleep(Duration::from_millis(500));

        // Send interval 2 to trigger boundary after PeerLeft, then drive callbacks.
        let frame2 = make_test_interval_frame("peer-disconnect", 2);
        stream.write_all(&frame2).expect("Failed to write audio frame 2");
        std::thread::sleep(Duration::from_millis(500));
        for i in 121..=220u64 {
            process_one_buffer(&mut processor, buf_size, i * buf_size as u64);
        }

        // Post-boundary: all output must be silence
        let mut post_disconnect_silent = true;
        for i in 221..=270u64 {
            let (_, out_l, _) = process_one_buffer(&mut processor, buf_size, i * buf_size as u64);
            if rms(&out_l) > 0.001 {
                eprintln!("Non-silent buffer at callback {i}: RMS={:.4}", rms(&out_l));
                post_disconnect_silent = false;
            }
        }
        assert!(
            post_disconnect_silent,
            "Recv plugin should output silence after PeerLeft + interval boundary crossing"
        );
        eprintln!("[recv] Scenario 2 PASSED: silence after PeerLeft");

        let stopped = processor.stop_processing();
        host.deactivate(stopped);
    }

    // --- Scenario 3: audio playback with small (128-sample) buffers ---
    //
    // Exercises the recv plugin with buffers much smaller than the Opus frame
    // size (960 stereo samples). At 128 samples/call, the ring buffer's
    // process() is called ~3000 times per interval — stressing boundary
    // detection precision and per-call state management.
    {
        let buf_size: u32 = 128;
        let (listener, addr) = random_listener();
        unsafe { std::env::set_var("WAIL_IPC_ADDR", addr.to_string()); }

        let stopped = host
            .activate(48000.0, 32, buf_size)
            .expect("Failed to activate for small-buffer test");
        let mut processor = stopped
            .start_processing()
            .expect("Failed to start processing");

        let (mut stream, role, _) = accept_ipc_connection(&listener, Duration::from_secs(5));
        assert_eq!(role, IPC_ROLE_RECV);

        // Establish interval 0
        process_one_buffer(&mut processor, buf_size, 0);

        // Send interval 0, drive callbacks to establish current_interval = Some(0)
        let frame0 = make_test_interval_frame("test-peer-small", 0);
        stream.write_all(&frame0).expect("Failed to write audio frame 0");
        std::thread::sleep(Duration::from_secs(1));
        for i in 1..=10u64 {
            process_one_buffer(&mut processor, buf_size, i * buf_size as u64);
        }

        // Send interval 1 to trigger boundary (remote_interval 0 → 1)
        let frame1 = make_test_interval_frame("test-peer-small", 1);
        stream.write_all(&frame1).expect("Failed to write audio frame 1");
        std::thread::sleep(Duration::from_millis(500));

        let num_callbacks: u64 = 3200;
        let mut found_audio = false;
        for i in 11..=(num_callbacks + 10) {
            let (_, out_l, _) = process_one_buffer(&mut processor, buf_size, i * buf_size as u64);
            if rms(&out_l) > 0.001 {
                found_audio = true;
            }
        }
        assert!(
            found_audio,
            "Recv plugin should produce non-silent audio with {buf_size}-sample buffers \
             (checked {num_callbacks} buffers)"
        );
        eprintln!("[recv] Scenario 3 PASSED: audio with {buf_size}-sample buffers");

        let stopped = processor.stop_processing();
        host.deactivate(stopped);
    }

    // --- Scenario 4: multi-interval continuity ---
    //
    // Feeds 3 consecutive intervals and verifies continuous non-silent playback
    // across all interval boundaries. This tests the ring buffer's swap + crossfade
    // path over multiple transitions and catches bugs where audio drops out between
    // intervals (e.g., pending_remote not queued correctly, or swap_intervals
    // clearing data prematurely).
    //
    // Intervals are sent incrementally (one per interval of process() calls) to
    // match production behavior. The IPC channel is bounded(512) and each interval
    // is ~400 decoded frames, so sending all at once would overflow the channel.
    {
        let (listener, addr) = random_listener();
        unsafe { std::env::set_var("WAIL_IPC_ADDR", addr.to_string()); }

        let stopped = host
            .activate(48000.0, 32, 4096)
            .expect("Failed to activate for multi-interval test");
        let mut processor = stopped
            .start_processing()
            .expect("Failed to start processing");

        let (mut stream, role, _) = accept_ipc_connection(&listener, Duration::from_secs(5));
        assert_eq!(role, IPC_ROLE_RECV);

        let buf_size: u32 = 4096;
        let callbacks_per_interval: u64 = 94; // ceil(384000/4096)
        let num_intervals: i64 = 3;

        // Establish interval 0 and send the first interval's audio.
        process_one_buffer(&mut processor, buf_size, 0);
        let frame0 = make_test_interval_frame("peer-multi", 0);
        stream.write_all(&frame0).expect("Failed to write interval 0");
        std::thread::sleep(Duration::from_millis(500));

        // Drive through all intervals, sending each new interval's audio at the
        // start of the interval where it should be queued (one interval ahead of
        // when it plays back, matching the NINJAM 1-interval latency).
        let total_callbacks = (num_intervals as u64 + 1) * callbacks_per_interval + 10;
        let mut next_interval_to_send: i64 = 1;

        // Track per-interval audio coverage.
        // Start at interval 0 (established by the initial process_one_buffer call).
        let mut interval_audio: Vec<(u64, u32, u32)> = Vec::new();
        let mut cur_idx: u64 = 0;
        let mut cur_ns = 0u32;
        let mut cur_total = 0u32;

        // Track continuity: after audio starts, count max consecutive silent callbacks
        let mut audio_started = false;
        let mut current_gap = 0u32;
        let mut max_gap = 0u32;

        for i in 1..=total_callbacks {
            let steady = i * buf_size as u64;
            let interval = steady / 384_000;

            // Send the next interval's audio when we enter a new interval
            if interval != cur_idx {
                interval_audio.push((cur_idx, cur_ns, cur_total));
                cur_idx = interval;
                cur_ns = 0;
                cur_total = 0;

                // Feed the next interval's audio (if we have one to send).
                // Send up to num_intervals (exclusive), plus one extra "trigger"
                // interval to flush the last real interval into playback.
                if next_interval_to_send <= num_intervals {
                    let frame = make_test_interval_frame("peer-multi", next_interval_to_send);
                    stream.write_all(&frame).expect("Failed to write interval frame");
                    next_interval_to_send += 1;
                    // Give IPC thread time to read, decode, and push all frames
                    std::thread::sleep(Duration::from_millis(500));
                }
            }
            cur_total += 1;

            let (_, out_l, _) = process_one_buffer(&mut processor, buf_size, steady);
            if rms(&out_l) > 0.001 {
                cur_ns += 1;
                if audio_started {
                    max_gap = max_gap.max(current_gap);
                    current_gap = 0;
                }
                audio_started = true;
            } else if audio_started {
                current_gap += 1;
            }
        }
        interval_audio.push((cur_idx, cur_ns, cur_total));
        // Don't count trailing silence (after all intervals consumed) as a gap
        // — only gaps BETWEEN non-silent buffers matter.

        // Log results
        let max_gap_ms = max_gap as f64 * buf_size as f64 / 48000.0 * 1000.0;
        for (idx, ns, total) in &interval_audio {
            let pct = if *total > 0 { *ns as f64 / *total as f64 * 100.0 } else { 0.0 };
            eprintln!("[recv]   Interval {idx}: {ns}/{total} ({pct:.0}%) non-silent");
        }
        eprintln!("[recv]   Max gap: {max_gap} callbacks ({max_gap_ms:.0}ms)");

        // Interval 0 is warmup (silent — no audio queued yet for playback).
        // Intervals 1..=num_intervals should have >75% audio coverage.
        for (idx, ns, total) in &interval_audio {
            if *idx == 0 { continue; } // skip warmup
            if *idx > num_intervals as u64 { continue; } // skip tail
            let pct = *ns as f64 / *total as f64 * 100.0;
            assert!(
                pct > 75.0,
                "Interval {idx}: only {pct:.0}% audio ({ns}/{total}). \
                 Multi-interval continuity broken — audio should be continuous across boundaries."
            );
        }

        // Continuity: no gaps of more than 1 callback (~85ms) between non-silent
        // buffers once audio has started. This catches dropouts at interval
        // boundaries that per-interval coverage alone would miss.
        assert!(
            max_gap <= 1,
            "Detected a gap of {max_gap} consecutive silent callbacks ({max_gap_ms:.0}ms) \
             between non-silent buffers. Audio must be continuous across interval boundaries."
        );

        eprintln!("[recv] Scenario 4 PASSED: {num_intervals} intervals continuous, max_gap={max_gap}");

        let stopped = processor.stop_processing();
        host.deactivate(stopped);
    }

    host.leak();
}
