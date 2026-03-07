use std::collections::HashMap;

use anyhow::Result;
use futures_util::{SinkExt, StreamExt};
use tokio::sync::mpsc;
use tokio_tungstenite::tungstenite::Message;
use tracing::{debug, error, info, warn};

use wail_core::protocol::SignalMessage;

/// WebSocket signaling client that connects to the WAIL signaling server,
/// joins a room, and relays WebRTC signaling messages in real time.
pub struct SignalingClient {
    pub incoming_rx: mpsc::UnboundedReceiver<SignalMessage>,
    pub outgoing_tx: mpsc::UnboundedSender<SignalMessage>,
}

/// A public room returned by the signaling server's list endpoint.
#[derive(Debug, Clone, serde::Deserialize, serde::Serialize)]
pub struct PublicRoom {
    pub room: String,
    pub created_at: i64,
    pub peer_count: u32,
    pub display_names: Vec<String>,
    #[serde(default)]
    pub bpm: Option<f64>,
}

#[derive(serde::Deserialize)]
struct ListResponse {
    rooms: Vec<PublicRoom>,
}

/// Fetch the list of public rooms from a signaling server.
///
/// Uses the HTTP `/rooms` endpoint (not WebSocket).
pub async fn list_public_rooms(base_url: &str) -> Result<Vec<PublicRoom>> {
    // Convert ws(s):// to http(s):// for the REST endpoint
    let http_url = base_url
        .replace("wss://", "https://")
        .replace("ws://", "http://");
    let base = http_url.trim_end_matches('/');
    let resp = reqwest::Client::new()
        .get(format!("{base}/rooms"))
        .send()
        .await?
        .error_for_status()?;
    let list: ListResponse = resp.json().await?;
    Ok(list.rooms)
}

/// Server response to a join request.
#[derive(serde::Deserialize)]
#[serde(tag = "type")]
enum ServerMsg {
    #[serde(rename = "join_ok")]
    JoinOk {
        peers: Vec<String>,
        #[serde(default)]
        peer_display_names: HashMap<String, Option<String>>,
    },
    #[serde(rename = "join_error")]
    JoinError {
        code: String,
        #[serde(default)]
        min_version: Option<String>,
        #[serde(default)]
        slots_available: Option<u64>,
    },
    #[serde(rename = "peer_joined")]
    PeerJoined {
        peer_id: String,
        display_name: Option<String>,
    },
    #[serde(rename = "peer_left")]
    PeerLeft {
        peer_id: String,
    },
    #[serde(rename = "signal")]
    Signal {
        to: String,
        from: String,
        payload: serde_json::Value,
    },
    #[serde(rename = "evicted")]
    Evicted,
}

impl SignalingClient {
    /// Connect to the WebSocket signaling server and join a room.
    pub async fn connect(
        server_url: &str,
        room: &str,
        peer_id: &str,
        password: Option<&str>,
    ) -> Result<(Self, HashMap<String, Option<String>>)> {
        Self::connect_with_options(server_url, room, peer_id, password, 1, None).await
    }

