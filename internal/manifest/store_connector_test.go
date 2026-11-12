package manifest

import (
	"context"
	"database/sql/driver"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestAStoreOpensThroughTheConnectorATestSetForItsPathAlone pins the seam a
// test that waits for one transaction's rollback at the driver opens its
// store through (SetConnectorForTest, backlog B869's third round): a store
// opened at the path a wrap was set for, however that path was spelled, opens
// every connection through the wrap of the registered driver's connector, and
// works as one opened without it: its VFS opens the file, its migration
// indexes the tracks on unicode_lower, a function registered on the driver,
// and a write maintains that index. A store at another path, or one opened
// once the seam is restored, opens no connection through it.
func TestAStoreOpensThroughTheConnectorATestSetForItsPathAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "set", "bridge.db")
	var connects atomic.Int64
	restore := setStoreConnector(dir+"/set/./bridge.db", func(base driver.Connector) driver.Connector {
		return connectCounter{Connector: base, n: &connects}
	})
	defer restore()
	s := openTestStoreAt(t, path)
	putTrack(t, s, &Track{Path: "Album/01.flac", Title: "Zephyr Keep", Artist: "A", Album: "L"})
	if connects.Load() == 0 {
		t.Fatal("the store at the path its connector was set for opened no connection through it")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	n := connects.Load()
	other := openTestStoreAt(t, filepath.Join(dir, "other", "bridge.db"))
	putTrack(t, other, &Track{Path: "Album/01.flac", Title: "Zephyr Keep", Artist: "A", Album: "L"})
	restore()
	again := openTestStoreAt(t, path)
	putTrack(t, again, &Track{Path: "Album/02.flac", Title: "Second", Artist: "A", Album: "L"})
	if got := connects.Load(); got != n {
		t.Errorf("%d connections of a store at another path, or opened after the restore, went through the connector", got-n)
	}
}

// connectCounter counts the connections its connector opens.
type connectCounter struct {
	driver.Connector
	n *atomic.Int64
}

func (c connectCounter) Connect(ctx context.Context) (driver.Conn, error) {
	c.n.Add(1)
	return c.Connector.Connect(ctx)
}
