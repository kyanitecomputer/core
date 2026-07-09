// Package ramnor provides an in-memory NOR flash driver for bring-up and host
// tests. It implements [driver.NORFlash] from src.kyanite.computer/scree and is
// shared by the Kyanite device runtimes (vein, cairn).
package ramnor

import (
	"errors"
	"sync"

	"src.kyanite.computer/scree/driver"
)

// Flash is an in-memory [driver.NORFlash].
type Flash struct {
	mu   sync.Mutex
	geom driver.Geometry
	data []byte
}

// New returns an erased in-memory NOR flash with the given geometry.
func New(geom driver.Geometry) (*Flash, error) {
	if err := geom.Validate(); err != nil {
		return nil, err
	}
	data := make([]byte, geom.TotalSize)
	for i := range data {
		data[i] = geom.EraseValue
	}
	return &Flash{geom: geom, data: data}, nil
}

// Geometry implements [driver.Flash].
func (f *Flash) Geometry() driver.Geometry { return f.geom }

// ReadAt implements [driver.NORFlash].
func (f *Flash) ReadAt(dst []byte, addr int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if addr < 0 || addr+int64(len(dst)) > int64(len(f.data)) {
		return 0, ErrOutOfRange
	}
	copy(dst, f.data[addr:addr+int64(len(dst))])
	return len(dst), nil
}

// WriteAt implements [driver.NORFlash].
func (f *Flash) WriteAt(src []byte, addr int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if addr < 0 || addr+int64(len(src)) > int64(len(f.data)) {
		return 0, ErrOutOfRange
	}
	if addr%int64(f.geom.PageSize) != 0 || len(src)%f.geom.PageSize != 0 {
		return 0, ErrUnaligned
	}
	start := int(addr)
	for i, b := range src {
		if old := f.data[start+i]; old&b != b {
			return 0, ErrNotErased
		}
	}
	for i, b := range src {
		f.data[start+i] &= b
	}
	return len(src), nil
}

// EraseBlock implements [driver.NORFlash].
func (f *Flash) EraseBlock(addr int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if addr < 0 || addr >= int64(len(f.data)) {
		return ErrOutOfRange
	}
	if addr%int64(f.geom.EraseBlockSize) != 0 {
		return ErrUnaligned
	}
	start := int(addr)
	end := start + f.geom.EraseBlockSize
	for i := start; i < end; i++ {
		f.data[i] = f.geom.EraseValue
	}
	return nil
}

var (
	// ErrOutOfRange reports an access outside the emulated flash.
	ErrOutOfRange = errors.New("ramnor: out of range")
	// ErrUnaligned reports a write or erase that does not match NOR geometry.
	ErrUnaligned = errors.New("ramnor: unaligned operation")
	// ErrNotErased reports a write that would set a bit from 0 to 1.
	ErrNotErased = errors.New("ramnor: target is not erased")
)

var _ driver.NORFlash = (*Flash)(nil)
