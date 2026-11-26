package trash

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTwoTrashesOfOneListMoveEachFileOnce pins the trash under two requests
// for the same files at once, as two presses of an album's Delete send them:
// the first held at its root record once its first file has moved, while the
// second runs the whole list. Each file is moved by one request and refused
// by the other; each result's Paths, which the handler retires whatever the
// result's counts, holds exactly the files that request moved; and every file
// restores with its own bytes, its root record naming its own path. The
// requests' stamps differ in one case and are one nanosecond in the other, so
// both requests move files into one stamp's folder.
func TestTwoTrashesOfOneListMoveEachFileOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		same bool
	}{{"each request a stamp of its own", false}, {"both requests one stamp", true}} {
		t.Run(tc.name, func(t *testing.T) {
			base := time.Unix(1_800_000_000, 0)
			var ticks atomic.Int64
			m, root := newTestManager(t, WithClock(func() time.Time {
				if tc.same {
					return base
				}
				return base.Add(time.Duration(ticks.Add(1)))
			}))
			rels := []string{"Artist/Album/01.flac", "Artist/Album/02.flac", "Artist/Album/03.flac", "Artist/Other/04.flac"}
			bodies := map[string]string{}
			for i, rel := range rels {
				bodies[rel] = "audio " + strconv.Itoa(i) + " of " + rel
				seed(t, root, rel, bodies[rel])
			}

			// The first root record written is the first request's, for the
			// file it moved first: it waits there, outside the Once that marks
			// it, until the second request has answered.
			held := make(chan struct{})
			release := make(chan struct{})
			var first sync.Once
			t.Cleanup(SetTrashWriteSeamsForTest(func(trashDir, audio, r, rel string) error {
				mine := false
				first.Do(func() { mine = true })
				if mine {
					close(held)
					<-release
				}
				return writeOriginRecord(trashDir, audio, r, rel)
			}, nil))
			done := make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-done:
				case <-time.After(30 * time.Second):
					t.Error("the first request did not end once let go")
				}
			})
			letGo := sync.OnceFunc(func() { close(release) })
			t.Cleanup(letGo)

			var resA *Result
			var errA error
			go func() {
				defer close(done)
				resA, errA = m.Trash("", rels)
			}()
			select {
			case <-held:
			case <-time.After(30 * time.Second):
				t.Fatal("the first request never reached its first root record")
			}
			resB, errB := m.Trash("", rels)
			letGo()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("the first request did not end once let go")
			}
			if errA != nil || errB != nil {
				t.Fatalf("the requests answered %v and %v", errA, errB)
			}

			moved := func(res *Result) (paths []string) {
				for _, o := range res.Outcomes {
					if o.Status == "trashed" {
						paths = append(paths, o.Path)
					}
				}
				return paths
			}
			for _, rel := range rels {
				var by []string
				for name, res := range map[string]*Result{"first": resA, "second": resB} {
					for _, o := range res.Outcomes {
						if o.Path == rel {
							by = append(by, name+":"+o.Status)
						}
					}
				}
				slices.Sort(by)
				if !slices.Equal(by, []string{"first:failed", "second:trashed"}) && !slices.Equal(by, []string{"first:trashed", "second:failed"}) {
					t.Errorf("%s: outcomes %v, want one request to move it and the other to fail", rel, by)
				}
			}
			if resA.OK+resB.OK != len(rels) || resA.Failed+resB.Failed != len(rels) {
				t.Errorf("counts: first %d moved %d failed, second %d moved %d failed; want %d of each in all",
					resA.OK, resA.Failed, resB.OK, resB.Failed, len(rels))
			}
			for name, res := range map[string]*Result{"first": resA, "second": resB} {
				got, want := slices.Sorted(slices.Values(res.Paths)), slices.Sorted(slices.Values(moved(res)))
				if !slices.Equal(got, want) {
					t.Errorf("the %s request's Paths are %v, want exactly the files it moved, %v", name, got, want)
				}
			}
			if !slices.Equal(moved(resA), rels[:1]) {
				t.Errorf("the first request moved %v, want its first file alone", moved(resA))
			}

			entries, err := m.List()
			if err != nil {
				t.Fatal(err)
			}
			var listed, ids []string
			for _, e := range entries {
				listed = append(listed, e.OriginalPath)
				ids = append(ids, e.ID)
				rec, rerr := readOriginRecord(originSidecar(filepath.Join(m.trashRoot(root), filepath.FromSlash(e.ID))))
				if rerr != nil || rec.Root != root || rec.Rel != e.OriginalPath {
					t.Errorf("%s: root record %+v (%v), want root %s and its own path", e.ID, rec, rerr, root)
				}
			}
			if !slices.Equal(slices.Sorted(slices.Values(listed)), rels) {
				t.Fatalf("the trash lists %v, want each file once: %v", listed, rels)
			}
			res, err := m.Restore(ids)
			if err != nil || res.OK != len(rels) {
				t.Fatalf("restore: %+v, %v", res, err)
			}
			for _, rel := range rels {
				got, rerr := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
				if rerr != nil || string(got) != bodies[rel] {
					t.Errorf("%s restored as %q (%v), want %q", rel, got, rerr, bodies[rel])
				}
			}
		})
	}
}
