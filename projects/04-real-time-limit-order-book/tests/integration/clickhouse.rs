//! ClickHouse sink against a stand-in HTTP server: accepted writes, rejected
//! writes with retry, and back-pressure. (No real ClickHouse is involved.)

use axum::extract::{Query, State};
use axum::http::StatusCode;
use axum::routing::post;
use axum::Router;
use serde_json::json;
use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering::SeqCst};
use std::sync::{Arc, Mutex};
use std::time::Duration;
use stream_api::clickhouse::{ClickHouseConfig, ClickHouseSink, Table};
use stream_api::metrics::Metrics;

#[derive(Default)]
struct Fake {
    reject: AtomicBool,
    requests: AtomicU64,
    inserts: Mutex<Vec<(String, String)>>,
}

async fn handler(
    State(fake): State<Arc<Fake>>,
    Query(q): Query<HashMap<String, String>>,
    body: String,
) -> (StatusCode, &'static str) {
    fake.requests.fetch_add(1, SeqCst);
    if fake.reject.load(SeqCst) {
        return (
            StatusCode::INTERNAL_SERVER_ERROR,
            "Code: 60. DB::Exception: Table lob.book_metrics does not exist",
        );
    }
    fake.inserts
        .lock()
        .unwrap()
        .push((q["query"].clone(), body));
    (StatusCode::OK, "")
}

async fn fake_clickhouse() -> (Arc<Fake>, String) {
    let fake = Arc::new(Fake::default());
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let app = Router::new()
        .route("/", post(handler))
        .with_state(fake.clone());
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    (fake, url)
}

fn config(url: String) -> ClickHouseConfig {
    ClickHouseConfig {
        flush_interval: Duration::from_millis(40),
        retry_backoff: Duration::from_millis(10),
        max_retries: 2,
        ..ClickHouseConfig::new(url)
    }
}

async fn until(what: &str, cond: impl Fn() -> bool) {
    for _ in 0..500 {
        if cond() {
            return;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    panic!("timed out waiting for {what}");
}

#[tokio::test]
async fn rows_are_batched_per_table_as_json_each_row() {
    let (fake, url) = fake_clickhouse().await;
    let metrics = Arc::new(Metrics::default());
    let sink = ClickHouseSink::spawn(config(url), metrics.clone());
    for seq in 0..5 {
        sink.offer(
            Table::BookMetrics,
            &json!({"symbol": "XYZ", "seq": seq, "microprice": null}),
        );
    }
    sink.offer(
        Table::FeedEvents,
        &json!({"symbol": "XYZ", "kind": "gap_detected"}),
    );

    until("rows written", || {
        metrics.sink_rows_written.load(SeqCst) == 6
    })
    .await;
    let inserts = fake.inserts.lock().unwrap().clone();
    let rows_for = |table: &str| -> Vec<serde_json::Value> {
        inserts
            .iter()
            .filter(|(q, _)| q == &format!("INSERT INTO lob.{table} FORMAT JSONEachRow"))
            .flat_map(|(_, body)| {
                body.lines()
                    .map(|l| serde_json::from_str(l).unwrap())
                    .collect::<Vec<_>>()
            })
            .collect()
    };
    let metric_rows = rows_for("book_metrics");
    assert_eq!(metric_rows.len(), 5);
    assert_eq!(
        metric_rows[4],
        json!({"symbol": "XYZ", "seq": 4, "microprice": null})
    );
    assert_eq!(rows_for("feed_events").len(), 1);
    assert_eq!(metrics.sink_rows_dropped.load(SeqCst), 0);
}

#[tokio::test]
async fn rejected_writes_are_retried_then_dropped_and_counted() {
    let (fake, url) = fake_clickhouse().await;
    fake.reject.store(true, SeqCst);
    let metrics = Arc::new(Metrics::default());
    let sink = ClickHouseSink::spawn(config(url), metrics.clone());
    sink.offer(Table::BookMetrics, &json!({"seq": 1}));
    sink.offer(Table::BookMetrics, &json!({"seq": 2}));

    until("batch dropped", || {
        metrics.sink_rows_dropped.load(SeqCst) == 2
    })
    .await;
    assert_eq!(
        fake.requests.load(SeqCst),
        3,
        "one attempt plus two retries"
    );
    assert_eq!(metrics.sink_write_errors.load(SeqCst), 3);
    assert_eq!(metrics.sink_rows_written.load(SeqCst), 0);

    // The sink keeps working once the store accepts writes again.
    fake.reject.store(false, SeqCst);
    sink.offer(Table::BookMetrics, &json!({"seq": 3}));
    until("recovery", || metrics.sink_rows_written.load(SeqCst) == 1).await;
}

#[tokio::test]
async fn unreachable_store_never_blocks_the_producer() {
    // Nothing listens on this port; the queue is tiny.
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    drop(listener);
    let metrics = Arc::new(Metrics::default());
    let sink = ClickHouseSink::spawn(
        ClickHouseConfig {
            queue: 8,
            ..config(url)
        },
        metrics.clone(),
    );

    let start = std::time::Instant::now();
    for seq in 0..10_000 {
        sink.offer(Table::BookDeltas, &json!({"seq": seq}));
    }
    assert!(
        start.elapsed() < Duration::from_secs(2),
        "offer must not wait on the store"
    );
    until("drops counted", || {
        metrics.sink_rows_dropped.load(SeqCst) >= 9_000
    })
    .await;
    assert_eq!(metrics.sink_rows_written.load(SeqCst), 0);
}
