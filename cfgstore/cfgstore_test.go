package cfgstore

import (
	"bytes"
	"errors"
	"testing"

	"src.kyanite.computer/core/ramnor"
	"src.kyanite.computer/scree/blkdev"
	"src.kyanite.computer/scree/driver"
)

func driverGeom() driver.Geometry {
	return driver.Geometry{
		TotalSize:      2 * 1024 * 1024,
		EraseBlockSize: ramEraseBlockSize,
		PageSize:       ramPageSize,
		EraseValue:     ramEraseValue,
	}
}

// openOn mounts (formatting if needed) a config store over an existing flash,
// so a test can remount the same backing store.
func openOn(flash *ramnor.Flash) (*Store, error) {
	dev, err := blkdev.NewNOR(flash)
	if err != nil {
		return nil, err
	}
	return Open(dev, dev.BlockCount()-4)
}

func TestRoundTrip(t *testing.T) {
	st, err := OpenRAM(2 * 1024 * 1024)
	if err != nil {
		t.Fatalf("OpenRAM: %v", err)
	}

	// Missing key.
	if _, err := st.Get("sys.hostname"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: want ErrNotFound, got %v", err)
	}

	// Set/Get.
	if err := st.Set("sys.hostname", []byte("vega")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := st.Get("sys.hostname")
	if err != nil || !bytes.Equal(v, []byte("vega")) {
		t.Fatalf("Get: %q, %v", v, err)
	}

	// Overwrite.
	if err := st.Set("sys.hostname", []byte("router1")); err != nil {
		t.Fatalf("Set overwrite: %v", err)
	}
	if v, _ := st.Get("sys.hostname"); !bytes.Equal(v, []byte("router1")) {
		t.Fatalf("Get after overwrite: %q", v)
	}

	// Second key + Keys() sorted.
	if err := st.Set("sys.mgmt_ip", []byte("192.168.1.1/24")); err != nil {
		t.Fatalf("Set 2: %v", err)
	}
	keys, err := st.Keys()
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 2 || keys[0] != "sys.hostname" || keys[1] != "sys.mgmt_ip" {
		t.Fatalf("Keys: %v", keys)
	}

	// Delete.
	if err := st.Delete("sys.mgmt_ip"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Get("sys.mgmt_ip"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete: want ErrNotFound, got %v", err)
	}
}

func TestPersistenceAcrossRemount(t *testing.T) {
	flash, err := ramnor.New(driverGeom())
	if err != nil {
		t.Fatalf("ramnor.New: %v", err)
	}

	// First mount: format + write.
	st1, err := openOn(flash)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if err := st1.Set("port.0.admin", []byte("down")); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Remount the same flash: value must survive.
	st2, err := openOn(flash)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	v, err := st2.Get("port.0.admin")
	if err != nil || string(v) != "down" {
		t.Fatalf("Get after remount: %q, %v", v, err)
	}
}
