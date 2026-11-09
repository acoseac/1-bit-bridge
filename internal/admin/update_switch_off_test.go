package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The console's off of update checks (backlog B761's round 3): a settings
// PATCH that turns the switch off asks Deps.UpdateChecksOffSave first, before
// the console's lock and the holder's turn, and lets its hold go only once the
// save is published and the updater told; its refusal answers the PATCH whole,
// nothing saved, the live settings as they were and the updater told nothing.

// offSaveWatch stands in for serve's Deps.UpdateChecksOffSave and its
// UpdateChecksSwitched, recording, in order, what the PATCH did with them and
// what held at each step; refuse, when set, is what the ask answers.
type offSaveWatch struct {
	srv    *Server
	refuse error

	mu     sync.Mutex
	events []string
	asks   int
}

func watchOffSave(srv *Server) *offSaveWatch {
	w := &offSaveWatch{srv: srv}
	srv.deps.UpdateChecksSwitched = func() { w.note("the updater told") }
	srv.deps.UpdateChecksOffSave = w.ask
	return w
}

func (w *offSaveWatch) note(event string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, event)
}

// consolesLockFree says whether the console's lock is free, taking and
// letting it go where it is.
func (w *offSaveWatch) consolesLockFree() bool {
	if !w.srv.mu.TryLock() {
		return false
	}
	w.srv.mu.Unlock()
	return true
}

func (w *offSaveWatch) ask(context.Context) (func(), error) {
	w.mu.Lock()
	w.asks++
	w.mu.Unlock()
	if !w.consolesLockFree() {
		w.note("asked with the console's lock held")
	}
	if !w.srv.deps.CfgHolder.Load().Update.EffectiveCheckEnabled() {
		w.note("asked with the switch already off")
	}
	if w.refuse != nil {
		w.note("refused")
		return nil, w.refuse
	}
	w.note("the lock taken")
	return func() {
		if !w.consolesLockFree() {
			w.note("let go with the console's lock held")
		}
		w.note("the lock let go")
	}, nil
}

func (w *offSaveWatch) seen() (events []string, asks int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.events...), w.asks
}

// TestAnOffOfUpdateChecksIsDecidedUnderTheInstallMarkersLockFirst drives the
// real settings handler: a PATCH that turns update checks off, beside another
// field, asks UpdateChecksOffSave once, with the console's lock free and the
// switch still on, saves the whole PATCH, tells the updater, and lets the hold
// go after it, the console's lock free again; a PATCH that turns them on, one
// that sends the off they hold, and one that does not name the switch ask
// nothing.
func TestAnOffOfUpdateChecksIsDecidedUnderTheInstallMarkersLockFirst(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	w := watchOffSave(srv)
	var resp settingsPatchResponse
	if code := doJSON(t, srv.Handler(), http.MethodPatch, "/api/settings",
		map[string]any{"libraryName": "After", "updateCheckEnabled": false}, &resp); code != http.StatusOK ||
		resp.Fields["updateCheckEnabled"].Status != applyLive {
		t.Fatalf("PATCH off = %d %+v, want 200 and live", code, resp.Fields["updateCheckEnabled"])
	}
	events, asks := w.seen()
	if got, want := strings.Join(events, ", "), "the lock taken, the updater told, the lock let go"; asks != 1 || got != want {
		t.Errorf("the off asked %d time(s) and did %q, want once: %q", asks, got, want)
	}
	if v, there := fileUpdateCheckEnabled(t, cfgPath); !there || v != false {
		t.Errorf("the file holds update.checkEnabled = %v (there %v), want false", v, there)
	}
	if got := srv.deps.CfgHolder.Load().LibraryName; got != "After" {
		t.Errorf("the library name beside the off = %q, want it saved with it", got)
	}

	for _, body := range []map[string]any{
		{"updateCheckEnabled": false},
		{"libraryName": "Again"},
		{"updateCheckEnabled": true},
	} {
		if code := doJSON(t, srv.Handler(), http.MethodPatch, "/api/settings", body, nil); code != http.StatusOK {
			t.Fatalf("PATCH %v = %d, want 200", body, code)
		}
	}
	if _, asks := w.seen(); asks != 1 {
		t.Errorf("the off was asked %d time(s) over PATCHes that turn nothing off, want only the first off's", asks)
	}
}

