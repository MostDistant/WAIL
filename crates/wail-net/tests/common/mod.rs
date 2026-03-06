//! Shared test helpers for wail-net integration tests.

#![allow(dead_code)]

use std::collections::{HashMap, HashSet};
use std::future::IntoFuture;
use std::net::TcpListener as StdTcpListener;
use std::sync::Arc;
use std::time::Duration;

use axum::extract::{Query, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use tokio::net::TcpListener;
use tokio::sync::Mutex;
use wail_audio::AudioBridge;
use wail_net::PeerMesh;

// ---------------------------------------------------------------------------
// Configurable in-process HTTP signaling server (mirrors Val Town endpoint)
// ---------------------------------------------------------------------------

/// Server-side configuration injected at startup for §1.x signaling tests.
#[derive(Default, Clone)]
pub struct TestServerConfig {
    /// Minimum acceptable `client_version` (semver string). `None` = no check.
    pub min_version: Option<String>,
    /// Required room password. `None` = all rooms are public (no password required).
    pub password: Option<String>,
    /// Maximum stream slots per room. `None` = unlimited.
    pub room_capacity: Option<usize>,
    /// When `true`, all requests immediately return HTTP 429.
    pub rate_limit_mode: bool,
}

#[derive(Default)]
struct SignalingState {
    /// room → peer_ids
    rooms: HashMap<String, Vec<String>>,
    /// "room:peer_id" → stream_count, for capacity accounting
    peer_slots: HashMap<String, usize>,
    /// Queued signaling messages
    messages: Vec<StoredMessage>,
    next_seq: i64,
    /// "room:peer_id" keys scheduled to receive `evicted: true` on next poll
    evicted_peers: HashSet<String>,
    config: TestServerConfig,
}

struct StoredMessage {
    seq: i64,
    room: String,
    to_peer: String,
    body: serde_json::Value,
}

type SharedState = Arc<Mutex<SignalingState>>;

// ---------------------------------------------------------------------------
// Public test handle (exposes admin operations)
// ---------------------------------------------------------------------------

/// A handle to the running test server that allows in-test control.
#[derive(Clone)]
pub struct TestServerHandle {
    /// Base URL of the signaling server (e.g. `"http://127.0.0.1:PORT"`).
    pub url: String,
    state: SharedState,
}

impl TestServerHandle {
    /// Schedule a peer to receive `evicted: true` on its next poll, causing
    /// the signaling client to close its channel and trigger session reconnection.
    pub async fn evict_peer(&self, room: &str, peer_id: &str) {
        self.state
            .lock()
            .await
            .evicted_peers
            .insert(format!("{room}:{peer_id}"));
    }

    /// Return the IDs of all peers currently in a room.
    pub async fn peers_in_room(&self, room: &str) -> Vec<String> {
        self.state
            .lock()
            .await
            .rooms
            .get(room)
            .cloned()
            .unwrap_or_default()
    }

    /// Total stream slots used in a room.
    pub async fn slots_used(&self, room: &str) -> usize {
        let s = self.state.lock().await;
        s.rooms
            .get(room)
            .map(|peers| {
                peers
                    .iter()
                    .map(|p| {
                        s.peer_slots
                            .get(&format!("{room}:{p}"))
                            .copied()
                            .unwrap_or(1)
                    })
                    .sum()
            })
            .unwrap_or(0)
    }
}

// ---------------------------------------------------------------------------
// Simple semver comparison for the min_version check
// ---------------------------------------------------------------------------

/// Returns `true` if `a` is strictly less than `b` under semver ordering.
fn semver_less_than(a: &str, b: &str) -> bool {
    fn parse(s: &str) -> (u64, u64, u64) {
        let mut parts = s.split('.').filter_map(|p| p.parse::<u64>().ok());
        (
            parts.next().unwrap_or(0),
            parts.next().unwrap_or(0),
            parts.next().unwrap_or(0),
        )
    }
    parse(a) < parse(b)
}

// ---------------------------------------------------------------------------
// Request handlers
// ---------------------------------------------------------------------------

async fn handle_join(
    State(state): State<SharedState>,
    Json(body): Json<serde_json::Value>,
) -> Response {
    let room = body["room"].as_str().unwrap_or("").to_string();
    let peer_id = body["peer_id"].as_str().unwrap_or("").to_string();
    let display_name = body["display_name"].as_str().map(|s| s.to_string());
    let stream_count = body["stream_count"].as_u64().unwrap_or(1) as usize;
    let client_version = body["client_version"].as_str().unwrap_or("0.0.0").to_string();
    let password = body["password"].as_str().map(|s| s.to_string());

    let mut s = state.lock().await;

    // §1.1 — version check
    if let Some(min) = &s.config.min_version.clone() {
        if semver_less_than(&client_version, min) {
            return (
                StatusCode::UPGRADE_REQUIRED,
                Json(serde_json::json!({
                    "error": "client_version_too_old",
                    "min_version": min
                })),
            )
                .into_response();
        }
    }

    // §1.1 — password check
    if let Some(required) = &s.config.password.clone() {
        let sent = password.as_deref().unwrap_or("");
        if sent != required {
            return (
                StatusCode::UNAUTHORIZED,
                Json(serde_json::json!({ "error": "invalid_password" })),
            )
                .into_response();
        }
    }

    // §1.1 — capacity check
    if let Some(capacity) = s.config.room_capacity {
        let used: usize = s
            .rooms
            .get(&room)
            .map(|peers| {
                peers
                    .iter()
                    .map(|p| {
                        s.peer_slots
                            .get(&format!("{room}:{p}"))
                            .copied()
                            .unwrap_or(1)
                    })
                    .sum()
            })
            .unwrap_or(0);
        if used + stream_count > capacity {
            return (
                StatusCode::CONFLICT,
                Json(serde_json::json!({
                    "error": "room_full",
                    "slots_available": capacity.saturating_sub(used)
                })),
            )
                .into_response();
        }
    }

    // Standard join
    let peers_in_room = s.rooms.entry(room.clone()).or_default();
    let existing: Vec<String> = peers_in_room
        .iter()
        .filter(|p| *p != &peer_id)
        .cloned()
        .collect();
    if !peers_in_room.contains(&peer_id) {
        peers_in_room.push(peer_id.clone());
    }
    s.peer_slots
        .insert(format!("{room}:{peer_id}"), stream_count);

    for p in &existing {
        s.next_seq += 1;
        let seq = s.next_seq;
        s.messages.push(StoredMessage {
            seq,
            room: room.clone(),
            to_peer: p.clone(),
            body: serde_json::json!({
                "type": "PeerJoined",
                "peer_id": peer_id,
                "display_name": display_name,
            }),
        });
    }

    let peer_display_names: HashMap<String, Option<String>> = existing
        .iter()
        .map(|id| (id.clone(), None))
        .collect();

    Json(serde_json::json!({
        "peers": existing,
        "peer_display_names": peer_display_names
    }))
    .into_response()
}

async fn handle_signal(
    State(state): State<SharedState>,
    Json(body): Json<serde_json::Value>,
) -> Response {
    let to = body["to"].as_str().unwrap_or("").to_string();
    let from = body["from"].as_str().unwrap_or("").to_string();

    let mut s = state.lock().await;
    let room = s
        .rooms
        .iter()
        .find(|(_, peers)| peers.contains(&from))
        .map(|(r, _)| r.clone())
        .unwrap_or_default();

    s.next_seq += 1;
    let seq = s.next_seq;
    s.messages.push(StoredMessage {
        seq,
        room,
        to_peer: to,
        body,
    });

    Json(serde_json::json!({ "ok": true })).into_response()
}

async fn handle_leave(
    State(state): State<SharedState>,
    Json(body): Json<serde_json::Value>,
) -> Response {
    let room = body["room"].as_str().unwrap_or("").to_string();
    let peer_id = body["peer_id"].as_str().unwrap_or("").to_string();

    let mut s = state.lock().await;
    if let Some(peers) = s.rooms.get_mut(&room) {
        peers.retain(|p| p != &peer_id);
        // §1.1 — delete room when empty so it can be recreated with different config
        if peers.is_empty() {
            s.rooms.remove(&room);
        }
    }
    s.peer_slots.remove(&format!("{room}:{peer_id}"));

    // Notify remaining peers
    let remaining: Vec<String> = s
        .rooms
        .get(&room)
        .cloned()
        .unwrap_or_default();
    for p in remaining {
        s.next_seq += 1;
        let seq = s.next_seq;
        s.messages.push(StoredMessage {
            seq,
            room: room.clone(),
            to_peer: p,
            body: serde_json::json!({
                "type": "PeerLeft",
                "peer_id": peer_id,
            }),
        });
    }

    Json(serde_json::json!({ "ok": true })).into_response()
}

#[derive(serde::Deserialize)]
struct PollQuery {
    room: String,
    peer_id: String,
    after: Option<i64>,
}

async fn handle_poll(
    State(state): State<SharedState>,
    Query(q): Query<PollQuery>,
) -> Response {
    let after = q.after.unwrap_or(0);
    let mut s = state.lock().await;

    // §1.2 — eviction support
    let evict_key = format!("{}:{}", q.room, q.peer_id);
    let evicted = s.evicted_peers.remove(&evict_key);

    let messages: Vec<serde_json::Value> = s
        .messages
        .iter()
        .filter(|m| m.room == q.room && m.to_peer == q.peer_id && m.seq > after)
        .map(|m| serde_json::json!({ "seq": m.seq, "body": m.body }))
        .collect();

    Json(serde_json::json!({
        "messages": messages,
        "evicted": evicted,
    }))
    .into_response()
}

async fn handle_post(
    Query(params): Query<HashMap<String, String>>,
    State(state): State<SharedState>,
    body: Json<serde_json::Value>,
) -> Response {
    // §1.2 — rate-limit mode
    if state.lock().await.config.rate_limit_mode {
        return (
            StatusCode::TOO_MANY_REQUESTS,
            Json(serde_json::json!({ "error": "rate_limited" })),
        )
            .into_response();
    }

    match params.get("action").map(|s| s.as_str()) {
        Some("join") => handle_join(State(state), body).await,
        Some("signal") => handle_signal(State(state), body).await,
        Some("leave") => handle_leave(State(state), body).await,
        _ => Json(serde_json::json!({ "error": "unknown action" })).into_response(),
    }
}

async fn handle_get(
    Query(params): Query<HashMap<String, String>>,
    State(state): State<SharedState>,
) -> Response {
    // §1.2 — rate-limit mode
    if state.lock().await.config.rate_limit_mode {
        return (
            StatusCode::TOO_MANY_REQUESTS,
            Json(serde_json::json!({ "error": "rate_limited" })),
        )
            .into_response();
    }

    if params.get("action").map(|s| s.as_str()) == Some("poll") {
        let q = PollQuery {
            room: params.get("room").cloned().unwrap_or_default(),
            peer_id: params.get("peer_id").cloned().unwrap_or_default(),
            after: params.get("after").and_then(|s| s.parse().ok()),
        };
        handle_poll(State(state), Query(q)).await
    } else {
        Json(serde_json::json!({ "error": "unknown action" })).into_response()
    }
}

// ---------------------------------------------------------------------------
// Server startup
// ---------------------------------------------------------------------------

fn build_app(state: SharedState) -> Router {
    Router::new()
        .route("/", post(handle_post))
        .route("/", get(handle_get))
        .with_state(state)
}

/// Start a plain test signaling server. Returns the base URL.
/// All existing tests use this function — signature is unchanged.
pub async fn start_test_signaling_server() -> String {
    start_configured_signaling_server(TestServerConfig::default())
        .await
        .url
}

/// Start a configurable test signaling server. Returns a handle with admin methods.
pub async fn start_configured_signaling_server(config: TestServerConfig) -> TestServerHandle {
    let state: SharedState = Arc::new(Mutex::new(SignalingState {
        config,
        ..Default::default()
    }));

    let app = build_app(state.clone());
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(axum::serve(listener, app).into_future());

    TestServerHandle {
        url: format!("http://{}", addr),
        state,
    }
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

/// Generate a recognizable test signal: sine wave at a given frequency.
pub fn sine_wave(freq_hz: f32, duration_samples: usize, channels: u16, sample_rate: u32) -> Vec<f32> {
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
pub fn rms(samples: &[f32]) -> f32 {
    if samples.is_empty() {
        return 0.0;
    }
    let sum: f32 = samples.iter().map(|s| s * s).sum();
    (sum / samples.len() as f32).sqrt()
}

/// Produce an encoded audio interval from an AudioBridge.
/// Records a sine wave through one full interval, crosses the boundary, returns wire bytes.
pub fn produce_interval(freq_hz: f32) -> Vec<u8> {
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
pub async fn establish_connection(mesh_a: &mut PeerMesh, mesh_b: &mut PeerMesh) {
    establish_connection_timeout(mesh_a, mesh_b, 15).await;
}

pub async fn establish_connection_timeout(
    mesh_a: &mut PeerMesh,
    mesh_b: &mut PeerMesh,
    timeout_secs: u64,
) {
    let deadline = tokio::time::Instant::now() + Duration::from_secs(timeout_secs);

    loop {
        tokio::select! {
            result = mesh_a.poll_signaling() => {
                if let Err(e) = result {
                    eprintln!("[test] mesh_a poll error: {e}");
                }
            }
            result = mesh_b.poll_signaling() => {
                if let Err(e) = result {
                    eprintln!("[test] mesh_b poll error: {e}");
                }
            }
            _ = tokio::time::sleep(Duration::from_millis(200)) => {
                let both_open = mesh_a.any_audio_dc_open() && mesh_b.any_audio_dc_open();
                if both_open {
                    tokio::time::sleep(Duration::from_millis(500)).await;
                    return;
                }
            }
            _ = tokio::time::sleep_until(deadline) => {
                panic!(
                    "WebRTC connection timed out. Peers: A={:?}, B={:?}, DCs: A={}, B={}",
                    mesh_a.connected_peers(),
                    mesh_b.connected_peers(),
                    mesh_a.any_audio_dc_open(),
                    mesh_b.any_audio_dc_open(),
                );
            }
        }
    }
}

/// Pump signaling for three meshes until all six directed DataChannel paths are open.
/// `ids` is `(id_a, id_b, id_c)` matching mesh_a, mesh_b, mesh_c respectively.
pub async fn establish_three_way_connection(
    mesh_a: &mut PeerMesh,
    mesh_b: &mut PeerMesh,
    mesh_c: &mut PeerMesh,
    ids: (&str, &str, &str),
) {
    let deadline = tokio::time::Instant::now() + Duration::from_secs(30);

    loop {
        tokio::select! {
            result = mesh_a.poll_signaling() => {
                if let Err(e) = result { eprintln!("[test] mesh_a poll error: {e}"); }
            }
            result = mesh_b.poll_signaling() => {
                if let Err(e) = result { eprintln!("[test] mesh_b poll error: {e}"); }
            }
            result = mesh_c.poll_signaling() => {
                if let Err(e) = result { eprintln!("[test] mesh_c poll error: {e}"); }
            }
            _ = tokio::time::sleep(Duration::from_millis(200)) => {
                // All six directed paths must have open audio DCs.
                let all_open =
                    mesh_a.is_peer_audio_dc_open(ids.1) && mesh_a.is_peer_audio_dc_open(ids.2)
                    && mesh_b.is_peer_audio_dc_open(ids.0) && mesh_b.is_peer_audio_dc_open(ids.2)
                    && mesh_c.is_peer_audio_dc_open(ids.0) && mesh_c.is_peer_audio_dc_open(ids.1);
                if all_open {
                    tokio::time::sleep(Duration::from_millis(500)).await;
                    return;
                }
            }
            _ = tokio::time::sleep_until(deadline) => {
                panic!(
                    "3-way WebRTC connection timed out. \
                     A→B={} A→C={} B→A={} B→C={} C→A={} C→B={}",
                    mesh_a.is_peer_audio_dc_open(ids.1),
                    mesh_a.is_peer_audio_dc_open(ids.2),
                    mesh_b.is_peer_audio_dc_open(ids.0),
                    mesh_b.is_peer_audio_dc_open(ids.2),
                    mesh_c.is_peer_audio_dc_open(ids.0),
                    mesh_c.is_peer_audio_dc_open(ids.1),
                );
            }
        }
    }
}

/// Produce a realistically-sized encoded audio interval from an AudioBridge.
///
/// Unlike `produce_interval()` which only records a handful of buffers,
/// this simulates a real DAW callback loop: 256-frame buffers at 120 BPM,
/// advancing beat position proportionally, filling the full 8-second interval.
///
/// Returns `(wire_bytes, expected_interleaved_samples)`.
pub fn produce_full_interval(freq_hz: f32) -> (Vec<u8>, usize) {
    let sr = 48000u32;
    let ch = 2u16;
    let bpm = 120.0_f64;
    let buf_frames: usize = 256;
    let buf_size = buf_frames * ch as usize;

    let mut bridge = AudioBridge::new(sr, ch, 4, 4.0, 128);

    let signal = sine_wave(freq_hz, buf_frames, ch, sr);
    let mut out = vec![0.0f32; buf_size];

    let beats_per_callback = buf_frames as f64 / sr as f64 * bpm / 60.0;
    let mut beat = 0.0_f64;

    // Fill interval 0 (beats 0..16)
    while beat < 16.0 {
        bridge.process(&signal, &mut out, beat);
        beat += beats_per_callback;
    }

    // Cross boundary — this triggers encode and returns wire bytes
    let wire_msgs = bridge.process(&signal, &mut out, beat);
    assert_eq!(wire_msgs.len(), 1, "Should produce exactly 1 interval");

    // Expected interleaved sample count for a full interval
    let expected_samples = (sr as f64 * ch as f64 * 16.0 / (bpm / 60.0)) as usize;
    (wire_msgs.into_iter().next().unwrap(), expected_samples)
}

/// Find a random available port by binding to :0.
pub fn random_port() -> u16 {
    let listener = StdTcpListener::bind("127.0.0.1:0").unwrap();
    listener.local_addr().unwrap().port()
}

// ---------------------------------------------------------------------------
// semver_less_than unit tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn semver_comparison_basic() {
        assert!(semver_less_than("1.0.0", "2.0.0"));
        assert!(semver_less_than("1.2.3", "1.10.0")); // would fail lexicographic
        assert!(!semver_less_than("2.0.0", "1.9.9"));
        assert!(!semver_less_than("1.2.3", "1.2.3")); // equal is not less than
    }
}
