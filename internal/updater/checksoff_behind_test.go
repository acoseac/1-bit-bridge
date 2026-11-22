package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The console's off of update checks behind another process's install, on
// the bridge's Windows service (backlog B831): that install's swap stops the
// service through the SCM first (swap_windows.go), which ends serve and every
// console request with it, so an off waiting for the install marker's lock
// ended with serve and the new version started with the checks on. While
// another open holds the lock, at its first answer and at every retry, the
// off is refused at once where this process is the bridge's Windows service
// and the install marker, read without the lock, is an install of another
// version that this process's updater did not arm (ChecksOffWait); everywhere
// else it waits as before. Every wait here is bounded from the test binary's
// deadline (behindBound), and every off is synchronized on the lock's held
// answers (heldTries), never on a sleep.

const (
	behindCurrent = "0.1.0"
	behindTarget  = "0.9.0"
	// behindOther is another process's install of another target than this
	// updater's own.
	behindOther = "0.8.0"
)

// behindBound is how long a wait in these tests may take: until the test
// binary's deadline less a reserve for the cleanups, or ten minutes where the
// binary has none.
func behindBound(t *testing.T) time.Duration {
	t.Helper()
	if d, ok := t.Deadline(); ok {
		return time.Until(d) - 30*time.Second
	}
	return 10 * time.Minute
}

// heldTries counts the answers the install marker's lock gives a try while
// another open holds it (SetStateTryLockForTest), says when the try of a
// given count answers, and can hold that answer back until the test lets it
// go, so a test arms a marker between two tries of one wait.
type heldTries struct {
	mu      sync.Mutex
	n       int
	reached map[int]chan struct{}
	holds   map[int]chan struct{}
	lets    []func()
}

// newHeldTries counts from now, for the rest of the test.
func newHeldTries(t *testing.T) *heldTries {
	t.Helper()
	h := &heldTries{reached: map[int]chan struct{}{}, holds: map[int]chan struct{}{}}
	t.Cleanup(SetStateTryLockForTest(h.try))
	// Registered after the seam's restore, so it runs first: no try is left
	// held back.
	t.Cleanup(h.letAll)
	return h
}

func (h *heldTries) try(f *os.File) error {
	err := fsutil.TryLock(f)
	if !errors.Is(err, fsutil.ErrLocked) {
		return err
	}
	h.mu.Lock()
	h.n++
	reached, hold := h.reached[h.n], h.holds[h.n]
	h.mu.Unlock()
	if reached != nil {
		close(reached)
	}
	if hold != nil {
		<-hold
	}
	return err
}

// reach is closed once the nth held answer is given (or, where holdAt holds
// it, about to be).
func (h *heldTries) reach(n int) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reached[n] == nil {
		h.reached[n] = make(chan struct{})
	}
	return h.reached[n]
}

// holdAt holds the nth held answer back until let is called, or the test
// ends; reached is closed once that try is held.
func (h *heldTries) holdAt(n int) (reached <-chan struct{}, let func()) {
	reached = h.reach(n)
	hold := make(chan struct{})
	var once sync.Once
	let = func() { once.Do(func() { close(hold) }) }
	h.mu.Lock()
	h.holds[n] = hold
	h.lets = append(h.lets, let)
	h.mu.Unlock()
	return reached, let
}

func (h *heldTries) letAll() {
	h.mu.Lock()
	lets := append([]func(){}, h.lets...)
	h.mu.Unlock()
	for _, let := range lets {
		let()
	}
}

func (h *heldTries) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// holdMarkerLock holds the install marker's lock in dir, as another process
// does, until the test lets it go or ends.
func holdMarkerLock(t *testing.T, dir string) *StateLock {
	t.Helper()
	lock, err := LockState(context.Background(), dir)
	if err != nil || !lock.Held() {
		t.Fatalf("hold the install marker's lock as another process does: %+v, %v", lock, err)
	}
	t.Cleanup(lock.Release)
	return lock
}

