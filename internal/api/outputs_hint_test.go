package api

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/mpd/conf"
)

func wantHint(t *testing.T, got OutputReading, argument, path string) {
	t.Helper()
	b, err := json.Marshal(got.MPD)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["configProvenance"]; ok {
		t.Error("obsolete configProvenance emitted")
	}
	raw, present := wire["configHint"]
	if argument == "" {
		if present {
			t.Errorf("unexpected hint: %s", raw)
		}
		return
	}
	var hint map[string]string
	if err := json.Unmarshal(raw, &hint); err != nil {
		t.Fatalf("missing hint: %v", err)
	}
	if hint["argument"] != argument || hint["path"] != path {
		t.Errorf("hint=%v, want %s %q", hint, argument, path)
	}
	_, hasPath := hint["path"]
	if hasPath != (argument == "different") {
		t.Errorf("path presence=%v for %s", hasPath, argument)
	}
	wantFields := 1
	if hasPath {
		wantFields++
	}
	if len(hint) != wantFields {
		t.Errorf("extra hint fields: %v", hint)
	}
}

func TestReadingConfigHintValues(t *testing.T) {
	for _, kind := range []string{"same", "symlink same", "hardlink same", "different", "unreadable different", "none", "no_config"} {
		t.Run(kind, func(t *testing.T) {
			r, root, pid := provenanceRig(t)
			r.confPath = filepath.Join(root, "etc/configured.conf")
			r.writeConf(volumioConf(t, r.music))
			arg, want, path := "/etc/actual.conf", "different", "/etc/actual.conf"
			writeRigFile(t, root, "etc/actual.conf", "volume_normalization \"yes\"\n")
			switch kind {
			case "same", "symlink same", "hardlink same":
				if err := os.Remove(filepath.Join(root, "etc/actual.conf")); err != nil {
					t.Fatal(err)
				}
				if kind == "hardlink same" {
					if err := os.Link(r.confPath, filepath.Join(root, "etc/actual.conf")); err != nil {
						t.Skip(err)
					}
				} else if kind == "symlink same" {
					if err := os.Symlink(r.confPath, filepath.Join(root, "etc/actual.conf")); err != nil {
						t.Skip(err)
					}
				} else {
					rel, err := filepath.Rel(root, r.confPath)
					if err != nil {
						t.Fatal(err)
					}
					arg = "/" + filepath.ToSlash(rel)
				}
				want, path = "same", ""
			case "unreadable different":
				if err := os.Chmod(filepath.Join(root, "etc/actual.conf"), 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "etc/actual.conf"), 0600) })
			case "none":
				arg, want, path = "", "none", ""
			case "no_config":
				arg, want, path = "--no-config", "no_config", ""
			}
			args := "mpd\x00"
			if arg != "" {
				args += arg + "\x00"
			}
			writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/cmdline", args)
			got := provenanceReading(t, r)
			wantHint(t, got, want, path)
			if got.MPD == nil || got.MPD.Config == nil || got.MPD.Config.VolumeNormalization != "no" {
				t.Errorf("configured config replaced: %+v", got.MPD)
			}
			for _, w := range got.Verdict.Warnings {
				if w.Code == "mpd_normalization" {
					t.Error("argument file supplied a warning")
				}
			}
		})
	}
}

func TestReadingHintKeepsConfiguredWarningsAndReadsOnlyConfiguredFile(t *testing.T) {
	for _, args := range []string{"mpd\x00", "mpd\x00--no-config\x00", "mpd\x00/etc/actual.conf\x00"} {
		t.Run(strconv.Itoa(len(args)), func(t *testing.T) {
			r, root, pid := provenanceRig(t)
			r.writeConf(volumioConf(t, r.music) + "\nvolume_normalization \"yes\"\n")
			writeRigFile(t, root, "etc/actual.conf", volumioConf(t, "/not-the-library"))
			writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/cmdline", args)
			// Neither an unusable environment nor absent HOME may trigger inference.
			writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/environ", "not-an-environment")
			reads := 0
			got := provenanceReading(t, r, func(o *localOutputs) {
				read := o.confFile
				o.confFile = func(root, logical, host string) (conf.File, error) {
					reads++
					if host != r.confPath {
						t.Errorf("read argument/default file %q", host)
					}
					return read(root, logical, host)
				}
			})
			if reads != 1 {
				t.Errorf("config reads=%d, want 1", reads)
			}
			argument, path := "none", ""
			if strings.Contains(args, "--no-config") {
				argument = "no_config"
			} else if strings.Contains(args, "actual.conf") {
				argument, path = "different", "/etc/actual.conf"
			}
			wantConfigAndHint(t, got, argument, path, "yes")
		})
	}
}

func TestReadingHintOmissionEvidence(t *testing.T) {
	for _, kind := range []string{"partial card", "missing operand", "missing configured", "unreadable configured", "cmdline error", "config step late", "hint step late"} {
		t.Run(kind, func(t *testing.T) {
			r, root, pid := provenanceRig(t)
			writeRigFile(t, root, "etc/actual.conf", "volume_normalization \"yes\"\n")
			writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/cmdline", "mpd\x00/etc/actual.conf\x00")
			switch kind {
			case "partial card":
				read := r.cards
				r.cards = func() (alsa.Snapshot, error) {
					s, e := read()
					for i := range s.Cards {
						s.Cards[i].Partial = true
					}
					return s, e
				}
			case "missing operand":
				if err := os.Remove(filepath.Join(root, "etc/actual.conf")); err != nil {
					t.Fatal(err)
				}
			case "missing configured":
				if err := os.Remove(r.confPath); err != nil {
					t.Fatal(err)
				}
			}
			released, finished := make(chan struct{}), make(chan struct{})
			var local *localOutputs
			got := provenanceReading(t, r, func(o *localOutputs) {
				local = o
				switch kind {
				case "cmdline error":
					o.cmdline = func(string, string, int64) ([]byte, error) { return nil, fs.ErrPermission }
				case "unreadable configured":
					o.confFile = func(string, string, string) (conf.File, error) { return conf.File{}, fs.ErrPermission }
				case "hint step late":
					o.limits.hint = time.Millisecond
					o.cmdline = func(string, string, int64) ([]byte, error) {
						defer close(finished)
						<-released
						return []byte("mpd\x00"), nil
					}
				case "config step late":
					o.limits.conf = time.Millisecond
					o.confFile = func(string, string, string) (conf.File, error) {
						defer close(finished)
						<-released
						return conf.File{}, fs.ErrPermission
					}
				}
			})
			if strings.HasSuffix(kind, "step late") {
				close(released)
				<-finished
				// Wait for the entire bounded worker, including hint metadata,
				// before the fixture's temp directories can be removed.
				timer := time.NewTimer(5 * time.Second)
				defer timer.Stop()
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for {
					local.mu.Lock()
					busy := local.reading.conf || local.reading.hint
					local.mu.Unlock()
					if !busy {
						break
					}
					select {
					case <-timer.C:
						t.Fatal("config worker did not finish")
					case <-tick.C:
					}
				}
				if kind == "hint step late" {
					if got.MPD.Config == nil {
						t.Error("late hint erased configured config")
					}
				} else if got.MPD.Config != nil {
					t.Error("late config step published partial config")
				}
			}
			if kind == "unreadable configured" {
				wantHint(t, got, "different", "/etc/actual.conf")
				if got.MPD.Config != nil {
					t.Error("unread configured file shown")
				}
			} else {
				wantHint(t, got, "", "")
			}
		})
	}
}
