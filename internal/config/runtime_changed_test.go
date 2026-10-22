package config

import (
	"errors"
	"path/filepath"
	"testing"
)

// closed reports whether ch is closed, without waiting.
func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestChangedIsClosedByTheNextStore: Changed is a channel the next Store
// closes, the one an Update makes included, and an Update that changes
// nothing (fn refused it, or the save failed) leaves it open. Each call
// after a Store is a new channel, and a consumer that takes the channel
// before it Loads never misses a Store in between: the player's hotplug
// watcher follows output.player this way (B470 PR 4a2).
func TestChangedIsClosedByTheNextStore(t *testing.T) {
	dir := t.TempDir()
	rc := NewRuntimeConfig(updateTestCfg(t, dir))
	ch := rc.Changed()
	if closed(ch) || rc.Changed() != ch {
		t.Fatal("Changed is closed, or new, with no Store since")
	}
	next := updateTestCfg(t, dir)
	next.LibraryName = "Stored"
	rc.Store(next)
	if !closed(ch) {
		t.Fatal("Store left Changed open")
	}
	ch = rc.Changed()
	if closed(ch) {
		t.Fatal("the channel after a Store is already closed")
	}
	refused := errors.New("refused")
	if err := rc.Update(filepath.Join(dir, "bridge.yaml"), func(*Config) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("Update: %v", err)
	}
	if err := rc.Update(filepath.Join(dir, "missing", "bridge.yaml"), func(c *Config) error { c.LibraryName = "Unsaved"; return nil }); err == nil {
		t.Fatal("an Update whose save fails reported none")
	}
	if closed(ch) {
		t.Fatal("an Update that stored nothing closed Changed")
	}
	if err := rc.Update(filepath.Join(dir, "bridge.yaml"), func(c *Config) error { c.LibraryName = "Updated"; return nil }); err != nil {
		t.Fatal(err)
	}
	if !closed(ch) || rc.Load().LibraryName != "Updated" {
		t.Fatalf("Update left Changed open, or stored %q", rc.Load().LibraryName)
	}
	var nilHolder *RuntimeConfig
	if nilHolder.Changed() != nil {
		t.Error("a nil holder's Changed is not nil")
	}
}