// armedOver is an install marker of target over behindCurrent, armed now and
// not yet swapped, as an install writes it just before its swap.
func armedOver(target string) State {
	return State{Status: "installing", TargetVersion: target, PreviousVersion: behindCurrent, AttemptedAt: time.Now().UTC()}
}

// armAnother writes another process's install marker of target into dir.
func armAnother(t *testing.T, dir, target string) {
	t.Helper()
	if err := SaveState(dir, armedOver(target)); err != nil {
		t.Fatal(err)
	}
}

// behindAnswer is what an off's LockForChecksOff answered.
type behindAnswer struct {
	lock *StateLock
	err  error
}

// offBehind takes the install marker's lock in dir for an off as the console
// does, on a goroutine of its own, waiting as w says.
func offBehind(ctx context.Context, dir string, w ChecksOffWait) <-chan behindAnswer {
	answered := make(chan behindAnswer, 1)
	go func() {
		lock, err := LockForChecksOff(ctx, dir, w)
		answered <- behindAnswer{lock, err}
	}()
	return answered
}

// answerBehind is the off's answer, failing the test past its bound; a lock
// the off took is let go at the test's end.
func answerBehind(t *testing.T, answered <-chan behindAnswer) behindAnswer {
	t.Helper()
	select {
	case a := <-answered:
		if a.lock != nil {
			t.Cleanup(a.lock.Release)
		}
		return a
	case <-time.After(behindBound(t)):
		t.Fatal("the off never answered")
		return behindAnswer{}
	}
}

// waitBehind waits for ch, failing the test past its bound, or at once where
// the off answers first.
func waitBehind(t *testing.T, ch <-chan struct{}, answered <-chan behindAnswer, what string) {
	t.Helper()
	select {
	case <-ch:
	case a := <-answered:
		if a.lock != nil {
			a.lock.Release()
		}
		t.Fatalf("the off answered %v (a lock %v) before %s", a.err, a.lock != nil, what)
	case <-time.After(behindBound(t)):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// requireRefusedBehind requires err to be the off's refusal behind another
// process's install of target, given at once (its own cause, never a wait's
// end), in words that name the target and no path below dir.
func requireRefusedBehind(t *testing.T, err error, target, dir string) {
	t.Helper()
	if !errors.Is(err, ErrChecksOffLocked) || !errors.Is(err, ErrChecksOffBehindAnInstall) || errors.Is(err, ErrStateLockHeld) {
		t.Fatalf("the off behind another process's install of %s = %v, want ErrChecksOffLocked for "+
			"ErrChecksOffBehindAnInstall, refused at once, not at the end of the lock's wait", target, err)
	}
	msg := err.Error()
	for _, says := range []string{"update checks were not turned off", "an install of " + target + " by another process",
		"holds the install marker's lock", "replacing the bridge's binary", "can stop this service and this save with it",
		"nothing was saved", "once the bridge answers, turn the checks off again if they are on"} {
		if !strings.Contains(msg, says) {
			t.Errorf("the refusal does not say %q: %q", says, msg)
		}
	}
	if strings.Contains(msg, dir) {
		t.Errorf("the refusal names a path: %q", msg)
	}
}

// requireTookNothing requires the off to have taken nothing: once holder, the
// test's hold, lets go, another taker finds the lock free.
func requireTookNothing(t *testing.T, holder *StateLock, dir string) {
	t.Helper()
	if !holder.Held() {
		t.Fatal("the test's hold of the install marker's lock was let go before the off answered")
	}
	holder.Release()
	lock, err := LockState(context.Background(), dir)
	if err != nil || !lock.Held() {
		t.Fatalf("the install marker's lock after the refused off = %+v, %v; want it free", lock, err)
	}
	lock.Release()
}

// TestAnOffBehindAnotherProcesssInstallOnTheWindowsServiceIsRefusedAtOnce:
// another process holds the install marker's lock and has armed an install of
// another version, as `bridge update` does just before its swap; this process
// is the bridge's Windows service. The off is refused at the held answer the
// marker was there for: at the first, where the marker was armed before the
// off's first try, and at the first retry after the arming where it was armed
// once the off's wait had begun. The refusal is ErrChecksOffLocked's (the
// console's 409 marker-locked) with ErrChecksOffBehindAnInstall's cause, never
// the lock's wait ending (which answers ErrStateLockHeld after 30 s), in words
// that name the target; it is given while the other process still holds the
// lock, and takes nothing.
func TestAnOffBehindAnotherProcesssInstallOnTheWindowsServiceIsRefusedAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		// armedAt is the held answer the marker is armed before: 1, before the
		// off's first try; 2, between its first try and its first retry.
		armedAt int
	}{
		{"armed before the off's first try", 1},
		{"armed once the off's wait had begun", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			holder := holdMarkerLock(t, dir)
			tries := newHeldTries(t)
			if tc.armedAt == 1 {
				armAnother(t, dir, behindTarget)
			}
			held, let := tries.holdAt(2)
			answered := offBehind(context.Background(), dir, ChecksOffWait{Service: true, Current: behindCurrent, Updater: New(Options{})})
			if tc.armedAt == 2 {
				waitBehind(t, held, answered, "the off's first retry")
				armAnother(t, dir, behindTarget)
			}
			let()
			a := answerBehind(t, answered)
			if a.lock != nil {
				t.Fatalf("the off took the lock (held %v), want it refused", a.lock.Held())
			}
			requireRefusedBehind(t, a.err, behindTarget, dir)
			if n := tries.count(); n != tc.armedAt {
				t.Errorf("the off was refused after %d held answers, want %d: at the first after the marker's arming", n, tc.armedAt)
			}
			requireTookNothing(t, holder, dir)
		})
	}
}

