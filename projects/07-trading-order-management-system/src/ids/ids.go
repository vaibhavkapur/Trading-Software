// Package ids generates identifiers.
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
)

// Generator returns a new unique id with the given prefix, such as "ord".
type Generator interface {
	New(prefix string) string
}

// Random generates ids from 96 bits of cryptographic randomness.
type Random struct{}

func (Random) New(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("ids: no entropy: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// Sequence generates predictable ids (ord_000001, ord_000002, ...) for
// deterministic replay tests. One counter is shared by all prefixes.
type Sequence struct {
	n atomic.Int64
}

func (s *Sequence) New(prefix string) string {
	return fmt.Sprintf("%s_%06d", prefix, s.n.Add(1))
}
