package library

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// storedPathRefusal is StoredPath's refusal of ref: its reason, failing the
// test on a path it answered or an error that is no refusal, or one that
// names the library's root or the asked-for path.
func storedPathRefusal(t *testing.T, f *libraryFixture, ctx context.Context, ref engine.Reference, named string) engine.Reason {
	t.Helper()
	p, err := f.r.StoredPath(ctx, ref)
	if err == nil {
		t.Fatalf("StoredPath(%s) = %q, want a refusal", ref, p)
	}
	if p != "" {
		t.Errorf("StoredPath(%s) refused with a path beside it: %q", ref, p)
	}
	for _, s := range []string{f.root, named} {
		if s != "" && strings.Contains(err.Error(), s) {
			t.Errorf("StoredPath(%s)'s refusal names a path (%q): %v", ref, s, err)
		}
	}
	if errors.Is(err, ErrCancelled) {
		return ""
	}
	var u *engine.Unplayable
	if !errors.As(err, &u) {
		t.Fatalf("StoredPath(%s): %v (%T), want an *engine.Unplayable", ref, err, err)
	}
	return u.Reason
}

// TestStoredPathNamesTheTrackAReferenceNames pins the console's lookup of
// the track the player plays (B601): either stored form of a reference names
// the path the index holds for it, whatever is at that path, since it opens
// nothing; a reference no row has, and what is no reference at all, are
// not_indexed; two rows with one id are ambiguous_reference; a lookup whose
// context has ended answers ErrCancelled; and no answer names a path but the
// one asked for.
func TestStoredPathNamesTheTrackAReferenceNames(t *testing.T) {
	f := newLibraryFixture(t)
	opened := 0
	f.r.cfg.Seams = &Seams{BeforeOpen: func() { opened++ }}
	const rel = "Artist/Album (2020)/01 Track One.flac"
	ref := f.add(rel, []byte("one"))
	for _, s := range []string{ref.String(), "http://192.168.1.20:7790/dlna/file/" + ref.String(), "http://bridge.local:7790/dlna/file/" + ref.String()} {
		r, err := engine.NewReference(s)
		if err != nil {
			t.Fatal(err)
		}
		got, err := f.r.StoredPath(context.Background(), r)
		if err != nil || got != rel {
			t.Fatalf("StoredPath(%s) = %q, %v; want %q", s, got, err, rel)
		}
	}
	nothing := f.index("Artist/Album (2020)/02 nothing here.flac")
	if got, err := f.r.StoredPath(context.Background(), nothing); err != nil || got != "Artist/Album (2020)/02 nothing here.flac" {
		t.Fatalf("an indexed track with no file at its path: %q, %v; want its path (StoredPath opens nothing)", got, err)
	}
	if opened != 0 {
		t.Fatalf("StoredPath opened %d files", opened)
	}
	if got := storedPathRefusal(t, f, context.Background(), Reference("never/indexed.flac"), "never/indexed.flac"); got != ReasonNotIndexed {
		t.Errorf("a reference no row has: %s, want not_indexed", got)
	}
	for _, s := range []string{"file:1", "0123456789ABCDEF", "http://192.168.1.20:7790/dlna/file/" + ref.String() + "/"} {
		r, err := engine.NewReference(s)
		if err != nil {
			t.Fatal(err)
		}
		if got := storedPathRefusal(t, f, context.Background(), r, ""); got != ReasonNotIndexed {
			t.Errorf("StoredPath(%q): %s, want not_indexed", s, got)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.r.StoredPath(ctx, ref); !errors.Is(err, ErrCancelled) {
		t.Errorf("a lookup whose context has ended: %v, want ErrCancelled", err)
	}
	f.r.idOf = func(string) string { return "00000000000000aa" }
	f.r.ids.reset()
	shared, err := engine.NewReference("00000000000000aa")
	if err != nil {
		t.Fatal(err)
	}
	if got := storedPathRefusal(t, f, context.Background(), shared, rel); got != ReasonAmbiguous {
		t.Errorf("two rows with one id: %s, want ambiguous_reference", got)
	}
}

// TestStoredPathCostsWhatMemberCosts pins StoredPath's cost over a large
// synthetic library: the first lookup builds the index, as Member's does, and
// a lookup after it reads no row of the stamps' index but its own track's,
// so a status that names the track heard does not read the library.
func TestStoredPathCostsWhatMemberCosts(t *testing.T) {
	const n = 5_000
	r, s := syntheticResolver(t, n)
	if got, err := r.StoredPath(context.Background(), Reference(syntheticPath(7))); err != nil || got != syntheticPath(7) {
		t.Fatalf("the first lookup: %q, %v", got, err)
	}
	if handed, _ := s.counts(); handed != n {
		t.Fatalf("the first lookup handed %d rows over, want the library's %d once", handed, n)
	}
	h0, g0 := s.counts()
	if got, err := r.StoredPath(context.Background(), Reference(syntheticPath(4321))); err != nil || got != syntheticPath(4321) {
		t.Fatalf("a second lookup: %q, %v", got, err)
	}
	if h1, g1 := s.counts(); h1 != h0 || g1 != g0+1 {
		t.Fatalf("a second lookup handed %d rows and read %d tracks, want 0 and its own 1", h1-h0, g1-g0)
	}
}
