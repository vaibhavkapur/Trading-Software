package unit

import (
	"reflect"
	"testing"

	"mdp/src/seqtrack"
)

func TestSequenceClassification(t *testing.T) {
	tr := seqtrack.New()
	steps := []struct {
		session  string
		seq      uint64
		want     seqtrack.Verdict
		from, to uint64
	}{
		{"S1", 7, seqtrack.OK, 0, 0}, // first sighting: nothing to compare against
		{"S1", 8, seqtrack.OK, 0, 0},
		{"S1", 8, seqtrack.Duplicate, 0, 0},
		{"S1", 3, seqtrack.Duplicate, 0, 0},
		{"S1", 12, seqtrack.Gap, 9, 11},
		{"S1", 11, seqtrack.Duplicate, 0, 0}, // a late arrival inside a declared gap is not re-admitted
		{"S1", 13, seqtrack.OK, 0, 0},
		{"S2", 1, seqtrack.Reset, 0, 0}, // numbering restarts with the session
		{"S2", 2, seqtrack.OK, 0, 0},
		{"S2", 4, seqtrack.Gap, 3, 3},
	}
	for i, s := range steps {
		got := tr.Observe("d", s.session, s.seq)
		if got.Verdict != s.want || got.MissingFrom != s.from || got.MissingTo != s.to {
			t.Fatalf("step %d (%s@%d): got %v [%d,%d], want %v [%d,%d]", i, s.session, s.seq,
				got.Verdict, got.MissingFrom, got.MissingTo, s.want, s.from, s.to)
		}
	}
	if st, _ := tr.Get("d"); st != (seqtrack.State{Session: "S2", Last: 4}) {
		t.Errorf("final state %+v", st)
	}
}

func TestSequenceDomainsAreIndependent(t *testing.T) {
	tr := seqtrack.New()
	tr.Observe("XYZ", "S", 1)
	tr.Observe("ABC", "S", 50)
	if r := tr.Observe("XYZ", "S", 2); r.Verdict != seqtrack.OK {
		t.Errorf("XYZ 2 after ABC 50: %v", r.Verdict)
	}
	if r := tr.Observe("ABC", "S", 52); r.Verdict != seqtrack.Gap || r.MissingFrom != 51 || r.MissingTo != 51 {
		t.Errorf("ABC 52: %+v", r)
	}
	if got := tr.Domains(); !reflect.DeepEqual(got, []string{"ABC", "XYZ"}) {
		t.Errorf("domains %v", got)
	}
}

func TestClassifyDoesNotRecord(t *testing.T) {
	tr := seqtrack.New()
	tr.Observe("d", "S", 5)
	if r := tr.Classify("d", "S", 9); r.Verdict != seqtrack.Gap {
		t.Fatalf("classify: %v", r.Verdict)
	}
	if r := tr.Observe("d", "S", 6); r.Verdict != seqtrack.OK {
		t.Errorf("Classify must not move the position: 6 after 5 gave %v", r.Verdict)
	}
}

func TestSnapshotRestoresExactly(t *testing.T) {
	a := seqtrack.New()
	a.Observe("x", "S1", 10)
	a.Observe("y", "S2", 3)
	snap := a.Snapshot()
	a.Observe("x", "S1", 11) // the snapshot is a copy
	b := seqtrack.New()
	b.Restore(snap)
	if r := b.Observe("x", "S1", 11); r.Verdict != seqtrack.OK {
		t.Errorf("restored tracker should expect 11 next, got %v", r.Verdict)
	}
	if r := b.Observe("y", "S2", 3); r.Verdict != seqtrack.Duplicate {
		t.Errorf("restored tracker should know y@3, got %v", r.Verdict)
	}
	if r := b.Observe("x", "S1", 14); r.Verdict != seqtrack.Gap || r.MissingFrom != 12 || r.MissingTo != 13 {
		t.Errorf("a gap spanning the restore: %+v", r)
	}
}

func TestFirstSightingIsMarked(t *testing.T) {
	tr := seqtrack.New()
	// The first number seen of a domain is accepted whatever it is, and marked,
	// so a caller that knows where the provider starts counting can tell
	// "joined at the start" from "joined late".
	if r := tr.Observe("d", "S1", 40); r.Verdict != seqtrack.OK || !r.First {
		t.Errorf("first sighting: %+v", r)
	}
	if r := tr.Observe("d", "S1", 41); r.Verdict != seqtrack.OK || r.First {
		t.Errorf("second event: %+v", r)
	}
	if r := tr.Observe("d", "S2", 7); r.Verdict != seqtrack.Reset || r.First {
		t.Errorf("a new session is a reset, not a first sighting: %+v", r)
	}
	if r := tr.Observe("other", "S2", 1); !r.First {
		t.Errorf("each domain has its own first sighting: %+v", r)
	}
	// A restored domain has been seen before.
	restored := seqtrack.New()
	restored.Restore(tr.Snapshot())
	if r := restored.Observe("d", "S2", 8); r.First || r.Verdict != seqtrack.OK {
		t.Errorf("after restore: %+v", r)
	}
}
