package unit

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"riskengine/src/audit"
	"riskengine/src/domain"
	"riskengine/tests/harness"
)

func writeLog(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := audit.Open(path, harness.Key(), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		ev := domain.Event{Type: domain.EvRelease, At: harness.T0, Release: &domain.ReleaseEvent{OrderID: "o", Reason: strings.Repeat("x", i+1)}}
		seal, err := l.Append(ev)
		if err != nil || seal.Seq != uint64(i+1) {
			t.Fatalf("append %d: seal=%+v err=%v", i, seal, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func pub() ed25519.PublicKey { return harness.Key().Public().(ed25519.PublicKey) }

func TestAuditChainVerifies(t *testing.T) {
	path := writeLog(t, 5)
	n, head, err := audit.Verify(path, pub())
	if err != nil || n != 5 || len(head) != 64 {
		t.Fatalf("verify: n=%d head=%q err=%v", n, head, err)
	}
}

func TestAuditDetectsTampering(t *testing.T) {
	edit := func(name string, f func(lines [][]byte) [][]byte) {
		path := writeLog(t, 5)
		raw, _ := os.ReadFile(path)
		lines := bytes.SplitAfter(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
		lines[len(lines)-1] = append(lines[len(lines)-1], '\n')
		os.WriteFile(path, bytes.Join(f(lines), nil), 0o600)
		if _, _, err := audit.Verify(path, pub()); err == nil {
			t.Errorf("%s: verification passed", name)
		}
		if _, err := audit.Open(path, harness.Key(), false, nil); err == nil {
			t.Errorf("%s: engine would have started on a tampered log", name)
		}
	}
	edit("altered event", func(l [][]byte) [][]byte {
		l[2] = bytes.Replace(l[2], []byte(`xxx`), []byte(`xxy`), 1)
		return l
	})
	edit("deleted record", func(l [][]byte) [][]byte { return append(l[:2], l[3:]...) })
	edit("reordered records", func(l [][]byte) [][]byte { l[1], l[2] = l[2], l[1]; return l })
	edit("dropped first record", func(l [][]byte) [][]byte { return l[1:] })
	edit("garbage line in the middle", func(l [][]byte) [][]byte { l[2] = []byte("{not json}\n"); return l })

	// A different key must not verify records signed by ours.
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	if _, _, err := audit.Verify(writeLog(t, 2), other); err == nil {
		t.Error("verification passed with the wrong public key")
	}
}

// Truncating the tail is the one tamper a hash chain alone cannot see;
// callers detect it by comparing against a head hash they saved earlier.
func TestAuditTailTruncationChangesHead(t *testing.T) {
	path := writeLog(t, 5)
	_, head5, _ := audit.Verify(path, pub())
	raw, _ := os.ReadFile(path)
	lines := bytes.SplitAfter(raw, []byte("\n"))
	os.WriteFile(path, bytes.Join(lines[:4], nil), 0o600)
	n, head4, err := audit.Verify(path, pub())
	if err != nil || n != 4 || head4 == head5 {
		t.Fatalf("n=%d err=%v sameHead=%v", n, err, head4 == head5)
	}
}

func TestAuditRecoversFromTornFinalWrite(t *testing.T) {
	path := writeLog(t, 3)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"seq":4,"prev_hash":"abc","event":{"type":"FI`) // crash mid-write: no newline
	f.Close()

	if _, _, err := audit.Verify(path, pub()); err == nil {
		t.Error("Verify must report the partial record")
	}
	replayed := 0
	l, err := audit.Open(path, harness.Key(), false, func(audit.Record) error { replayed++; return nil })
	if err != nil || replayed != 3 {
		t.Fatalf("open after torn write: replayed=%d err=%v", replayed, err)
	}
	seal, err := l.Append(domain.Event{Type: domain.EvRelease, At: harness.T0, Release: &domain.ReleaseEvent{OrderID: "o"}})
	if err != nil || seal.Seq != 4 {
		t.Fatalf("append after recovery: %+v %v", seal, err)
	}
	l.Close()
	if n, _, err := audit.Verify(path, pub()); err != nil || n != 4 {
		t.Fatalf("after recovery: n=%d err=%v", n, err)
	}
}

func TestSigningKeyIsCreatedPrivateAndReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.key")
	k1, err := audit.LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", fi.Mode().Perm())
	}
	k2, _ := audit.LoadOrCreateKey(path)
	if !k1.Equal(k2) {
		t.Error("key changed between loads")
	}
	os.WriteFile(path, []byte("not a key"), 0o600)
	if _, err := audit.LoadOrCreateKey(path); err == nil {
		t.Error("garbage key file accepted")
	}
}
