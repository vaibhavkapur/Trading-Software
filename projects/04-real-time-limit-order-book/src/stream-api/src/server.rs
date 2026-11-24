//! HTTP + WebSocket API, the feed sources that drive it, and the pump that
//! connects them to the [`Engine`].
//!
//! ```text
//! source task ──mpsc──► pump ──► Engine (Mutex, single writer)
//!      ▲                 │
//!      └── resync/fault ─┤──broadcast──► WebSocket clients
//!                        └──bounded queue──► ClickHouse sink
//! ```

use crate::clickhouse::{ClickHouseConfig, ClickHouseSink, Table};
use crate::engine::{BookView, Engine, EngineConfig, Output, StatusEvent, StatusKind, ALL_SYMBOLS};
use crate::metrics::Metrics;
use axum::extract::ws::{Message, WebSocket, WebSocketUpgrade};
use axum::extract::{Path, Query, State};
use axum::http::{header, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use book_core::TsMicros;
use feed_adapter::{Fault, Frame, Manifest, SimConfig, SimSession};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering::Relaxed};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use tokio::sync::{broadcast, mpsc};

pub fn now_us() -> TsMicros {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_micros() as TsMicros)
        .unwrap_or(0)
}

pub enum SourceKind {
    /// Live simulated venue producing `rate` events per second.
    Sim { cfg: SimConfig, rate: u32 },
    /// Replay of a capture at `speed`× its recorded pace (0 = unpaced).
    Replay {
        frames: Vec<Frame>,
        manifest: Manifest,
        speed: f64,
        looped: bool,
    },
}

pub struct ServeConfig {
    pub addr: SocketAddr,
    pub source: SourceKind,
    pub engine: EngineConfig,
    /// Directory with the built UI, served at `/` when present.
    pub ui_dir: Option<PathBuf>,
    /// Enables the unauthenticated fault-injection / replay-control routes.
    pub admin: bool,
    pub clickhouse: Option<ClickHouseConfig>,
    /// Also store every applied delta (high volume) in ClickHouse.
    pub store_deltas: bool,
}

pub enum SourceMsg {
    Frame {
        frame: Frame,
        at: Instant,
    },
    /// The upstream session restarted; sequence numbers begin again.
    Reset,
}

pub enum SourceCtl {
    Resync(String),
    Fault(Fault),
}

#[derive(Debug)]
pub struct ReplayCtl {
    speed_milli: AtomicU64,
    paused: AtomicBool,
}

