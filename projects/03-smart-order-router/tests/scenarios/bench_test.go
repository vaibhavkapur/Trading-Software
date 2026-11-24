package scenarios

import (
	"path/filepath"
	"testing"

	"sor/src/scenario"
)

// BenchmarkScenario measures one full simulated 10-minute TWAP (router run
// only): ~6,000 venue steps x 3 venues plus all routing and order handling.
func BenchmarkScenario(b *testing.B) {
	sc, err := scenario.Load(filepath.Join("..", "..", "examples", "venue-outage.json"))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := scenario.Run(sc, "", nil); err != nil {
			b.Fatal(err)
		}
	}
}
