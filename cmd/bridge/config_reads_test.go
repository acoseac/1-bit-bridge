package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// bindsOwnRead makes the third read a turn of bridge.yaml makes, which is
// bind's own comparison before its rename (after the turn's read when it is
// taken and the turn's comparison), answer err once, and atomicwrite ask
// again on errors that match retry: the process-global seams of the turn's
// read and of what a read asks again on, so neither test here calls
// t.Parallel.
func bindsOwnRead(t *testing.T, err error, retry error) {
	t.Helper()
	n := 0
	t.Cleanup(config.SetTurnReadForTest(func(p string) ([]byte, error) {
		if filepath.Base(p) == "bridge.yaml" {
			n++
			if n == 3 {
				return nil, &fs.PathError{Op: "open", Path: p, Err: err}
			}
		}
		return fsutil.ReadAsFile(p)
	}))
	t.Cleanup(atomicwrite.SetReadRetryableForTest(func(e error) bool { return errors.Is(e, retry) }))
}

// errBusyForTest stands for Windows' sharing violation on a read of a file
// another process holds.
var errBusyForTest = errors.New("the process cannot access the file because it is being used by another process")

// TestBindsOwnComparisonAsksAgainWhileTheConfigIsBusy: bind's own comparison
// of the config before its rename reads the file as the turn's reads do,
// asking again while Windows answers that another process holds it, so an
// unchanged config behind one such answer is bound. Until B506's third round
// bind read it once with os.ReadFile and took the refusal for a change.
func TestBindsOwnComparisonAsksAgainWhileTheConfigIsBusy(t *testing.T) {
	root := dacTree(t)
	cfgPath, _ := lockTestConfig(t, []string{libraryDir(t, "Music")}, "")
	bindsOwnRead(t, errBusyForTest, errBusyForTest)
	_, errOut, code := runDAC(t, dacEnv(), append([]string{"bind", "5", "--binding", "serial", "--config", cfgPath}, treeFlags(root)...)...)
	if code != 0 {
		t.Fatalf("bind behind one refused read of an unchanged config: exit %d\n%s", code, errOut)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Output.Player.Device.Binding != config.BindingSerial {
		t.Errorf("the binding is not in the config: %+v", cfg.Output.Player.Device)
	}
}

// TestBindsOwnComparisonSaysAReadFailedForWhatItIs: a read of the config by
// bind's own comparison that fails for good is reported as the read that
// failed, with the config left as it was, never as a config another writer
// changed, which would send the operator after a writer there is none of.
func TestBindsOwnComparisonSaysAReadFailedForWhatItIs(t *testing.T) {
	root := dacTree(t)
	cfgPath, _ := lockTestConfig(t, []string{libraryDir(t, "Music")}, "")
	before := readBytes(t, cfgPath)
	bindsOwnRead(t, fs.ErrPermission, errBusyForTest)
	_, errOut, code := runDAC(t, dacEnv(), append([]string{"bind", "5", "--binding", "serial", "--config", cfgPath}, treeFlags(root)...)...)
	if code == 0 {
		t.Fatalf("bind whose comparison could not read the config exited 0:\n%s", errOut)
	}
	if strings.Contains(errOut, "changed after bind read it") || !strings.Contains(errOut, "permission denied") {
		t.Errorf("bind's refusal does not say the read failed, and how:\n%s", errOut)
	}
	if after := readBytes(t, cfgPath); string(after) != string(before) {
		t.Errorf("bind wrote the config:\n%s", after)
	}
}

// TestARestoreRefusesAConfigChangedWhileItAsked: bridge restore reads the
// config before it takes the data dir's lock and asks, and works out where
// every file goes from that read (the data dir, the TLS pair). A config
// another command changed while it asked, in a turn of the config's lock,
// which the restore does not hold while it waits, is refused once the
// restore has its own turn, with nothing restored. Until B506's third round
// the restore compared the file with what its own turn found, after the
// change, and restored into the data dir the config no longer names.
func TestARestoreRefusesAConfigChangedWhileItAsked(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(c *config.Config, dir string)
	}{
		{"its data dir", func(c *config.Config, dir string) { c.DataDir = filepath.Join(dir, "moved-data") }},
		{"its TLS pair", func(c *config.Config, dir string) {
			c.TLSCertPath, c.TLSKeyPath = filepath.Join(dir, "moved.crt"), filepath.Join(dir, "moved.key")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, dataDir, snap := seedRestorableInstall(t, closedAdminAddr(t))
			// A row the snapshot does not hold, which a restore of the
			// database removes.
			store, err := manifest.OpenStore(manifest.DefaultDBPath(dataDir))
			if err != nil {
				t.Fatal(err)
			}
			const since = "Artist/Album/Since.flac"
			if err := store.UpsertTrack(context.Background(), &manifest.Track{Path: since, Size: 1, ModTime: time.Unix(0, 0).UTC(), Title: "Since"}); err != nil {
				store.Close()
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			pr, pw := io.Pipe()
			t.Cleanup(func() { pw.Close() })
			var stdout, stderr safeBuffer
			done := make(chan int, 1)
			go func() {
				done <- restoreCmd(context.Background(), []string{"--config", cfgPath, snap}, pr, &stdout, &stderr)
			}()
			waitForRestorePrompt(t, &stderr, done)
			if err := config.Edit(cfgPath, func(c *config.Config) (bool, error) {
				tc.change(c, filepath.Dir(cfgPath))
				return true, nil
			}); err != nil {
				t.Fatalf("the change while restore asked: %v", err)
			}
			changed := readBytes(t, cfgPath)
			if _, err := pw.Write([]byte("yes\n")); err != nil {
				t.Fatal(err)
			}
			var code int
			select {
			case code = <-done:
			case <-time.After(time.Minute):
				t.Fatal("the restore did not return")
			}
			if code == 0 {
				t.Errorf("the restore went through over a config changed while it asked:\n%s%s", stdout.String(), stderr.String())
			} else if out := stderr.String(); !strings.Contains(out, "changed") || !strings.Contains(out, "nothing was restored") {
				t.Errorf("the refusal does not say the config changed and that nothing was restored:\n%s", out)
			}
			if !trackExists(t, filepath.Dir(dataDir), since) {
				t.Error("the refused restore replaced the database")
			}
			if after := readBytes(t, cfgPath); string(after) != string(changed) {
				t.Errorf("the refused restore wrote bridge.yaml:\n%s", after)
			}
		})
	}
}
