use book_core::{BTreeLadder, Ladder, Level, Side, VecLadder};
use feed_adapter::rng::Rng;

fn exercise<L: Ladder>() {
    let mut l = L::default();
    assert_eq!(l.best(Side::Bid), None);
    assert_eq!(l.set(Side::Bid, 100, 5), None);
    assert_eq!(l.set(Side::Bid, 102, 6), None);
    assert_eq!(l.set(Side::Bid, 101, 7), None);
    assert_eq!(l.set(Side::Bid, 101, 8), Some(7));
    assert_eq!(l.set(Side::Ask, 105, 1), None);
    assert_eq!(l.set(Side::Ask, 103, 2), None);
    assert_eq!(l.set(Side::Ask, 104, 3), None);
    let px = |v: Vec<Level>| v.into_iter().map(|x| x.price_ticks).collect::<Vec<_>>();
    assert_eq!(px(l.top(Side::Bid, 10)), [102, 101, 100]);
    assert_eq!(px(l.top(Side::Ask, 10)), [103, 104, 105]);
    assert_eq!(px(l.top(Side::Bid, 2)), [102, 101]);
    assert_eq!(l.top(Side::Ask, 0), vec![]);
    assert_eq!(l.best(Side::Bid).unwrap().price_ticks, 102);
    assert_eq!(l.best(Side::Ask).unwrap().price_ticks, 103);
    assert_eq!(l.get(Side::Bid, 101), Some(8));
    assert_eq!(l.get(Side::Ask, 101), None, "sides are independent");
    assert_eq!(l.remove(Side::Bid, 102), Some(6));
    assert_eq!(l.remove(Side::Bid, 102), None);
    assert_eq!(l.levels(Side::Bid), 2);
    l.clear();
    assert_eq!((l.levels(Side::Bid), l.levels(Side::Ask)), (0, 0));
}

#[test]
fn btree_ladder_contract() {
    exercise::<BTreeLadder>();
}

#[test]
fn vec_ladder_contract() {
    exercise::<VecLadder>();
}

#[test]
fn implementations_agree_on_random_operations() {
    for seed in 0..20 {
        let mut rng = Rng::new(seed);
        let (mut a, mut b) = (BTreeLadder::default(), VecLadder::default());
        for step in 0..5_000 {
            let side = if rng.chance(1, 2) {
                Side::Bid
            } else {
                Side::Ask
            };
            let price = 1 + rng.below(60) as i64;
            if rng.chance(1, 3) {
                assert_eq!(
                    a.remove(side, price),
                    b.remove(side, price),
                    "seed {seed} step {step}"
                );
            } else {
                let qty = 1 + rng.below(1_000);
                assert_eq!(
                    a.set(side, price, qty),
                    b.set(side, price, qty),
                    "seed {seed} step {step}"
                );
            }
            for s in [Side::Bid, Side::Ask] {
                assert_eq!(a.best(s), b.best(s));
                assert_eq!(a.levels(s), b.levels(s));
            }
            if step % 97 == 0 {
                for s in [Side::Bid, Side::Ask] {
                    assert_eq!(a.top(s, 7), b.top(s, 7));
                    assert_eq!(a.top(s, usize::MAX), b.top(s, usize::MAX));
                }
            }
        }
    }
}
