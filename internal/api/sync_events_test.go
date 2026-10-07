package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// manualClock fires AfterFunc tasks when Advance moves now to the deadline.
// AfterFunc itself never runs the callback, so a publisher can arm a timer
// while holding its own lock.
type manualClock struct {
	now   time.Time
	tasks []*manualTask
}

type manualTask struct {
	deadline time.Time
	fn       func()
}

func (c *manualClock) Now() time.Time { return c.now }

func (c *manualClock) AfterFunc(d time.Duration, fn func()) func() {
	task := &manualTask{deadline: c.now.Add(d), fn: fn}
	c.tasks = append(c.tasks, task)
	return func() { task.fn = nil }
}

func (c *manualClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
	var due []func()
	for _, task := range c.tasks {
		if task.fn == nil || task.deadline.After(c.now) {
			continue
		}
		due = append(due, task.fn)
		task.fn = nil
	}
	for _, fn := range due {
		fn()
	}
}

func startBroker(t *testing.T) *eventBroker {
	t.Helper()
	b := newEventBroker()
	t.Cleanup(b.Start())
	return b
}

func replayTopics(t *testing.T, b *eventBroker) []string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.replayBuffer))
	for i, env := range b.replayBuffer {
		out[i] = env.Topic
	}
	return out
}

func awaitReplay(t *testing.T, b *eventBroker, n int) {
	t.Helper()
	awaitTrue(t, 500*time.Millisecond, "broker did not record the events", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.replayBuffer) >= n
	})
}

func TestLibraryChangedIsOneEventAfterAQuietThirtySeconds(t *testing.T) {
	b := startBroker(t)
	clk := &manualClock{}
	ns := time.Date(2026, 10, 7, 10, 15, 4, 0, time.UTC).UnixNano()
	pub := newSyncEventPublisher(b, func() bool { return false }, nil, clk, libraryChangeDebounce, true)
	pub.NoteLibrary(ns - 1)
	pub.NoteLibrary(ns - 1)
	pub.NoteLibrary(ns)
	clk.Advance(29 * time.Second)
	if got := replayTopics(t, b); len(got) != 0 {
		t.Fatalf("topics before the quiet period: %v", got)
	}
	clk.Advance(time.Second)
	awaitReplay(t, b, 1)
	got := replayTopics(t, b)
	if len(got) != 1 || got[0] != topicLibraryChanged {
		t.Fatalf("topics %v", got)
	}
	b.mu.Lock()
	data := append([]byte(nil), b.replayBuffer[0].Data...)
	b.mu.Unlock()
	var payload libraryChanged
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.IndexedAt != "2026-10-07T10:15:04.000Z" {
		t.Fatalf("indexedAt %q", payload.IndexedAt)
	}
}

func TestLibraryChangedIsSuppressedWhileAScanRunsAndEmittedOnceAtTheEnd(t *testing.T) {
	b := startBroker(t)
	clk := &manualClock{}
	scanning := true
	wm := time.Date(2026, 10, 7, 10, 15, 4, 0, time.UTC).UnixNano()
	pub := newSyncEventPublisher(b,
		func() bool { return scanning },
		func(context.Context) (int64, bool) { return wm, true },
		clk, libraryChangeDebounce, true)
	pub.NoteLibrary(wm)
	clk.Advance(time.Minute)
	if got := replayTopics(t, b); len(got) != 0 {
		t.Fatalf("mid-scan topics %v", got)
	}
	scanning = false
	pub.ScanEnded(context.Background())
	awaitReplay(t, b, 1)
	clk.Advance(time.Minute)
	awaitReplay(t, b, 1)
	if got := replayTopics(t, b); len(got) != 1 || got[0] != topicLibraryChanged {
		t.Fatalf("topics %v", got)
	}
}

func TestAScanEndSupersedesAPendingLibraryDebounce(t *testing.T) {
	b := startBroker(t)
	clk := &manualClock{}
	wm := int64(1_700_000_000_000_000_000)
	pub := newSyncEventPublisher(b, func() bool { return false },
		func(context.Context) (int64, bool) { return wm, true },
		clk, libraryChangeDebounce, true)
	pub.NoteLibrary(wm)
	pub.ScanEnded(context.Background())
	awaitReplay(t, b, 1)
	clk.Advance(time.Minute)
	// Publish hands the broker a goroutine. Wait out one turn so a debounce
	// ScanEnded failed to cancel is in the buffer before the count.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) && len(replayTopics(t, b)) < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := replayTopics(t, b); len(got) != 1 {
		t.Fatalf("topics %v, the pending debounce also fired", got)
	}
}

