package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// raceClock models time.AfterFunc: Stop on a timer that has already fired
// is a no-op, and the callback may still be waiting to run.
type raceClock struct {
	now   time.Time
	tasks []*raceTask
}

type raceTask struct {
	fn             func()
	fired, stopped bool
}

func (c *raceClock) Now() time.Time { return c.now }

func (c *raceClock) AfterFunc(d time.Duration, fn func()) func() {
	task := &raceTask{fn: fn}
	c.tasks = append(c.tasks, task)
	return func() {
		if task.fired {
			return
		}
		task.stopped = true
		task.fn = nil
	}
}

func TestLibraryDebounceTrailsTheLastWrite(t *testing.T) {
	b := startBroker(t)
	clk := &manualClock{now: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)}
	base := clk.now.UnixNano()
	pub := newSyncEventPublisher(b, func() bool { return false },
		func(context.Context) (int64, bool, error) { return base, true, nil },
		clk, libraryChangeDebounce, true)
	pub.NoteLibrary(base)
	clk.Advance(20 * time.Second)
	pub.NoteLibrary(base + 1)
	clk.Advance(15 * time.Second)
	// Publish hands the broker a goroutine. Wait out one turn so a timer
	// the second note failed to replace is in the buffer before the count.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) && len(replayTopics(t, b)) < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := replayTopics(t, b); len(got) != 0 {
		t.Fatalf("an event fired 15s after the last write: %v", got)
	}
	clk.Advance(15 * time.Second)
	awaitReplay(t, b, 1)
	if got := replayTopics(t, b); len(got) != 1 {
		t.Fatalf("topics %v", got)
	}
}

func TestANoteRacingAFiredTimerKeepsTheTrailingDebounce(t *testing.T) {
	b := startBroker(t)
	clk := &raceClock{now: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)}
	base := clk.now.UnixNano()
	pub := newSyncEventPublisher(b, func() bool { return false },
		func(context.Context) (int64, bool, error) { return base, true, nil },
		clk, libraryChangeDebounce, true)
	pub.NoteLibrary(base)
	if len(clk.tasks) != 1 || clk.tasks[0].fn == nil {
		t.Fatal("the first note did not arm a timer")
	}
	clk.tasks[0].fired = true
	pub.NoteLibrary(base + 1)
	if len(clk.tasks) != 2 {
		t.Fatalf("timers %d, want the racing note to arm a second", len(clk.tasks))
	}
	clk.tasks[0].fn()
	pub.NoteLibrary(base + 2)
	pub.NoteLibrary(base + 3)
	if !clk.tasks[1].stopped {
		t.Fatal("the timer armed by the racing note was not stopped")
	}
}

func TestAnEmptyLibraryStillPublishesAtScanEnd(t *testing.T) {
	b := startBroker(t)
	when := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	clk := &manualClock{now: when}
	pub := newSyncEventPublisher(b, func() bool { return false },
		func(context.Context) (int64, bool, error) { return 0, false, nil },
		clk, libraryChangeDebounce, true)
	pub.ScanEnded(context.Background())
	awaitReplay(t, b, 1)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.replayBuffer) != 1 {
		t.Fatalf("events %d", len(b.replayBuffer))
	}
	var got libraryChanged
	if err := json.Unmarshal(b.replayBuffer[0].Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.IndexedAt != formatIndexedAt(when.UnixNano()) {
		t.Fatalf("indexedAt %q, want the scan-end clock", got.IndexedAt)
	}
}

func TestAWatermarkQueryErrorPublishesTheScanEndClock(t *testing.T) {
	rec := loggingtest.Record(t)
	b := startBroker(t)
	when := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	clk := &manualClock{now: when}
	pub := newSyncEventPublisher(b, func() bool { return false },
		func(context.Context) (int64, bool, error) { return 0, false, errors.New("db") },
		clk, libraryChangeDebounce, true)
	pub.ScanEnded(context.Background())
	awaitReplay(t, b, 1)
	b.mu.Lock()
	var got libraryChanged
	err := json.Unmarshal(b.replayBuffer[0].Data, &got)
	b.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got.IndexedAt != formatIndexedAt(when.UnixNano()) {
		t.Fatalf("indexedAt %q, want the scan-end clock", got.IndexedAt)
	}
	if lines := rec.Failures("library watermark"); len(lines) != 1 {
		t.Fatalf("watermark log %v", lines)
	}
}

func TestAScanEndPublishesTheWatermarkWhenTheLibraryHoldsOne(t *testing.T) {
	b := startBroker(t)
	when := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	wm := when.Add(time.Hour).UnixNano()
	clk := &manualClock{now: when}
	pub := newSyncEventPublisher(b, func() bool { return false },
		func(context.Context) (int64, bool, error) { return wm, true, nil },
		clk, libraryChangeDebounce, true)
	pub.ScanEnded(context.Background())
	awaitReplay(t, b, 1)
	b.mu.Lock()
	defer b.mu.Unlock()
	var got libraryChanged
	if err := json.Unmarshal(b.replayBuffer[0].Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.IndexedAt != formatIndexedAt(wm) {
		t.Fatalf("indexedAt %q, want the watermark", got.IndexedAt)
	}
}
