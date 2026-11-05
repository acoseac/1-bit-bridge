package dsdstate

import (
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
