package fsutil

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

// B738's round 2 (CodeRabbit on #1454): a listing is whole only where its read
// reached the end. os.File's ReadDir(n) may answer fewer than n entries with
// no error before the end (on Linux, where it counts an entry before it stats
// it and leaves out one gone in between), so osFolding's readDir reads on
// through one open of the folder (readDirUpTo) and readListing takes only the
// end, io.EOF, for whole. These tests ask the two layers apart: the raw reader
// a scripted one (scriptedListing), and the listing osFolding reads over it.

// scriptedListing is a folder opened for its listing, scripted: each ReadDir
// answers the next step, whatever it was asked for, and records what it was
// asked for (asked); past its last step it answers io.EOF.
type scriptedListing struct {
	steps  []listingStep
	asked  []int
	closed bool
}

// listingStep is one ReadDir's answer: the names, and the error beside them.
type listingStep struct {
	names []string
	err   error
}

func (s *scriptedListing) ReadDir(n int) ([]fs.DirEntry, error) {
	s.asked = append(s.asked, n)
	if len(s.asked) > len(s.steps) {
		return nil, io.EOF
	}
	step := s.steps[len(s.asked)-1]
	return entriesOf(step.names), step.err
}

func (s *scriptedListing) Close() error {
	s.closed = true
	return nil
}

// entriesOf is a listing of names, each a file of the probe's seam.
func entriesOf(names []string) []fs.DirEntry {
	if names == nil {
		return nil
	}
	out := make([]fs.DirEntry, len(names))
	for i, name := range names {
		out[i] = fs.FileInfoToDirEntry(probeInfo{name: name})
	}
	return out
}

// namesOf is the names of entries, in order.
func namesOf(entries []fs.DirEntry) []string {
	if entries == nil {
		return nil
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name()
	}
	return out
}

// TestTheListingReadsOnPastAShortBatchUpToItsBound: readDirUpTo, over a
// scripted reader, reads on after a batch that came back short with no error,
// each read asking for what is left of the bound, never for 0 (which reads
// all the rest) and never past it, and ends at each of its exits: io.EOF
// beside what it read once a read reached the end; no error once it holds the
// bound, with the end not looked for; a failed read's error beside what it
// read before; no error after a batch that brings nothing; the bound kept
// against a reader that answers more than it was asked; and where n is not
// positive, one read of all of it, answered as it came.
func TestTheListingReadsOnPastAShortBatchUpToItsBound(t *testing.T) {
	eio := &fs.PathError{Op: "readdirent", Path: "/music", Err: syscall.EIO}
	step := func(err error, names ...string) listingStep { return listingStep{names, err} }
	for _, c := range []struct {
		name  string
		n     int
		steps []listingStep
		asked []int
		want  []string
		err   error
	}{
		{"short batches with no error, then more, then the end", 5,
			[]listingStep{step(nil, "a"), step(nil, "b", "c"), step(io.EOF)},
			[]int{5, 4, 2}, []string{"a", "b", "c"}, io.EOF},
		{"a short batch, then the rest of the bound", 3,
			[]listingStep{step(nil, "a"), step(nil, "b", "c")},
			[]int{3, 2}, []string{"a", "b", "c"}, nil},
		{"the bound in one batch, the end not looked for", 2,
			[]listingStep{step(nil, "a", "b")},
			[]int{2}, []string{"a", "b"}, nil},
		{"the end answered beside entries", 5,
			[]listingStep{step(nil, "a"), step(io.EOF, "b")},
			[]int{5, 4}, []string{"a", "b"}, io.EOF},
		{"an empty listing", 5,
			[]listingStep{step(io.EOF)},
			[]int{5}, nil, io.EOF},
		{"a read that failed after a short batch", 5,
			[]listingStep{step(nil, "a"), step(eio, "b")},
			[]int{5, 4}, []string{"a", "b"}, eio},
		{"a read that failed at once", 5,
			[]listingStep{step(eio)},
			[]int{5}, nil, eio},
		{"a batch that brings nothing and no error", 5,
			[]listingStep{step(nil, "a"), step(nil)},
			[]int{5, 4}, []string{"a"}, nil},
		{"a batch that brings more than was asked", 2,
			[]listingStep{step(nil, "a"), step(io.EOF, "b", "c", "d")},
			[]int{2, 1}, []string{"a", "b"}, nil},
		{"all of it, in one read", -1,
			[]listingStep{step(nil, "a", "b", "c")},
			[]int{-1}, []string{"a", "b", "c"}, nil},
		{"all of it, a read that failed", -1,
			[]listingStep{step(eio, "a")},
			[]int{-1}, []string{"a"}, eio},
	} {
		d := &scriptedListing{steps: c.steps}
		got, err := readDirUpTo(d, c.n)
		if names := namesOf(got); !slices.Equal(names, c.want) || err != c.err {
			t.Errorf("%s: read %q, %v; want %q, %v", c.name, names, err, c.want, c.err)
		}
		if !slices.Equal(d.asked, c.asked) {
			t.Errorf("%s: the reads asked for %v entries, want %v", c.name, d.asked, c.asked)
		}
		if c.n > 0 {
			if len(got) > c.n {
				t.Errorf("%s: %d entries read, past the bound of %d", c.name, len(got), c.n)
			}
			for _, a := range d.asked {
				if a <= 0 {
					t.Errorf("%s: a read asked for %d entries, which reads all the rest", c.name, a)
				}
			}
		}
	}
}