    /// Connect with full options including stream count and display name.
    pub async fn connect_with_options(
        server_url: &str,
        room: &str,
        peer_id: &str,
        password: Option<&str>,
        stream_count: u16,
        display_name: Option<&str>,
    ) -> Result<(Self, HashMap<String, Option<String>>)> {
        // Build WebSocket URL
        let ws_url = format!("{}/ws", server_url.trim_end_matches('/'));

        let (ws_stream, _) = tokio_tungstenite::connect_async(&ws_url).await?;
        let (mut ws_write, mut ws_read) = ws_stream.split();

        // Send join message
        let mut join_msg = serde_json::json!({
            "type": "join",
            "room": room,
            "peer_id": peer_id,
            "stream_count": stream_count,
            "client_version": env!("CARGO_PKG_VERSION"),
        });
        if let Some(pw) = password {
            join_msg["password"] = serde_json::Value::String(pw.to_string());
        }
        if let Some(name) = display_name {
            join_msg["display_name"] = serde_json::Value::String(name.to_string());
        }
        ws_write
            .send(Message::Text(join_msg.to_string()))
            .await?;

        // Wait for join_ok or join_error
        let join_response = loop {
            match ws_read.next().await {
                Some(Ok(Message::Text(text))) => {
                    break serde_json::from_str::<ServerMsg>(&text)?;
                }
                Some(Ok(Message::Close(_))) | None => {
                    anyhow::bail!("WebSocket closed before join response");
                }
                Some(Err(e)) => {
                    anyhow::bail!("WebSocket error waiting for join response: {e}");
                }
                _ => continue, // skip ping/pong/binary
            }
        };

        let (peers, initial_peer_names) = match join_response {
            ServerMsg::JoinOk {
                peers,
                peer_display_names,
            } => (peers, peer_display_names),
            ServerMsg::JoinError {
                code,
                min_version,
                slots_available,
            } => match code.as_str() {
                "unauthorized" => {
                    anyhow::bail!(
                        "Invalid room password — the room exists and the password doesn't match"
                    );
                }
                "room_full" => {
                    let slots = slots_available.unwrap_or(0);
                    anyhow::bail!("Room full — only {slots} stream slots available");
                }
                "version_outdated" => {
                    let min = min_version.as_deref().unwrap_or("unknown");
                    anyhow::bail!(
                        "Your WAIL version ({}) is outdated. Please update to at least version {min}.",
                        env!("CARGO_PKG_VERSION")
                    );
                }
                other => anyhow::bail!("Join failed: {other}"),
            },
            _ => anyhow::bail!("Unexpected server message before join_ok"),
        };

        info!(
            %server_url, %room, %peer_id,
            existing_peers = peers.len(),
            "Joined signaling room via WebSocket"
        );

        let (incoming_tx, incoming_rx) = mpsc::unbounded_channel();
        let (outgoing_tx, mut outgoing_rx) = mpsc::unbounded_channel::<SignalMessage>();

        // Push PeerList so PeerMesh sees existing peers
        if incoming_tx
            .send(SignalMessage::PeerList {
                peers,
            })
            .is_err()
        {
            anyhow::bail!("incoming channel closed immediately");
        }

        // Spawn read task: server → incoming channel
        let incoming_tx2 = incoming_tx.clone();
        tokio::spawn(async move {
            while let Some(msg_result) = ws_read.next().await {
                match msg_result {
                    Ok(Message::Text(text)) => {
                        match serde_json::from_str::<ServerMsg>(&text) {
                            Ok(server_msg) => {
                                let signal = match server_msg {
                                    ServerMsg::PeerJoined {
                                        peer_id,
                                        display_name,
                                    } => SignalMessage::PeerJoined {
                                        peer_id,
                                        display_name,
                                    },
                                    ServerMsg::PeerLeft { peer_id } => {
                                        SignalMessage::PeerLeft { peer_id }
                                    }
                                    ServerMsg::Signal { to, from, payload } => {
                                        // Reconstruct SignalPayload from the raw JSON
                                        match serde_json::from_value(payload) {
                                            Ok(p) => SignalMessage::Signal { to, from, payload: p },
                                            Err(e) => {
                                                warn!(error = %e, "Failed to parse signal payload");
                                                continue;
                                            }
                                        }
                                    }
                                    ServerMsg::Evicted => {
                                        warn!("Server evicted us — closing signaling");
                                        return;
                                    }
                                    _ => continue,
                                };
                                debug!(?signal, "WS received");
                                if incoming_tx2.send(signal).is_err() {
                                    info!("Incoming channel closed, stopping WS read");
                                    return;
                                }
                            }
                            Err(e) => {
                                warn!(error = %e, body = %text, "Failed to parse server message");
                            }
                        }
                    }
                    Ok(Message::Close(_)) => {
                        info!("WebSocket closed by server");
                        return;
                    }
                    Err(e) => {
                        error!(error = %e, "WebSocket read error");
                        return;
                    }
                    _ => {} // ping/pong handled by tungstenite
                }
            }
            info!("WebSocket stream ended");
        });

        // Spawn write task: outgoing channel → server
        tokio::spawn(async move {
            while let Some(msg) = outgoing_rx.recv().await {
                debug!(?msg, "Sending signal via WS");
                let raw = serde_json::json!({
                    "type": "signal",
                    "to": match &msg {
                        SignalMessage::Signal { to, .. } => to.as_str(),
                        _ => "",
                    },
                    "from": match &msg {
                        SignalMessage::Signal { from, .. } => from.as_str(),
                        _ => "",
                    },
                    "payload": match &msg {
                        SignalMessage::Signal { payload, .. } => serde_json::to_value(payload).unwrap_or_default(),
                        _ => serde_json::Value::Null,
                    },
                });
                if ws_write
                    .send(Message::Text(raw.to_string()))
                    .await
                    .is_err()
                {
                    warn!("WebSocket write failed — connection lost");
                    return;
                }
            }
            // Outgoing channel closed — send leave
            info!("Outgoing channel closed, sending leave");
            let _ = ws_write
                .send(Message::Text(
                    serde_json::json!({"type": "leave"}).to_string(),
                ))
                .await;
            let _ = ws_write.close().await;
        });

        Ok((
            Self {
                incoming_rx,
                outgoing_tx,
            },
            initial_peer_names,
        ))
    }
}
