package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// B651's fifth round (CodeRabbit on #1397): config.ResolvePath cleans a path
// and resolves no link, so serve recorded the data dir in the spelling its
// config gave, and a spelling through a link on the data dir's own volume
// says nothing once the volume is away: no link of it is left to resolve. A
// bridge.yaml changed to name the data dir in another spelling then read as
// one naming another data dir, which refuses nothing, and serve made a fresh
// data dir on the bare mount point. The record keeps both spellings now: the
// one the config gave and where it led, while the volume is there.

// lateVolume is a data dir's volume that can be taken away. While it is
// there, its mount point holds the folder real and current, a link to real
// on the volume; taken away, the mount point is left bare, and the link goes
// with the volume, as an unmount leaves them.
type lateVolume struct {
	base, lib, mnt, parked, cfg string
	// viaLink and viaTarget are the data dir through current and through
	// real.
	viaLink, viaTarget string
}

func newLateVolume(t *testing.T) *lateVolume {
	t.Helper()
	base := t.TempDir()
	v := &lateVolume{
		base:   base,
		lib:    filepath.Join(base, "Music"),
		mnt:    filepath.Join(base, "mnt"),
		parked: filepath.Join(base, "parked"),
		cfg:    filepath.Join(base, "bridge.yaml"),
	}
	for _, d := range []string{v.lib, filepath.Join(v.mnt, "real")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	symlinkOrSkip(t, "real", filepath.Join(v.mnt, "current"))
	v.viaLink, v.viaTarget = filepath.Join(v.mnt, "current", "bridge"), filepath.Join(v.mnt, "real", "bridge")
	return v
}

// configure writes the config, naming dataDir.
func (v *lateVolume) configure(t *testing.T, dataDir string) {
	t.Helper()
	writeServeConfig(t, v.cfg, v.lib, dataDir, "127.0.0.1:0", "127.0.0.1:0")
}

// serve runs serve once over the volume and stops it.
func (v *lateVolume) serve(t *testing.T) {
	t.Helper()
	stopServed(t, bootServe(t, "--config", v.cfg))
}

// takeAway unmounts the volume: its tree goes, and the bare mount point stays.
func (v *lateVolume) takeAway(t *testing.T) {
	t.Helper()
	if err := os.Rename(v.mnt, v.parked); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(v.mnt, 0o755); err != nil {
		t.Fatal(err)
	}
}

// mustRefuseWhileAway runs serve with the volume away and requires it to
// refuse the data dir as its config gives it, dataDir, before it makes or
// writes anything on the bare mount point.
func (v *lateVolume) mustRefuseWhileAway(t *testing.T, dataDir string) {
	t.Helper()
	code, served, stdout, stderr := runServeWith(t, serveOpts{configPath: v.cfg})
	switch {
	case served:
		t.Errorf("serve served while its data dir's volume was away; stdout=%s", stdout)
	case code != 1:
		t.Errorf("serve exited %d while its data dir's volume was away, want 1; stderr=%s", code, stderr)
	case !strings.Contains(stderr, "serve: the data directory "):
		t.Errorf("serve's refusal is not the data directory's; stderr=%s", stderr)
	default:
		mustRefuseAsConfigured(t, stderr, dataDir, dataDirRecordFile(v.cfg))
	}
	if entries, err := os.ReadDir(v.mnt); err != nil || len(entries) != 0 {
		t.Errorf("serve wrote into the bare mount point (read: %v, %d entries)", err, len(entries))
	}
}

// recordedResolved is where the record beside cfg says its data dir led, ""
// for a record that says nothing of it.
func recordedResolved(t *testing.T, cfg string) string {
	t.Helper()
	b, err := os.ReadFile(dataDirRecordFile(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Resolved string `json:"resolved"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("the record beside %s does not read: %v (%q)", cfg, err, b)
	}
	return r.Resolved
}

// TestServeRefusesTheDataDirItRecordedThroughALinkOnItsVolumeByItsTarget: a
// serve whose config named the data dir through a link on the data dir's
// volume records where the link led; the config then names that target, and
// with the volume away serve refuses it, though nothing resolves the link any
// more.
func TestServeRefusesTheDataDirItRecordedThroughALinkOnItsVolumeByItsTarget(t *testing.T) {
	v := newLateVolume(t)
	v.configure(t, v.viaLink)
	v.serve(t)
	v.configure(t, v.viaTarget)
	v.takeAway(t)
	v.mustRefuseWhileAway(t, v.viaTarget)
}

// TestServeRefusesTheDataDirItRecordedByItsTargetThroughALinkOnItsVolume: a
// serve whose config named the target recorded it; the config then names the
// data dir through the link, a serve over the volume writes the record again
// in that spelling and where it led, and with the volume away serve refuses
// the link's spelling. Until the fifth round that serve kept the record as it
// was, since it named the same directory (fsutil.SamePath), and the link's
// spelling was nowhere once the volume went.
func TestServeRefusesTheDataDirItRecordedByItsTargetThroughALinkOnItsVolume(t *testing.T) {
	v := newLateVolume(t)
	v.configure(t, v.viaTarget)
	v.serve(t)
	v.configure(t, v.viaLink)
	v.serve(t)
	v.takeAway(t)
	v.mustRefuseWhileAway(t, v.viaLink)
}

// TestServeRefusesTheDataDirThroughALinkAtItsMountPoint: a link outside the
// volume, to its mount point, still resolves with the volume away (Argon on
// the brief), so either spelling recorded and the other configured, serve
// refuses, with no serve between the change and the volume going.
func TestServeRefusesTheDataDirThroughALinkAtItsMountPoint(t *testing.T) {
	for _, recordedThroughTheLink := range []bool{true, false} {
		name := "recorded at the mount point, configured through the link"
		if recordedThroughTheLink {
			name = "recorded through the link, configured at the mount point"
		}
		t.Run(name, func(t *testing.T) {
			v := newLateVolume(t)
			link := filepath.Join(v.base, "data")
			symlinkOrSkip(t, v.mnt, link)
			first, then := filepath.Join(v.mnt, "bridge"), filepath.Join(link, "bridge")
			if recordedThroughTheLink {
				first, then = then, first
			}
			v.configure(t, first)
			v.serve(t)
			v.configure(t, then)
			v.takeAway(t)
			v.mustRefuseWhileAway(t, then)
		})
	}
}

// TestServeWritesARecordOfOneSpellingAgainInBoth: a record written before
// B651's fifth round holds the data dir as its config gave it, alone. Serve
// reads it as that one spelling, with no error: over a data dir that holds its
// database it refuses nothing, and it writes the record again with where the
// data dir leads (Argon on the brief), which the refusal reads once the
// volume is away and the config names that target.
func TestServeWritesARecordOfOneSpellingAgainInBoth(t *testing.T) {
	v := newLateVolume(t)
	v.configure(t, v.viaLink)
	v.serve(t)
	writeDataDirRecord(t, v.cfg, v.viaLink)
	v.serve(t)
	if got := recordedDataDir(t, v.cfg); got != v.viaLink {
		t.Errorf("the record names %q as the config gave the data dir, want %q", got, v.viaLink)
	}
	resolved := recordedResolved(t, v.cfg)
	if resolved == "" {
		t.Fatal("serve kept a record of one spelling over a data dir it opened")
	}
	got, err := os.Stat(resolved)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(v.viaTarget)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(got, want) {
		t.Errorf("the record says the data dir led to %s, which is not %s", resolved, v.viaTarget)
	}
	if again, err := filepath.EvalSymlinks(resolved); err != nil || again != resolved {
		t.Errorf("where the record says the data dir led, %s, still goes through a link (%s, %v)", resolved, again, err)
	}
	v.configure(t, v.viaTarget)
	v.takeAway(t)
	v.mustRefuseWhileAway(t, v.viaTarget)
}
