package upnp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Backlog B164: an upstream whose container tree never ends — a hostile,
// broken or spoofed server — must not hold the walk (and with it every other
// upstream's walk, since the ingest serialises them) until a restart. The walk
// ends at a container budget or a time budget, marked truncated so the ingest
// reaps nothing, and a container that is its own ancestor (a cycle) is skipped
// rather than walked again.

var objectIDArg = regexp.MustCompile(`<ObjectID>([^<]*)</ObjectID>`)

// treeDispatcher answers every Browse from children(objectID): the containers
// it lists, no items, all on one page. It counts the Browse calls it served.
type treeDispatcher struct {
	children func(objectID string) []string
	calls    atomic.Int64
}

func (d *treeDispatcher) Do(_ context.Context, req *http.Request) (*http.Response, error) {
	d.calls.Add(1)
	body, _ := io.ReadAll(req.Body)
	m := objectIDArg.FindStringSubmatch(string(body))
	if m == nil {
		return nil, errors.New("treeDispatcher: no ObjectID in the request")
	}
	parent := m[1]
	var didl strings.Builder
	didl.WriteString(xmlnsHeader)
	kids := d.children(parent)
	for _, id := range kids {
		didl.WriteString(ct(id, parent, "c"+id))
	}
	didl.WriteString(`</DIDL-Lite>`)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(browsePage(didl.String(), len(kids), len(kids)))),
		Header:     make(http.Header),
	}, nil
}

// wide is the tree the review measured: four children per container, each
// named for its path, down to depth 12, and no items anywhere. It ends AT
// MaxDepth, so MaxDepth never trips, and MaxItems counts audio items only, so
// nothing but a container budget bounds a walk of its 22M containers. (A tree
// that goes deeper fails at MaxDepth on its first branch, which a cycle did
// too: see the cycle case.)
func wide(objectID string) []string {
	if strings.Count(objectID, ".") >= 12 {
		return nil
	}
	return []string{objectID + ".0", objectID + ".1", objectID + ".2", objectID + ".3"}
}

func walkTree(ctx context.Context, d interface {
	Do(context.Context, *http.Request) (*http.Response, error)
}, opts WalkOptions) (WalkStats, error) {
	opts.RootObjectID = "r"
	return BrowseFoldersWalk(ctx, NewContentDirectoryClient(d), testControlURL, opts,
		func(Walked) error { return nil })
}

// Walks the whole default budget (about 1.5 s, 25 s under -race) because the
// default is what the ingest relies on: it passes no MaxContainers.
func TestBrowseFoldersWalk_AWideTreeEndsAtTheDefaultContainerBudgetAsTruncated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	d := &treeDispatcher{children: wide}
	stats, err := walkTree(ctx, d, WalkOptions{})
	if !errors.Is(err, ErrWalkTruncated) {
		t.Fatalf("walk of a wide tree ended with %v after %d Browse calls, want ErrWalkTruncated", err, d.calls.Load())
	}
	if !stats.Truncated {
		t.Errorf("stats.Truncated = false, want true: the ingest must not reap a partial walk")
	}
	if stats.Containers != defaultMaxContainers {
		t.Errorf("visited %d containers, want the default budget %d", stats.Containers, defaultMaxContainers)
	}
}

func TestBrowseFoldersWalk_AContainerThatIsItsOwnAncestorIsSkipped(t *testing.T) {
	// r holds a and b; a holds itself and b; b holds r (the root) and a.
	tree := map[string][]string{"r": {"a", "b"}, "a": {"a", "b"}, "b": {"r", "a"}}
	d := &treeDispatcher{children: func(id string) []string { return tree[id] }}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stats, err := walkTree(ctx, d, WalkOptions{})
	if err != nil {
		t.Fatalf("walk of a tree with cycles: %v after %d Browse calls, want no error", err, d.calls.Load())
	}
	// r, r/a, r/a/b, r/b and r/b/a: every path that does not revisit one of
	// its own ancestors, each once.
	if stats.Containers != 5 {
		t.Errorf("visited %d containers, want 5 (each acyclic path once)", stats.Containers)
	}
	// a in a; r and a in r/a/b; r in r/b; a and b in r/b/a.
	if stats.CyclesSkipped != 6 {
		t.Errorf("skipped %d cycles, want 6", stats.CyclesSkipped)
	}
	if stats.Truncated {
		t.Errorf("stats.Truncated = true: skipping a cycle loses nothing, so the walk is whole")
	}
}

