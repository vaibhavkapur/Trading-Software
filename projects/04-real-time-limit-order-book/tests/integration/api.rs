//! The running service: HTTP API, WebSocket stream, fault injection and
//! replay, exercised over real sockets against the simulated venue.

use book_core::{checksum_levels, Action, BTreeLadder, Ladder, Side};
use feed_adapter::{checksum_hex, parse_fault_plan, SimConfig, SimSymbol};
use futures_util::{SinkExt, StreamExt};
use serde_json::{json, Value};
use std::time::Duration;
use stream_api::engine::EngineConfig;
use stream_api::offline::record;
use stream_api::server::{start, Running, ServeConfig, SourceKind};
use tokio::time::timeout;
use tokio_tungstenite::tungstenite::Message;

const WAIT: Duration = Duration::from_secs(15);

fn sim() -> SimConfig {
    SimConfig {
        symbols: vec![SimSymbol::new("XYZ", 10_000), SimSymbol::new("ABC", 12_500)],
        seed: 9,
        ..SimConfig::default()
    }
}

async fn serve(source: SourceKind, engine: EngineConfig, admin: bool) -> Running {
    start(ServeConfig {
        addr: "127.0.0.1:0".parse().unwrap(),
        source,
        engine,
        ui_dir: None,
        admin,
        clickhouse: None,
        store_deltas: false,
    })
    .await
    .expect("server starts")
}

async fn serve_sim(engine: EngineConfig) -> Running {
    serve(
        SourceKind::Sim {
            cfg: sim(),
            rate: 2_000,
        },
        engine,
        true,
    )
    .await
}

async fn get(server: &Running, path: &str) -> (u16, String) {
    let resp = reqwest::get(format!("http://{}{path}", server.addr))
        .await
        .unwrap();
    (resp.status().as_u16(), resp.text().await.unwrap())
}

async fn get_json(server: &Running, path: &str) -> Value {
    let (code, body) = get(server, path).await;
    assert_eq!(code, 200, "{path}: {body}");
    serde_json::from_str(&body).unwrap()
}

async fn post(server: &Running, path: &str, body: Value) -> (u16, Value) {
    let resp = reqwest::Client::new()
        .post(format!("http://{}{path}", server.addr))
        .json(&body)
        .send()
        .await
        .unwrap();
    let code = resp.status().as_u16();
    (code, resp.json().await.unwrap_or(Value::Null))
}