func TestAFullScanPublishesOneLibraryEvent(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "lib")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.flac"), []byte("not-a-real-flac"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sc := manifest.NewScanner([]string{root}, store, "")
	b := startBroker(t)
	var during int
	pub := NewSyncEventPublisher(b, sc.IsScanning, store.LibraryWatermark)
	hooks := pub.Hooks()
	store.SetSyncHooks(manifest.SyncHooks{
		Library: func(ns int64) {
			if sc.IsScanning() {
				during++
			}
			hooks.Library(ns)
		},
	})
	sc.SetLibraryScanEnded(func() { pub.ScanEnded(context.Background()) })
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if during == 0 {
		t.Fatal("the scan wrote no library note while it was running")
	}
	awaitReplay(t, b, 1)
	if got := replayTopics(t, b); len(got) != 1 || got[0] != topicLibraryChanged {
		t.Fatalf("topics %v (notes during the scan: %d)", got, during)
	}
}

func TestADisabledPublisherEmitsNoneOfTheThreeTopics(t *testing.T) {
	b := startBroker(t)
	clk := &manualClock{}
	pub := newSyncEventPublisher(b, func() bool { return false },
		func(context.Context) (int64, bool) { return 5, true },
		clk, libraryChangeDebounce, false)
	pub.FavoritesChanged(8)
	pub.PlaylistsChanged("abc")
	pub.NoteLibrary(5)
	pub.ScanEnded(context.Background())
	clk.Advance(time.Minute)
	if got := replayTopics(t, b); len(got) != 0 {
		t.Fatalf("topics %v", got)
	}
}

func TestSyncTopicsReplayOnLastEventID(t *testing.T) {
	b := startBroker(t)
	clk := &manualClock{now: time.Unix(0, 0)}
	pub := newSyncEventPublisher(b, func() bool { return false }, nil, clk, libraryChangeDebounce, true)
	b.Publish("upscale.stats", map[string]int{"n": 1})
	awaitReplay(t, b, 1)
	b.mu.Lock()
	marker := b.replayBuffer[0].ID
	b.mu.Unlock()
	pub.FavoritesChanged(8)
	pub.PlaylistsChanged("7f3c1a9e4b20d8aa11c0e6f54d2b7091")
	pub.NoteLibrary(time.Date(2026, 10, 7, 10, 15, 4, 0, time.UTC).UnixNano())
	clk.Advance(libraryChangeDebounce)
	awaitReplay(t, b, 4)
	_, replay, err := b.subscribe(nil, marker, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay) != 3 {
		t.Fatalf("replay %d, want the three topics", len(replay))
	}
	want := []string{topicFavoritesChanged, topicPlaylistsChanged, topicLibraryChanged}
	for i, env := range replay {
		if env.Topic != want[i] || env.ID == "" {
			t.Fatalf("replay[%d] topic %q id %q", i, env.Topic, env.ID)
		}
	}
	var fav favoritesChanged
	if err := json.Unmarshal(replay[0].Data, &fav); err != nil || fav.Revision != 8 {
		t.Fatalf("favorites payload %s %v", replay[0].Data, err)
	}
	var pl playlistsChanged
	if err := json.Unmarshal(replay[1].Data, &pl); err != nil || pl.Epoch != "7f3c1a9e4b20d8aa11c0e6f54d2b7091" {
		t.Fatalf("playlists payload %s %v", replay[1].Data, err)
	}
}

func TestADemoBridgeDoesNotAdvertiseSyncEvents(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{t.TempDir()},
		ListenAddress: ":7788",
		LibraryName:   "T",
	}
	authStore, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, authStore, nil, "fp").WithDemoMode(true)
	t.Cleanup(srv.StartEventBroker())
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	resp := authGet(t, hs, "/v1/health", "")
	body := readAllOrFail(t, resp)
	resp.Body.Close()
	var got HealthResponse
	if err := jsonUnmarshalForTest(body, &got); err != nil {
		t.Fatal(err)
	}
	for _, f := range got.Features {
		if f == "syncEvents" {
			t.Fatalf("demo features include syncEvents: %v", got.Features)
		}
	}
}