func TestBrowseFoldersWalk_AnExplicitContainerBudgetEndsTheWalkThere(t *testing.T) {
	d := &treeDispatcher{children: wide}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stats, err := walkTree(ctx, d, WalkOptions{MaxContainers: 50})
	if !errors.Is(err, ErrWalkTruncated) || !errors.Is(err, ErrWalkContainerBudget) {
		t.Fatalf("err = %v, want ErrWalkContainerBudget (an ErrWalkTruncated)", err)
	}
	if stats.Containers != 50 || d.calls.Load() != 50 {
		t.Errorf("visited %d containers in %d Browse calls, want 50 and 50", stats.Containers, d.calls.Load())
	}
}

func TestBrowseFoldersWalk_ATimeBudgetEndsTheWalkAsTruncated(t *testing.T) {
	start := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64 // seconds past start; each Browse costs one
	d := &treeDispatcher{children: func(id string) []string {
		clock.Add(1)
		return wide(id)
	}}
	opts := WalkOptions{MaxDuration: 30 * time.Second}
	opts.now = func() time.Time { return start.Add(time.Duration(clock.Load()) * time.Second) }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stats, err := walkTree(ctx, d, opts)
	if !errors.Is(err, ErrWalkTruncated) || !errors.Is(err, ErrWalkTimeBudget) {
		t.Fatalf("err = %v, want ErrWalkTimeBudget (an ErrWalkTruncated)", err)
	}
	if !stats.Truncated {
		t.Errorf("stats.Truncated = false, want true")
	}
	if n := d.calls.Load(); n != 30 {
		t.Errorf("%d Browse calls before the budget ran out, want 30 (one per clock second)", n)
	}
}

// pagingDispatcher answers every Browse with one more child container and a
// TotalMatches that is never reached, after a short pause: ONE container's
// pages, which BrowseAll follows up to MaxBrowseAllItems (100,000) before it
// returns, so a deadline checked only between containers never sees them.
type pagingDispatcher struct {
	pause time.Duration
	calls atomic.Int64
}

func (d *pagingDispatcher) Do(_ context.Context, req *http.Request) (*http.Response, error) {
	n := d.calls.Add(1)
	time.Sleep(d.pause)
	m := objectIDArg.FindStringSubmatch(readAll(req))
	parent := "r"
	if m != nil {
		parent = m[1]
	}
	didl := xmlnsHeader + ct("p"+strconv.FormatInt(n, 10), parent, "page") + `</DIDL-Lite>`
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(browsePage(didl, 1, 1_000_000))),
		Header:     make(http.Header),
	}, nil
}

func readAll(req *http.Request) string {
	b, _ := io.ReadAll(req.Body)
	return string(b)
}

// CodeRabbit on #1241: the time budget also ends a single container's
// endless pages, not only a walk between containers.
func TestBrowseFoldersWalk_ATimeBudgetAlsoEndsOneContainersEndlessPages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := &pagingDispatcher{pause: 2 * time.Millisecond}
	stats, err := walkTree(ctx, d, WalkOptions{MaxDuration: 300 * time.Millisecond})
	if !errors.Is(err, ErrWalkTimeBudget) || !errors.Is(err, ErrWalkTruncated) {
		t.Fatalf("err = %v after %d Browse calls of one container, want ErrWalkTimeBudget (an ErrWalkTruncated)", err, d.calls.Load())
	}
	if !stats.Truncated {
		t.Errorf("stats.Truncated = false, want true")
	}
}