/// Polls until `check` accepts the JSON at `path`.
async fn eventually(server: &Running, path: &str, check: impl Fn(&Value) -> bool) -> Value {
    let poll = async {
        loop {
            let v = get_json(server, path).await;
            if check(&v) {
                return v;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    };
    timeout(WAIT, poll)
        .await
        .unwrap_or_else(|_| panic!("condition on {path} not met in time"))
}

type Ws =
    tokio_tungstenite::WebSocketStream<tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>>;

async fn connect(server: &Running, path: &str) -> Ws {
    let (ws, _) = tokio_tungstenite::connect_async(format!("ws://{}{path}", server.addr))
        .await
        .expect("websocket connects");
    ws
}

async fn next_msg(ws: &mut Ws) -> Value {
    loop {
        let msg = timeout(WAIT, ws.next())
            .await
            .expect("message in time")
            .expect("stream open")
            .unwrap();
        if let Message::Text(text) = msg {
            return serde_json::from_str(text.as_str()).unwrap();
        }
    }
}

/// Reads until a message satisfies `pred`, returning everything read.
async fn read_until(ws: &mut Ws, pred: impl Fn(&Value) -> bool) -> Vec<Value> {
    let mut seen = Vec::new();
    loop {
        let m = next_msg(ws).await;
        let done = pred(&m);
        seen.push(m);
        if done {
            return seen;
        }
    }
}

fn is(kind: &'static str) -> impl Fn(&Value) -> bool {
    move |m| m["type"] == kind
}

fn status(kind: &'static str) -> impl Fn(&Value) -> bool {
    move |m| m["type"] == "status" && m["kind"] == kind
}

#[tokio::test]
async fn rest_snapshot_health_and_errors() {
    let server = serve_sim(EngineConfig::default()).await;
    let snap = eventually(&server, "/v1/books/XYZ/snapshot?depth=5", |v| {
        v["state"] == "synced" && v["seq"].as_u64() > Some(50)
    })
    .await;
    let (bids, asks) = (
        snap["bids"].as_array().unwrap(),
        snap["asks"].as_array().unwrap(),
    );
    assert_eq!((bids.len(), asks.len()), (5, 5));
    let px = |l: &Vec<Value>| {
        l.iter()
            .map(|x| x["price_ticks"].as_i64().unwrap())
            .collect::<Vec<_>>()
    };
    assert!(px(bids).windows(2).all(|w| w[0] > w[1]), "bids best-first");
    assert!(px(asks).windows(2).all(|w| w[0] < w[1]), "asks best-first");
    assert!(px(bids)[0] < px(asks)[0]);
    assert_eq!(snap["integrity"], "ok");
    assert_eq!(snap["stale"], false);
    assert!(snap["event_ts"].as_i64().unwrap() > 0 && snap["recv_ts"].as_i64().unwrap() > 0);
    assert!(snap["asof"].as_i64().unwrap() >= snap["recv_ts"].as_i64().unwrap());

    // Depth defaults to 20 and is clamped to the configured maximum.
    let default_depth = get_json(&server, "/v1/books/XYZ/snapshot").await["bids"]
        .as_array()
        .unwrap()
        .len();
    assert!((8..=20).contains(&default_depth), "{default_depth}");
    let deep = get_json(&server, "/v1/books/XYZ/snapshot?depth=9999").await["bids"]
        .as_array()
        .unwrap()
        .len();
    assert!((8..=50).contains(&deep), "{deep}");
    assert_eq!(
        get_json(&server, "/v1/books/XYZ/snapshot?depth=0").await["bids"]
            .as_array()
            .unwrap()
            .len(),
        1
    );

    let health = get_json(&server, "/v1/books/ABC/health").await;
    assert_eq!(health["symbol"], "ABC");
    assert_eq!(health["state"], "synced");
    assert_eq!(health["stale"], false);
    assert_eq!(health["gap_count"], 0);
    assert!(health["last_seq"].as_u64().unwrap() > 0);

    let quote = get_json(&server, "/v1/books/XYZ/quote").await;
    assert!(
        quote["quote"]["bid_px"].as_i64().unwrap() < quote["quote"]["ask_px"].as_i64().unwrap()
    );

    let symbols = get_json(&server, "/v1/symbols").await;
    assert_eq!(
        symbols["symbols"][0],
        json!({"symbol": "ABC", "tick_size": "0.01", "lot_size": "1"})
    );
    assert_eq!(get_json(&server, "/v1/source").await["simulated"], true);

    let refused =
        tokio_tungstenite::connect_async(format!("ws://{}/v1/books/NOPE/stream", server.addr))
            .await;
    assert!(
        matches!(refused, Err(tokio_tungstenite::tungstenite::Error::Http(r)) if r.status() == 404)
    );
    for path in [
        "/v1/books/NOPE/snapshot",
        "/v1/books/NOPE/health",
        "/v1/books/NOPE/quote",
    ] {
        let (code, body) = get(&server, path).await;
        assert_eq!(code, 404, "{path}");
        assert!(body.contains("unknown symbol"), "{body}");
    }
    assert_eq!(
        get(&server, "/v1/books/XYZ/snapshot?depth=abc").await.0,
        400
    );
    assert_eq!(get(&server, "/healthz").await, (200, "ok".to_string()));
}

#[tokio::test]
async fn prometheus_metrics_are_exposed() {
    let server = serve_sim(EngineConfig::default()).await;
    eventually(&server, "/v1/books/XYZ/health", |v| {
        v["last_seq"].as_u64() > Some(100)
    })
    .await;
    let (code, body) = get(&server, "/metrics").await;
    assert_eq!(code, 200);
    for needle in [
        "lob_frames_total ",
        "lob_deltas_applied_total{symbol=\"XYZ\"}",
        "lob_sequence_gaps_total{symbol=\"ABC\"} 0",
        "lob_synced{symbol=\"XYZ\"} 1",
        "lob_feed_stale{symbol=\"XYZ\"} 0",
        "lob_feed_to_book_seconds_bucket{le=\"+Inf\"}",
        "lob_gap_recovery_seconds_count 0",
        "lob_replay_checkpoints_total{result=\"mismatched\"} 0",
        "# TYPE lob_feed_to_book_seconds histogram",
    ] {
        assert!(body.contains(needle), "missing `{needle}` in:\n{body}");
    }
    let frames: u64 = body
        .lines()
        .find_map(|l| l.strip_prefix("lob_frames_total "))
        .unwrap()
        .parse()
        .unwrap();
    assert!(frames > 100);
}

#[tokio::test]
async fn websocket_stream_delivers_base_image_then_updates() {
    let server = serve_sim(EngineConfig::default()).await;
    eventually(&server, "/v1/books/XYZ/health", |v| v["state"] == "synced").await;
    let mut ws = connect(&server, "/v1/books/XYZ/stream?depth=7").await;

    let hello = next_msg(&mut ws).await;
    assert_eq!(hello["type"], "hello");
    assert_eq!(hello["instrument"]["tick_size"], "0.01");
    assert_eq!(hello["source"]["simulated"], true);
    assert_eq!(hello["stale_after_ms"], 2_000);
    let base = next_msg(&mut ws).await;
    assert_eq!(
        (&base["type"], base["bids"].as_array().unwrap().len()),
        (&json!("snapshot"), 7)
    );
    assert_eq!(next_msg(&mut ws).await["type"], "health");

    let seen = read_until(&mut ws, |m| {
        m["type"] == "health" && m["last_seq"].as_u64() > Some(200)
    })
    .await;
    let books: Vec<&Value> = seen.iter().filter(|m| m["type"] == "book").collect();
    assert!(books.len() >= 2);
    for b in &books {
        assert_eq!(
            b["symbol"], "XYZ",
            "only the subscribed symbol is delivered"
        );
        assert_eq!(b["bids"].as_array().unwrap().len(), 7);
        assert_eq!(b["state"], "synced");
        assert_eq!(b["stale"], false);
    }
    assert!(books
        .windows(2)
        .all(|w| w[0]["seq"].as_u64() <= w[1]["seq"].as_u64()));
    let metric = seen
        .iter()
        .find(|m| m["type"] == "metric")
        .expect("metric published");
    assert!(metric["spread_ticks"].as_i64().unwrap() >= 1);
    assert!(metric["mid_px"].is_number() && metric["microprice"].is_number());
    assert!((-1.0..=1.0).contains(&metric["imbalance"].as_f64().unwrap()));
    assert!(
        !seen.iter().any(|m| m["type"] == "delta"),
        "deltas are opt-in"
    );
}

/// A subscriber that rebuilds the book from `snapshot` + `delta` messages
/// must agree with the server's own periodic image, including across a gap.
#[tokio::test]
async fn client_side_reconstruction_from_deltas_matches_server_book() {
    let server = serve_sim(EngineConfig::default()).await;
    let mut ws = connect(&server, "/v1/books/XYZ/stream?depth=10&deltas=true").await;
    let mut book = BTreeLadder::default();
    let mut seq = 0u64;
    let (mut compared, mut injected, mut resynced) = (0, false, false);

    let run = async {
        while compared < 40 || !resynced {
            let m = next_msg(&mut ws).await;
            match m["type"].as_str().unwrap() {
                "snapshot" => {
                    book.clear();
                    for (side, key) in [(Side::Bid, "bids"), (Side::Ask, "asks")] {
                        for l in m[key].as_array().unwrap() {
                            book.set(
                                side,
                                l["price_ticks"].as_i64().unwrap(),
                                l["qty"].as_u64().unwrap(),
                            );
                        }
                    }
                    seq = m["seq"].as_u64().unwrap();
                }
                "delta" => {
                    let d: book_core::BookDelta = serde_json::from_value(m.clone()).unwrap();
                    if d.seq <= seq {
                        continue; // already contained in the base image
                    }
                    assert_eq!(
                        d.seq,
                        seq + 1,
                        "deltas reach subscribers gap-free between snapshots"
                    );
                    seq = d.seq;
                    if d.action == Action::Delete {
                        book.remove(d.side, d.price_ticks);
                    } else {
                        book.set(d.side, d.price_ticks, d.qty);
                    }
                }
                "book" if m["state"] == "synced" && m["seq"].as_u64() == Some(seq) => {
                    // Periodic images honour depth=10; the local book is full depth.
                    let sum = checksum_hex(checksum_levels(
                        &book.top(Side::Bid, 10),
                        &book.top(Side::Ask, 10),
                    ));
                    assert_eq!(m["checksum"], sum, "at seq {seq}");
                    assert!(book.levels(Side::Bid) > 10, "base image was full depth");
                    compared += 1;
                    if compared == 10 && !injected {
                        injected = true;
                        let (code, _) = post(
                            &server,
                            "/v1/admin/faults",
                            json!({"fault": "drop", "count": 5}),
                        )
                        .await;
                        assert_eq!(code, 200);
                    }
                }
                "status" if m["kind"] == "resynced" => resynced = true,
                _ => {}
            }
        }
    };
    timeout(Duration::from_secs(30), run)
        .await
        .expect("reconstruction completes");
}

#[tokio::test]
async fn injected_gap_is_reported_and_recovered_over_the_stream() {
    let server = serve_sim(EngineConfig::default()).await;
    let mut ws = connect(&server, "/v1/books/XYZ/stream").await;
    read_until(&mut ws, is("book")).await;

    let (code, body) = post(
        &server,
        "/v1/admin/faults",
        json!({"fault": "drop", "count": 30}),
    )
    .await;
    assert_eq!((code, &body["injected"]["fault"]), (200, &json!("drop")));

    let seen = read_until(&mut ws, status("resynced")).await;
    let gap = seen
        .iter()
        .position(status("gap_detected"))
        .expect("gap reported");
    let requested = seen
        .iter()
        .position(status("resync_requested"))
        .expect("resync requested");
    assert!(gap < requested);
    // Anything published between detection and recovery is marked unsynced.
    for m in &seen[gap..seen.len() - 1] {
        if m["type"] == "book" {
            assert_eq!(m["state"], "resyncing");
        }
        assert_ne!(m["type"], "metric", "no metrics from an untrusted book");
    }
    let resynced = seen.last().unwrap();
    assert!(resynced["value_ms"].as_i64().unwrap() >= 0);
    // A fresh base image follows recovery.
    let after = read_until(&mut ws, is("snapshot")).await;
    assert_eq!(after.last().unwrap()["state"], "synced");

    let health = eventually(&server, "/v1/books/XYZ/health", |v| v["state"] == "synced").await;
    assert_eq!(health["gap_count"], 1);
    assert_eq!(health["recovery_count"], 1);
    let (_, metrics) = get(&server, "/metrics").await;
    // The 30 lost deltas may have hit the other symbol's book too.
    assert!(
        !metrics.contains("lob_gap_recovery_seconds_count 0"),
        "{metrics}"
    );
}

#[tokio::test]
async fn stalled_feed_raises_stale_alert_and_clears_on_recovery() {
    let engine = EngineConfig {
        stale_after_ms: 300,
        ..EngineConfig::default()
    };
    let server = serve_sim(engine).await;
    let mut ws = connect(&server, "/v1/books/XYZ/stream").await;
    read_until(&mut ws, is("book")).await;

    assert_eq!(
        post(
            &server,
            "/v1/admin/faults",
            json!({"fault": "stall", "ms": 1500})
        )
        .await
        .0,
        200
    );
    let seen = read_until(&mut ws, status("stale")).await;
    assert!(seen.last().unwrap()["value_ms"].as_i64().unwrap() >= 300);
    // Subsequent images keep arriving, explicitly marked stale.
    let seen = read_until(&mut ws, |m| m["type"] == "book" && m["stale"] == true).await;
    let stale_book = seen.last().unwrap();
    assert!(
        stale_book["asof"].as_i64().unwrap() - stale_book["recv_ts"].as_i64().unwrap() > 300_000
    );
    assert_eq!(
        get_json(&server, "/v1/books/XYZ/health").await["stale"],
        true
    );
    assert!(get(&server, "/metrics")
        .await
        .1
        .contains("lob_feed_stale{symbol=\"XYZ\"} 1"));

    read_until(&mut ws, status("fresh")).await;
    read_until(&mut ws, |m| m["type"] == "book" && m["stale"] == false).await;
    assert_eq!(
        get_json(&server, "/v1/books/XYZ/health").await["stale"],
        false
    );
}

#[tokio::test]
async fn admin_routes_validate_and_can_be_disabled() {
    let server = serve_sim(EngineConfig::default()).await;
    assert_eq!(
        post(
            &server,
            "/v1/admin/faults",
            json!({"fault": "stall", "ms": 600_000})
        )
        .await
        .0,
        400
    );
    assert_eq!(
        post(&server, "/v1/admin/faults", json!({"fault": "meteor"}))
            .await
            .0,
        422
    );
    assert_eq!(
        post(&server, "/v1/admin/replay", json!({"speed": 2.0}))
            .await
            .0,
        409,
        "not a replay source"
    );

    let locked = serve(
        SourceKind::Sim {
            cfg: sim(),
            rate: 500,
        },
        EngineConfig::default(),
        false,
    )
    .await;
    let (code, body) = post(&locked, "/v1/admin/faults", json!({"fault": "duplicate"})).await;
    assert_eq!(code, 403);
    assert!(body["error"].as_str().unwrap().contains("disabled"));
}

fn capture() -> (Vec<feed_adapter::Frame>, feed_adapter::Manifest) {
    let faults = "drop@400:3,dup@900,reorder@1400";
    let cfg = SimConfig {
        checkpoint_every: 50,
        ..sim()
    };
    record(
        cfg,
        parse_fault_plan(faults).unwrap(),
        faults,
        3_000,
        EngineConfig::default(),
    )
}

#[tokio::test]
async fn served_replay_reaches_recorded_end_state_then_goes_stale() {
    let (frames, manifest) = capture();
    let total = manifest.checkpoints.len();
    let last = |sym: &str| {
        manifest
            .checkpoints
            .iter()
            .filter(|c| c.symbol == sym)
            .map(|c| c.seq)
            .max()
            .unwrap()
    };
    let last_xyz = last("XYZ");
    let offline = stream_api::offline::replay(&frames, &manifest, EngineConfig::default());
    let offline_gaps: u64 = offline.health.iter().map(|h| h.gap_count).sum();
    assert!(offline_gaps >= 2);
    let source = SourceKind::Replay {
        frames,
        manifest,
        speed: 0.0,
        looped: false,
    };
    let engine = EngineConfig {
        stale_after_ms: 300,
        ..EngineConfig::default()
    };
    let server = serve(source, engine, true).await;

    let health = eventually(&server, "/v1/books/XYZ/health", |v| {
        v["last_seq"].as_u64() >= Some(last_xyz) && v["stale"] == true
    })
    .await;
    assert_eq!(health["state"], "synced");
    assert_eq!(
        health["gap_count"].as_u64().unwrap()
            + get_json(&server, "/v1/books/ABC/health").await["gap_count"]
                .as_u64()
                .unwrap(),
        offline_gaps,
        "served replay sees what the offline replay sees"
    );
    // Venue timestamps are from the capture; receive timestamps are now.
    assert!(health["venue_lag_ms"].as_i64().unwrap() > 0);

    let (_, metrics) = get(&server, "/metrics").await;
    let value = |name: &str| -> usize {
        metrics
            .lines()
            .find_map(|l| l.strip_prefix(name))
            .unwrap()
            .trim()
            .parse()
            .unwrap()
    };
    assert_eq!(
        value("lob_replay_checkpoints_total{result=\"mismatched\"}"),
        0
    );
    let matched = value("lob_replay_checkpoints_total{result=\"matched\"}");
    assert!(matched > 0 && matched + 4 >= total, "{matched} of {total}");
    let src = get_json(&server, "/v1/source").await;
    assert_eq!(
        (&src["kind"], &src["simulated"]),
        (&json!("replay"), &json!(true))
    );
}

#[tokio::test]
async fn replay_speed_and_pause_are_controllable_and_loop_restarts_cleanly() {
    let (frames, manifest) = capture();
    let source = SourceKind::Replay {
        frames,
        manifest,
        speed: 200.0,
        looped: true,
    };
    let server = serve(source, EngineConfig::default(), true).await;
    let mut ws = connect(&server, "/v1/books/XYZ/stream").await;

    // Looping: the session restarts, the book is rebuilt from the new snapshot.
    let seen = read_until(&mut ws, status("source_restarted")).await;
    assert!(seen
        .iter()
        .any(|m| m["type"] == "book" && m["state"] == "synced"));
    let after = read_until(&mut ws, |m| {
        m["type"] == "book" && m["state"] == "synced" && m["seq"].as_u64() > Some(20)
    })
    .await;
    assert!(after.iter().any(is("snapshot")));

    let (code, body) = post(
        &server,
        "/v1/admin/replay",
        json!({"paused": true, "speed": 1.0}),
    )
    .await;
    assert_eq!(code, 200);
    assert_eq!(
        (&body["replay_paused"], &body["replay_speed"]),
        (&json!(true), &json!(1.0))
    );
    tokio::time::sleep(Duration::from_millis(300)).await;
    let a = get_json(&server, "/v1/books/XYZ/health").await["last_seq"]
        .as_u64()
        .unwrap();
    tokio::time::sleep(Duration::from_millis(300)).await;
    let b = get_json(&server, "/v1/books/XYZ/health").await["last_seq"]
        .as_u64()
        .unwrap();
    assert_eq!(a, b, "paused replay does not advance");

    assert_eq!(
        post(&server, "/v1/admin/replay", json!({"speed": -1}))
            .await
            .0,
        400
    );
    assert_eq!(
        post(
            &server,
            "/v1/admin/replay",
            json!({"paused": false, "speed": 500.0})
        )
        .await
        .0,
        200
    );
    eventually(&server, "/v1/books/XYZ/health", |v| {
        v["last_seq"].as_u64() != Some(b)
    })
    .await;
    assert_eq!(
        post(&server, "/v1/admin/faults", json!({"fault": "duplicate"}))
            .await
            .0,
        409
    );
    ws.close(None).await.ok();
    ws.send(Message::Close(None)).await.ok();
}
