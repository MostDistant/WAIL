use std::time::{Duration, Instant};

use anyhow::{bail, Result};
use clap::Parser;
use tokio::time::timeout;
use tracing::{info, warn};
use uuid::Uuid;

use wail_audio::codec::{AudioDecoder, AudioEncoder};
use wail_audio::wire::AudioWire;
use wail_audio::AudioInterval;
use wail_core::protocol::SyncMessage;
use wail_net::{fetch_metered_ice_servers, metered_stun_fallback, MeshEvent, PeerMesh};

#[derive(Parser)]
#[command(name = "wail-e2e", about = "Two-machine end-to-end test for WAIL")]
struct Args {
    /// Signaling server URL
    #[arg(long, default_value = "wss://wail-signal.fly.dev")]
    server: String,

    /// Room name (both machines must use the same room)
    #[arg(long)]
    room: Option<String>,

    /// Max seconds to wait for the full test
    #[arg(long, default_value = "120")]
    timeout: u64,

    /// Enable debug-level tracing
    #[arg(long)]
    verbose: bool,
}

struct TestResult {
    phase: &'static str,
    passed: bool,
    message: String,
    duration: Duration,
}

impl TestResult {
    fn pass(phase: &'static str, message: String, duration: Duration) -> Self {
        Self { phase, passed: true, message, duration }
    }
    fn fail(phase: &'static str, message: String, duration: Duration) -> Self {
        Self { phase, passed: false, message, duration }
    }
}

fn print_result(r: &TestResult) {
    let tag = if r.passed { "PASS" } else { "FAIL" };
    println!("[{tag}] {}: {} ({:.1?})", r.phase, r.message, r.duration);
}

#[tokio::main]
async fn main() -> Result<()> {
    let args = Args::parse();

    let filter = if args.verbose { "debug" } else { "info" };
    tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_target(false)
        .init();

    let room = args.room.unwrap_or_else(|| format!("e2e-{}", &Uuid::new_v4().to_string()[..8]));
    let peer_id = format!("e2e-{}", &Uuid::new_v4().to_string()[..8]);
    let global_timeout = Duration::from_secs(args.timeout);

    println!("=== WAIL E2E Test ===");
    println!("Room:    {room}");
    println!("Peer ID: {peer_id}");
    println!("Server:  {}", args.server);
    println!("Timeout: {global_timeout:.0?}");
    println!();

    match timeout(global_timeout, run_test(&args.server, &room, &peer_id)).await {
        Ok(Ok(())) => {
            println!("\n=== ALL TESTS PASSED ===");
            Ok(())
        }
        Ok(Err(e)) => {
            println!("\n=== TEST FAILED: {e} ===");
            std::process::exit(1);
        }
        Err(_) => {
            println!("\n=== TEST TIMED OUT after {global_timeout:.0?} ===");
            std::process::exit(1);
        }
    }
}