impl ReplayCtl {
    fn new(speed: f64) -> Self {
        let ctl = Self {
            speed_milli: AtomicU64::new(0),
            paused: AtomicBool::new(false),
        };
        ctl.set_speed(speed);
        ctl
    }
    pub fn speed(&self) -> f64 {
        self.speed_milli.load(Relaxed) as f64 / 1_000.0
    }
    pub fn set_speed(&self, speed: f64) {
        let clamped = if speed.is_finite() {
            speed.clamp(0.0, 1_000.0)
        } else {
            1.0
        };
        self.speed_milli.store((clamped * 1_000.0) as u64, Relaxed);
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct SourceInfo {
    pub kind: &'static str,
    /// Every source in this repository is synthetic or a replay of synthetic data.
    pub simulated: bool,
    pub detail: String,
}

enum Payload {
    /// Pre-serialized message.
    Json(String),
    /// Pre-serialized delta; only sent to clients that asked for deltas.
    Delta(String),
    /// Periodic book image, cut to each client's depth.
    Book(BookView),
    /// New base image. Sent whole to delta subscribers, cut for the rest.
    Snapshot(BookView),
}

pub struct Outbound {
    symbol: String,
    payload: Payload,
}

#[derive(Clone)]
pub struct AppState {
    pub engine: Arc<Mutex<Engine>>,
    pub metrics: Arc<Metrics>,
    tx: broadcast::Sender<Arc<Outbound>>,
    ctl: mpsc::UnboundedSender<SourceCtl>,
    source: Arc<SourceInfo>,
    replay: Arc<ReplayCtl>,
    admin: bool,
}

impl AppState {
    fn engine(&self) -> MutexGuard<'_, Engine> {
        self.engine.lock().unwrap_or_else(|e| e.into_inner())
    }
}

pub struct Running {
    pub addr: SocketAddr,
    pub state: AppState,
    tasks: Vec<tokio::task::JoinHandle<()>>,
}

impl Running {
    pub fn shutdown(&self) {
        for t in &self.tasks {
            t.abort();
        }
    }
    pub async fn wait(mut self) {
        if let Some(server) = self.tasks.pop() {
            let _ = server.await;
        }
    }
}

impl Drop for Running {
    fn drop(&mut self) {
        self.shutdown();
    }
}

fn tagged(kind: &str, value: &impl Serialize) -> String {
    let mut v = serde_json::to_value(value).unwrap_or(Value::Null);
    if let Value::Object(map) = &mut v {
        map.insert("type".to_string(), Value::String(kind.to_string()));
    }
    v.to_string()
}

/// Binds the listener and starts the source, pump and HTTP server.
pub async fn start(cfg: ServeConfig) -> anyhow::Result<Running> {
    let (frame_tx, frame_rx) = mpsc::channel::<SourceMsg>(8_192);
    let (ctl_tx, ctl_rx) = mpsc::unbounded_channel::<SourceCtl>();
    let (tx, _) = broadcast::channel::<Arc<Outbound>>(4_096);
    let metrics = Arc::new(Metrics::default());
    let mut tasks = Vec::new();

    let (engine, info, replay) = match cfg.source {
        SourceKind::Sim { cfg: sim, rate } => {
            let session = SimSession::new(sim);
            let engine = Engine::new(cfg.engine, session.instruments());
            let info = SourceInfo {
                kind: "sim",
                simulated: true,
                detail: format!("simulated venue, ~{rate} events/s"),
            };
            tasks.push(tokio::spawn(run_sim(session, rate, frame_tx, ctl_rx)));
            (engine, info, Arc::new(ReplayCtl::new(1.0)))
        }
        SourceKind::Replay {
            frames,
            manifest,
            speed,
            looped,
        } => {
            let mut engine = Engine::new(cfg.engine, manifest.instruments.clone());
            if !looped {
                engine.set_checkpoints(&manifest.checkpoints, manifest.checkpoint_depth);
            }
            let info = SourceInfo {
                kind: "replay",
                simulated: manifest.data_origin == "simulated",
                detail: format!(
                    "replay of {} captured frames (origin: {})",
                    frames.len(),
                    manifest.data_origin
                ),
            };
            let replay = Arc::new(ReplayCtl::new(speed));
            tasks.push(tokio::spawn(run_replay(
                frames,
                looped,
                replay.clone(),
                frame_tx,
                ctl_rx,
            )));
            (engine, info, replay)
        }
    };

    let state = AppState {
        engine: Arc::new(Mutex::new(engine)),
        metrics: metrics.clone(),
        tx,
        ctl: ctl_tx,
        source: Arc::new(info),
        replay,
        admin: cfg.admin,
    };
    let sink = cfg.clickhouse.map(|c| ClickHouseSink::spawn(c, metrics));
    tasks.push(tokio::spawn(pump(
        state.clone(),
        frame_rx,
        sink,
        cfg.store_deltas,
    )));

    let listener = tokio::net::TcpListener::bind(cfg.addr).await?;
    let addr = listener.local_addr()?;
    let app = router(state.clone(), cfg.ui_dir);
    tasks.push(tokio::spawn(async move {
        if let Err(e) = axum::serve(listener, app).await {
            tracing::error!("server stopped: {e}");
        }
    }));
    Ok(Running { addr, state, tasks })
}

// ---------------------------------------------------------------- sources --

async fn run_sim(
    mut session: SimSession,
    rate: u32,
    tx: mpsc::Sender<SourceMsg>,
    mut ctl: mpsc::UnboundedReceiver<SourceCtl>,
) {
    const TICK: Duration = Duration::from_millis(5);
    let per_tick = f64::from(rate.max(1)) * TICK.as_secs_f64();
    let mut tick = tokio::time::interval(TICK);
    tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    let mut carry = 0.0;
    loop {
        tick.tick().await;
        while let Ok(c) = ctl.try_recv() {
            match c {
                SourceCtl::Resync(symbol) => session.request_snapshot(&symbol),
                SourceCtl::Fault(f) => session.inject(f),
            }
        }
        if let Some(ms) = session.take_stall() {
            tokio::time::sleep(Duration::from_millis(ms)).await;
            continue;
        }
        carry += per_tick;
        while carry >= 1.0 {
            carry -= 1.0;
            let frame = session.next_frame(Some(now_us()));
            let msg = SourceMsg::Frame {
                frame,
                at: Instant::now(),
            };
            if tx.send(msg).await.is_err() {
                return;
            }
        }
    }
}

async fn run_replay(
    frames: Vec<Frame>,
    looped: bool,
    ctl: Arc<ReplayCtl>,
    tx: mpsc::Sender<SourceMsg>,
    mut source_ctl: mpsc::UnboundedReceiver<SourceCtl>,
) {
    loop {
        let mut prev: Option<TsMicros> = None;
        for frame in &frames {
            // A capture already contains the snapshots its session asked for,
            // so resync requests need no action here.
            while source_ctl.try_recv().is_ok() {}
            while ctl.paused.load(Relaxed) {
                tokio::time::sleep(Duration::from_millis(50)).await;
            }
            let speed = ctl.speed();
            if let (Some(p), true) = (prev, speed > 0.0) {
                let gap = (frame.recv_ts - p).max(0) as f64 / speed;
                if gap >= 1.0 {
                    tokio::time::sleep(Duration::from_micros(gap as u64)).await;
                }
            }
            prev = Some(frame.recv_ts);
            let msg = SourceMsg::Frame {
                frame: Frame {
                    recv_ts: now_us(),
                    raw: frame.raw.clone(),
                },
                at: Instant::now(),
            };
            if tx.send(msg).await.is_err() {
                return;
            }
        }
        if !looped {
            // Keep the control channel open; the feed simply goes quiet and
            // the staleness alarm takes over.
            std::future::pending::<()>().await;
        }
        if tx.send(SourceMsg::Reset).await.is_err() {
            return;
        }
    }
}

// ------------------------------------------------------------------- pump --

async fn pump(
    state: AppState,
    mut frames: mpsc::Receiver<SourceMsg>,
    sink: Option<ClickHouseSink>,
    store_deltas: bool,
) {
    let mut tick = tokio::time::interval(Duration::from_millis(100));
    tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    let mut ticks = 0u64;
    let mut open = true;
    let mut batch: Vec<SourceMsg> = Vec::with_capacity(256);
    loop {
        tokio::select! {
            n = frames.recv_many(&mut batch, 256), if open => {
                if n == 0 {
                    open = false;
                    continue;
                }
                let mut outputs = Vec::new();
                {
                    let mut engine = state.engine();
                    for msg in batch.drain(..) {
                        match msg {
                            SourceMsg::Frame { frame, at } => {
                                outputs.extend(engine.on_frame(&frame.raw, frame.recv_ts));
                                state.metrics.feed_to_book.observe(at.elapsed().as_secs_f64());
                            }
                            SourceMsg::Reset => outputs.extend(engine.reset(now_us())),
                        }
                    }
                }
                dispatch(&state, sink.as_ref(), store_deltas, outputs);
            }
            _ = tick.tick() => {
                ticks += 1;
                let now = now_us();
                let mut out = Vec::new();
                let outputs = {
                    let mut engine = state.engine();
                    let outputs = engine.tick(now);
                    let depth = engine.config().depth;
                    for symbol in engine.symbols() {
                        if let Some(view) = engine.view(&symbol, depth, now) {
                            out.push(Outbound { symbol: symbol.clone(), payload: Payload::Book(view) });
                        }
                        if let Some(metric) = engine.take_metric(&symbol) {
                            if let Some(sink) = &sink {
                                sink.offer(Table::BookMetrics, &json!({
                                    "recv_ts": now, "symbol": metric.symbol, "event_ts": metric.ts,
                                    "seq": metric.seq, "spread_ticks": metric.spread_ticks,
                                    "mid_ticks_x2": metric.mid_ticks_x2, "microprice": metric.microprice,
                                    "imbalance": metric.imbalance, "depth_imbalance": metric.depth_imbalance,
                                    "bid_depth": metric.bid_depth, "ask_depth": metric.ask_depth, "ofi": metric.ofi,
                                }));
                            }
                            out.push(Outbound { symbol: symbol.clone(), payload: Payload::Json(tagged("metric", &metric)) });
                        }
                        if ticks % 5 == 0 {
                            if let Some(health) = engine.health(&symbol, now) {
                                out.push(Outbound { symbol: symbol.clone(), payload: Payload::Json(tagged("health", &health)) });
                            }
                        }
                    }
                    outputs
                };
                dispatch(&state, sink.as_ref(), store_deltas, outputs);
                for o in out {
                    let _ = state.tx.send(Arc::new(o));
                }
            }
        }
    }
}

fn dispatch(
    state: &AppState,
    sink: Option<&ClickHouseSink>,
    store_deltas: bool,
    outputs: Vec<Output>,
) {
    let listening = state.tx.receiver_count() > 0;
    for output in outputs {
        let (symbol, payload) = match output {
            Output::ResyncRequest { symbol } => {
                let _ = state.ctl.send(SourceCtl::Resync(symbol));
                continue;
            }
            Output::Delta(d) => {
                if let (Some(sink), true) = (sink, store_deltas) {
                    sink.offer(
                        Table::BookDeltas,
                        &json!({
                            "event_ts": d.event_ts, "symbol": d.symbol, "seq": d.seq,
                            "side": d.side, "price_ticks": d.price_ticks,
                            "quantity": d.qty, "action": d.action,
                        }),
                    );
                }
                if !listening {
                    continue;
                }
                (d.symbol.clone(), Payload::Delta(tagged("delta", &d)))
            }
            Output::Snapshot(view) => (view.symbol.clone(), Payload::Snapshot(view)),
            Output::Status(ev) => {
                if ev.kind == StatusKind::Resynced {
                    if let Some(ms) = ev.value_ms {
                        state.metrics.gap_recovery.observe(ms as f64 / 1_000.0);
                    }
                }
                if let Some(sink) = sink {
                    sink.offer(
                        Table::FeedEvents,
                        &json!({
                            "recv_ts": ev.ts, "symbol": ev.symbol, "kind": ev.kind,
                            "seq": ev.seq, "value_ms": ev.value_ms, "detail": ev.detail,
                        }),
                    );
                }
                (ev.symbol.clone(), Payload::Json(tagged("status", &ev)))
            }
        };
        let _ = state.tx.send(Arc::new(Outbound { symbol, payload }));
    }
}

// ----------------------------------------------------------------- routes --

pub fn router(state: AppState, ui_dir: Option<PathBuf>) -> Router {
    let mut app = Router::new()
        .route("/healthz", get(|| async { "ok" }))
        .route("/metrics", get(prometheus))
        .route("/v1/symbols", get(symbols))
        .route("/v1/source", get(source))
        .route("/v1/books/{symbol}/snapshot", get(snapshot))
        .route("/v1/books/{symbol}/quote", get(quote))
        .route("/v1/books/{symbol}/health", get(health))
        .route("/v1/books/{symbol}/stream", get(stream))
        .route("/v1/admin/faults", post(admin_fault))
        .route("/v1/admin/replay", post(admin_replay))
        .with_state(state);
    if let Some(dir) = ui_dir.filter(|d| d.is_dir()) {
        app = app.fallback_service(tower_http::services::ServeDir::new(dir));
    }
    app
}

fn error(code: StatusCode, message: impl Into<String>) -> Response {
    (code, Json(json!({ "error": message.into() }))).into_response()
}

fn unknown(symbol: &str) -> Response {
    error(StatusCode::NOT_FOUND, format!("unknown symbol `{symbol}`"))
}

async fn prometheus(State(s): State<AppState>) -> impl IntoResponse {
    let body = s.metrics.render(&s.engine(), now_us());
    ([(header::CONTENT_TYPE, "text/plain; version=0.0.4")], body)
}

async fn symbols(State(s): State<AppState>) -> Json<Value> {
    let engine = s.engine();
    let list: Vec<Value> = engine
        .symbols()
        .iter()
        .filter_map(|sym| engine.instrument(sym))
        .map(|i| json!(i))
        .collect();
    Json(json!({ "symbols": list }))
}

fn source_json(s: &AppState) -> Value {
    json!({
        "kind": s.source.kind,
        "simulated": s.source.simulated,
        "detail": s.source.detail,
        "admin": s.admin,
        "replay_speed": (s.source.kind == "replay").then(|| s.replay.speed()),
        "replay_paused": (s.source.kind == "replay").then(|| s.replay.paused.load(Relaxed)),
    })
}

async fn source(State(s): State<AppState>) -> Json<Value> {
    Json(source_json(&s))
}

#[derive(Deserialize)]
struct DepthQuery {
    depth: Option<usize>,
    deltas: Option<bool>,
}

const DEFAULT_DEPTH: usize = 20;

fn clamp_depth(q: &DepthQuery, max: usize) -> usize {
    q.depth.unwrap_or(DEFAULT_DEPTH).clamp(1, max.max(1))
}

async fn snapshot(
    State(s): State<AppState>,
    Path(symbol): Path<String>,
    Query(q): Query<DepthQuery>,
) -> Response {
    let engine = s.engine();
    let depth = clamp_depth(&q, engine.config().depth);
    match engine.view(&symbol, depth, now_us()) {
        Some(view) => Json(view).into_response(),
        None => unknown(&symbol),
    }
}

async fn quote(State(s): State<AppState>, Path(symbol): Path<String>) -> Response {
    let engine = s.engine();
    let now = now_us();
    match (engine.quote(&symbol), engine.health(&symbol, now)) {
        (Some(quote), Some(h)) => Json(json!({
            "quote": quote, "state": h.state, "integrity": h.integrity,
            "stale": h.stale, "stale_ms": h.stale_ms,
        }))
        .into_response(),
        _ => unknown(&symbol),
    }
}

async fn health(State(s): State<AppState>, Path(symbol): Path<String>) -> Response {
    match s.engine().health(&symbol, now_us()) {
        Some(h) => Json(h).into_response(),
        None => unknown(&symbol),
    }
}

async fn stream(
    State(s): State<AppState>,
    Path(symbol): Path<String>,
    Query(q): Query<DepthQuery>,
    ws: WebSocketUpgrade,
) -> Response {
    let depth = {
        let engine = s.engine();
        if engine.instrument(&symbol).is_none() {
            return unknown(&symbol);
        }
        clamp_depth(&q, engine.config().depth)
    };
    let deltas = q.deltas.unwrap_or(false);
    ws.on_upgrade(move |socket| async move {
        s.metrics.ws_clients.fetch_add(1, Relaxed);
        client(socket, &s, &symbol, depth, deltas).await;
        s.metrics.ws_clients.fetch_sub(1, Relaxed);
    })
}

async fn send(socket: &mut WebSocket, text: String) -> bool {
    socket.send(Message::Text(text.into())).await.is_ok()
}

/// Messages every client needs to (re)establish a base: image and health.
/// Delta subscribers get the full book, since a depth-limited image cannot be
/// kept correct by applying deltas (levels below the cut would be unknown).
fn base_messages(s: &AppState, symbol: &str, depth: usize, deltas: bool) -> Vec<String> {
    let depth = if deltas { usize::MAX } else { depth };
    let engine = s.engine();
    let now = now_us();
    let mut msgs = Vec::new();
    if let Some(view) = engine.view(symbol, depth, now) {
        msgs.push(tagged("snapshot", &view));
    }
    if let Some(h) = engine.health(symbol, now) {
        msgs.push(tagged("health", &h));
    }
    msgs
}

async fn client(mut socket: WebSocket, s: &AppState, symbol: &str, depth: usize, deltas: bool) {
    // Subscribe before reading the base image so no update can fall between.
    let mut rx = s.tx.subscribe();
    let hello = {
        let engine = s.engine();
        json!({
            "type": "hello",
            "symbol": symbol,
            "instrument": engine.instrument(symbol),
            "depth": depth,
            "deltas": deltas,
            "stale_after_ms": engine.config().stale_after_ms,
            "metric_depth": engine.config().metric_depth,
            "source": source_json(s),
            "server_time": now_us(),
        })
        .to_string()
    };
    if !send(&mut socket, hello).await {
        return;
    }
    for msg in base_messages(s, symbol, depth, deltas) {
        if !send(&mut socket, msg).await {
            return;
        }
    }
    loop {
        tokio::select! {
            msg = rx.recv() => match msg {
                Ok(m) => {
                    if m.symbol != symbol && m.symbol != ALL_SYMBOLS {
                        continue;
                    }
                    let text = match &m.payload {
                        Payload::Json(j) => j.clone(),
                        Payload::Delta(j) if deltas => j.clone(),
                        Payload::Delta(_) => continue,
                        Payload::Book(view) => tagged("book", &view.truncated(depth)),
                        Payload::Snapshot(view) if deltas => tagged("snapshot", view),
                        Payload::Snapshot(view) => tagged("snapshot", &view.truncated(depth)),
                    };
                    if !send(&mut socket, text).await {
                        return;
                    }
                }
                Err(broadcast::error::RecvError::Lagged(missed)) => {
                    // Same recovery as the feed side: tell the client it
                    // lost messages and hand it a fresh base image.
                    s.metrics.ws_lagged.fetch_add(1, Relaxed);
                    let notice = tagged("status", &StatusEvent {
                        symbol: symbol.to_string(),
                        ts: now_us(),
                        kind: StatusKind::ClientLagged,
                        seq: None,
                        value_ms: None,
                        detail: format!("subscriber fell behind by {missed} messages; re-based"),
                    });
                    if !send(&mut socket, notice).await {
                        return;
                    }
                    for msg in base_messages(s, symbol, depth, deltas) {
                        if !send(&mut socket, msg).await {
                            return;
                        }
                    }
                }
                Err(broadcast::error::RecvError::Closed) => return,
            },
            incoming = socket.recv() => match incoming {
                None | Some(Err(_)) | Some(Ok(Message::Close(_))) => return,
                Some(Ok(_)) => {}
            },
        }
    }
}

async fn admin_fault(State(s): State<AppState>, Json(fault): Json<Fault>) -> Response {
    if !s.admin {
        return error(
            StatusCode::FORBIDDEN,
            "admin routes are disabled (start with --admin)",
        );
    }
    if s.source.kind != "sim" {
        return error(
            StatusCode::CONFLICT,
            "fault injection needs the simulated source",
        );
    }
    if let Fault::Stall { ms } = fault {
        if ms > 60_000 {
            return error(StatusCode::BAD_REQUEST, "stall is limited to 60000 ms");
        }
    }
    match s.ctl.send(SourceCtl::Fault(fault)) {
        Ok(()) => Json(json!({ "injected": fault })).into_response(),
        Err(_) => error(
            StatusCode::SERVICE_UNAVAILABLE,
            "feed source is not running",
        ),
    }
}

#[derive(Deserialize)]
struct ReplayRequest {
    speed: Option<f64>,
    paused: Option<bool>,
}

async fn admin_replay(State(s): State<AppState>, Json(req): Json<ReplayRequest>) -> Response {
    if !s.admin {
        return error(
            StatusCode::FORBIDDEN,
            "admin routes are disabled (start with --admin)",
        );
    }
    if s.source.kind != "replay" {
        return error(StatusCode::CONFLICT, "the active source is not a replay");
    }
    if let Some(speed) = req.speed {
        if !speed.is_finite() || speed < 0.0 {
            return error(
                StatusCode::BAD_REQUEST,
                "speed must be a non-negative number",
            );
        }
        s.replay.set_speed(speed);
    }
    if let Some(paused) = req.paused {
        s.replay.paused.store(paused, Relaxed);
    }
    Json(source_json(&s)).into_response()
}
