package dsdstate

import (
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// ForRestore is what a restore installs from a snapshot's copy of the state
// (the design's section 2.5, its last bullet): everything the user chose and
// heard, and none of what was observed on the box the snapshot was taken on.
// It keeps defaultMode, the marker, so a restored state never migrates the
// restored bridge.yaml's legacy keys again, a migrated off stays off; the
// warnings; and per DAC the choice, the DoP confirmation, the first-use
// acknowledgements and the listener answers. It drops every evidence
// observation (driverAcceptedAt, kernel, streamingObservedAt), and first
// use's observedAt, which playback on this box records again, removing an
// evidence entry left with no observation, and a DAC left holding nothing.
// A state that decodes is never installed as the snapshot's bytes: one the
// writer cannot write once nothing observed is left, which only a file
// denser than the writer's own can hold (encode), is the restore's error
// (ErrTooLarge), and the restore restores nothing (Codex on round 2: the
// snapshot went in as it was, the old box's observations with it). Bytes that
// do not decode as a state are installed as they are, so the next load
// refuses them, keeps them aside and refuses DSD, as it would have on the box
// they came from: they are no state, and nothing reads what they hold.
func ForRestore(snapshot []byte) ([]byte, error) {
	s, err := Decode(snapshot)
	if err != nil {
		return slices.Clone(snapshot), nil
	}
	for i := range s.DACs {
		d := &s.DACs[i]
		for j := range d.Evidence {
			d.Evidence[j].DriverAcceptedAt, d.Evidence[j].Kernel, d.Evidence[j].StreamingObservedAt = time.Time{}, "", time.Time{}
		}
		d.Evidence = slices.DeleteFunc(d.Evidence, func(e Evidence) bool { return !e.observed() })
		for j := range d.FirstUse {
			d.FirstUse[j].ObservedAt = time.Time{}
		}
	}
	out, err := s.encode()
	if err != nil {
		return nil, fmt.Errorf("dsdstate: the snapshot's state, with nothing observed, cannot be written: %w", err)
	}
	return out, nil
}

// WriteRestored installs at path, the state's file in the data directory
// that holds it, what a restore installs from a snapshot's copy of the state
// (ForRestore), in place of whatever the box held rather than merged with it,
// and durably, through the writer every write of the state takes
// (atomicwrite.WriteBytesDurable, under that directory). Its caller holds the
// data directory's exclusive lock (backup.RestoreWithOptions's does), so no
// store writes beside it. A snapshot ForRestore refuses, and every write
// where no directory can be synced (Windows), is refused, writing nothing. A
// barrier that fails once the writer's rename has landed is the writer's
// error marked atomicwrite.ErrLanded: the box's state is then the
// snapshot's, and not known to survive a crash.
func WriteRestored(path string, snapshot []byte) error {
	out, err := ForRestore(snapshot)
	if err != nil {
		return err
	}
	return atomicwrite.WriteBytesDurable(filepath.Dir(path), path, out, tmpPrefix)
}