async fn run_test(server_url: &str, room: &str, peer_id: &str) -> Result<()> {
    let mut results: Vec<TestResult> = Vec::new();

    // --- Phase 1: ICE servers ---
    let t = Instant::now();
    let ice_servers = match fetch_metered_ice_servers().await {
        Ok(servers) => {
            results.push(TestResult::pass(
                "ICE",
                format!("fetched {} servers from Metered", servers.len()),
                t.elapsed(),
            ));
            servers
        }
        Err(e) => {
            warn!("Metered API failed ({e}), using STUN fallback");
            results.push(TestResult::pass(
                "ICE",
                "Metered unreachable, using STUN fallback".into(),
                t.elapsed(),
            ));
            metered_stun_fallback()
        }
    };
    print_result(results.last().unwrap());

    // --- Phase 2: Signaling connection ---
    let t = Instant::now();
    let (mut mesh, mut sync_rx, mut audio_rx) = match timeout(
        Duration::from_secs(10),
        PeerMesh::connect_full(
            server_url,
            room,
            peer_id,
            None, // no password
            ice_servers,
            false, // not relay-only
            1,     // stream_count
            Some("e2e-test"),
        ),
    )
    .await
    {
        Ok(Ok(v)) => {
            results.push(TestResult::pass(
                "Signaling",
                format!("connected to {server_url}"),
                t.elapsed(),
            ));
            v
        }
        Ok(Err(e)) => {
            results.push(TestResult::fail("Signaling", format!("{e}"), t.elapsed()));
            print_result(results.last().unwrap());
            bail!("Signaling connection failed: {e}");
        }
        Err(_) => {
            results.push(TestResult::fail("Signaling", "timeout (10s)".into(), t.elapsed()));
            print_result(results.last().unwrap());
            bail!("Signaling connection timed out");
        }
    };
    print_result(results.last().unwrap());

    // --- Phase 3: Peer discovery + WebRTC negotiation ---
    println!("\nWaiting for peer to join room \"{room}\"...");
    println!("  Run on the other machine:");
    println!("  cargo run -p wail-e2e --release -- --room {room} --server {server_url}");
    println!();

    let t = Instant::now();
    let remote_peer_id = loop {
        match timeout(Duration::from_secs(1), mesh.poll_signaling()).await {
            Ok(Ok(Some(MeshEvent::PeerJoined { peer_id: rid, display_name }))) => {
                info!(peer = %rid, name = ?display_name, "Peer joined");
                break rid;
            }
            Ok(Ok(Some(MeshEvent::PeerListReceived(count)))) => {
                if count > 0 {
                    // Peer was already in the room — check connected peers
                    let peers = mesh.connected_peers();
                    if let Some(rid) = peers.into_iter().next() {
                        break rid;
                    }
                }
            }
            Ok(Ok(Some(_))) => {} // other events, keep polling
            Ok(Ok(None)) => bail!("Signaling channel closed"),
            Ok(Err(e)) => bail!("Signaling error: {e}"),
            Err(_) => {} // 1s poll timeout, keep waiting
        }
    };
    results.push(TestResult::pass(
        "Discovery",
        format!("peer {remote_peer_id} found"),
        t.elapsed(),
    ));
    print_result(results.last().unwrap());

    // --- Phase 4: Wait for DataChannels to open ---
    let t = Instant::now();
    let dc_timeout = Duration::from_secs(30);
    let dc_result = timeout(dc_timeout, async {
        loop {
            if mesh.any_audio_dc_open() {
                return Ok::<(), anyhow::Error>(());
            }
            match timeout(Duration::from_secs(1), mesh.poll_signaling()).await {
                Ok(Ok(Some(_))) => {}
                Ok(Ok(None)) => bail!("Signaling closed during negotiation"),
                Ok(Err(e)) => bail!("Signaling error during negotiation: {e}"),
                Err(_) => {} // poll timeout
            }
        }
    })
    .await;

    match dc_result {
        Ok(Ok(())) => {
            results.push(TestResult::pass(
                "WebRTC",
                "DataChannels open".into(),
                t.elapsed(),
            ));
        }
        Ok(Err(e)) => {
            results.push(TestResult::fail("WebRTC", format!("{e}"), t.elapsed()));
            print_result(results.last().unwrap());
            bail!("WebRTC negotiation failed: {e}");
        }
        Err(_) => {
            // Collect diagnostic info
            let state = mesh.peer_network_state(&remote_peer_id);
            let msg = match state {
                Some((ice, sync_dc, audio_dc)) => {
                    format!("timeout ({dc_timeout:.0?}) — ICE={ice}, sync_dc={sync_dc}, audio_dc={audio_dc}")
                }
                None => format!("timeout ({dc_timeout:.0?}) — peer not in mesh"),
            };
            results.push(TestResult::fail("WebRTC", msg.clone(), t.elapsed()));
            print_result(results.last().unwrap());
            bail!("WebRTC negotiation failed: {msg}");
        }
    }
    print_result(results.last().unwrap());

    // --- Phase 5: Sync message exchange (Hello + Ping/Pong) ---
    let t = Instant::now();

    // Send Hello
    mesh.broadcast(&SyncMessage::Hello {
        peer_id: peer_id.to_string(),
        display_name: Some("e2e-test".into()),
        identity: None,
    })
    .await;

    // Send Ping
    let ping_sent = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_micros() as i64;
    mesh.broadcast(&SyncMessage::Ping {
        id: 1,
        sent_at_us: ping_sent,
    })
    .await;

    // Wait for Hello and Pong
    let mut got_hello = false;
    let mut rtt_us: Option<i64> = None;
    let sync_timeout = Duration::from_secs(10);

    let sync_result = timeout(sync_timeout, async {
        loop {
            tokio::select! {
                Some((from, msg)) = sync_rx.recv() => {
                    match msg {
                        SyncMessage::Hello { peer_id: rid, .. } => {
                            info!(peer = %rid, "Got Hello from {from}");
                            got_hello = true;
                        }
                        SyncMessage::Ping { id, sent_at_us } => {
                            // Reply with Pong
                            let now = std::time::SystemTime::now()
                                .duration_since(std::time::UNIX_EPOCH)
                                .unwrap()
                                .as_micros() as i64;
                            mesh.send_to(&from, &SyncMessage::Pong {
                                id,
                                ping_sent_at_us: sent_at_us,
                                pong_sent_at_us: now,
                            }).await.ok();
                        }
                        SyncMessage::Pong { ping_sent_at_us, .. } => {
                            let now = std::time::SystemTime::now()
                                .duration_since(std::time::UNIX_EPOCH)
                                .unwrap()
                                .as_micros() as i64;
                            rtt_us = Some(now - ping_sent_at_us);
                            info!(rtt_us = rtt_us.unwrap(), "Got Pong");
                        }
                        _ => {}
                    }
                    if got_hello && rtt_us.is_some() {
                        return Ok::<(), anyhow::Error>(());
                    }
                }
                result = mesh.poll_signaling() => {
                    result?;
                }
            }
        }
    })
    .await;

    match sync_result {
        Ok(Ok(())) => {
            let rtt_ms = rtt_us.unwrap_or(0) as f64 / 1000.0;
            results.push(TestResult::pass(
                "Sync",
                format!("Hello exchanged, RTT={rtt_ms:.1}ms"),
                t.elapsed(),
            ));
        }
        Ok(Err(e)) => {
            results.push(TestResult::fail("Sync", format!("{e}"), t.elapsed()));
            print_result(results.last().unwrap());
            bail!("Sync exchange failed: {e}");
        }
        Err(_) => {
            let detail = match (got_hello, rtt_us) {
                (false, _) => "no Hello received",
                (true, None) => "Hello OK but no Pong received",
                _ => "unknown",
            };
            results.push(TestResult::fail(
                "Sync",
                format!("timeout ({sync_timeout:.0?}) — {detail}"),
                t.elapsed(),
            ));
            print_result(results.last().unwrap());
            bail!("Sync exchange timed out: {detail}");
        }
    }
    print_result(results.last().unwrap());

    // --- Phase 6: Audio interval exchange ---
    let t = Instant::now();

    // Generate a test interval: 960 stereo samples of 440Hz sine wave
    let sample_rate = 48000u32;
    let channels = 2u16;
    let num_samples = 960 * channels as usize; // 20ms worth
    let mut samples = vec![0.0f32; num_samples];
    let freq = 440.0;
    for i in 0..960 {
        let val = (2.0 * std::f32::consts::PI * freq * i as f32 / sample_rate as f32).sin() * 0.5;
        samples[i * 2] = val;     // L
        samples[i * 2 + 1] = val; // R
    }

    // Encode with Opus
    let mut encoder = AudioEncoder::new(sample_rate, channels, 128)?;
    let opus_data = encoder.encode_interval(&samples)?;

    let interval = AudioInterval {
        index: 0,
        stream_id: 0,
        opus_data,
        sample_rate,
        channels,
        num_frames: 960,
        bpm: 120.0,
        quantum: 4.0,
        bars: 4,
    };
    let wire_bytes = AudioWire::encode(&interval);
    info!(bytes = wire_bytes.len(), "Sending test audio interval");

    mesh.broadcast_audio(&wire_bytes).await;

    // Wait for audio from remote peer
    let audio_timeout = Duration::from_secs(15);
    let audio_result = timeout(audio_timeout, async {
        loop {
            tokio::select! {
                Some((from, data)) = audio_rx.recv() => {
                    info!(peer = %from, bytes = data.len(), "Received audio data");
                    return Ok::<Vec<u8>, anyhow::Error>(data);
                }
                result = mesh.poll_signaling() => {
                    result?;
                }
            }
        }
    })
    .await;

    match audio_result {
        Ok(Ok(data)) => {
            // Validate the received audio
            match validate_audio(&data, sample_rate, channels) {
                Ok(detail) => {
                    results.push(TestResult::pass("Audio", detail, t.elapsed()));
                }
                Err(e) => {
                    results.push(TestResult::fail(
                        "Audio",
                        format!("received {len} bytes but validation failed: {e}", len = data.len()),
                        t.elapsed(),
                    ));
                    print_result(results.last().unwrap());
                    bail!("Audio validation failed: {e}");
                }
            }
        }
        Ok(Err(e)) => {
            results.push(TestResult::fail("Audio", format!("{e}"), t.elapsed()));
            print_result(results.last().unwrap());
            bail!("Audio exchange failed: {e}");
        }
        Err(_) => {
            results.push(TestResult::fail(
                "Audio",
                format!("timeout ({audio_timeout:.0?}) — no audio received"),
                t.elapsed(),
            ));
            print_result(results.last().unwrap());
            bail!("Audio exchange timed out");
        }
    }
    print_result(results.last().unwrap());

    // --- Summary ---
    println!("\n--- Summary ---");
    for r in &results {
        print_result(r);
    }

    let all_passed = results.iter().all(|r| r.passed);
    if !all_passed {
        bail!("Some tests failed");
    }

    Ok(())
}

