package enrich

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// A busy MusicBrainz answers 503, then 429, then hangs past the client
// deadline. Each of those is transient, so the track stays unenriched.
// A later 200 is the answer that ends the streak.
const (
	busyUpstreamSleep = 800 * time.Millisecond
	busyClientTimeout = 200 * time.Millisecond
	logArtistName     = "Busy Artist"
	logAlbumName      = "Busy Album"
	logArtistBody     = `{"artists":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","score":100,"name":"Busy Artist"}]}`
)

// statusHang is a script step that sleeps instead of writing a status,
// so a short client timeout returns before the handler finishes.
const statusHang = -1

type upstreamRun struct {
	rec     *loggingtest.Recorder
	retried []string
	stamped []string
}

// runBusyUpstreams enriches six tracks. The first three carry local art,
// so the only MusicBrainz call is the artist search: 503, 429, then a
// timeout. The next two miss Cover Art Archive by timing out. The last
// one is a JPEG. MusicBrainz and Cover Art Archive are separate servers.
func runBusyUpstreams(t *testing.T) upstreamRun {
	t.Helper()
	mb := httptest.NewServer(scriptHandler(t, []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, statusHang, http.StatusOK}, logArtistBody))
	defer mb.Close()
	caa := httptest.NewServer(scriptHandler(t, []int{statusHang, statusHang, http.StatusOK}, ""))
	defer caa.Close()

	rig := newLogTestRig(t, mb, caa, &http.Client{Timeout: busyClientTimeout}, &http.Client{Timeout: busyClientTimeout})
	var retried, stamped []string
	for i, local := range []bool{true, true, true, false, false, false} {
		tr := logTrack(i, local)
		if rig.enrich(t, tr) {
			retried = append(retried, tr.Path)
		} else {
			stamped = append(stamped, tr.Path)
		}
	}
	return upstreamRun{retried: retried, stamped: stamped}
}

// logTestRig is the store and the enricher every case here drives, with
// MusicBrainz and Cover Art Archive at the two test servers.
type logTestRig struct {
	store *manifest.Store
	e     *Enricher
}

// newLogTestRig opens a store in a temp dir and an enricher over the two
// servers; a nil client takes the package default.
func newLogTestRig(t *testing.T, mb, caa *httptest.Server, mbClient, caaClient *http.Client) logTestRig {
	t.Helper()
	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	e := NewEnricher(store,
		NewMusicBrainzClient(mb.URL, "upstream-log-test", mbClient),
		NewCoverArtClient(caa.URL, "upstream-log-test", caaClient),
		nil, filepath.Join(dir, "art"))
	tuneForTest(e)
	return logTestRig{store: store, e: e}
}

// enrich upserts tr, enriches it once, and reports whether it was left
// unenriched for a later batch (retried) rather than stamped.
func (r logTestRig) enrich(t *testing.T, tr *manifest.Track) bool {
	t.Helper()
	ctx := context.Background()
	if err := r.store.UpsertTrack(ctx, tr); err != nil {
		t.Fatal(err)
	}
	r.e.enrichOne(ctx, tr)
	left, err := r.store.UnenrichedTracks(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	return unenrichedPath(left, tr.Path)
}

// artistThen answers MusicBrainz's artist search with logArtistBody and
// every other request with rest.
func artistThen(rest http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/artist") {
			io.WriteString(w, logArtistBody)
			return
		}
		rest(w, r)
	}
}

func TestABusyUpstreamWarnsOnceAndAnswersAtInfo(t *testing.T) {
	rec := loggingtest.Record(t)
	run := runBusyUpstreams(t)
	run.rec = rec

	mbWarn := linesAt(rec, slog.LevelWarn, "MB artist search")
	mbErr := linesAt(rec, slog.LevelError, "MB artist search")
	if len(mbErr) != 0 || len(mbWarn) != 1 {
		t.Fatalf("MusicBrainz artist search: %d error and %d warn, want 0 and 1\n%s", len(mbErr), len(mbWarn), strings.Join(rec.All(), "\n"))
	}
	artWarn := linesAt(rec, slog.LevelWarn, "artwork")
	artErr := linesAt(rec, slog.LevelError, "artwork")
	if len(artErr) != 0 || len(artWarn) != 1 {
		t.Fatalf("artwork: %d error and %d warn, want 0 and 1\n%s", len(artErr), len(artWarn), strings.Join(rec.All(), "\n"))
	}
	if got := answeredLines(rec, "musicbrainz"); len(got) != 1 {
		t.Fatalf("musicbrainz recovery: %d lines, want 1\n%s", len(got), strings.Join(rec.All(), "\n"))
	}
	if got := answeredLines(rec, "coverart"); len(got) != 1 {
		t.Fatalf("coverart recovery: %d lines, want 1\n%s", len(got), strings.Join(rec.All(), "\n"))
	}
}

