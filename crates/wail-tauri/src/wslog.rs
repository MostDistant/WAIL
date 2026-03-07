use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use tokio::sync::broadcast;
use tracing::field::{Field, Visit};
use tracing::{Event, Subscriber};
use tracing_subscriber::layer::Context;
use tracing_subscriber::registry::LookupSpan;
use tracing_subscriber::Layer;

const CHANNEL_CAPACITY: usize = 256;

// ---------------------------------------------------------------------------
// StructuredLogEntry
// ---------------------------------------------------------------------------

#[derive(Debug, Clone)]
pub struct StructuredLogEntry {
    pub level: String,
    pub target: String,
    pub message: String,
    pub timestamp_us: u64,
}

// ---------------------------------------------------------------------------
// Field visitor (reuses pattern from filelog.rs)
// ---------------------------------------------------------------------------

#[derive(Default)]
struct MessageVisitor {
    message: String,
}

impl Visit for MessageVisitor {
    fn record_debug(&mut self, field: &Field, value: &dyn std::fmt::Debug) {
        if field.name() == "message" {
            self.message = format!("{:?}", value);
        }
    }

    fn record_str(&mut self, field: &Field, value: &str) {
        if field.name() == "message" {
            self.message = value.to_string();
        }
    }
}

// ---------------------------------------------------------------------------
// WsLogHandle — shared runtime control
// ---------------------------------------------------------------------------

#[derive(Clone)]
pub struct WsLogHandle {
    enabled: Arc<AtomicBool>,
    tx: broadcast::Sender<StructuredLogEntry>,
}

impl WsLogHandle {
    pub fn set_enabled(&self, enabled: bool) {
        self.enabled.store(enabled, Ordering::Relaxed);
    }

    pub fn is_enabled(&self) -> bool {
        self.enabled.load(Ordering::Relaxed)
    }

    pub fn subscribe(&self) -> broadcast::Receiver<StructuredLogEntry> {
        self.tx.subscribe()
    }
}

// ---------------------------------------------------------------------------
// WsLogLayer — tracing subscriber layer
// ---------------------------------------------------------------------------

pub struct WsLogLayer {
    enabled: Arc<AtomicBool>,
    tx: broadcast::Sender<StructuredLogEntry>,
}

pub fn new() -> (WsLogLayer, WsLogHandle) {
    let enabled = Arc::new(AtomicBool::new(false));
    let (tx, _) = broadcast::channel(CHANNEL_CAPACITY);
    let handle = WsLogHandle {
        enabled: enabled.clone(),
        tx: tx.clone(),
    };
    (WsLogLayer { enabled, tx }, handle)
}

impl<S> Layer<S> for WsLogLayer
where
    S: Subscriber + for<'a> LookupSpan<'a>,
{
    fn on_event(&self, event: &Event<'_>, _ctx: Context<'_, S>) {
        if !self.enabled.load(Ordering::Relaxed) {
            return;
        }

        // Only forward INFO and above; skip DEBUG and TRACE to avoid flooding.
        if *event.metadata().level() > tracing::Level::INFO {
            return;
        }

        let metadata = event.metadata();
        let level = metadata.level().as_str().to_lowercase();
        let target = metadata.target().to_string();

        let mut visitor = MessageVisitor::default();
        event.record(&mut visitor);

        let timestamp_us = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_micros() as u64;

        let entry = StructuredLogEntry {
            level,
            target,
            message: visitor.message,
            timestamp_us,
        };

        // Non-blocking send; lagged receivers silently drop old entries.
        let _ = self.tx.send(entry);
    }
}
