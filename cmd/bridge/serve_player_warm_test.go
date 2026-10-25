package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/dlna/ssdptest"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
	"github.com/acoseac/1-bit-bridge/internal/player/lan/lantest"
	"github.com/acoseac/1-bit-bridge/internal/player/library"
)

// The lines of the library index's build ahead of the first lookup: the
// resolver's (library.Resolver.Warm) and the player's (joinWarm).
const (
	warmBuiltLine     = "player: the library index is built ahead of the first lookup"
	warmStoppedLine   = "player: the library index's build ahead of the first lookup stopped before it was done; the next lookup builds the rest"
	warmAbandonedLine = "player: the library index's build ahead of the first lookup did not stop within its bound: a read of the index is held; it is abandoned and ends with the process"
)

// heldIndex is the library index serve's player reads
// (servePlayerSeams.store): the manifest store, but for its first read of
// the paths, the build of the resolver's index, which it holds until letGo,
// or until the read's context ends, which it records; with pastCancel, until
// the test ends, whatever the context says, as a read held in the kernel
// is. It keeps where each read of the paths began (after).
type heldIndex struct {
	library.Store
	pastCancel bool
	ended      <-chan struct{}
	held       chan struct{}
	release    chan struct{}
	letGoOnce  sync.Once

	mu        sync.Mutex
	afters    []int64
	cancelled bool
}

