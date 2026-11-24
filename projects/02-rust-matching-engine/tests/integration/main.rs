//! Integration tests: journal + recovery on a real filesystem, and the TCP
//! gateway end to end (including restarts and client disconnects).

mod gateway;
mod recovery;

use matching_engine::*;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU32, Ordering};

/// Self-cleaning temp directory (no external crate needed).
pub struct TempDir(PathBuf);

impl TempDir {
    pub fn new(tag: &str) -> Self {
        static N: AtomicU32 = AtomicU32::new(0);
        let dir = std::env::temp_dir().join(format!(
            "me-test-{tag}-{}-{}",
            std::process::id(),
            N.fetch_add(1, Ordering::Relaxed)
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        TempDir(dir)
    }

    pub fn path(&self) -> &Path {
        &self.0
    }
}

impl Drop for TempDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

pub fn limit(order_id: u64, side: Side, price: i64, qty: u64) -> Command {
    Command::New(NewOrder {
        order_id,
        account_id: 7,
        symbol: Symbol::new("XYZ").unwrap(),
        side,
        ord_type: OrdType::Limit,
        qty,
        price_ticks: Some(price),
    })
}
