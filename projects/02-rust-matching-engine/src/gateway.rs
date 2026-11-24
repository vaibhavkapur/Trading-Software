//! `gateway`: line-delimited JSON over TCP.
//!
//! This is the only async code in the crate. Connection tasks decode client
//! lines and push them onto one bounded channel; a dedicated OS thread owns
//! the [`Pipeline`] and drains that channel, so the matching loop stays
//! single-writer and the channel order *is* the arrival order.
//!
//! Protocol: the client sends one [`Command`] JSON object per line. For each
//! line the server replies, to that client only, with the resulting events
//! (one JSON object per line) followed by a terminator line
//! `{"type":"DONE","cmd_seq":N,"events":K}`. Lines that cannot be decoded, or
//! commands refused because the engine is halted, get a single
//! `{"type":"ERROR","message":"..."}` line and are never sequenced.
//!
//! There is no authentication or TLS: bind to loopback only.

use crate::matching_core::Command;
use crate::pipeline::Pipeline;
use std::io;
use std::thread::JoinHandle;
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::mpsc;
use tokio::task::JoinSet;

/// Longest accepted input line, in bytes.
pub const MAX_LINE_BYTES: usize = 4096;

type Reply = mpsc::UnboundedSender<Vec<u8>>;

pub enum Request {
    Command {
        cmd: Command,
        reply: Reply,
    },
    /// An undecodable line; routed through the engine thread so its error
    /// reply stays in order with replies to earlier commands.
    Invalid {
        message: String,
        reply: Reply,
    },
}

fn error_line(message: &str) -> Vec<u8> {
    #[derive(serde::Serialize)]
    struct ErrorLine<'a> {
        r#type: &'static str,
        message: &'a str,
    }
    let mut line = serde_json::to_vec(&ErrorLine {
        r#type: "ERROR",
        message,
    })
    .expect("error line serialises");
    line.push(b'\n');
    line
}

/// Starts the single-writer matching loop on its own thread. The loop ends,
/// returning the pipeline, when every request sender has been dropped.
pub fn spawn_engine(
    mut pipeline: Pipeline,
    mut rx: mpsc::Receiver<Request>,
) -> io::Result<JoinHandle<Pipeline>> {
    std::thread::Builder::new()
        .name("matching-loop".into())
        .spawn(move || {
            while let Some(request) = rx.blocking_recv() {
                // A failed send only means the client has gone away; its
                // command has still been sequenced and applied.
                match request {
                    Request::Invalid { message, reply } => {
                        let _ = reply.send(error_line(&message));
                    }
                    Request::Command { cmd, reply } => {
                        let out = match pipeline.submit(cmd) {
                            Ok((seq, events)) => {
                                let mut out = Vec::with_capacity(160 * (events.len() + 1));
                                for event in events {
                                    serde_json::to_writer(&mut out, event)
                                        .expect("events serialise");
                                    out.push(b'\n');
                                }
                                let done = format!(
                                    r#"{{"type":"DONE","cmd_seq":{seq},"events":{}}}"#,
                                    events.len()
                                );
                                out.extend_from_slice(done.as_bytes());
                                out.push(b'\n');
                                out
                            }
                            Err(e) => {
                                eprintln!("matching-loop: {e}");
                                error_line(&e.to_string())
                            }
                        };
                        let _ = reply.send(out);
                    }
                }
            }
            pipeline
        })
}

/// Accepts connections until the returned future is dropped, forwarding
/// decoded commands to `tx`. Dropping it also aborts every open connection,
/// which releases their senders and lets the matching loop drain and exit.
pub async fn serve(listener: TcpListener, tx: mpsc::Sender<Request>) -> io::Result<()> {
    let mut connections = JoinSet::new();
    loop {
        let (stream, _peer) = listener.accept().await?;
        let _ = stream.set_nodelay(true);
        while connections.try_join_next().is_some() {}
        connections.spawn(handle_connection(stream, tx.clone()));
    }
}

async fn handle_connection(stream: TcpStream, tx: mpsc::Sender<Request>) {
    let (read_half, mut write_half) = stream.into_split();
    let (reply, mut replies) = mpsc::unbounded_channel::<Vec<u8>>();
    let writer = tokio::spawn(async move {
        while let Some(bytes) = replies.recv().await {
            if write_half.write_all(&bytes).await.is_err() {
                break;
            }
        }
    });

    let mut reader = BufReader::new(read_half);
    let mut line = Vec::new();
    loop {
        line.clear();
        // Bound how much a single line may buffer.
        let mut limited = (&mut reader).take(MAX_LINE_BYTES as u64 + 1);
        match limited.read_until(b'\n', &mut line).await {
            Ok(0) | Err(_) => break,
            Ok(_) => {}
        }
        if line.last() != Some(&b'\n') && line.len() > MAX_LINE_BYTES {
            // Discard the rest of the over-long line in bounded chunks.
            let mut eof = false;
            while line.last() != Some(&b'\n') && !eof {
                line.clear();
                let mut limited = (&mut reader).take(MAX_LINE_BYTES as u64);
                eof = !matches!(limited.read_until(b'\n', &mut line).await, Ok(n) if n > 0);
            }
            let sent = tx
                .send(Request::Invalid {
                    message: format!("line exceeds {MAX_LINE_BYTES} bytes; ignored"),
                    reply: reply.clone(),
                })
                .await;
            if eof || sent.is_err() {
                break;
            }
            continue;
        }
        let text = String::from_utf8_lossy(&line);
        let text = text.trim();
        if text.is_empty() {
            continue;
        }
        let request = match serde_json::from_str::<Command>(text) {
            Ok(cmd) => Request::Command {
                cmd,
                reply: reply.clone(),
            },
            Err(e) => Request::Invalid {
                message: format!("invalid command: {e}"),
                reply: reply.clone(),
            },
        };
        if tx.send(request).await.is_err() {
            break; // engine thread has stopped
        }
    }
    drop(reply);
    let _ = writer.await;
}