func TestABusyMusicBrainzRetriesAndACoverMissStillStamps(t *testing.T) {
	run := runBusyUpstreams(t)
	if len(run.retried) != 3 || len(run.stamped) != 3 {
		t.Fatalf("retried %v stamped %v, want the three artist-search failures retried and the three artwork tracks stamped", run.retried, run.stamped)
	}
	for _, path := range run.retried {
		if !strings.Contains(path, "local") {
			t.Fatalf("retried %s, want only the local-art tracks", path)
		}
	}
}

func TestACleanMissIsDebugAndStillStamps(t *testing.T) {
	mb := httptest.NewServer(artistThen(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mb.Close()
	caa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer caa.Close()
	rig := newLogTestRig(t, mb, caa, nil, nil)

	rec := loggingtest.Record(t)
	for i := 0; i < 2; i++ {
		if rig.enrich(t, logTrack(10+i, false)) {
			t.Fatalf("clean miss %d left unenriched, want it stamped", i)
		}
	}
	for _, msg := range []string{"release-group lookup", "artwork"} {
		debug := linesAt(rec, slog.LevelDebug, msg)
		errs := linesAt(rec, slog.LevelError, msg)
		if len(errs) != 0 || len(debug) == 0 || len(debug) > 2 {
			t.Fatalf("%s: %d error and %d debug, want 0 error and 1 or 2 debug\n%s", msg, len(errs), len(debug), strings.Join(rec.All(), "\n"))
		}
	}
}

func TestAnUnexpectedUpstreamFailureStaysErrorAndStamps(t *testing.T) {
	mb := httptest.NewServer(artistThen(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "{")
	}))
	defer mb.Close()
	caa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 400 stays a plain error. 429 and 5xx are the transient status
		// and join the streak (TestARunOfTransientStatusesWarnsOnce).
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer caa.Close()
	rig := newLogTestRig(t, mb, caa, nil, nil)

	rec := loggingtest.Record(t)
	if rig.enrich(t, logTrack(20, false)) {
		t.Fatal("a non-transient artwork failure left the track unenriched")
	}
	if got := linesAt(rec, slog.LevelError, "artwork"); len(got) != 1 {
		t.Fatalf("artwork errors: %d, want 1\n%s", len(got), strings.Join(rec.All(), "\n"))
	}
}

// A run of 503s from Cover Art Archive and from Deezer is one Warn
// each. The response is transient, so it does not end the streak, and
// the next track stays quiet. A cover or portrait miss still stamps.
func TestARunOfTransientStatusesWarnsOnce(t *testing.T) {
	mb := httptest.NewServer(artistThen(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "{}")
	}))
	defer mb.Close()
	caa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer caa.Close()
	dz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer dz.Close()
	rig := newLogTestRig(t, mb, caa, nil, nil)
	rig.e.deezer = NewDeezerClient(dz.URL, "upstream-log-test", dz.Client())
	rig.e.watchUpstreamAnswers()

	rec := loggingtest.Record(t)
	for i := 0; i < 3; i++ {
		if rig.enrich(t, logTrack(30+i, false)) {
			t.Fatalf("track %d left unenriched; a cover or portrait miss still stamps", i)
		}
	}
	for _, msg := range []string{"artwork", "artist image"} {
		warns := linesAt(rec, slog.LevelWarn, msg)
		errs := linesAt(rec, slog.LevelError, msg)
		if len(warns) != 1 || len(errs) != 0 {
			t.Fatalf("%s: %d warn and %d error, want 1 and 0\n%s", msg, len(warns), len(errs), strings.Join(rec.All(), "\n"))
		}
	}
	if got := answeredLines(rec, upstreamCoverArt); len(got) != 0 {
		t.Fatalf("cover art recovery during a 503 run: %d, want 0\n%s", len(got), strings.Join(rec.All(), "\n"))
	}
	if got := answeredLines(rec, upstreamDeezer); len(got) != 0 {
		t.Fatalf("deezer recovery during a 503 run: %d, want 0\n%s", len(got), strings.Join(rec.All(), "\n"))
	}
}

// A nil reports field is an Enricher built field by field. It warns on
// every transient failure and never grows a map the clients were not
// pointed at.
func TestANilReportsFieldKeepsNoStreak(t *testing.T) {
	rec := loggingtest.Record(t)
	e := &Enricher{}
	err := &httpError{StatusCode: http.StatusServiceUnavailable, Body: "unavailable"}
	e.logUpstream(upstreamCoverArt, "artwork", err)
	e.logUpstream(upstreamCoverArt, "artwork", err)
	warns := linesAt(rec, slog.LevelWarn, "artwork")
	if len(warns) != 2 || e.reports != nil {
		t.Fatalf("warns=%d reports-nil=%v, want 2 warns and a nil reports\n%s", len(warns), e.reports == nil, strings.Join(rec.All(), "\n"))
	}
}

