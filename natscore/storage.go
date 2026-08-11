package natscore

import (
	"fmt"
	"sync"

	"github.com/nats-io/nats-server/v2/server"
	"src.kyanite.computer/core/ramnor"
	"src.kyanite.computer/scree"
	"src.kyanite.computer/scree/blkdev"
	"src.kyanite.computer/scree/driver"
	screejs "src.kyanite.computer/scree/jetstream"
)

const (
	// ScreeStorage is the NATS storage type used for Scree-backed streams.
	ScreeStorage = server.StorageType(44)

	screeStorageName  = "scree"
	ramEraseBlockSize = 4096
	ramPageSize       = 512
	ramEraseValue     = 0xFF
)

var screeProvider = struct {
	sync.RWMutex
	dev blkdev.BlockDevice
}{}

var (
	registerScreeOnce sync.Once
	registerScreeErr  error
)

// RegisterRAMStore creates a RAM-backed Scree volume and registers it with JetStream.
func RegisterRAMStore(size int64) (blkdev.BlockDevice, error) {
	flash, err := ramnor.New(driver.Geometry{
		TotalSize:      size,
		EraseBlockSize: ramEraseBlockSize,
		PageSize:       ramPageSize,
		EraseValue:     ramEraseValue,
	})
	if err != nil {
		return nil, fmt.Errorf("create RAM NOR: %w", err)
	}
	dev, err := blkdev.NewNOR(flash)
	if err != nil {
		return nil, fmt.Errorf("create Scree NOR block device: %w", err)
	}
	if err := scree.Format(dev, scree.FormatOptions{JournalBlocks: dev.BlockCount() - 4}); err != nil {
		return nil, fmt.Errorf("format Scree: %w", err)
	}
	return RegisterStore(dev)
}

// RegisterStore registers an already-formatted Scree block device (e.g. a
// flash-backed volume built by the device runtime) as the JetStream store. It
// is the persistent-storage counterpart to RegisterRAMStore: the caller owns
// the device's construction and formatting; natscore owns the single JetStream
// store-type registration. Returns the device unchanged for convenience.
func RegisterStore(dev blkdev.BlockDevice) (blkdev.BlockDevice, error) {
	if dev == nil {
		return nil, fmt.Errorf("natscore: store device is nil")
	}

	screeProvider.Lock()
	screeProvider.dev = dev
	screeProvider.Unlock()

	registerScreeOnce.Do(func() {
		registerScreeErr = screejs.Register(ScreeStorage, screeStorageName, func(server.StreamStoreConfig) (screejs.Options, error) {
			screeProvider.RLock()
			dev := screeProvider.dev
			screeProvider.RUnlock()
			if dev == nil {
				return screejs.Options{}, fmt.Errorf("natscore: Scree store is not configured")
			}
			return screejs.Options{Device: dev}, nil
		})
	})
	if registerScreeErr != nil {
		return nil, fmt.Errorf("register Scree JetStream store: %w", registerScreeErr)
	}
	return dev, nil
}