// wordedRefusal is a refusal of UpdateChecksOffSave as serve's wraps the
// updater's: its words, and the console's sentinel it matches.
type wordedRefusal struct {
	sentinel error
	words    string
}

func (e *wordedRefusal) Error() string        { return e.words }
func (e *wordedRefusal) Is(target error) bool { return target == e.sentinel }

// TestAnOffTheInstallMarkersLockOrItsDecisionRefusesSavesNothing: an off whose
// ask is refused answers the PATCH whole, with the refusal's code and words:
// the install marker's lock that could not be held (409 marker-locked), the
// crash rollback the off would keep from its backup (409
// update-checks-off-refused), a backup that did not answer (409
// update-checks-off-unchecked), a wait the request's end stopped (503
// stopping, saying an off waiting for an install's swap on Windows is lost
// with the stop), and anything else (500 save-config, its error in the log
// alone). Nothing is saved, the other field beside it included, the live
// settings stay as they were and the updater is told nothing.
func TestAnOffTheInstallMarkersLockOrItsDecisionRefusesSavesNothing(t *testing.T) {
	const secret = "/srv/private/update-state.lock"
	for _, tc := range []struct {
		name         string
		refuse       error
		status       int
		code         string
		says         []string
		logged, hide string
	}{
		{name: "the install marker's lock cannot be held", refuse: &wordedRefusal{ErrUpdateChecksOffLocked,
			"update checks were not turned off: another process held the install marker's lock for 30s; nothing was saved"},
			status: http.StatusConflict, code: "marker-locked", says: []string{"held the install marker's lock", "nothing was saved"}},
		{name: "the crash rollback would keep its backup", refuse: &wordedRefusal{ErrUpdateChecksOffRefused,
			"update checks were not turned off: 0.2.4 is on probation since its install; nothing was saved"},
			status: http.StatusConflict, code: "update-checks-off-refused", says: []string{"on probation", "nothing was saved"}},
		{name: "the backup did not answer", refuse: &wordedRefusal{ErrUpdateChecksOffUnchecked,
			"update checks were not turned off: its backup did not answer; nothing was saved"},
			status: http.StatusConflict, code: "update-checks-off-unchecked", says: []string{"did not answer", "nothing was saved"}},
		{name: "the wait was stopped", refuse: context.Canceled, status: http.StatusServiceUnavailable, code: "stopping",
			says: []string{"stopped while it waited for an install's swap", "on Windows", "nothing was saved",
				"starts again with update checks as they were", "turn them off again"}},
		{name: "anything else", refuse: errors.New("the lock file " + secret + " could not be opened"),
			status: http.StatusInternalServerError, code: "save-config",
			says:   []string{"update checks were not turned off", "nothing was saved", "the bridge log says why"},
			logged: secret, hide: secret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := loggingtest.Record(t)
			srv, _, cfgPath := newTestServer(t)
			w := watchOffSave(srv)
			w.refuse = tc.refuse
			before := mustRead(t, cfgPath)
			var body struct{ Error, Message string }
			code := doJSON(t, srv.Handler(), http.MethodPatch, "/api/settings",
				map[string]any{"libraryName": "After", "updateCheckEnabled": false}, &body)
			if code != tc.status || body.Error != tc.code {
				t.Fatalf("PATCH off = %d %+v, want %d %s", code, body, tc.status, tc.code)
			}
			for _, says := range tc.says {
				if !strings.Contains(body.Message, says) {
					t.Errorf("the answer does not say %q: %q", says, body.Message)
				}
			}
			if tc.hide != "" && strings.Contains(body.Message, tc.hide) {
				t.Errorf("the answer carries the error's text: %q", body.Message)
			}
			if tc.logged != "" && len(logs.Carrying(tc.logged)) == 0 {
				t.Errorf("the error is not in the log whole: %q", logs.All())
			}
			if after := mustRead(t, cfgPath); !bytes.Equal(after, before) {
				t.Errorf("the refused off changed the file:\n%s", after)
			}
			live := srv.deps.CfgHolder.Load()
			if !live.Update.EffectiveCheckEnabled() || live.LibraryName != "Test Library" {
				t.Errorf("the live settings after the refused off: checks on %v, library name %q; want them as they were",
					live.Update.EffectiveCheckEnabled(), live.LibraryName)
			}
			if events, _ := w.seen(); strings.Join(events, ", ") != "refused" {
				t.Errorf("the refused off did %q, want only its refusal: no updater told, no hold to let go", events)
			}
		})
	}
}

