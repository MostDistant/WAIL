//! Network integration tests: two WAIL peers exchanging audio over real WebRTC.
//!
//! These tests exercise the full path:
//!   Signaling server → WebRTC negotiation → DataChannel establishment → audio exchange
//!
//! No external services needed: in-process signaling server, localhost ICE candidates.

use std::time::Duration;

use tokio::net::TcpListener;
use wail_audio::AudioBridge;
use wail_net::PeerMesh;

/// Generate a recognizable test signal: sine wave at a given frequency.
fn sine_wave(freq_hz: f32, duration_samples: usize, channels: u16, sample_rate: u32) -> Vec<f32> {
    let mut out = Vec::with_capacity(duration_samples * channels as usize);
    for i in 0..duration_samples {
        let t = i as f32 / sample_rate as f32;
        let sample = (t * freq_hz * 2.0 * std::f32::consts::PI).sin() * 0.5;
        for _ in 0..channels {
            out.push(sample);
        }
    }
    out
}

/// Compute RMS energy of a signal.
fn rms(samples: &[f32]) -> f32 {
    if samples.is_empty() {
        return 0.0;
    }
    let sum: f32 = samples.iter().map(|s| s * s).sum();
    (sum / samples.len() as f32).sqrt()
}

/// Produce an encoded audio interval from an AudioBridge.
/// Records a sine wave through one full interval, crosses the boundary, returns wire bytes.
fn produce_interval(freq_hz: f32) -> Vec<u8> {
    let sr = 48000u32;
    let ch = 2u16;
    let buf_size = 4096;
    let mut bridge = AudioBridge::new(sr, ch, 4, 4.0, 128);
    let signal = sine_wave(freq_hz, buf_size / ch as usize, ch, sr);
    let mut out = vec![0.0f32; buf_size];

    for beat in [0.0, 4.0, 8.0, 12.0] {
        bridge.process(&signal, &mut out, beat);
    }
    let wire_msgs = bridge.process(&signal, &mut out, 16.0);
    assert_eq!(wire_msgs.len(), 1, "Should produce exactly 1 interval");
    wire_msgs.into_iter().next().unwrap()
}

/// Pump signaling for both meshes until they see each other, then wait for DataChannels.
async fn establish_connection(mesh_a: &mut PeerMesh, mesh_b: &mut PeerMesh) {
    let deadline = tokio::time::Instant::now() + Duration::from_secs(15);
    let min_settle = tokio::time::Instant::now() + Duration::from_secs(2);

    loop {
        tokio::select! {
            _ = mesh_a.poll_signaling() => {}
            _ = mesh_b.poll_signaling() => {}
            _ = tokio::time::sleep(Duration::from_millis(200)) => {
                let both_connected = !mesh_a.connected_peers().is_empty()
                    && !mesh_b.connected_peers().is_empty();
                if both_connected && tokio::time::Instant::now() > min_settle {
                    // Extra settle time for SCTP/DataChannels to open
                    tokio::time::sleep(Duration::from_secs(1)).await;
                    return;
                }
            }
            _ = tokio::time::sleep_until(deadline) => {
                panic!(
                    "WebRTC connection timed out. Peers: A={:?}, B={:?}",
                    mesh_a.connected_peers(),
                    mesh_b.connected_peers()
                );
            }
        }
    }
}

// ---------------------------------------------------------------
// Test: Two peers exchange audio intervals over real WebRTC
// ---------------------------------------------------------------

#[tokio::test(flavor = "multi_thread")]
async fn two_peers_exchange_audio_over_webrtc() {
    let _ = tracing_subscriber::fmt()
        .with_env_filter("info")
        .try_init();

    // 1. Start in-process signaling server on a random port
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(wail_signaling::run(listener));

    let server_url = format!("ws://{}", addr);

    // 2. Connect both peers to the signaling server
    //    "peer-a" < "peer-b" → peer-a will be the WebRTC initiator
    let (mut mesh_a, _sync_rx_a, mut audio_rx_a) =
        PeerMesh::connect(&server_url, "test-room", "peer-a")
            .await
            .expect("Peer A failed to connect to signaling");

    tokio::time::sleep(Duration::from_millis(100)).await;

    let (mut mesh_b, _sync_rx_b, mut audio_rx_b) =
        PeerMesh::connect(&server_url, "test-room", "peer-b")
            .await
            .expect("Peer B failed to connect to signaling");

    // 3. Pump signaling until WebRTC DataChannels are established
    establish_connection(&mut mesh_a, &mut mesh_b).await;

    // 4. Peer A → Peer B: send audio interval over WebRTC
    let wire_a = produce_interval(440.0);
    mesh_a.broadcast_audio(&wire_a).await;

    let (from, received) = tokio::time::timeout(Duration::from_secs(5), audio_rx_b.recv())
        .await
        .expect("Timed out waiting for audio from A")
        .expect("Audio channel B closed");

    assert_eq!(from, "peer-a");
    assert!(!received.is_empty(), "Wire data should be non-empty");

    // Decode and verify it's real audio
    let sr = 48000u32;
    let ch = 2u16;
    let buf_size = 4096;
    let mut bridge_b = AudioBridge::new(sr, ch, 4, 4.0, 128);
    let silence = vec![0.0f32; buf_size];
    let mut out = vec![0.0f32; buf_size];

    bridge_b.process(&silence, &mut out, 0.0); // start interval 0
    bridge_b.receive_wire(&from, &received);
    bridge_b.process(&silence, &mut out, 16.0); // cross boundary — play remote

    let energy = rms(&out);
    assert!(
        energy > 0.01,
        "Peer B should hear Peer A's audio over WebRTC, RMS={energy}"
    );

    // 5. Peer B → Peer A: send audio interval (bidirectional test)
    let wire_b = produce_interval(880.0);
    mesh_b.broadcast_audio(&wire_b).await;

    let (from_b, received_b) = tokio::time::timeout(Duration::from_secs(5), audio_rx_a.recv())
        .await
        .expect("Timed out waiting for audio from B")
        .expect("Audio channel A closed");

    assert_eq!(from_b, "peer-b");
    assert!(!received_b.is_empty(), "Wire data should be non-empty");

    // Decode and verify
    let mut bridge_a = AudioBridge::new(sr, ch, 4, 4.0, 128);
    bridge_a.process(&silence, &mut out, 0.0);
    bridge_a.receive_wire(&from_b, &received_b);
    bridge_a.process(&silence, &mut out, 16.0);

    let energy_b = rms(&out);
    assert!(
        energy_b > 0.01,
        "Peer A should hear Peer B's audio over WebRTC, RMS={energy_b}"
    );
}
