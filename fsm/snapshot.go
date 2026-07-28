// SPDX-License-Identifier: BSD-3-Clause

package fsm

import (
	"encoding/binary"
	"hash/fnv"
)

// Snapshot is a portable, minimal record of an instance's position, suitable
// for write-through to external storage (Scree, NVRAM, JetStream KV). It never
// captures event payloads: only the deferred triggers are recorded, since the
// generic event type E is not serializable by the library. The fire path never
// touches storage; the owner decides when to Snapshot and persist.
type Snapshot[S ~uint8] struct {
	// Version is a stable hash of the compiled topology. Restore fails on a
	// mismatch so a firmware update that changes the machine cannot silently
	// resume into a state that no longer means the same thing.
	Version uint32
	// State is the current leaf state.
	State S
	// Deferred holds the deferred-trigger ring contents in FIFO order.
	Deferred []uint8
}

// Snapshot captures the instance's current position.
func (i *Instance[S, T, E]) Snapshot() Snapshot[S] {
	var def []uint8
	if i.dlen > 0 {
		def = make([]uint8, i.dlen)
		for k := 0; k < i.dlen; k++ {
			def[k] = uint8(i.deferred[(i.dhead+k)%len(i.deferred)].t)
		}
	}
	return Snapshot[S]{
		Version:  i.cfg.version,
		State:    i.state,
		Deferred: def,
	}
}

// Restore repositions the instance from a snapshot. It returns
// [ErrSnapshotVersion] if the snapshot was taken from a different compiled
// topology, or [ErrUnknownState] if the state is out of range. Deferred
// triggers are restored with zero event payloads (payloads are not persisted).
// The run-to-completion queue is cleared and the state timeout re-armed.
func (i *Instance[S, T, E]) Restore(s Snapshot[S]) error {
	if s.Version != i.cfg.version {
		return ErrSnapshotVersion
	}
	if int(s.State) >= i.cfg.numStates {
		return ErrUnknownState
	}

	i.state = s.State
	i.qhead, i.qlen = 0, 0
	i.dhead, i.dlen = 0, 0
	// Snapshots store leaves; descend initials defensively if a composite index
	// was supplied.
	for i.cfg.initialSub[i.state] >= 0 {
		i.state = S(i.cfg.initialSub[i.state])
	}

	if len(s.Deferred) > 0 && i.deferred != nil {
		for _, tv := range s.Deferred {
			if i.dlen >= len(i.deferred) {
				break
			}
			i.deferred[(i.dhead+i.dlen)%len(i.deferred)] = queued[T, E]{t: T(tv)}
			i.dlen++
		}
	}

	i.armTimeout()
	return nil
}

// computeVersion hashes the compiled topology into a stable 32-bit value. It
// covers structure only (states, triggers, transition kinds/targets/guard
// counts, hierarchy, deferrals, timeouts); guard and action function identities
// cannot be hashed, but a topology change — the case that makes a restore
// unsafe — always changes the result.
func (c *Config[S, T, E]) computeVersion() uint32 {
	h := fnv.New32a()
	var buf [8]byte
	wr := func(v uint64) {
		binary.LittleEndian.PutUint64(buf[:], v)
		_, _ = h.Write(buf[:])
	}

	wr(uint64(c.initial))
	wr(uint64(c.numStates))
	wr(uint64(c.numTriggers))

	for s := 0; s < c.numStates; s++ {
		wr(uint64(uint32(c.parent[s])))
		wr(uint64(uint32(c.initialSub[s])))
		wr(uint64(c.timeoutDur[s]))
		wr(uint64(c.timeoutTrig[s]))
	}
	for _, w := range c.deferMask {
		wr(w)
	}
	for s := 0; s < c.numStates; s++ {
		for t := 0; t < c.numTriggers; t++ {
			cl := c.cell(s, t)
			wr(uint64(len(cl.candidates)))
			for k := range cl.candidates {
				cand := &cl.candidates[k]
				wr(uint64(cand.kind))
				wr(uint64(cand.dst))
				wr(uint64(len(cand.guards)))
			}
		}
	}
	return h.Sum32()
}
