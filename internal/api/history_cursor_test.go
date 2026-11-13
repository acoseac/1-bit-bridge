package api

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	_ "modernc.org/sqlite"
)

// TestEveryFullHistoryPageAnswersItsLastEventsID pins the cursor a client
// reading the whole of GET /v1/history relies on (PROTOCOL.md, the feed's
// nextCursor sentence; backlog B875). A full page, one holding as many
// events as the effective limit (the limit asked for, 200 when none is,
// 1000 when more is), answers the stored id of its last event, even when no
// event lies past it; only a short or empty page answers 0. So a feed whose
// length is a multiple of the limit ends with an empty page answering 0,
// and the walk from after=0 visits every event once, by descending id,
// which is the order the bridge stored them in and not the order they were
// played in.
func TestEveryFullHistoryPageAnswersItsLastEventsID(t *testing.T) {
	cases := []struct {
		name  string
		limit string // the request's limit; "" sends none
		full  int    // the limit the handler serves it as
		pages int    // full pages
		rest  int    // events past them, fewer than full
	}{
		{"limit 3, a multiple", "3", 3, 3, 0},
		{"limit 3, one past a multiple", "3", 3, 3, 1},
		{"limit 3, two past a multiple", "3", 3, 3, 2},
		{"no limit, a multiple", "", 200, 2, 0},
		{"no limit, one past a multiple", "", 200, 2, 1},
		{"limit 1001, a multiple", "1001", 1000, 2, 0},
		{"limit 1001, one past a multiple", "1001", 1000, 2, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			token, srv, store, dbPath := newHistoryCursorServer(t)
			total := c.full*c.pages + c.rest
			seedHistoryWithGaps(t, store, total)
			stored := storedHistory(t, dbPath)
			if len(stored) != total {
				t.Fatalf("the store holds %d events, want %d", len(stored), total)
			}
			order := historyFixtureOrder(t, stored)

			pages := walkHistory(t, srv, token, c.limit, c.pages+2)
			var walked []string
			for i, p := range pages {
				at := fmt.Sprintf("page %d of %d (after=%d, %d events)", i+1, len(pages), p.after, len(p.paths))
				switch n := len(p.paths); {
				case n > c.full:
					t.Errorf("%s: more events than the limit %d", at, c.full)
				case n == c.full:
					last := p.paths[n-1]
					if p.next != stored[last].id {
						t.Errorf("%s: a full page answers nextCursor %d, want %d, the id of its last event %q",
							at, p.next, stored[last].id, last)
					}
				case p.next != 0:
					t.Errorf("%s: a short page answers nextCursor %d, want 0", at, p.next)
				}
				walked = append(walked, p.paths...)
			}

			last := pages[len(pages)-1]
			if len(pages) != c.pages+1 {
				t.Errorf("the walk took %d pages, want %d: %d full, then one holding %d events and answering 0",
					len(pages), c.pages+1, c.pages, c.rest)
			} else if len(last.paths) != c.rest {
				t.Errorf("page %d of %d (after=%d) is the last and holds %d events, want %d",
					len(pages), len(pages), last.after, len(last.paths), c.rest)
			}
			if len(last.paths) == 0 && string(last.events) != "[]" {
				t.Errorf("page %d of %d (after=%d) is empty and sends events %s, want []",
					len(pages), len(pages), last.after, last.events)
			}
			if i := firstDifference(walked, order); i >= 0 {
				t.Errorf("the walk visited %d events, want each of the %d once by descending id; they part at %d: %s",
					len(walked), len(order), i, describeAt(walked, order, i, stored))
			}

			// The first page asked for with neither limit nor after is the
			// walk's first page.
			if c.limit == "" {
				bare := getHistoryPage(t, srv, token, "/v1/history")
				if first := pages[0]; bare.next != first.next || !slices.Equal(bare.paths, first.paths) {
					t.Errorf("GET /v1/history answers %d events and nextCursor %d; after=0 answers %d and %d",
						len(bare.paths), bare.next, len(first.paths), first.next)
				}
			}
		})
	}
}

// newHistoryCursorServer is newHistoryTestServer's server, with the path of
// its database, which the test reads each event's stored id from.
func newHistoryCursorServer(t *testing.T) (token string, srv *Server, store *manifest.Store, dbPath string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	authStore, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	token, _, err = authStore.Mint("test")
	if err != nil {
		t.Fatal(err)
	}
	dbPath = filepath.Join(dir, "bridge.db")
	store, err = manifest.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv = New(cfg, authStore, nil, "fp").WithDeviceRegistrar(store).WithHistoryStore(store)
	return token, srv, store, dbPath
}