// answeredListing is a folder whose listing, however it is asked for, is one
// scripted answer: names, and err beside them. Only readListing asks it.
type answeredListing struct {
	osFolding
	names []string
	err   error
}

func (a answeredListing) readDir(string, int) ([]fs.DirEntry, error) {
	return entriesOf(a.names), a.err
}

// TestAListingIsWholeOnlyWhereItsReadReachedTheEnd: readListing takes a
// listing read for at most n entries for whole only where its read reached
// the end (io.EOF), never where it came back short with no error, nor where
// it holds n, nor beside a failed read; and one read of all of it (n not
// positive) where it answered no error, which is how that read ends.
func TestAListingIsWholeOnlyWhereItsReadReachedTheEnd(t *testing.T) {
	eio := &fs.PathError{Op: "readdirent", Path: "/music", Err: syscall.EIO}
	for _, c := range []struct {
		name    string
		n, read int
		err     error
		whole   bool
	}{
		{"short with no error", foldingProbeEntries + 1, foldingProbeEntries, nil, false},
		{"short with the end", foldingProbeEntries + 1, foldingProbeEntries, io.EOF, true},
		{"the bound with no error", foldingProbeEntries + 1, foldingProbeEntries + 1, nil, false},
		{"the first read's bound with no error", foldingProbeEntries, foldingProbeEntries, nil, false},
		{"an empty listing", foldingProbeEntries, 0, io.EOF, true},
		{"short beside a failed read", foldingProbeEntries + 1, 10, eio, false},
		{"all of it with no error", -1, 300, nil, true},
		{"all of it beside a failed read", -1, 300, eio, false},
		{"all of it beside io.EOF", -1, 300, io.EOF, false},
	} {
		got, whole := readListing(answeredListing{names: caseless(c.read), err: c.err}, "/music", c.n)
		if whole != c.whole || len(got) != c.read {
			t.Errorf("%s: %d entries read for %d answered %d entries, whole %v; want %d, whole %v",
				c.name, c.read, c.n, len(got), whole, c.read, c.whole)
		}
	}
}

// TestTheFirstReadReadsOnPastAShortBatch: the probe's first read of a folder,
// through osFolding's own readDir, reads on after a batch that came back short
// with no error: one that stopped there would leave out what the folder still
// lists, here the one name with a letter in it, and answer Unknown where the
// folder tells. The listing is scripted (osFolding.open), each read asking for
// what is left; the lookups are this host's, so the answer is what a lookup of
// ALBUM beside Album finds here.
func TestTheFirstReadReadsOnPastAShortBatch(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, filepath.Join(dir, "000"), filepath.Join(dir, "Album"))
	want := CaseSensitive
	if _, err := os.Lstat(filepath.Join(dir, "ALBUM")); err == nil {
		want = CaseInsensitive
	}
	var opened []*scriptedListing
	o := osFolding{open: func(string) (listedDir, error) {
		d := &scriptedListing{steps: []listingStep{{names: []string{"000"}}, {names: []string{"Album"}}, {err: io.EOF}}}
		opened = append(opened, d)
		return d, nil
	}}
	if got := foldingIn(o, dir); got != want {
		t.Errorf("a first read whose first batch came back short answered %s, want %s", got, want)
	}
	if len(opened) == 0 {
		t.Fatal("the probe opened no listing")
	}
	if want := []int{foldingProbeEntries, foldingProbeEntries - 1, foldingProbeEntries - 2}; !slices.Equal(opened[0].asked, want) {
		t.Errorf("the first read asked for %v entries, want %v", opened[0].asked, want)
	}
	for i, d := range opened {
		if !d.closed {
			t.Errorf("listing %d was left open", i)
		}
	}
}

// TestTheOSListingEndsWhereItsReaderReachesTheEnd: osFolding's readDir, over
// this host's own reader, answers io.EOF beside a listing that ended within
// its bound, no error beside one that holds the bound (the end not looked
// for), and no error beside all of it where n is not positive; so readListing
// takes a folder of 255 entries read for 256, or of 256 read for 257, for
// whole, and neither one of 256 read for 256 nor one of 257 read for 257.
func TestTheOSListingEndsWhereItsReaderReachesTheEnd(t *testing.T) {
	base := t.TempDir()
	folders := map[int]string{}
	for _, k := range []int{0, 255, 256, 257} {
		dir := filepath.Join(base, fmt.Sprint(k))
		mkdirs(t, dir)
		for i := range k {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%03d", i)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		folders[k] = dir
	}
	for _, c := range []struct {
		entries, n, read int
		err              error
		whole            bool
	}{
		{0, foldingProbeEntries, 0, io.EOF, true},
		{255, foldingProbeEntries, 255, io.EOF, true},
		{256, foldingProbeEntries, 256, nil, false},
		{257, foldingProbeEntries, 256, nil, false},
		{256, foldingProbeEntries + 1, 256, io.EOF, true},
		{257, foldingProbeEntries + 1, 257, nil, false},
		{257, -1, 257, nil, true},
	} {
		got, err := osFolding{}.readDir(folders[c.entries], c.n)
		if len(got) != c.read || !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
			t.Errorf("a folder of %d entries read for %d answered %d entries, %v; want %d, %v",
				c.entries, c.n, len(got), err, c.read, c.err)
		}
		if _, whole := readListing(osFolding{}, folders[c.entries], c.n); whole != c.whole {
			t.Errorf("a folder of %d entries read for %d read as whole %v, want %v", c.entries, c.n, whole, c.whole)
		}
	}
}
