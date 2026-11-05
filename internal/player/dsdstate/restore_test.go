package dsdstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// TestARestoreKeepsWhatWasChosenAndHeard: what a restore installs from a
// snapshot's state (ForRestore) is everything chosen and heard and nothing
// observed on the box the snapshot was taken on (the design's section 2.5):
// defaultMode, the marker and the warnings, each DAC's choice, DoP
// confirmation, acknowledgements with their rates and listener answers; no
// driverAcceptedAt, kernel or streamingObservedAt, and no first use's
// observedAt; an evidence entry left with nothing is removed, and so is a
// DAC left holding nothing. Bytes that do not decode are installed as they
// are, so the next load refuses them.
func TestARestoreKeepsWhatWasChosenAndHeard(t *testing.T) {
	snapshot := schemaOneState(t)
	snapshot.DACs = append(snapshot.DACs, DAC{Key: brooklyn(t), Evidence: []Evidence{{Transport: TransportDoP, Format: "S32_LE", BitRate: DSD128, BCDDevice: "0100", StreamingObservedAt: t0}}})
	in, err := snapshot.encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(ForRestore(in))
	if err != nil {
		t.Fatal(err)
	}
	want := schemaOneState(t)
	want.DACs[1].Evidence = nil
	want.DACs[1].FirstUse[0].ObservedAt = time.Time{}
	if !reflect.DeepEqual(got.normalized(), want.normalized()) {
		t.Fatalf("a restore installs\n%s\nwant\n%s", show(t, got), show(t, want))
	}

	damaged := []byte(`{"schemaVersion": 1, "defaultMode": "au`)
	if out := ForRestore(damaged); string(out) != string(damaged) {
		t.Fatalf("a damaged state is restored as %q, want its bytes", out)
	}
}