func newHeldIndex(t *testing.T) *heldIndex {
	s := &heldIndex{ended: t.Context().Done(), held: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(s.letGo)
	return s
}

// wrap is servePlayerSeams.store.
func (s *heldIndex) wrap(st library.Store) library.Store {
	s.Store = st
	return s
}

// letGo lets the held build go on.
func (s *heldIndex) letGo() { s.letGoOnce.Do(func() { close(s.release) }) }

func (s *heldIndex) TrackPathsIndexedAfter(ctx context.Context, after int64, fn func(string, int64) error) error {
	s.mu.Lock()
	s.afters = append(s.afters, after)
	first := len(s.afters) == 1
	s.mu.Unlock()
	if first {
		close(s.held)
		if s.pastCancel {
			<-s.ended
			return errors.New("the test ended")
		}
		select {
		case <-s.release:
		case <-ctx.Done():
			s.mu.Lock()
			s.cancelled = true
			s.mu.Unlock()
			return ctx.Err()
		case <-s.ended:
			return errors.New("the test ended")
		}
	}
	return s.Store.TrackPathsIndexedAfter(ctx, after, fn)
}

// awaitHeld waits for the player to begin the build, which the index holds.
func (s *heldIndex) awaitHeld(t *testing.T) {
	t.Helper()
	select {
	case <-s.held:
	case <-serveGiveUp(t):
		t.Fatal("the player began no build of its library's index")
	}
}

// counts are the reads of the paths, the builds among them (a read from where
// the first, the held build, began: the first row), and whether the held
// build's context ended.
func (s *heldIndex) counts() (reads, builds int, cancelled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.afters {
		if a == s.afters[0] {
			builds++
		}
	}
	return len(s.afters), builds, s.cancelled
}

// TestServeBuildsThePlayersLibraryIndexBesideItsSurface pins serve's warm of
// the library's index (B470 PR 5a4, CodeRabbit on #1347): the player begins
// the build as it starts its OpenHome surface, before any request, and
// neither the surface's listener nor a read waits for it, the build held
// meanwhile; once the build is done, the first Insert of a track the library
// holds is taken, and its lookup builds nothing: it reads the library from
// where the build left it, never from the first row again.
func TestServeBuildsThePlayersLibraryIndexBesideItsSurface(t *testing.T) {
	needsDirectorySync(t)
	logs := watchLog(t)
	const rel, id = "Artist/Album (2020)/01 Track One.flac", "b5b4c8dc9764772e"
	t.Cleanup(advertise.SetInterfacesForTest([]advertise.InterfaceForTest{{Name: "eth9",
		Flags: net.FlagUp | net.FlagBroadcast | net.FlagMulticast | net.FlagRunning, Addrs: []string{"192.168.7.7"}}}))
	root := t.TempDir()
	file := filepath.Join(root, "Music", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedTrack(t, filepath.Join(root, "data"), rel, 3)
	idx := newHeldIndex(t)
	box := lantest.NewMemory(t)
	addr := lantest.Address("192.168.1.20/24", "eth0", 2)
	box.Set([]lan.Address{addr})
	ps, started := bootDiscoveryServe(t, root, false, goBarPlayer+"    openhome: true\n    openhomePort: 0\n", "linux", box, ssdptest.New(),
		func(o *serveOpts) { o.playerSeams = &servePlayerSeams{store: idx.wrap} })
	idx.awaitHeld(t)
	s := discoverySurface(t, started)
	var at netip.AddrPort
	for _, l := range s.Status().Listening {
		if l.Address == addr {
			at = l.Bound
		}
	}
	if !at.IsValid() {
		t.Fatalf("the surface does not listen on %s while the build is held: %+v", addr.Prefix, s.Status())
	}
	client := box.Client(netip.MustParseAddr("192.168.1.50"))
	soap := func(action, args string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "http://"+at.String()+"/openhome/playlist/control",
			strings.NewReader(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:`+action+
				` xmlns:u="urn:av-openhome-org:service:Playlist:1">`+args+`</u:`+action+`></s:Body></s:Envelope>`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
		req.Header.Set("SOAPAction", `"urn:av-openhome-org:service:Playlist:1#`+action+`"`)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		return res.StatusCode, string(raw)
	}
	if code, body := soap("TransportState", ""); code != http.StatusOK || !strings.Contains(body, "<Value>Stopped</Value>") {
		t.Errorf("TransportState while the build is held: %d %s; want the engine's, Stopped", code, body)
	}
	if reads, _, _ := idx.counts(); reads != 1 {
		t.Errorf("%d reads of the library's paths while its build is held, want the build's alone", reads)
	}
	idx.letGo()
	logs.await(t, warmBuiltLine, "paths=1")
	uri := "http://192.168.7.7:7790/dlna/file/" + id
	if code, body := soap("Insert", "<AfterId>0</AfterId><Uri>"+uri+"</Uri><Metadata></Metadata>"); code != http.StatusOK || !strings.Contains(body, "<NewId>") {
		t.Errorf("the first Insert once the index is built: %d %s; want it taken", code, body)
	}
	if reads, builds, _ := idx.counts(); reads < 2 || builds != 1 {
		t.Errorf("the library's paths were read %d times, %d of them from the first row; want the build's read from it and the Insert's from where it ended", reads, builds)
	}
	stopDiscoveryServe(t, ps)
}

// TestServeStopEndsThePlayersLibraryIndexBuild pins the build's context: it
// ends with the player's, serve's stop, and with the player's own stop when
// its engine stopped on its own (a panic on the goroutine that runs it): the
// held read sees its context end, the build says it stopped, and the
// player's shutdown waits for it before it says it is shut down, with no
// build abandoned.
func TestServeStopEndsThePlayersLibraryIndexBuild(t *testing.T) {
	needsDirectorySync(t)
	for _, tc := range []struct {
		what string
		// engineRuns is the seam the engine's goroutine runs, before Run.
		engineRuns func(idx *heldIndex)
	}{
		{"by serve's stop", nil},
		{"by the player's stop, its engine stopped on its own", func(idx *heldIndex) {
			<-idx.held
			panic("on the engine's goroutine")
		}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			rec := loggingtest.Record(t)
			idx := newHeldIndex(t)
			box := newOpenHomeBox(t)
			seams := &servePlayerSeams{store: idx.wrap}
			if tc.engineRuns != nil {
				seams.runs = func() { tc.engineRuns(idx) }
			}
			ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + "    openhome: true\n", standIn: newPlayerStandIn(),
				files: map[string][]byte{"a.flac": []byte("a")}, seams: seams,
				with: func(o *serveOpts) { o.playerOpenHome = box.wiring() }})
			idx.awaitHeld(t)
			if tc.engineRuns != nil {
				select {
				case <-ps.player.done:
				case <-serveGiveUp(t):
					t.Fatalf("the player did not stop after its engine's panic; stderr=%s", ps.stderr.String())
				}
				if lines := rec.Lines("player: a panic was recovered; the library is served as before"); len(lines) != 1 || !strings.Contains(lines[0], "the player's engine") {
					t.Errorf("the engine's panic logged %q, want once, on its goroutine", lines)
				}
			}
			if code := ps.stopServe(t); code != 0 {
				t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
			}
			if _, _, cancelled := idx.counts(); !cancelled {
				t.Error("the build's context did not end with the player's")
			}
			if lines := rec.Lines(warmStoppedLine); len(lines) != 1 || !strings.HasPrefix(lines[0], "INFO ") {
				t.Errorf("the build's stop logged %q, want once, at Info", lines)
			}
			if lines := rec.Lines(warmAbandonedLine); len(lines) != 0 {
				t.Errorf("the player abandoned the build: %q", lines)
			}
			order := map[string]int{}
			for i, l := range rec.All() {
				for _, msg := range []string{warmStoppedLine, "player: shut down"} {
					if _, seen := order[msg]; !seen && strings.Contains(l, msg) {
						order[msg] = i
					}
				}
			}
			if stopped, ok := order[warmStoppedLine]; !ok || stopped > order["player: shut down"] {
				t.Errorf("the lines came in the order %v: the build must stop before the player says it is shut down", order)
			}
		})
	}
}

// TestServeAbandonsALibraryIndexBuildHeldPastItsBound pins the player's wait
// for the build at its stop: a read of the index held past its context's
// end, as a read held in the kernel is, holds the player's stop for the
// build's bound alone (here 100 ms), and the player says it abandoned it;
// serve exits 0.
func TestServeAbandonsALibraryIndexBuildHeldPastItsBound(t *testing.T) {
	needsDirectorySync(t)
	rec := loggingtest.Record(t)
	idx := newHeldIndex(t)
	idx.pastCancel = true
	box := newOpenHomeBox(t)
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + "    openhome: true\n", standIn: newPlayerStandIn(),
		files: map[string][]byte{"a.flac": []byte("a")}, seams: &servePlayerSeams{store: idx.wrap},
		bounds: servePlayerBounds{warm: 100 * time.Millisecond},
		with:   func(o *serveOpts) { o.playerOpenHome = box.wiring() }})
	idx.awaitHeld(t)
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
	if lines := rec.Failures(warmAbandonedLine); len(lines) != 1 || !strings.Contains(lines[0], "bound=100ms") {
		t.Errorf("the abandonment logged %q, want once, naming its bound", lines)
	}
	if lines := rec.Lines("player: shut down"); len(lines) != 1 {
		t.Errorf("the player's shutdown logged %q, want it shut down once", lines)
	}
}
