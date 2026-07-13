// SPDX-License-Identifier: BSD-3-Clause

//go:build !tamago

package supervise

import (
	"bytes"
	"runtime/pprof"
	"strings"
	"testing"
)

// AssertNoLeaks fails the test if the goroutineleak profile (GA in Go 1.27)
// reports any goroutine still labelled as a supervise child — i.e. a leaked
// guard. It is a no-op skip where the profile is unavailable. Because every
// guard goroutine carries pprof labels (see guard.go), the profile output
// self-attributes to the exact tree path and child.
//
// It is excluded from tamago builds (it imports testing); on device the
// equivalent signal is the /debug/pprof/goroutineleak endpoint.
func AssertNoLeaks(t *testing.T) {
	t.Helper()
	p := pprof.Lookup("goroutineleak")
	if p == nil {
		t.Skip("goroutineleak profile unavailable (needs Go 1.27+)")
		return
	}
	var buf bytes.Buffer
	if err := p.WriteTo(&buf, 1); err != nil {
		t.Fatalf("write goroutineleak profile: %v", err)
	}
	if strings.Contains(buf.String(), labelChild) {
		t.Fatalf("supervise: leaked goroutine(s):\n%s", buf.String())
	}
}
