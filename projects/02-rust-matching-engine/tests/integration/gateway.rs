use super::*;
use matching_engine::gateway::{self, Request};
use matching_engine::pipeline::{Pipeline, PipelineOptions};
use matching_engine::sequencer::LogicalClock;
use serde_json::Value;
use std::net::SocketAddr;
use std::thread::JoinHandle;
use std::time::Duration;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::mpsc;

struct Server {
    addr: SocketAddr,
    accept: tokio::task::JoinHandle<std::io::Result<()>>,
    engine: JoinHandle<Pipeline>,
}

impl Server {
    async fn start(dir: &Path) -> Server {
        let (pipeline, _) = Pipeline::open(
            dir,
            Box::new(LogicalClock::default()),
            PipelineOptions::default(),
        )
        .unwrap();
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let (tx, rx) = mpsc::channel::<Request>(64);
        let engine = gateway::spawn_engine(pipeline, rx).unwrap();
        let accept = tokio::spawn(gateway::serve(listener, tx));
        Server {
            addr,
            accept,
            engine,
        }
    }

    /// Stops accepting, drops all connections and returns the pipeline.
    async fn stop(self) -> Pipeline {
        self.accept.abort();
        let _ = self.accept.await;
        tokio::task::spawn_blocking(move || self.engine.join().unwrap())
            .await
            .unwrap()
    }
}

struct Client {
    lines: tokio::io::Lines<BufReader<tokio::net::tcp::OwnedReadHalf>>,
    write: tokio::net::tcp::OwnedWriteHalf,
}

impl Client {
    async fn connect(addr: SocketAddr) -> Client {
        let (read, write) = TcpStream::connect(addr).await.unwrap().into_split();
        Client {
            lines: BufReader::new(read).lines(),
            write,
        }
    }

    async fn send_raw(&mut self, line: &str) {
        self.write.write_all(line.as_bytes()).await.unwrap();
        self.write.write_all(b"\n").await.unwrap();
    }

    async fn next(&mut self) -> Value {
        let line = tokio::time::timeout(Duration::from_secs(10), self.lines.next_line())
            .await
            .expect("timed out waiting for the gateway")
            .unwrap()
            .expect("gateway closed the connection");
        serde_json::from_str(&line).unwrap()
    }

    /// Sends one line and collects the reply up to its DONE/ERROR terminator.
    async fn request(&mut self, line: &str) -> Vec<Value> {
        self.send_raw(line).await;
        let mut reply = Vec::new();
        loop {
            let v = self.next().await;
            let last = v["type"] == "DONE" || v["type"] == "ERROR";
            reply.push(v);
            if last {
                return reply;
            }
        }
    }
}

