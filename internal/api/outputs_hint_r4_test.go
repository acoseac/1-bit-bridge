package api

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/mpd"
)

func TestReadingKeepsInspectNamedConfig(t *testing.T) {
	r, root, _ := provenanceRig(t)
	r.confPath = filepath.Join(root, "etc/mpd.conf")
	writeRigFile(t, root, "opt/mpd/main.conf", volumioConf(t, r.music)+"\ninclude \"x.conf\"\n")
	writeRigFile(t, root, "etc/x.conf", "volume_normalization \"yes\"\n")
	writeRigFile(t, root, "opt/mpd/x.conf", "volume_normalization \"no\"\n")
	if err := os.Symlink("../opt/mpd/main.conf", r.confPath); err != nil {
		t.Skip(err)
	}
	r.cfg.Output = func() config.OutputConfig {
		return config.OutputConfig{MPD: "on", Conf: r.confPath, Endpoint: "127.0.0.1:6600"}
	}
	// Run the real inspection/parser without a listening daemon; live commands
	// still use the existing scripted session through the production reading.
	r.cfg.Inspect = func(_ context.Context, _ mpd.Options) mpd.Report {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rep := mpd.Inspect(ctx, mpd.Options{Root: confRoot("/", r.confPath), Conf: r.confPath, Endpoint: "127.0.0.1:6600"})
		if rep.VolumeNormalization != "yes" {
			t.Errorf("probe normalization=%q", rep.VolumeNormalization)
		}
		return rep
	}
	got := provenanceReading(t, r)
	if got.MPD == nil || got.MPD.Config == nil || got.MPD.Config.VolumeNormalization != "yes" {
		t.Fatalf("reading lost named include directory: %+v", got.MPD)
	}
}

func TestReadingOmitsProcessRelativeConfigHint(t *testing.T) {
	for _, operand := range []string{"/proc/self/config", "/proc/123/config", "/dev/fd/7", "/dev/stdin", "/etc/proc-link.conf", "/etc/fd-link.conf", "/etc/stdin-link.conf"} {
		t.Run(operand, func(t *testing.T) {
			r, root, pid := provenanceRig(t)
			target := operand
			switch operand {
			case "/etc/proc-link.conf":
				target = "/proc/123/config"
			case "/etc/fd-link.conf":
				target = "/dev/fd/7"
			case "/etc/stdin-link.conf":
				target = "/dev/stdin"
			}
			writeRigFile(t, root, target[1:], "volume_normalization \"yes\"\n")
			if target != operand {
				writeRigFile(t, root, "etc/placeholder", "")
				if err := os.Symlink(".."+target, filepath.Join(root, operand[1:])); err != nil {
					t.Skip(err)
				}
			}
			writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/cmdline", "mpd\x00"+operand+"\x00")
			wantConfigAndHint(t, provenanceReading(t, r), "", "", "no")
		})
	}
}

func TestReadingHintRefusesAbsoluteLinkOutsideInjectedRoot(t *testing.T) {
	r, root, pid := provenanceRig(t)
	// The target exists on the host, but is outside the injected config root.
	// A corresponding in-root file must not turn this into host identity evidence.
	outside := filepath.Join(t.TempDir(), "mpd.conf")
	if err := os.WriteFile(outside, []byte("volume_normalization \"yes\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	writeRigFile(t, root, outside[1:], "volume_normalization \"no\"\n")
	writeRigFile(t, root, "etc/placeholder", "")
	if err := os.Symlink(outside, filepath.Join(root, "etc/actual.conf")); err != nil {
		t.Skip(err)
	}
	writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/cmdline", "mpd\x00/etc/actual.conf\x00")
	wantConfigAndHint(t, provenanceReading(t, r), "", "", "no")
}

func TestConfiguredDoPVetoWithoutLiveAttribute(t *testing.T) {
	r, _, _ := provenanceRig(t)
	r.mpd.set(func(m *scriptedMPD) { m.outputs = volumioOutputs("1") })
	o := newLocalOutputs(r.cfg)
	t.Cleanup(o.close)
	s := o.takeReading(context.Background())
	sub, _ := pickSubstream(s.cards, &s.picks[0])
	hs := holders(s, sub)
	if len(hs) != 1 {
		t.Fatal("no unique holder")
	}
	if _, live := hs[0].Attribute("dop"); live {
		t.Fatal("fixture has live dop")
	}
	// Both settings must be read from the configured block when no live
	// attribute supplies the value.
	for _, value := range []string{"yes", "no"} {
		for i := range s.conf.Outputs {
			if s.conf.Outputs[i].Name == hs[0].Name {
				s.conf.Outputs[i].DOP = value
			}
		}
		if got := holderOutput(s, hs[0]).DoP; got != (value == "yes") {
			t.Errorf("configured dop=%s, got %v", value, got)
		}
	}
}

func TestHungHintDoesNotBlockLaterConfigReads(t *testing.T) {
	r, _, pid := provenanceRig(t)
	writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/cmdline", "mpd\x00")
	o := newLocalOutputs(r.cfg)
	t.Cleanup(o.close)
	o.limits.hint = time.Millisecond
	release := make(chan struct{})
	var calls atomic.Int32
	o.cmdline = func(string, string, int64) ([]byte, error) {
		calls.Add(1)
		<-release
		return []byte("mpd\x00"), nil
	}
	t.Cleanup(func() {
		close(release)
		deadline := time.After(5 * time.Second)
		for {
			o.mu.Lock()
			busy := o.reading.conf || o.reading.hint
			o.mu.Unlock()
			if !busy {
				return
			}
			select {
			case <-deadline:
				t.Fatal("hint worker did not finish")
			case <-time.After(time.Millisecond):
			}
		}
	})
	for _, value := range []string{"no", "yes"} {
		r.writeConf(volumioConf(t, r.music) + "\nvolume_normalization \"" + value + "\"\n")
		s := o.takeReading(context.Background())
		if !s.confRead || s.conf.VolumeNormalization != value {
			t.Errorf("hung hint erased fresh config: read=%v normalization=%q, want %q", s.confRead, s.conf.VolumeNormalization, value)
		}
		if len(s.cardHints) != 0 {
			t.Error("late hint published")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("hung hint started %d workers, want 1", got)
	}
}

func TestFailedHintKeepsConfiguredFile(t *testing.T) {
	r, _, _ := provenanceRig(t)
	got := provenanceReading(t, r, func(o *localOutputs) {
		o.cmdline = func(string, string, int64) ([]byte, error) { panic("hint failure") }
	})
	wantConfigAndHint(t, got, "", "", "no")
}