// A 503 opens the MusicBrainz streak. A later DNS failure is persistent
// and is not an answer, so it logs no recovery. The next real response does.
func TestATransientStreakSurvivesANonResponse(t *testing.T) {
	var n atomic.Int32
	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, logArtistBody)
	}))
	defer mb.Close()
	caa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("cover art fetched for local art: %s", r.URL.Path)
	}))
	defer caa.Close()
	client := mb.Client()
	inner := client.Transport
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch n.Add(1) {
		case 1:
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Status:     "503 Service Unavailable",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("busy")),
				Request:    r,
			}, nil
		case 2:
			return nil, &net.DNSError{Err: "no such host", Name: r.URL.Hostname(), IsNotFound: true}
		default:
			return inner.RoundTrip(r)
		}
	})
	rig := newLogTestRig(t, mb, caa, client, nil)

	rec := loggingtest.Record(t)
	artists := []string{"Busy Artist", "Other Artist", "Third Artist"}
	wantRetry := []bool{true, false, false}
	for i, artist := range artists {
		tr := logTrack(40+i, true)
		tr.Artist = artist
		if got := rig.enrich(t, tr); got != wantRetry[i] {
			t.Fatalf("track %d unenriched=%v, want %v\n%s", i, got, wantRetry[i], strings.Join(rec.All(), "\n"))
		}
		if i == 1 && len(answeredLines(rec, upstreamMusicBrainz)) != 0 {
			t.Fatalf("a DNS failure ended the streak\n%s", strings.Join(rec.All(), "\n"))
		}
	}
	if got := answeredLines(rec, upstreamMusicBrainz); len(got) != 1 {
		t.Fatalf("musicbrainz recovery: %d, want 1\n%s", len(got), strings.Join(rec.All(), "\n"))
	}
	if got := linesAt(rec, slog.LevelWarn, "MB artist search"); len(got) != 1 {
		t.Fatalf("warns: %d, want 1\n%s", len(got), strings.Join(rec.All(), "\n"))
	}
	if got := linesAt(rec, slog.LevelError, "MB artist search"); len(got) != 1 {
		t.Fatalf("dns errors: %d, want 1\n%s", len(got), strings.Join(rec.All(), "\n"))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAtlasBusyWarnsOnceThenAnswers(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(premiumJPEG)
	}))
	defer srv.Close()
	dir := t.TempDir()
	f := NewAtlasPremiumFetcher(fakeCred{token: "tok", base: srv.URL, ok: true}, "ua", nil, dir)
	rec := loggingtest.Record(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if f.TryCache(ctx, filepath.Join(dir, fmt.Sprintf("atlas-%d.jpg", i)), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", 500) {
			t.Fatal("a 503 reported a cached cover")
		}
	}
	if !f.TryCache(ctx, filepath.Join(dir, "atlas-ok.jpg"), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", 500) {
		t.Fatal("the JPEG was not cached")
	}
	warns := linesAt(rec, slog.LevelWarn, "atlas premium cover fetch")
	errs := linesAt(rec, slog.LevelError, "atlas premium cover fetch")
	if len(errs) != 0 || len(warns) != 1 {
		t.Fatalf("atlas: %d error and %d warn, want 0 and 1\n%s", len(errs), len(warns), strings.Join(rec.All(), "\n"))
	}
	if got := answeredLines(rec, "atlas"); len(got) != 1 {
		t.Fatalf("atlas recovery: %d, want 1\n%s", len(got), strings.Join(rec.All(), "\n"))
	}
}

func logTrack(i int, localArt bool) *manifest.Track {
	tr := &manifest.Track{
		Path:               fmt.Sprintf("Album/%02d.flac", i),
		Artist:             logArtistName,
		Album:              logAlbumName,
		Title:              fmt.Sprintf("Track %d", i),
		MusicBrainzAlbumID: fmt.Sprintf("11111111-1111-4111-8111-%012d", i+1),
	}
	if localArt {
		tr.Path = fmt.Sprintf("Album/local-%02d.flac", i)
		tr.ArtworkMBID = "local-" + strings.Repeat("ab", 32)
	}
	return tr
}

func scriptHandler(t *testing.T, steps []int, okBody string) http.Handler {
	t.Helper()
	var n atomic.Int32
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		if i >= len(steps) {
			i = len(steps) - 1
		}
		if steps[i] == statusHang {
			time.Sleep(busyUpstreamSleep)
			return
		}
		if steps[i] == http.StatusOK {
			if okBody != "" {
				io.WriteString(w, okBody)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(premiumJPEG)
			return
		}
		w.WriteHeader(steps[i])
	})
}

func linesAt(rec *loggingtest.Recorder, level slog.Level, msg string) []string {
	prefix := level.String() + " " + msg
	var out []string
	for _, line := range rec.Lines(msg) {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return out
}

func answeredLines(rec *loggingtest.Recorder, upstream string) []string {
	var out []string
	for _, line := range rec.Lines("upstream answered") {
		if strings.Contains(line, "upstream="+upstream) {
			out = append(out, line)
		}
	}
	return out
}

func unenrichedPath(tracks []manifest.Track, path string) bool {
	for _, tr := range tracks {
		if tr.Path == path {
			return true
		}
	}
	return false
}
