// Package unit tests each package in isolation, with no sockets and no
// external services.
package unit

import (
	"testing"

	"mdp/src/storage"
	"mdp/src/stream"
	"mdp/tests/contract"
)

func TestMemBusHonoursTheBusContract(t *testing.T) {
	contract.RunBus(t, stream.NewMemBus())
}

func TestMemStoreHonoursTheStoreContract(t *testing.T) {
	contract.RunStore(t, storage.NewMemStore())
}