// TestARestoreReplacesTheBoxsStateDurably: WriteRestored puts what a restore
// installs in place of the box's state, never merged with it, through the
// durable writer, and refuses, writing nothing, where no directory can be
// synced (Windows).
func TestARestoreReplacesTheBoxsStateDurably(t *testing.T) {
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
	data := t.TempDir()
	path := filepath.Join(data, FileName)
	box := State{DefaultMode: ModeAuto, DACs: []DAC{{Key: hugo(t), Choice: &Choice{Mode: ModeConvert, Revision: 1, SetAt: t0, SetBy: ByApp}}}}
	b, err := box.encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := schemaOneState(t).encode()
	if err != nil {
		t.Fatal(err)
	}
	restoreSync := atomicwrite.SetSyncDirForTest(nil)
	if err := WriteRestored(path, snap); err == nil || !strings.Contains(err.Error(), "cannot fsync a directory") {
		t.Fatalf("WriteRestored with no directory barrier = %v", err)
	}
	if now, _ := os.ReadFile(path); string(now) != string(b) {
		t.Fatalf("a refused restore wrote the state:\n%s", now)
	}
	restoreSync()
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error { return nil }))
	if err := WriteRestored(path, snap); err != nil {
		t.Fatal(err)
	}
	got := fileState(t, data)
	if mode, _, own := got.ChoiceFor(hugo(t)); mode != ModeAuto || own {
		t.Fatalf("the box's own choice survived the restore: %q (own %v)", mode, own)
	}
	if string(ForRestore(snap)) != string(mustRead(t, path)) {
		t.Fatal("the restored file is not what ForRestore gives")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// compactSnapshot is a snapshot's state of dacs DAC models, each with a
// choice, an acknowledgement and an evidence entry observed, and answers
// listener answers, each of a tuple of its own, as a writer of schema 1
// writes it with nothing between its tokens: the file's own shape, marshalled
// with no white space.
func compactSnapshot(t *testing.T, dacs, answers int) []byte {
	t.Helper()
	transports, rates := []string{TransportNative, TransportDoP}, []int{DSD64, DSD128, DSD256}
	f := fileJSON{SchemaVersion: SchemaVersion, DefaultMode: ModeAuto, DACs: []dacJSON{}, Warnings: []warningJSON{}}
	for i := range dacs {
		product := fmt.Sprintf("DAC %03d", i)
		d := dacJSON{
			Key:      keyJSON{VendorID: "20b1", ProductID: fmt.Sprintf("%04x", 0x3100+i), Interface: ptr(1), Product: &product},
			Choice:   &Choice{Mode: ModeNative, Revision: 1, SetAt: t0, SetBy: ByApp},
			FirstUse: []FirstUse{{Transport: TransportNative, BCDDevice: "0100", AcknowledgedAt: t0, ObservedAt: t0}},
			Listener: []Answer{},
			Evidence: []Evidence{{Transport: TransportNative, Format: "DSD_U32_BE", BitRate: DSD64, BCDDevice: "0100", DriverAcceptedAt: t0, Kernel: "6.12.47-v8-16k+", StreamingObservedAt: t0}},
		}
		for j := range answers {
			r := j % 6
			d.Listener = append(d.Listener, Answer{Transport: transports[r%2], BitRate: rates[r/2], BCDDevice: fmt.Sprintf("%04x", j/6), Answer: AnswerClean, On: "2026-11-05"})
		}
		f.DACs = append(f.DACs, d)
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestARestoreOfAStateTheReaderTakesKeepsNothingObserved: a snapshot's state
// that the reader takes is installed with nothing observed, whatever its size
// within the reader's bound (Codex on round 2: a compact state of 120 DACs
// with 64 answers each, within the bound, was past it indented, and the
// restore installed the snapshot as it was, the old box's observations with
// it). What is installed is within the bound, holds everything chosen and
// heard, and the store loads it and writes on it.
func TestARestoreOfAStateTheReaderTakesKeepsNothingObserved(t *testing.T) {
	durable(t)
	snap := compactSnapshot(t, 120, 64)
	if _, err := Decode(snap); err != nil || len(snap) > maxFileBytes {
		t.Fatalf("the snapshot is %d bytes and reads as %v: want a state the reader takes", len(snap), err)
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, snap, "", "  "); err != nil || indented.Len() <= maxFileBytes {
		t.Fatalf("the snapshot is %d bytes indented, %v: the case is a state within the bound compact and past it indented", indented.Len(), err)
	}
	t.Logf("the snapshot is %d bytes compact, %d indented", len(snap), indented.Len())
	data := t.TempDir()
	path := filepath.Join(data, FileName)
	if err := WriteRestored(path, snap); err != nil {
		t.Fatalf("the restore of a state the reader takes (%d bytes) = %v", len(snap), err)
	}
	if b := mustRead(t, path); len(b) > maxFileBytes {
		t.Fatalf("the restore installed %d bytes, past the reader's bound", len(b))
	}
	got := fileState(t, data)
	if len(got.DACs) != 120 {
		t.Fatalf("the restore installed %d DACs, want 120", len(got.DACs))
	}
	for _, d := range got.DACs {
		if len(d.Evidence) != 0 || len(d.FirstUse) != 1 || !d.FirstUse[0].ObservedAt.IsZero() {
			t.Fatalf("the restore kept the old box's observations: evidence %+v, first use %+v", d.Evidence, d.FirstUse)
		}
		if d.Choice == nil || len(d.Listener) != 64 {
			t.Fatalf("the restore lost what was chosen and heard: choice %+v, %d answers", d.Choice, len(d.Listener))
		}
	}
	s := openAt(t, data, options{})
	if r := s.Report(); r.Outcome != Loaded {
		t.Fatalf("the restored state opens as %+v", r)
	}
	if err := s.Update(chooseNative(t)); err != nil {
		t.Fatalf("a write on the restored state = %v", err)
	}
}

// TestARestoreThatCannotWriteItsStateRestoresNothing: a snapshot's state the
// reader takes and the writer cannot write within the bound once nothing
// observed is left, which only a file denser than the writer's own holds
// (here a product string of line separators held as they are, which the
// writer escapes, six bytes for three), is refused, never installed as the
// snapshot's bytes with the old box's observations in them: the box's state
// stays as it was.
func TestARestoreThatCannotWriteItsStateRestoresNothing(t *testing.T) {
	durable(t)
	product := "A" + strings.Repeat(" ", 200_000) + "Z"
	snap := []byte(`{"schemaVersion":1,"defaultMode":"auto","dacs":[{"key":{"vendorId":"20b1","productId":"3009","interface":1,"product":"` + product + `"},` +
		`"choice":{"mode":"native","maxBitRate":0,"revision":1,"setAt":"2026-11-05T10:00:00Z","setBy":"app"},"dopConfirmed":null,"firstUse":[],"listener":[],` +
		`"evidence":[{"transport":"native","format":"DSD_U32_BE","bitRate":2822400,"bcdDevice":"0100","driverAcceptedAt":"2026-11-05T10:00:00Z","kernel":"6.12.47-v8-16k+"}]}],` +
		`"migratedFrom":null,"warnings":[]}`)
	if _, err := Decode(snap); err != nil || len(snap) > maxFileBytes {
		t.Fatalf("the snapshot is %d bytes and reads as %v: want a state the reader takes", len(snap), err)
	}
	data := t.TempDir()
	path := filepath.Join(data, FileName)
	box, err := State{DefaultMode: ModeAuto, DACs: []DAC{{Key: hugo(t), Choice: &Choice{Mode: ModeConvert, Revision: 1, SetAt: t0, SetBy: ByApp}}}}.encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, box, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteRestored(path, snap); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("the restore of a state the writer cannot write = %v, want ErrTooLarge", err)
	}
	if now := mustRead(t, path); string(now) != string(box) {
		t.Fatalf("a refused restore changed the box's state: %d bytes, want its own %d", len(now), len(box))
	}
}