fn validate_audio(data: &[u8], sample_rate: u32, channels: u16) -> Result<String> {
    // Try WAIL (full interval) format first, then WAIF (streaming frame)
    if data.len() >= 4 && &data[0..4] == b"WAIL" {
        let decoded = AudioWire::decode(data)?;
        if decoded.sample_rate != sample_rate {
            bail!(
                "sample_rate mismatch: expected {sample_rate}, got {}",
                decoded.sample_rate
            );
        }
        if decoded.channels != channels {
            bail!(
                "channels mismatch: expected {channels}, got {}",
                decoded.channels
            );
        }
        if decoded.opus_data.is_empty() {
            bail!("opus_data is empty");
        }

        // Decode Opus and check RMS
        let mut decoder = AudioDecoder::new(sample_rate, channels)?;
        let pcm = decoder.decode_interval(&decoded.opus_data)?;
        let rms = rms(&pcm);

        if rms < 0.001 {
            bail!("decoded audio is silent (RMS={rms:.6})");
        }

        Ok(format!(
            "WAIL interval: {} bytes, {}/{} frames, RMS={rms:.4}, idx={}",
            data.len(),
            decoded.num_frames,
            decoded.channels,
            decoded.index,
        ))
    } else if data.len() >= 4 && &data[0..4] == b"WAIF" {
        // WAIF streaming frame — just validate the header
        let frame = wail_audio::AudioFrameWire::decode(data)?;
        Ok(format!(
            "WAIF frame: {} bytes, frame #{}, interval {}, final={}",
            data.len(),
            frame.frame_number,
            frame.interval_index,
            frame.is_final,
        ))
    } else {
        bail!(
            "unknown wire format: magic={:?}",
            &data[..data.len().min(4)]
        );
    }
}

fn rms(samples: &[f32]) -> f32 {
    if samples.is_empty() {
        return 0.0;
    }
    let sum: f64 = samples.iter().map(|&s| (s as f64) * (s as f64)).sum();
    (sum / samples.len() as f64).sqrt() as f32
}