// TestAnOffBehindTheInstallMarkersLockWaitsWhereItIsNoOtherProcesssInstall:
// another process holds the install marker's lock, and the off waits, takes
// the lock once it is let go, and answers no refusal, wherever the hold is no
// other process's install of another version on the bridge's Windows
// service: this process is not the service (a Startup-folder install, a
// foreground serve, every other platform); the marker is an install of the
// running version, an install confirmed, a rollback's record, one that does
// not parse (read without its Warn at every retry) or none; or it is an install
// this process's updater armed. The off is still waiting at its third held
// answer, so two inspections of the marker refused nothing.
func TestAnOffBehindTheInstallMarkersLockWaitsWhereItIsNoOtherProcesssInstall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		service bool
		// marker writes the marker in dir as the hold's holder left it; upd is
		// this process's updater.
		marker func(t *testing.T, dir string, upd *Updater)
	}{
		{"not the Windows service", false, func(t *testing.T, dir string, _ *Updater) { armAnother(t, dir, behindTarget) }},
		{"an install of the running version", true, func(t *testing.T, dir string, _ *Updater) { armAnother(t, dir, behindCurrent) }},
		{"an install confirmed", true, func(t *testing.T, dir string, _ *Updater) {
			st := armedOver(behindTarget)
			st.Status, st.InstalledAt = "installed", time.Now().UTC()
			if err := SaveState(dir, st); err != nil {
				t.Fatal(err)
			}
		}},
		{"a rollback's record", true, func(t *testing.T, dir string, _ *Updater) {
			st := armedOver(behindTarget)
			st.Status, st.RollbackStarted, st.SwapStarted = "", true, true
			if err := SaveState(dir, st); err != nil {
				t.Fatal(err)
			}
		}},
		{"a marker that does not parse", true, func(t *testing.T, dir string, _ *Updater) {
			if err := os.WriteFile(StatePath(dir), []byte(`{"status": "installing", "targetVersion": `), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"no marker", true, func(*testing.T, string, *Updater) {}},
		{"an install this process's updater armed", true, func(t *testing.T, dir string, upd *Updater) {
			st := armedOver(behindTarget)
			upd.noteArmed(st)
			if err := SaveState(dir, st); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := loggingtest.Record(t)
			dir := t.TempDir()
			upd := New(Options{})
			holder := holdMarkerLock(t, dir)
			tc.marker(t, dir, upd)
			tries := newHeldTries(t)
			third := tries.reach(3)
			answered := offBehind(context.Background(), dir, ChecksOffWait{Service: tc.service, Current: behindCurrent, Updater: upd})
			waitBehind(t, third, answered, "the off's third held answer")
			holder.Release()
			a := answerBehind(t, answered)
			if a.err != nil || !a.lock.Held() {
				t.Fatalf("the off once the lock was let go = %v (a lock %v), want it to take the lock", a.err, a.lock != nil)
			}
			if lines := logs.Carrying("update-state.json"); len(lines) != 0 {
				t.Errorf("the off's wait logged its reads of the marker: %q", lines)
			}
		})
	}
}

// TestAStopBeforeTheOffsRefusalKeepsItsError: this process is the bridge's
// Windows service and another process's install holds the install marker's
// lock, so the off would be refused; a stop it observes first keeps the
// stop's own answer, ctx's error, which the console says as 503 stopping in
// words that claim no cause. Already stopped, the off asks nothing of the
// marker; stopped while it reads the marker, it answers the stop.
func TestAStopBeforeTheOffsRefusalKeepsItsError(t *testing.T) {
	for _, tc := range []struct {
		name string
		// during is whether the stop comes during the marker's read, or before
		// the off.
		during bool
	}{
		{"a request stopped before it", false},
		{"a stop during the marker's inspection", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			holder := holdMarkerLock(t, dir)
			armAnother(t, dir, behindTarget)
			tries := newHeldTries(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var read atomic.Int32
			w := ChecksOffWait{Service: true, Current: behindCurrent, Updater: New(Options{})}
			w.read = func(string) State {
				read.Add(1)
				if tc.during {
					cancel()
				}
				return armedOver(behindTarget)
			}
			if !tc.during {
				cancel()
			}
			a := answerBehind(t, offBehind(ctx, dir, w))
			if a.lock != nil || !errors.Is(a.err, context.Canceled) || errors.Is(a.err, ErrChecksOffLocked) {
				t.Fatalf("the stopped off = %v (a lock %v), want the stop's own error and no refusal", a.err, a.lock != nil)
			}
			if want := map[bool]int32{false: 0, true: 1}[tc.during]; read.Load() != want {
				t.Errorf("the off read the marker %d times, want %d", read.Load(), want)
			}
			if n := tries.count(); n != 1 {
				t.Errorf("the stopped off asked for the lock %d times while it was held, want 1", n)
			}
			requireTookNothing(t, holder, dir)
		})
	}
}

// TestARestampKeepsWhatThisUpdaterArmed: the auto-installer's restamp of an
// install marker before its deferred restart (refreshInstallAttemptedAt)
// gives the marker a new time; a marker this updater armed stays its own
// through it (Armed), and another process's does not become its own.
func TestARestampKeepsWhatThisUpdaterArmed(t *testing.T) {
	for _, own := range []bool{true, false} {
		t.Run(map[bool]string{true: "this updater's", false: "another process's"}[own], func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(t.TempDir(), "bridge")
			if err := os.WriteFile(bin, []byte("bridge-binary"), 0o755); err != nil {
				t.Fatal(err)
			}
			upd := New(Options{})
			upd.autoInstallOpts = &InstallOptions{DataDir: dir, BinaryPath: bin}
			st := armedOver(behindTarget)
			st.AttemptedAt = st.AttemptedAt.Add(-time.Hour)
			if own {
				upd.noteArmed(st)
			}
			if err := SaveState(dir, st); err != nil {
				t.Fatal(err)
			}
			if err := upd.refreshInstallAttemptedAt(context.Background()); err != nil {
				t.Fatal(err)
			}
			restamped, err := LoadState(dir)
			if err != nil || restamped.AttemptedAt.Equal(st.AttemptedAt) {
				t.Fatalf("the marker after the restamp = %+v, %v; want a new time", restamped, err)
			}
			if got := upd.Armed(restamped); got != own {
				t.Errorf("Armed(the restamped marker) = %v, want %v", got, own)
			}
		})
	}
}

