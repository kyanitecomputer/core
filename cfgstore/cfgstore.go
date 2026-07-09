// Package cfgstore provides a flat configuration key-value store backed by the
// Scree flash filesystem (src.kyanite.computer/scree). It is shared by the
// Kyanite device runtimes (vein, cairn).
//
// Scree organizes data into namespaces; cfgstore binds a single namespace
// ("config") and exposes a flat string→[]byte KV API (Get/Set/Delete/Keys) that
// higher-level typed config layers build on. Values are stored as Scree
// namespace metadata records.
//
// # Bring-up vs hardware
//
// OpenRAM creates an in-memory NOR-backed store for bring-up and host tests
// (contents are lost on reboot). On real hardware, build a blkdev.BlockDevice
// over a QSPI driver.NORFlash (blkdev.NewNOR) and call Open — the KV API is
// identical.
package cfgstore

import (
	"errors"

	"src.kyanite.computer/core/ramnor"
	"src.kyanite.computer/scree"
	"src.kyanite.computer/scree/blkdev"
	"src.kyanite.computer/scree/driver"
)

// ErrNotFound is returned by Get when a key does not exist.
var ErrNotFound = errors.New("cfgstore: key not found")

// configNamespace is the Scree namespace holding all configuration keys.
const configNamespace = "config"

// Bring-up RAM device geometry. PageSize bounds the maximum KV record size
// (a Scree journal record must fit in one page); 512 bytes comfortably holds
// all current config values.
const (
	ramEraseBlockSize = 4096
	ramPageSize       = 512
	ramEraseValue     = 0xFF
)

// Store is a flat KV store bound to the Scree "config" namespace.
type Store struct {
	s  *scree.Store
	ns scree.NamespaceID
}

// Open mounts an existing Scree volume on dev (formatting it first if it is not
// yet formatted) and binds the config namespace. journalBlocks is used only
// when formatting; pass 0 for the Scree default.
func Open(dev blkdev.BlockDevice, journalBlocks int) (*Store, error) {
	st, err := scree.Mount(dev, scree.MountOptions{})
	if err != nil {
		// Unformatted or unsupported: format, then mount.
		if ferr := scree.Format(dev, scree.FormatOptions{JournalBlocks: journalBlocks}); ferr != nil {
			return nil, ferr
		}
		st, err = scree.Mount(dev, scree.MountOptions{})
		if err != nil {
			return nil, err
		}
	}
	// CreateNamespace is idempotent: it returns the existing ID if present.
	ns, err := st.CreateNamespace(configNamespace)
	if err != nil {
		return nil, err
	}
	return &Store{s: st, ns: ns}, nil
}

// OpenRAM creates an in-memory NOR-backed config store of the given total size.
// Intended for bring-up and host tests; contents do not persist across reboots.
func OpenRAM(size int64) (*Store, error) {
	flash, err := ramnor.New(driver.Geometry{
		TotalSize:      size,
		EraseBlockSize: ramEraseBlockSize,
		PageSize:       ramPageSize,
		EraseValue:     ramEraseValue,
	})
	if err != nil {
		return nil, err
	}
	dev, err := blkdev.NewNOR(flash)
	if err != nil {
		return nil, err
	}
	// Reserve the four super/master blocks; give the rest to the journal.
	journalBlocks := dev.BlockCount() - 4
	return Open(dev, journalBlocks)
}

// Get returns the value for key, or ErrNotFound if the key does not exist.
func (st *Store) Get(key string) ([]byte, error) {
	v, err := st.s.GetMeta(st.ns, key)
	if errors.Is(err, scree.ErrNotFound) {
		return nil, ErrNotFound
	}
	return v, err
}

// Set stores key=val.
func (st *Store) Set(key string, val []byte) error {
	return st.s.PutMeta(st.ns, key, val)
}

// Delete removes key.
func (st *Store) Delete(key string) error {
	return st.s.DeleteMeta(st.ns, key)
}

// Keys returns all stored keys, sorted.
func (st *Store) Keys() ([]string, error) {
	return st.s.MetaKeys(st.ns)
}
