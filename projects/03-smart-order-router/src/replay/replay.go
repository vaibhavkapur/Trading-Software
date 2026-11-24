// Package replay re-runs the planner against the frozen inputs recorded in an
// event log and checks that every routing decision is reproduced exactly.
package replay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"sor/src/planner"
)

type Result struct {
	Decisions  int
	Mismatches []string
}

func Verify(r io.Reader) (Result, error) {
	var res Result
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for line := 1; sc.Scan(); line++ {
		var ev struct {
			Kind string `json:"kind"`
			Data struct {
				Input    planner.Input   `json:"input"`
				Decision json.RawMessage `json:"decision"`
			} `json:"data"`
		}
		if !bytes.Contains(sc.Bytes(), []byte(`"kind":"route_decision"`)) {
			continue
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return res, fmt.Errorf("line %d: %w", line, err)
		}
		got, err := json.Marshal(planner.Plan(ev.Data.Input))
		if err != nil {
			return res, err
		}
		res.Decisions++
		if !bytes.Equal(got, ev.Data.Decision) {
			res.Mismatches = append(res.Mismatches, fmt.Sprintf("line %d: parent %s snapshot %s: recorded %s, replayed %s",
				line, ev.Data.Input.ParentID, ev.Data.Input.Snapshot.ID, ev.Data.Decision, got))
		}
	}
	return res, sc.Err()
}
