//! Criterion micro-benchmark of the matching core (no I/O, no journaling).
//! For the full latency/throughput/allocation report use `make bench`.

use criterion::{criterion_group, criterion_main, BatchSize, Criterion, Throughput};
use matching_engine::loadgen::{LoadGen, LoadProfile};
use matching_engine::{Engine, SequencedCommand};

const WARM: usize = 50_000;
const BATCH: usize = 10_000;

fn commands() -> Vec<SequencedCommand> {
    LoadGen::new(LoadProfile::new(42, 4))
        .take(WARM + BATCH)
        .enumerate()
        .map(|(i, cmd)| SequencedCommand {
            seq: i as u64 + 1,
            ts_ns: i as u64 + 1,
            cmd,
        })
        .collect()
}

fn bench(c: &mut Criterion) {
    let cmds = commands();
    let mut group = c.benchmark_group("matching_core");
    group.throughput(Throughput::Elements(BATCH as u64));
    group.bench_function("process_10k_mixed_on_warm_book", |b| {
        b.iter_batched(
            || {
                let mut engine = Engine::new();
                let mut out = Vec::with_capacity(256);
                for sc in &cmds[..WARM] {
                    out.clear();
                    engine.process(sc, &mut out).unwrap();
                }
                (engine, out)
            },
            |(mut engine, mut out)| {
                for sc in &cmds[WARM..] {
                    out.clear();
                    engine.process(sc, &mut out).unwrap();
                }
                engine
            },
            BatchSize::LargeInput,
        )
    });
    group.finish();
}

criterion_group!(benches, bench);
criterion_main!(benches);