fn types(reply: &[Value]) -> Vec<&str> {
    reply.iter().map(|v| v["type"].as_str().unwrap()).collect()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn readme_session_over_tcp() {
    let dir = TempDir::new("gw-readme");
    let server = Server::start(dir.path()).await;
    let mut c = Client::connect(server.addr).await;

    let r = c
        .request(r#"{"type":"NEW","order_id":1,"symbol":"XYZ","side":"BUY","qty":100,"price_ticks":10025}"#)
        .await;
    assert_eq!(types(&r), ["ACCEPTED", "BOOK_TOP", "DONE"]);
    assert_eq!(r[2]["cmd_seq"], 1);
    assert_eq!(r[2]["events"], 2);

    let r = c
        .request(r#"{"type":"NEW","order_id":2,"symbol":"XYZ","side":"SELL","qty":40,"price_ticks":10025}"#)
        .await;
    assert_eq!(
        types(&r),
        [
            "ACCEPTED",
            "TRADE",
            "EXECUTION",
            "EXECUTION",
            "BOOK_TOP",
            "DONE"
        ]
    );
    assert_eq!(
        (&r[1]["qty"], &r[1]["price_ticks"]),
        (&Value::from(40), &Value::from(10025))
    );
    assert_eq!(
        (&r[1]["maker_order_id"], &r[1]["taker_order_id"]),
        (&Value::from(1), &Value::from(2))
    );
    assert_eq!(r[4]["bid_qty"], 60);

    let r = c.request(r#"{"type":"CANCEL","order_id":1}"#).await;
    assert_eq!(types(&r), ["CANCELED", "BOOK_TOP", "DONE"]);
    assert_eq!(r[0]["canceled_qty"], 60);

    drop(c);
    let pipeline = server.stop().await;
    assert_eq!(pipeline.engine().last_seq(), 3);
    assert_eq!(pipeline.engine().open_orders(), 0);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn bad_input_gets_an_error_and_is_never_sequenced() {
    let dir = TempDir::new("gw-bad");
    let server = Server::start(dir.path()).await;
    let mut c = Client::connect(server.addr).await;

    for bad in [
        "this is not json",
        r#"{"type":"NEW","order_id":1}"#,
        r#"{"type":"NEW","order_id":1,"symbol":"XYZ","side":"BUY","qty":-5,"price_ticks":1}"#,
        r#"{"type":"NEW","order_id":1,"symbol":"XYZ","side":"BUY","qty":1,"price_ticks":1.5}"#,
    ] {
        let r = c.request(bad).await;
        assert_eq!(types(&r), ["ERROR"], "{bad}");
    }
    // Blank lines are ignored; a semantically invalid command is sequenced
    // and rejected by the engine (it is part of the deterministic log).
    c.send_raw("").await;
    let r = c
        .request(
            r#"{"type":"NEW","order_id":1,"symbol":"XYZ","side":"BUY","qty":0,"price_ticks":1}"#,
        )
        .await;
    assert_eq!(types(&r), ["REJECTED", "DONE"]);
    assert_eq!(r[0]["reason"], "INVALID_QTY");
    assert_eq!(
        r[1]["cmd_seq"], 1,
        "the four bad lines consumed no sequence numbers"
    );

    // An over-long line is discarded with an error; the connection survives.
    let huge = format!(
        r#"{{"type":"CANCEL","order_id":1,"pad":"{}"}}"#,
        "x".repeat(3 * 4096 + 17)
    );
    let r = c.request(&huge).await;
    assert_eq!(types(&r), ["ERROR"]);
    let r = c.request(r#"{"type":"CANCEL","order_id":1}"#).await;
    assert_eq!(types(&r), ["REJECTED", "DONE"]);
    assert_eq!(r[1]["cmd_seq"], 2);

    server.stop().await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn resting_orders_survive_client_disconnect_and_gateway_restart() {
    let dir = TempDir::new("gw-restart");
    let server = Server::start(dir.path()).await;

    // Client A rests an order, then vanishes mid-line without saying goodbye.
    let mut a = Client::connect(server.addr).await;
    let r = a
        .request(r#"{"type":"NEW","order_id":1,"symbol":"XYZ","side":"SELL","qty":50,"price_ticks":200}"#)
        .await;
    assert_eq!(types(&r), ["ACCEPTED", "BOOK_TOP", "DONE"]);
    a.write
        .write_all(br#"{"type":"CANCEL","order_"#)
        .await
        .unwrap();
    drop(a);

    // Client B trades against A's order.
    let mut b = Client::connect(server.addr).await;
    let r = b
        .request(
            r#"{"type":"NEW","order_id":2,"symbol":"XYZ","side":"BUY","qty":20,"price_ticks":200}"#,
        )
        .await;
    assert_eq!(
        types(&r),
        [
            "ACCEPTED",
            "TRADE",
            "EXECUTION",
            "EXECUTION",
            "BOOK_TOP",
            "DONE"
        ]
    );
    assert_eq!(r[1]["maker_order_id"], 1);

    // Stop the gateway while B is still connected, then start a new one on
    // the same data directory.
    let before = server.stop().await.engine().snapshot();
    let server = Server::start(dir.path()).await;
    let mut c = Client::connect(server.addr).await;
    let r = c
        .request(r#"{"type":"NEW","order_id":3,"symbol":"XYZ","side":"BUY","ord_type":"MARKET","qty":100}"#)
        .await;
    assert_eq!(
        types(&r),
        [
            "ACCEPTED",
            "TRADE",
            "EXECUTION",
            "EXECUTION",
            "CANCELED",
            "BOOK_TOP",
            "DONE"
        ]
    );
    assert_eq!(
        r[1]["qty"], 30,
        "the remaining 30 of order 1 survived the restart"
    );
    assert_eq!(
        r[6]["cmd_seq"], 3,
        "sequence continues where the journal left off"
    );
    assert_eq!(before.last_seq, 2);
    server.stop().await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn concurrent_clients_produce_one_gap_free_total_order() {
    let dir = TempDir::new("gw-concurrent");
    let server = Server::start(dir.path()).await;
    const CLIENTS: u64 = 8;
    const PER_CLIENT: u64 = 50;

    let mut tasks = Vec::new();
    for c in 0..CLIENTS {
        let addr = server.addr;
        tasks.push(tokio::spawn(async move {
            let mut client = Client::connect(addr).await;
            let mut seqs = Vec::new();
            for i in 0..PER_CLIENT {
                let id = c * 1000 + i + 1;
                let side = if (c + i) % 2 == 0 { "BUY" } else { "SELL" };
                let line = format!(
                    r#"{{"type":"NEW","order_id":{id},"symbol":"XYZ","side":"{side}","qty":3,"price_ticks":{}}}"#,
                    100 + (i % 3)
                );
                let reply = client.request(&line).await;
                seqs.push(reply.last().unwrap()["cmd_seq"].as_u64().unwrap());
            }
            seqs
        }));
    }
    let mut all = Vec::new();
    for t in tasks {
        let seqs = t.await.unwrap();
        assert!(
            seqs.windows(2).all(|w| w[0] < w[1]),
            "per-client order preserved"
        );
        all.extend(seqs);
    }
    all.sort_unstable();
    assert_eq!(all, (1..=CLIENTS * PER_CLIENT).collect::<Vec<_>>());

    let pipeline = server.stop().await;
    pipeline.engine().check_invariants().unwrap();
    let live = pipeline.engine().snapshot();
    drop(pipeline);

    // Whatever interleaving happened, the journal reproduces it exactly.
    let (replayed, _) = matching_engine::journal::recover(dir.path(), |_| {}).unwrap();
    assert_eq!(replayed.snapshot(), live);
}
