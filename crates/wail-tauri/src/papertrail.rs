use std::collections::BTreeMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use tokio::sync::mpsc;
use tracing::field::{Field, Visit};
use tracing::span;
use tracing::{Event, Subscriber};
use tracing_subscriber::layer::Context;
use tracing_subscriber::registry::LookupSpan;
use tracing_subscriber::Layer;

const ENDPOINT: &str = "https://logs.collector.na-01.cloud.solarwinds.com/v1/logs/bulk";
const TOKEN: &str = "SEm6aCu8rnELg4EKNLpW3ibNfmpKs2CTF-CBOhIxRBTeKVYYdxu4DOyLpkaPZJiLKqOUd2E";
const FLUSH_INTERVAL_SECS: u64 = 5;
const MAX_BUFFER_LINES: usize = 10_000;

// ---------------------------------------------------------------------------
// Span field storage (kept in span extensions)
// ---------------------------------------------------------------------------

#[derive(Default, Clone)]
struct SpanFields(BTreeMap<String, String>);

impl Visit for SpanFields {
    fn record_debug(&mut self, field: &Field, value: &dyn std::fmt::Debug) {
        self.0
            .insert(field.name().to_string(), format!("{:?}", value));
    }

    fn record_str(&mut self, field: &Field, value: &str) {
        self.0
            .insert(field.name().to_string(), value.to_string());
    }
}

// ---------------------------------------------------------------------------
// Event field visitor
// ---------------------------------------------------------------------------

#[derive(Default)]
struct EventVisitor {
    message: String,
    fields: Vec<(String, String)>,
}

impl Visit for EventVisitor {
    fn record_debug(&mut self, field: &Field, value: &dyn std::fmt::Debug) {
        if field.name() == "message" {
            self.message = format!("{:?}", value);
        } else {
            self.fields
                .push((field.name().to_string(), format!("{:?}", value)));
        }
    }

    fn record_str(&mut self, field: &Field, value: &str) {
        if field.name() == "message" {
            self.message = value.to_string();
        } else {
            self.fields
                .push((field.name().to_string(), value.to_string()));
        }
    }
}

// ---------------------------------------------------------------------------
// PapertrailLayer
// ---------------------------------------------------------------------------

/// Shared handle to toggle telemetry on/off at runtime.
#[derive(Clone)]
pub struct TelemetryHandle {
    enabled: Arc<AtomicBool>,
}

impl TelemetryHandle {
    pub fn set_enabled(&self, enabled: bool) {
        self.enabled.store(enabled, Ordering::Relaxed);
    }
}

pub struct PapertrailLayer {
    tx: mpsc::UnboundedSender<String>,
    enabled: Arc<AtomicBool>,
}

impl PapertrailLayer {
    pub fn new() -> (Self, TelemetryHandle) {
        let (tx, rx) = mpsc::unbounded_channel();
        let enabled = Arc::new(AtomicBool::new(true));

        // Spawn flusher on a dedicated thread with its own tokio runtime so it
        // doesn't depend on the Tauri async runtime lifecycle.
        std::thread::Builder::new()
            .name("papertrail-flusher".into())
            .spawn(move || {
                let rt = tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                    .expect("papertrail tokio runtime");
                rt.block_on(flusher_loop(rx));
            })
            .expect("papertrail thread");

        let handle = TelemetryHandle {
            enabled: enabled.clone(),
        };
        (Self { tx, enabled }, handle)
    }
}

impl<S> Layer<S> for PapertrailLayer
where
    S: Subscriber + for<'a> LookupSpan<'a>,
{
    fn on_new_span(&self, attrs: &span::Attributes<'_>, id: &span::Id, ctx: Context<'_, S>) {
        if let Some(span) = ctx.span(id) {
            let mut fields = SpanFields::default();
            attrs.record(&mut fields);
            span.extensions_mut().insert(fields);
        }
    }

    fn on_record(&self, id: &span::Id, values: &span::Record<'_>, ctx: Context<'_, S>) {
        if let Some(span) = ctx.span(id) {
            let mut ext = span.extensions_mut();
            if let Some(fields) = ext.get_mut::<SpanFields>() {
                values.record(fields);
            }
        }
    }

    fn on_event(&self, event: &Event<'_>, ctx: Context<'_, S>) {
        if !self.enabled.load(Ordering::Relaxed) {
            return;
        }

        let now = chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Millis, true);
        let metadata = event.metadata();
        let level = metadata.level();
        let target = metadata.target();

        // Extract event message and fields
        let mut visitor = EventVisitor::default();
        event.record(&mut visitor);

        // Collect span fields from parent scope
        let mut span_fields: BTreeMap<String, String> = BTreeMap::new();
        if let Some(scope) = ctx.event_scope(event) {
            for span in scope {
                if let Some(fields) = span.extensions().get::<SpanFields>() {
                    span_fields.extend(fields.0.iter().map(|(k, v)| (k.clone(), v.clone())));
                }
            }
        }

        // Append any event-level fields
        for (k, v) in &visitor.fields {
            span_fields.insert(k.clone(), v.clone());
        }

        let span_str = if span_fields.is_empty() {
            String::new()
        } else {
            let pairs: Vec<String> = span_fields
                .iter()
                .map(|(k, v)| format!("{k}={v}"))
                .collect();
            format!(" [{}]", pairs.join(" "))
        };

        let line = format!("{now} {level} {target}{span_str} {}", visitor.message);
        let _ = self.tx.send(line);
    }
}

// ---------------------------------------------------------------------------
// Background flusher
// ---------------------------------------------------------------------------

async fn flusher_loop(mut rx: mpsc::UnboundedReceiver<String>) {
    let client = reqwest::Client::new();
    let mut buffer: Vec<String> = Vec::new();
    let mut interval = tokio::time::interval(std::time::Duration::from_secs(FLUSH_INTERVAL_SECS));

    loop {
        tokio::select! {
            maybe_line = rx.recv() => {
                match maybe_line {
                    Some(line) => {
                        buffer.push(line);
                        // Cap buffer size — drop oldest if over limit
                        if buffer.len() > MAX_BUFFER_LINES {
                            let excess = buffer.len() - MAX_BUFFER_LINES;
                            buffer.drain(..excess);
                        }
                    }
                    None => {
                        // Channel closed — final flush and exit
                        flush(&client, &mut buffer).await;
                        return;
                    }
                }
            }
            _ = interval.tick() => {
                flush(&client, &mut buffer).await;
            }
        }
    }
}

async fn flush(client: &reqwest::Client, buffer: &mut Vec<String>) {
    if buffer.is_empty() {
        return;
    }

    let body = buffer.join("\n");

    match client
        .post(ENDPOINT)
        .header("Content-Type", "application/octet-stream")
        .header("Authorization", format!("Bearer {TOKEN}"))
        .body(body)
        .send()
        .await
    {
        Ok(resp) if resp.status().is_success() => {
            buffer.clear();
        }
        Ok(resp) => {
            eprintln!(
                "[papertrail] flush failed: HTTP {} — keeping {} lines in buffer",
                resp.status(),
                buffer.len()
            );
        }
        Err(e) => {
            eprintln!(
                "[papertrail] flush error: {e} — keeping {} lines in buffer",
                buffer.len()
            );
        }
    }
}
