//! Optional ClickHouse sink (HTTP interface, `JSONEachRow`).
//!
//! Storage is strictly downstream of the book: rows are offered through a
//! bounded queue and never block the pipeline. When ClickHouse is slow, down
//! or rejecting writes, batches are retried a few times and then dropped, and
//! the drop is counted (`lob_sink_rows_dropped_total`).

use crate::metrics::Metrics;
use serde::Serialize;
use std::sync::atomic::Ordering::Relaxed;
use std::sync::Arc;
use std::time::Duration;
use tokio::sync::mpsc;

#[derive(Debug, Clone)]
pub struct ClickHouseConfig {
    /// Base URL of the HTTP interface, e.g. `http://localhost:8123`.
    pub url: String,
    pub database: String,
    pub user: Option<String>,
    pub password: Option<String>,
    pub flush_interval: Duration,
    pub max_batch: usize,
    pub queue: usize,
    pub max_retries: u32,
    pub retry_backoff: Duration,
}

impl ClickHouseConfig {
    pub fn new(url: impl Into<String>) -> Self {
        Self {
            url: url.into(),
            database: "lob".to_string(),
            user: None,
            password: None,
            flush_interval: Duration::from_secs(1),
            max_batch: 5_000,
            queue: 50_000,
            max_retries: 3,
            retry_backoff: Duration::from_millis(200),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Table {
    BookDeltas,
    BookMetrics,
    FeedEvents,
}

impl Table {
    const ALL: [Table; 3] = [Table::BookDeltas, Table::BookMetrics, Table::FeedEvents];

    pub fn name(self) -> &'static str {
        match self {
            Table::BookDeltas => "book_deltas",
            Table::BookMetrics => "book_metrics",
            Table::FeedEvents => "feed_events",
        }
    }
}

#[derive(Clone)]
pub struct ClickHouseSink {
    tx: mpsc::Sender<(Table, String)>,
    metrics: Arc<Metrics>,
}

impl ClickHouseSink {
    pub fn spawn(cfg: ClickHouseConfig, metrics: Arc<Metrics>) -> Self {
        let (tx, rx) = mpsc::channel(cfg.queue.max(1));
        tokio::spawn(run(cfg, rx, metrics.clone()));
        Self { tx, metrics }
    }

    /// Queues one row; drops it (and counts the drop) when the queue is full.
    pub fn offer(&self, table: Table, row: &impl Serialize) {
        let Ok(json) = serde_json::to_string(row) else {
            return;
        };
        if self.tx.try_send((table, json)).is_err() {
            self.metrics.sink_rows_dropped.fetch_add(1, Relaxed);
        }
    }
}

async fn run(
    cfg: ClickHouseConfig,
    mut rx: mpsc::Receiver<(Table, String)>,
    metrics: Arc<Metrics>,
) {
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(10))
        .build()
        .expect("http client");
    let mut batches: Vec<Vec<String>> = vec![Vec::new(); Table::ALL.len()];
    let mut flush = tokio::time::interval(cfg.flush_interval);
    flush.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    loop {
        let mut closed = false;
        tokio::select! {
            row = rx.recv() => match row {
                Some((table, json)) => {
                    let i = Table::ALL.iter().position(|t| *t == table).expect("known table");
                    batches[i].push(json);
                    if batches[i].len() < cfg.max_batch {
                        continue;
                    }
                }
                None => closed = true,
            },
            _ = flush.tick() => {}
        }
        for (table, batch) in Table::ALL.iter().zip(&mut batches) {
            if !batch.is_empty() {
                let rows = std::mem::take(batch);
                insert(&client, &cfg, *table, rows, &metrics).await;
            }
        }
        if closed {
            return;
        }
    }
}

async fn insert(
    client: &reqwest::Client,
    cfg: &ClickHouseConfig,
    table: Table,
    rows: Vec<String>,
    metrics: &Metrics,
) {
    let n = rows.len() as u64;
    let body = rows.join("\n");
    let query = format!(
        "INSERT INTO {}.{} FORMAT JSONEachRow",
        cfg.database,
        table.name()
    );
    for attempt in 0..=cfg.max_retries {
        let mut req = client
            .post(&cfg.url)
            .query(&[
                ("query", query.as_str()),
                ("input_format_skip_unknown_fields", "1"),
            ])
            .body(body.clone());
        if let Some(user) = &cfg.user {
            req = req.header("X-ClickHouse-User", user);
        }
        if let Some(password) = &cfg.password {
            req = req.header("X-ClickHouse-Key", password);
        }
        match req.send().await {
            Ok(resp) if resp.status().is_success() => {
                metrics.sink_rows_written.fetch_add(n, Relaxed);
                return;
            }
            Ok(resp) => {
                let code = resp.status();
                let text = resp.text().await.unwrap_or_default();
                tracing::warn!(table = table.name(), %code, attempt, "clickhouse rejected insert: {}", text.trim());
            }
            Err(e) => {
                tracing::warn!(
                    table = table.name(),
                    attempt,
                    "clickhouse insert failed: {e}"
                )
            }
        }
        metrics.sink_write_errors.fetch_add(1, Relaxed);
        if attempt < cfg.max_retries {
            tokio::time::sleep(cfg.retry_backoff * 2u32.pow(attempt)).await;
        }
    }
    metrics.sink_rows_dropped.fetch_add(n, Relaxed);
    tracing::error!(
        table = table.name(),
        rows = n,
        "dropping batch after retries"
    );
}