// TestAnOffWhoseSwitchWasTurnedOnSinceItsLookIsRefused: a PATCH that turns
// update checks off looks at the switch before the console's lock; found off
// there, it asks nothing of the install marker's lock. Another save that turns
// the checks on while it waits for the console's lock would leave the turn an
// off decided under no lock: the turn refuses it, 409 update-checks-changed,
// nothing saved, the switch on and the updater told nothing of it.
func TestAnOffWhoseSwitchWasTurnedOnSinceItsLookIsRefused(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	off := false
	cfg := srv.deps.CfgHolder.Clone()
	cfg.Update.CheckEnabled = &off
	srv.deps.CfgHolder.Store(cfg)
	w := watchOffSave(srv)
	waits := make(chan struct{})
	var once sync.Once
	srv.testSettingsWriteWaits = func() { once.Do(func() { close(waits) }) }

	srv.mu.Lock()
	type answer struct {
		code int
		body struct{ Error, Message string }
	}
	answered := make(chan answer, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(`{"updateCheckEnabled":false}`))
		req.RemoteAddr, req.Host = "127.0.0.1:54321", testConsoleHost
		req.Header.Set("content-type", "application/json")
		rw := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rw, req)
		var a answer
		a.code = rw.Code
		_ = json.NewDecoder(rw.Body).Decode(&a.body)
		answered <- a
	}()
	select {
	case <-waits:
	case a := <-answered:
		srv.mu.Unlock()
		t.Fatalf("the PATCH answered %d %+v while the console's lock was held", a.code, a.body)
	case <-time.After(time.Minute):
		srv.mu.Unlock()
		t.Fatal("the PATCH never waited for the console's lock")
	}
	// Another save turns the checks on, through the holder, while the PATCH
	// waits: the key goes, as on writes it.
	if err := srv.deps.CfgHolder.UpdateChecked(cfgPath, func(next *config.Config, _ func() error) error {
		next.Update.CheckEnabled = nil
		return nil
	}); err != nil {
		srv.mu.Unlock()
		t.Fatal(err)
	}
	srv.mu.Unlock()
	var a answer
	select {
	case a = <-answered:
	case <-time.After(time.Minute):
		t.Fatal("the PATCH never answered once the console's lock was free")
	}
	if a.code != http.StatusConflict || a.body.Error != "update-checks-changed" || !strings.Contains(a.body.Message, "nothing was saved") {
		t.Errorf("the off whose switch was turned on since its look = %d %+v, want 409 update-checks-changed", a.code, a.body)
	}
	if v, there := fileUpdateCheckEnabled(t, cfgPath); there {
		t.Errorf("the file holds update.checkEnabled = %v, want no key: the other save's on", v)
	}
	if !srv.deps.CfgHolder.Load().Update.EffectiveCheckEnabled() {
		t.Error("the switch is off: the refused off was saved")
	}
	if events, asks := w.seen(); asks != 0 || len(events) != 0 {
		t.Errorf("the off asked %d time(s) and did %q, want nothing: it found the switch off at its look", asks, events)
	}
}