// behindRelease is a release server for an install of target over this OS's
// archive (a zip holding bridge.exe on Windows, a tar.gz holding bridge
// elsewhere), whose archive's download can be held.
type behindRelease struct {
	srv *httptest.Server
	// held, where set, holds each download of the archive, closing reached
	// once one arrives, until the download's request ends or release closes.
	held    atomic.Bool
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBehindRelease(t *testing.T, target string) *behindRelease {
	t.Helper()
	osName, ext, binary := runtime.GOOS, ".tar.gz", "bridge"
	if osName == "darwin" {
		osName = "macos"
	}
	if runtime.GOOS == "windows" {
		ext, binary = ".zip", "bridge.exe"
	}
	archiveName := fmt.Sprintf("1-bit-bridge_%s_%s_%s%s", target, osName, runtime.GOARCH, ext)
	body := []byte("bridge-binary-" + target)
	var archive bytes.Buffer
	if ext == ".zip" {
		zw := zip.NewWriter(&archive)
		w, err := zw.Create(binary)
		if err == nil {
			_, err = w.Write(body)
		}
		if err == nil {
			err = zw.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
	} else {
		gz := gzip.NewWriter(&archive)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: binary, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256(archive.Bytes())
	r := &behindRelease{reached: make(chan struct{}), release: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/asset/"+archiveName, func(w http.ResponseWriter, req *http.Request) {
		if r.held.Load() {
			r.once.Do(func() { close(r.reached) })
			select {
			case <-req.Context().Done():
				panic(http.ErrAbortHandler)
			case <-r.release:
			}
		}
		_, _ = w.Write(archive.Bytes())
	})
	mux.HandleFunc("/asset/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), archiveName)
	})
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	// Before the server's Close, which waits for every request.
	t.Cleanup(func() {
		select {
		case <-r.release:
		default:
			close(r.release)
		}
	})
	allowTestAssetHost(t, r.srv.URL)
	rel, err := json.Marshal(Release{TagName: "v" + target, HTMLURL: r.srv.URL + "/release/v" + target, Assets: []ReleaseAsset{
		{Name: archiveName, BrowserDownloadURL: r.srv.URL + "/asset/" + archiveName},
		{Name: "checksums.txt", BrowserDownloadURL: r.srv.URL + "/asset/checksums.txt"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("/repos/fake/repo/releases/latest", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(rel) })
	return r
}

// localInstall is this process's updater, behindCurrent, whose last check
// found target, installing over a binary in a directory of its own with the
// install marker in dir; its swap stands in for the platform's, landing at
// once.
type localInstall struct {
	upd  *Updater
	dir  string
	opts InstallOptions
}

func newLocalInstall(t *testing.T, rel *behindRelease, target string) *localInstall {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(t.TempDir(), "bridge")
	if err := os.WriteFile(bin, []byte("bridge-binary-"+behindCurrent), 0o755); err != nil {
		t.Fatal(err)
	}
	upd := New(Options{RepoOverride: "fake/repo", Client: NewClient("fake/repo", time.Minute).WithBaseURL(rel.srv.URL)})
	upd.mu.Lock()
	upd.status.CurrentVersion = behindCurrent
	upd.status.LatestVersion = target
	upd.status.UpdateAvailable = true
	upd.status.LastCheck = time.Now().UTC()
	upd.mu.Unlock()
	return &localInstall{upd: upd, dir: dir, opts: InstallOptions{DataDir: dir, BinaryPath: bin, Force: true,
		Verifier: func(context.Context, string) error { return nil },
		swap:     func(_, _, _ string, markSwapStarted func() error) error { return markSwapStarted() }}}
}

// install runs the local install on a goroutine of its own, on ctx, and
// hands back its answer.
func (l *localInstall) install(ctx context.Context) <-chan error {
	answered := make(chan error, 1)
	go func() {
		_, err := l.upd.Install(ctx, l.opts)
		answered <- err
	}()
	return answered
}

// TestAnOffBehindAnotherProcesssInstallIsRefusedWhateverThisProcessInstalls:
// this process's own installs make no other process's install its own
// (backlog B831's review: the marker it armed is told by its identity, its
// target and its time, never by a flag). While its install is downloading
// (in flight, nothing armed), another process's install of another target
// that holds the install marker's lock refuses the off at once; after its own
// swap, which waits for a restart, a hold of the lock over its own marker
// leaves the off waiting, until another process arms an install of a
// different target under it, which refuses the off at the next retry.
func TestAnOffBehindAnotherProcesssInstallIsRefusedWhateverThisProcessInstalls(t *testing.T) {
	t.Run("another process's install during this process's download", func(t *testing.T) {
		rel := newBehindRelease(t, behindTarget)
		rel.held.Store(true)
		l := newLocalInstall(t, rel, behindTarget)
		ctx, cancel := context.WithCancel(context.Background())
		installed := l.install(ctx)
		select {
		case <-rel.reached:
		case err := <-installed:
			t.Fatalf("this process's install answered %v before its download", err)
		case <-time.After(behindBound(t)):
			t.Fatal("this process's install never began its download")
		}
		if !l.upd.installInFlight.Load() {
			t.Fatal("premise: this process's install is not in flight during its download")
		}
		holder := holdMarkerLock(t, l.dir)
		armAnother(t, l.dir, behindOther)
		tries := newHeldTries(t)
		a := answerBehind(t, offBehind(context.Background(), l.dir, ChecksOffWait{Service: true, Current: behindCurrent, Updater: l.upd}))
		if a.lock != nil {
			t.Fatalf("the off took the lock (held %v), want it refused", a.lock.Held())
		}
		requireRefusedBehind(t, a.err, behindOther, l.dir)
		if n := tries.count(); n != 1 {
			t.Errorf("the off was refused after %d held answers, want 1", n)
		}
		requireTookNothing(t, holder, l.dir)
		cancel()
		select {
		case <-installed:
		case <-time.After(behindBound(t)):
			t.Fatal("this process's install never ended once stopped")
		}
	})
	t.Run("another process's install of another target after this process's swap", func(t *testing.T) {
		rel := newBehindRelease(t, behindTarget)
		l := newLocalInstall(t, rel, behindTarget)
		select {
		case err := <-l.install(context.Background()):
			if err != nil {
				t.Fatalf("this process's install = %v, want its swap", err)
			}
		case <-time.After(behindBound(t)):
			t.Fatal("this process's install never answered")
		}
		if st, err := LoadState(l.dir); err != nil || !st.SwapStarted || st.TargetVersion != behindTarget || !l.upd.swapPending.Load() {
			t.Fatalf("premise: the marker after this process's install = %+v (%v), swap pending %v; want %s swapped, awaiting its restart",
				st, err, l.upd.swapPending.Load(), behindTarget)
		}
		holder := holdMarkerLock(t, l.dir)
		tries := newHeldTries(t)
		third := tries.reach(3)
		held, let := tries.holdAt(4)
		answered := offBehind(context.Background(), l.dir, ChecksOffWait{Service: true, Current: behindCurrent, Updater: l.upd})
		waitBehind(t, third, answered, "the off's third held answer over this process's own swap")
		waitBehind(t, held, answered, "the off's fourth held answer")
		armAnother(t, l.dir, behindOther)
		let()
		a := answerBehind(t, answered)
		if a.lock != nil {
			t.Fatalf("the off took the lock (held %v), want it refused", a.lock.Held())
		}
		requireRefusedBehind(t, a.err, behindOther, l.dir)
		if n := tries.count(); n != 4 {
			t.Errorf("the off was refused after %d held answers, want 4: at the first after the other process's arming", n)
		}
		requireTookNothing(t, holder, l.dir)
	})
}
