package simulator

import (
	"fmt"
	"io"
	"math/rand/v2"
	"time"

	"mdp/src/ingestors"
	"mdp/src/refdata"
)

// FaultAt schedules a fault at a step of a generated fixture.
type FaultAt struct {
	Step  int    `json:"step"`
	Venue string `json:"venue"` // alpha | beta
	Fault string `json:"fault"`
	N     int    `json:"n,omitempty"`
	Ms    int    `json:"ms,omitempty"`
}

// DefaultFaults is the fault plan of the committed fixture. It puts every
// kind of bad input the pipeline must handle into one capture.
func DefaultFaults() []FaultAt {
	return []FaultAt{
		{Step: 500, Venue: "alpha", Fault: "duplicate", N: 3},
		{Step: 900, Venue: "beta", Fault: "duplicate", N: 2},
		{Step: 1300, Venue: "alpha", Fault: "lose", N: 4},
		{Step: 1700, Venue: "beta", Fault: "lose", N: 5},
		{Step: 2100, Venue: "alpha", Fault: "garbage", N: 1},
		{Step: 2300, Venue: "beta", Fault: "garbage", N: 1},
		{Step: 2500, Venue: "alpha", Fault: "offtick", N: 2},
		{Step: 2700, Venue: "beta", Fault: "skew", Ms: 2000},
		{Step: 2760, Venue: "beta", Fault: "skew", Ms: 0},
		{Step: 3000, Venue: "alpha", Fault: "stale", N: 3, Ms: 30000},
		{Step: 3300, Venue: "beta", Fault: "drop", N: 2},
	}
}

// WriteFixture generates a capture without sockets or wall-clock time: each
// step publishes one event per venue on a virtual clock, and the frames each
// venue would have sent are written with a synthetic receive time. The
// output depends only on the arguments, so a fixture can be regenerated
// bit for bit.
func WriteFixture(reg *refdata.Registry, seed uint64, steps int, start time.Time, faults []FaultAt, w io.Writer) (frames int, err error) {
	sim, err := New(reg, Config{Seed: seed, Session: "FIX"})
	if err != nil {
		return 0, err
	}
	cw := ingestors.NewCaptureWriter(w)
	rng := rand.New(rand.NewPCG(seed, 0x666978)) // timing only; separate from the markets
	now := start.UTC()
	recv := now
	record := func(provider string) func([]byte) {
		return func(frame []byte) {
			frames++
			if werr := cw.Write(provider, recv, frame); werr != nil && err == nil {
				err = werr
			}
		}
	}
	sim.Alpha.Tap(record(ingestors.ProviderAlpha))
	sim.Beta.Tap(record(ingestors.ProviderBeta))

	venues := map[string]*Venue{"alpha": sim.Alpha, "beta": sim.Beta}
	byStep := map[int][]FaultAt{}
	for _, f := range faults {
		if venues[f.Venue] == nil {
			return 0, fmt.Errorf("simulator: fault at step %d: unknown venue %q", f.Step, f.Venue)
		}
		byStep[f.Step] = append(byStep[f.Step], f)
	}
	// arrive advances the receive clock: network latency after the event,
	// and never earlier than the previous frame, as on a real socket.
	arrive := func() {
		at := now.Add(150*time.Microsecond + time.Duration(rng.IntN(850))*time.Microsecond)
		if !at.After(recv) {
			at = recv.Add(time.Microsecond)
		}
		recv = at
	}
	for step := 0; step < steps; step++ {
		for _, f := range byStep[step] {
			if !ApplyFault(venues[f.Venue], f.Fault, max(f.N, 1), time.Duration(f.Ms)*time.Millisecond, "") {
				return 0, fmt.Errorf("simulator: fault at step %d: unknown fault %q", f.Step, f.Fault)
			}
		}
		now = now.Add(100*time.Microsecond + time.Duration(rng.IntN(2000))*time.Microsecond)
		arrive()
		sim.Alpha.Publish(sim.alphaMarket.Next(now))
		arrive()
		sim.Beta.Publish(sim.betaMarket.Next(now))
	}
	if ferr := cw.Flush(); ferr != nil && err == nil {
		err = ferr
	}
	return frames, err
}
