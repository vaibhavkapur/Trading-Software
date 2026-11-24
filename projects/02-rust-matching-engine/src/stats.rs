//! Latency summaries and a stable hash for comparing event streams.

/// Collects raw samples and reports nearest-rank percentiles. Exact (no
/// bucketing); memory is 8 bytes per sample.
#[derive(Default)]
pub struct LatencyRecorder {
    samples: Vec<u64>,
    sorted: bool,
}

impl LatencyRecorder {
    pub fn with_capacity(n: usize) -> Self {
        LatencyRecorder {
            samples: Vec::with_capacity(n),
            sorted: false,
        }
    }

    #[inline]
    pub fn record(&mut self, value: u64) {
        self.samples.push(value);
        self.sorted = false;
    }

    pub fn len(&self) -> usize {
        self.samples.len()
    }

    pub fn is_empty(&self) -> bool {
        self.samples.is_empty()
    }

    fn sort(&mut self) {
        if !self.sorted {
            self.samples.sort_unstable();
            self.sorted = true;
        }
    }

    /// Nearest-rank percentile, `p` in (0, 100]. Returns 0 with no samples.
    pub fn percentile(&mut self, p: f64) -> u64 {
        if self.samples.is_empty() {
            return 0;
        }
        self.sort();
        let n = self.samples.len();
        // The epsilon stops binary rounding (99.9% of 1000 = 999.0000000000001)
        // from pushing an exact rank up by one.
        let rank = ((p / 100.0) * n as f64 - 1e-9).ceil() as usize;
        self.samples[rank.clamp(1, n) - 1]
    }

    pub fn min(&mut self) -> u64 {
        self.sort();
        self.samples.first().copied().unwrap_or(0)
    }

    pub fn max(&mut self) -> u64 {
        self.sort();
        self.samples.last().copied().unwrap_or(0)
    }

    pub fn mean(&self) -> f64 {
        if self.samples.is_empty() {
            return 0.0;
        }
        self.samples.iter().sum::<u64>() as f64 / self.samples.len() as f64
    }
}

/// Incremental FNV-1a (64-bit). Stable across platforms and Rust versions,
/// unlike `std`'s default hasher.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Fnv1a(pub u64);

impl Default for Fnv1a {
    fn default() -> Self {
        Fnv1a(0xcbf2_9ce4_8422_2325)
    }
}

impl Fnv1a {
    pub fn update(&mut self, bytes: &[u8]) {
        for b in bytes {
            self.0 = (self.0 ^ u64::from(*b)).wrapping_mul(0x0000_0100_0000_01b3);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn percentiles_are_nearest_rank() {
        let mut r = LatencyRecorder::default();
        (1..=1000).for_each(|v| r.record(v));
        assert_eq!(r.percentile(50.0), 500);
        assert_eq!(r.percentile(99.0), 990);
        assert_eq!(r.percentile(99.9), 999);
        assert_eq!(r.percentile(100.0), 1000);
        assert_eq!((r.min(), r.max()), (1, 1000));
    }

    #[test]
    fn fnv1a_known_vector() {
        let mut h = Fnv1a::default();
        h.update(b"a");
        assert_eq!(h.0, 0xaf63dc4c8601ec8c);
    }
}
