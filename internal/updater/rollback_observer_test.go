package updater

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// errFileInUse stands in for Windows' refusal of a rename one of whose files
// another process holds without sharing its deletion (ERROR_SHARING_VIOLATION,
// in the words Windows gives it), which the observer exists to be told of.
var errFileInUse = errors.New("The process cannot access the file because it is being used by another process.")

// observedRollback is what one put-back did: the renames it made and the
// observer events it gave, in one order; its answer, with the test's paths
// spelled by their roles; what each role's place holds after it; and the
// events as the observer heard them, with the error each refused rename
// answered.
type observedRollback struct {
	trace    []string
	err      error
	said     string
	files    map[string]string
	events   []RollbackRename
	answered map[string]error
	dst, bak string
}

// putBackObserved runs replaceRunningBinary over a binary and its backup in
// a directory of its own (dstThere: whether the binary stands there), its
// renames through a seam on renameFunc that refuses the steps refuse names,
// "from->to" by role (dst, bak, aside), with errFileInUse; with observe, a
// test's observer set.
func putBackObserved(t *testing.T, dstThere bool, refuse []string, observe bool) observedRollback {
	t.Helper()
	dir := t.TempDir()
	o := observedRollback{dst: filepath.Join(dir, "bridge.exe"), answered: map[string]error{}}
	o.bak = o.dst + ".bak"
	if err := os.WriteFile(o.bak, []byte("KNOWN-GOOD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if dstThere {
		if err := os.WriteFile(o.dst, []byte("RUNNING"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	role := func(p string) string {
		switch {
		case p == o.dst:
			return "dst"
		case p == o.bak:
			return "bak"
		case strings.HasPrefix(p, o.dst+".rollback-"):
			return "aside"
		}
		return p
	}
	useRename(t, func(oldpath, newpath string) error {
		step := role(oldpath) + "->" + role(newpath)
		o.trace = append(o.trace, "rename "+step)
		if slices.Contains(refuse, step) {
			err := &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errFileInUse}
			o.answered[step] = err
			return err
		}
		return os.Rename(oldpath, newpath)
	})
	if observe {
		restore := SetRollbackObserverForTest(func(r RollbackRename) {
			o.trace = append(o.trace, "observe "+string(r.Stage)+" "+role(r.From)+"->"+role(r.To))
			o.events = append(o.events, r)
		})
		defer restore()
	}
	o.err = replaceRunningBinary(o.dst, o.bak)
	if o.err != nil {
		said := strings.ReplaceAll(o.err.Error(), o.bak, "<bak>")
		said = regexp.MustCompile(regexp.QuoteMeta(o.dst)+`\.rollback-[0-9]+`).ReplaceAllString(said, "<aside>")
		o.said = strings.ReplaceAll(said, o.dst, "<dst>")
	}
	o.files = map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		r := role(filepath.Join(dir, e.Name()))
		if _, twice := o.files[r]; twice {
			t.Fatalf("two files hold the place of %s after the put-back", r)
		}
		o.files[r] = string(data)
	}
	return o
}

// TestTheRollbackObserverHearsEachRefusedRenameAndChangesNothing: every
// rename of the put-back that Windows' RollbackBinary runs (replaceRunningBinary)
// that fails is told to a test's observer (SetRollbackObserverForTest) with
// its stage, both its paths and the error the rename answered, right after
// that rename and before the next step, so a diagnostic of what holds the
// file asks while it is still held (backlog B872); and the observer changes
// nothing: each row runs with no observer and with one, and both runs make
// the same renames in the same order, leave the same bytes in each place and
// answer the same error, which is the put-back's message as it was, byte for
// byte.
func TestTheRollbackObserverHearsEachRefusedRenameAndChangesNothing(t *testing.T) {
	refused := func(from, to string) string { return fmt.Sprintf("rename %s %s: %v", from, to, errFileInUse) }
	rows := []struct {
		name     string
		dstThere bool
		refuse   []string
		trace    []string
		said     string
		files    map[string]string
	}{
		{name: "none refused", dstThere: true,
			trace: []string{"rename dst->aside", "rename bak->dst"},
			files: map[string]string{"dst": "KNOWN-GOOD"}},
		{name: "install", refuse: []string{"bak->dst"},
			trace: []string{"rename bak->dst", "observe install bak->dst"},
			said:  "rollback rename <bak> -> <dst>: " + refused("<bak>", "<dst>"),
			files: map[string]string{"bak": "KNOWN-GOOD"}},
		{name: "vacate", dstThere: true, refuse: []string{"dst->aside"},
			trace: []string{"rename dst->aside", "observe vacate dst->aside"},
			said:  "rollback move <dst> aside: " + refused("<dst>", "<aside>"),
			files: map[string]string{"dst": "RUNNING", "bak": "KNOWN-GOOD"}},
		{name: "backup", dstThere: true, refuse: []string{"bak->dst"},
			trace: []string{"rename dst->aside", "rename bak->dst", "observe backup bak->dst", "rename aside->dst"},
			said:  "rollback rename <bak> -> <dst>: " + refused("<bak>", "<dst>") + " (restored)",
			files: map[string]string{"dst": "RUNNING", "bak": "KNOWN-GOOD"}},
		{name: "backup then putback", dstThere: true, refuse: []string{"bak->dst", "aside->dst"},
			trace: []string{"rename dst->aside", "rename bak->dst", "observe backup bak->dst", "rename aside->dst", "observe putback aside->dst"},
			said: "rollback rename <bak> -> <dst> failed (" + refused("<bak>", "<dst>") + "); restore also failed (" +
				refused("<aside>", "<dst>") + "); manual recovery needed",
			files: map[string]string{"bak": "KNOWN-GOOD", "aside": "RUNNING"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			none := putBackObserved(t, row.dstThere, row.refuse, false)
			set := putBackObserved(t, row.dstThere, row.refuse, true)

			// The put-back as it was: its message byte for byte, the error it
			// wraps, and what each place holds.
			if none.said != row.said {
				t.Errorf("with no observer the put-back answered %q, want %q", none.said, row.said)
			}
			if row.said != "" && !strings.HasSuffix(row.said, "manual recovery needed") && !errors.Is(none.err, errFileInUse) {
				t.Errorf("with no observer the answer %v does not wrap the refusal", none.err)
			}
			if !maps.Equal(none.files, row.files) {
				t.Errorf("with no observer the places hold %v, want %v", none.files, row.files)
			}

			// The observer changes none of it.
			if set.said != none.said || errors.Is(set.err, errFileInUse) != errors.Is(none.err, errFileInUse) {
				t.Errorf("with an observer the put-back answered %q (wrapping the refusal %v), with none %q (%v)",
					set.said, errors.Is(set.err, errFileInUse), none.said, errors.Is(none.err, errFileInUse))
			}
			if !maps.Equal(set.files, none.files) {
				t.Errorf("with an observer the places hold %v, with none %v", set.files, none.files)
			}
			renames := slices.DeleteFunc(slices.Clone(set.trace), func(s string) bool { return strings.HasPrefix(s, "observe ") })
			if !slices.Equal(renames, none.trace) {
				t.Errorf("with an observer the renames were %q, with none %q", renames, none.trace)
			}

			// What the observer heard: each refused rename, right after it, in
			// order, with its stage, its two paths and the error it answered.
			if !slices.Equal(set.trace, row.trace) {
				t.Errorf("renames and observed refusals = %q, want %q", set.trace, row.trace)
			}
			paths := map[string]string{"dst": set.dst, "bak": set.bak}
			for _, ev := range set.events {
				from, to, _ := strings.Cut(strings.TrimPrefix(stepOf(set.trace, ev), "rename "), "->")
				if want, ok := paths[from]; ok && ev.From != want {
					t.Errorf("the %s event's source is %s, want %s", ev.Stage, ev.From, want)
				}
				if want, ok := paths[to]; ok && ev.To != want {
					t.Errorf("the %s event's destination is %s, want %s", ev.Stage, ev.To, want)
				}
				if from == "aside" && !strings.HasPrefix(ev.From, set.dst+".rollback-") || to == "aside" && !strings.HasPrefix(ev.To, set.dst+".rollback-") {
					t.Errorf("the %s event names %s -> %s, want the binary's place set aside", ev.Stage, ev.From, ev.To)
				}
				if answered := set.answered[from+"->"+to]; answered == nil || ev.Err != answered {
					t.Errorf("the %s event's error is %v, want the error its rename answered, %v", ev.Stage, ev.Err, answered)
				}
			}
		})
	}
}

// stepOf is the rename trace holds just before ev's observation.
func stepOf(trace []string, ev RollbackRename) string {
	for i, s := range trace {
		if strings.HasPrefix(s, "observe "+string(ev.Stage)+" ") && i > 0 {
			return trace[i-1]
		}
	}
	return ""
}