// seedHistoryWithGaps stores n events from two devices and, before every
// third of them, one that the retention window then reaps, so the ids the
// feed pages by have gaps, as a retained history's do: a cursor that counted
// events could not answer a full page's last id by chance. Their startedAt
// is a shuffle of the order they are stored in, as when an offline device
// uploads its older plays after another device's newer ones.
func seedHistoryWithGaps(t *testing.T, store *manifest.Store, n int) {
	t.Helper()
	ctx := context.Background()
	cutoff := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	kept := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	played := rand.New(rand.NewPCG(875, uint64(n))).Perm(n)
	devices := []string{"aaaa1111", "bbbb2222"}
	rows := make([]manifest.PlaybackHistoryRow, 0, n+n/3+1)
	for i := range n {
		if i%3 == 0 {
			rows = append(rows, manifest.PlaybackHistoryRow{
				DeviceToken: devices[0], Path: fmt.Sprintf("Reaped/%04d.flac", i),
				StartedAt: cutoff - int64(time.Hour), DurationUsed: 30,
			})
		}
		rows = append(rows, manifest.PlaybackHistoryRow{
			DeviceToken: devices[i%2], Path: fmt.Sprintf("Kept/%04d.flac", i),
			StartedAt: kept + int64(played[i])*int64(time.Minute), DurationUsed: 30,
		})
	}
	if err := store.InsertHistoryBatch(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReapPlaybackHistory(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
}

type storedHistoryEvent struct {
	id        int64
	startedAt int64
}

// storedHistory reads every event's stored id and startedAt, keyed by its
// path, from the table itself: the wire carries no id, and the store's
// ListHistory is what the handler pages with.
func storedHistory(t *testing.T, dbPath string) map[string]storedHistoryEvent {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(dbPath, "mode=ro&_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, path, started_at FROM playback_history`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	stored := map[string]storedHistoryEvent{}
	for rows.Next() {
		var path string
		var e storedHistoryEvent
		if err := rows.Scan(&e.id, &path, &e.startedAt); err != nil {
			t.Fatal(err)
		}
		if _, ok := stored[path]; ok {
			t.Fatalf("two stored events have the path %q", path)
		}
		stored[path] = e
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return stored
}

// historyFixtureOrder is the order the feed must answer: the stored events
// by descending id. It fails the case where the fixture could not tell that
// order from another: ids with no gap, or startedAt sorted either way along
// them.
func historyFixtureOrder(t *testing.T, stored map[string]storedHistoryEvent) []string {
	t.Helper()
	order := make([]string, 0, len(stored))
	for path := range stored {
		order = append(order, path)
	}
	slices.SortFunc(order, func(a, b string) int { return cmp.Compare(stored[b].id, stored[a].id) })
	if len(order) < 3 {
		t.Fatalf("the fixture holds %d events, too few to tell the orders apart", len(order))
	}
	if span := stored[order[0]].id - stored[order[len(order)-1]].id + 1; span == int64(len(order)) {
		t.Fatal("the fixture's ids have no gap: a cursor counting events would answer them too")
	}
	rises, falls := false, false
	for i := 1; i < len(order); i++ {
		d := stored[order[i]].startedAt - stored[order[i-1]].startedAt
		rises, falls = rises || d > 0, falls || d < 0
	}
	if !rises || !falls {
		t.Fatal("the fixture's startedAt is sorted along the ids: an order by startedAt would pass")
	}
	return order
}

type historyCursorPage struct {
	after  int64           // the cursor the page was asked for with
	events json.RawMessage // the page's events, as sent
	paths  []string        // the path of each event, in the order sent
	next   int64           // its nextCursor
}

// walkHistory reads the feed from after=0, passing each page's nextCursor
// back as after until one answers 0, and fails past maxPages.
func walkHistory(t *testing.T, srv *Server, token, limit string, maxPages int) []historyCursorPage {
	t.Helper()
	var pages []historyCursorPage
	var after int64
	for len(pages) < maxPages {
		path := "/v1/history?after=" + strconv.FormatInt(after, 10)
		if limit != "" {
			path += "&limit=" + limit
		}
		page := getHistoryPage(t, srv, token, path)
		page.after = after
		pages = append(pages, page)
		if page.next == 0 {
			return pages
		}
		after = page.next
	}
	t.Fatalf("the walk read %d pages and none answered nextCursor 0 (the last answered %d)",
		len(pages), pages[len(pages)-1].next)
	return nil
}

// getHistoryPage asks for one page and decodes the two keys every page
// sends.
func getHistoryPage(t *testing.T, srv *Server, token, path string) historyCursorPage {
	t.Helper()
	resp := doReq(t, srv, http.MethodGet, path, token, "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	for _, key := range []string{"events", "nextCursor"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("GET %s sends no %s", path, key)
		}
	}
	page := historyCursorPage{events: body["events"]}
	var events []historyReadEventDTO
	if err := json.Unmarshal(body["events"], &events); err != nil {
		t.Fatalf("GET %s: events %s: %v", path, body["events"], err)
	}
	if err := json.Unmarshal(body["nextCursor"], &page.next); err != nil {
		t.Fatalf("GET %s: nextCursor %s: %v", path, body["nextCursor"], err)
	}
	for _, e := range events {
		page.paths = append(page.paths, e.Path)
	}
	return page
}

// firstDifference is the first index at which got and want differ, or -1.
func firstDifference(got, want []string) int {
	for i := range max(len(got), len(want)) {
		if i >= len(got) || i >= len(want) || got[i] != want[i] {
			return i
		}
	}
	return -1
}

func describeAt(got, want []string, i int, stored map[string]storedHistoryEvent) string {
	side := func(s []string) string {
		if i >= len(s) {
			return "nothing"
		}
		return fmt.Sprintf("%q (id %d)", s[i], stored[s[i]].id)
	}
	return "got " + side(got) + ", want " + side(want)
}
