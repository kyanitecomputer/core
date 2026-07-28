// SPDX-License-Identifier: BSD-3-Clause

package fsm

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "update golden export files")

func doorLabeler() Labeler[state, trigger] {
	return Labeler[state, trigger]{
		State: func(s state) string {
			switch s {
			case closed:
				return "closed"
			case open:
				return "open"
			case locked:
				return "locked"
			}
			return "?"
		},
		Trigger: func(t trigger) string {
			switch t {
			case openT:
				return "open"
			case closeT:
				return "close"
			case lockT:
				return "lock"
			case unlockT:
				return "unlock"
			case knockT:
				return "knock"
			}
			return "?"
		},
	}
}

func powerLabeler() Labeler[pstate, ptrig] {
	return Labeler[pstate, ptrig]{
		State: func(s pstate) string {
			switch s {
			case off:
				return "off"
			case on:
				return "on"
			case idle:
				return "idle"
			case active:
				return "active"
			case fault:
				return "fault"
			}
			return "?"
		},
		Trigger: func(t ptrig) string {
			switch t {
			case power:
				return "power"
			case shutdown:
				return "shutdown"
			case activate:
				return "activate"
			case deactivate:
				return "deactivate"
			case reset:
				return "reset"
			}
			return "?"
		},
	}
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", path, err)
	}
	if got != string(want) {
		t.Fatalf("export mismatch for %s:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestExportGolden(t *testing.T) {
	var trace []string
	door := doorConfig(t, &trace)
	power := buildPower(t, &trace)

	checkGolden(t, "door.mmd", door.Mermaid(doorLabeler()))
	checkGolden(t, "door.dot", door.DOT(doorLabeler()))
	checkGolden(t, "power.mmd", power.Mermaid(powerLabeler()))
	checkGolden(t, "power.dot", power.DOT(powerLabeler()))
}
