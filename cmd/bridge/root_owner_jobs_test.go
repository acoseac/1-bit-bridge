//go:build !windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// fsEntry is what the root test compares across a command: an entry's
// identity and owner.
type fsEntry struct {
	dev, ino uint64
	uid, gid uint32
	mode     fs.FileMode
}

// snapshotTrees records every entry under roots, by lstat, so a symlink is
// its own entry.
func snapshotTrees(t *testing.T, roots ...string) map[string]fsEntry {
	t.Helper()
	out := map[string]fsEntry{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := os.Lstat(p)
			if err != nil {
				return err
			}
			out[p] = entryOf(info)
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	return out
}

func entryOf(info fs.FileInfo) fsEntry {
	st := info.Sys().(*syscall.Stat_t)
	return fsEntry{dev: uint64(st.Dev), ino: uint64(st.Ino), uid: st.Uid, gid: st.Gid, mode: info.Mode()}
}

// ownerChange is one entry a command created or replaced, or one SQLite
// held open.
type ownerChange struct {
	cmd, path, how string
	e              fsEntry
}

func (c ownerChange) String() string {
	return fmt.Sprintf("%-22s %-8s %d:%d %v %s", c.cmd, c.how, c.e.uid, c.e.gid, c.e.mode, c.path)
}

// sineFLAC writes a stereo sine tone as FLAC with sox.
func sineFLAC(t *testing.T, path string, rate, bits int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sox", "-n", "-r", fmt.Sprint(rate), "-b", fmt.Sprint(bits), "-c", "2",
		path, "synth", "3", "sine", "440", "vol", "0.5").CombinedOutput()
	if err != nil {
		t.Fatalf("sox synth %s: %v\n%s", path, err, out)
	}
}

// coverJPEG writes a 600x600 JPEG, which the scanner scales into its
// artwork cache.
func coverJPEG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 600, 600))
	for y := 0; y < 600; y++ {
		for x := 0; x < 600; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// chownTree hands every entry under root to uid:gid, as a service install
// (or a deploy's chown) leaves it.
func chownTree(t *testing.T, root string, uid, gid int) {
	t.Helper()
	if err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	}); err != nil {
		t.Fatal(err)
	}
}

// moveTarget makes the directory a `variants move` is pointed into: root's,
// as a mount point is, on another filesystem than base when this host has
// one to offer (/dev/shm is a tmpfs in a container and on most Linux
// hosts), so the move copies across devices rather than renaming.
func moveTarget(t *testing.T, base string) (dir string, crossDevice bool) {
	t.Helper()
	baseInfo, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	if shm, err := os.Stat("/dev/shm"); err == nil && shm.IsDir() && entryOf(shm).dev != entryOf(baseInfo).dev {
		dir, err := os.MkdirTemp("/dev/shm", "bridge-root-move-")
		if err == nil {
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			return dir, true
		}
	}
	dir = filepath.Join(base, "mnt")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, false
}

var snapshotWrittenLine = regexp.MustCompile(`Snapshot written: (\S+)`)

// TestJobCLIsRunAsRootKeepTheInstallOwner runs the job and database CLIs as
// root over an install another uid owns (the service user), one after
// another, and records every entry each creates or replaces: in the config
// dir, the data dir under it (the store, artwork, variants, waveforms,
// backups), the library, the render scratch under the temp dir, and a
// `variants move` destination. Every one must belong to the service user
// afterwards, or the service cannot write it. On main (2026-09-28) 51
// entries came back root's: every directory and file those commands make,
// the files `restore` replaces, and the render scratch directory in the
// shared temp dir; SQLite's -wal and -shm were already the service's.
//
// It needs root and the audio toolchain (sox with FLAC, ffmpeg with the DSD
// decoders), so CI skips it, and TestJobWritersKeepTheInstallOwner carries
// the rule there; run it in a container as root with the toolchain
// (CLAUDE.md, dido):
// `go test ./cmd/bridge/ -run TestJobCLIsRunAsRootKeepTheInstallOwner -count=1 -v`.
func TestJobCLIsRunAsRootKeepTheInstallOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: it runs the CLI as root over an install another uid owns")
	}
	if _, err := exec.LookPath("sox"); err != nil {
		t.Skip("needs sox with FLAC on PATH: the job CLIs run it")
	}
	if !transcode.FFmpegSnapshot().HasDSD {
		t.Skip("needs ffmpeg with the DSD decoders on PATH: `bridge render` runs it")
	}

	base := t.TempDir()
	cfgDir := filepath.Join(base, "install")
	lib := filepath.Join(base, "Music")
	lib2 := filepath.Join(base, "More")
	// tmp stands in for /tmp: root's, world-writable and sticky, which is
	// where the render scratch lives when upscale.tempDir is unset.
	tmp := filepath.Join(base, "tmp")
	for _, d := range []string{cfgDir, lib, lib2, tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(tmp, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	mnt, crossDevice := moveTarget(t, base)

	sineFLAC(t, filepath.Join(lib, "Artist", "CD44", "01 Tone.flac"), 44100, 16)
	coverJPEG(t, filepath.Join(lib, "Artist", "CD44", "cover.jpg"))
	sineFLAC(t, filepath.Join(lib, "Artist", "HiRes", "01 Tone.flac"), 96000, 24)
	dsf := filepath.Join(lib, "Artist", "DSD", "01 Tone.dsf")
	if err := os.MkdirAll(filepath.Dir(dsf), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := dsdtone.MintDSF(dsf, dsdtone.Tone{RateHz: 2822400, Seconds: 2, AmplitudeDBFS: -6}); err != nil {
		t.Fatal(err)
	}
	sineFLAC(t, filepath.Join(lib2, "Other", "Album", "01 Tone.flac"), 44100, 16)

	if code, out := loopbackInit(t, cfgDir, lib, "Owned"); code != 0 {
		t.Fatalf("bridge init = %d:\n%s", code, out)
	}
	cfgPath := filepath.Join(cfgDir, "bridge.yaml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Upscale.Enabled = true
	cfg.Upscale.DSDRender.Enabled = true
	cfg.Analysis.Enabled = true
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	// The service has run once: its store exists and is its own, and a
	// device is paired, so tokens.json is in the backup and the restore.
	store, err := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var pairOut, pairErr bytes.Buffer
	if code := pairCmd([]string{"--config", cfgPath, "--name", "phone"}, &pairOut, &pairErr); code != 0 {
		t.Fatalf("bridge pair = %d: %s", code, pairErr.String())
	}
	for _, d := range []string{cfgDir, lib, lib2} {
		chownTree(t, d, serviceUID, serviceUID)
	}

	watched := []string{cfgDir, lib, lib2, tmp, mnt}
	var changes []ownerChange
	step := func(name string, cmd func(out, errOut *bytes.Buffer) int) string {
		t.Helper()
		before := snapshotTrees(t, watched...)
		var out, errOut bytes.Buffer
		code := cmd(&out, &errOut)
		after := snapshotTrees(t, watched...)
		if code != 0 {
			t.Errorf("%s as root = %d\nstdout:\n%s\nstderr:\n%s", name, code, out.String(), errOut.String())
		}
		paths := make([]string, 0, len(after))
		for p := range after {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			e := after[p]
			prev, existed := before[p]
			switch {
			case !existed:
				changes = append(changes, ownerChange{cmd: name, path: p, how: "created", e: e})
			case prev.dev != e.dev || prev.ino != e.ino:
				changes = append(changes, ownerChange{cmd: name, path: p, how: "replaced", e: e})
			case prev.uid != e.uid || prev.gid != e.gid:
				changes = append(changes, ownerChange{cmd: name, path: p, how: "chowned", e: e})
			}
		}
		return out.String()
	}
	// held records SQLite's own files as they are while a store is open,
	// which is what a run a crash or a kill ends leaves behind.
	held := func(name string, paths ...string) func(out, errOut *bytes.Buffer) int {
		return func(_, errOut *bytes.Buffer) int {
			s, err := manifest.OpenStore(paths[0])
			if err != nil {
				fmt.Fprintln(errOut, err)
				return 1
			}
			defer s.Close()
			for _, p := range paths[1:] {
				info, err := os.Lstat(p)
				if err != nil {
					fmt.Fprintf(errOut, "%s: %v\n", p, err)
					return 1
				}
				changes = append(changes, ownerChange{cmd: name, path: p, how: "open", e: entryOf(info)})
			}
			return 0
		}
	}
	ctx := context.Background()
	with := func(args ...string) []string { return append(args, "--config", cfgPath) }
	dbPath := manifest.DefaultDBPath(cfg.DataDir)

	step("scan", func(out, errOut *bytes.Buffer) int { return scanCmd(ctx, with(), out, errOut) })
	// SQLite gives its -wal and -shm the database file's owner itself, when
	// it runs as root: measured here, with the store the service's.
	step("store open", held("store open", dbPath, dbPath+"-wal", dbPath+"-shm"))
	step("upscale", func(out, errOut *bytes.Buffer) int {
		return upscaleCmd(ctx, with("--quality", "medium", "--workers", "1"), out, errOut)
	})
	step("optimize", func(out, errOut *bytes.Buffer) int {
		return optimizeCmd(ctx, with("--quality", "medium", "--workers", "1"), out, errOut)
	})
	step("render", func(out, errOut *bytes.Buffer) int {
		return renderCmd(ctx, with("--quality", "medium", "--workers", "1"), out, errOut)
	})
	step("analyze", func(out, errOut *bytes.Buffer) int {
		return analyzeCmd(ctx, with("--workers", "1"), out, errOut)
	})
	step("upscale --force", func(out, errOut *bytes.Buffer) int {
		return upscaleCmd(ctx, with("--quality", "medium", "--workers", "1", "--force", "--filter", "CD44"), out, errOut)
	})
	step("render --force", func(out, errOut *bytes.Buffer) int {
		return renderCmd(ctx, with("--quality", "medium", "--workers", "1", "--force"), out, errOut)
	})
	step("analyze --force", func(out, errOut *bytes.Buffer) int {
		return analyzeCmd(ctx, with("--workers", "1", "--force", "--filter", "CD44"), out, errOut)
	})
	step("scan (again)", func(out, errOut *bytes.Buffer) int { return scanCmd(ctx, with(), out, errOut) })
	step("upscale --gc", func(out, errOut *bytes.Buffer) int { return upscaleCmd(ctx, with("--gc"), out, errOut) })
	step("analyze --gc", func(out, errOut *bytes.Buffer) int { return analyzeCmd(ctx, with("--gc"), out, errOut) })
	step("artwork --gc", func(out, errOut *bytes.Buffer) int {
		return artworkCmd(ctx, with("--gc", "--confirm", artworkGCConfirmPhrase), out, errOut)
	})
	step("enrichment retry", func(out, errOut *bytes.Buffer) int {
		return enrichmentCmd(ctx, append([]string{"retry"}, with()...), out, errOut)
	})
	step("duplicates", func(out, errOut *bytes.Buffer) int { return duplicatesCmd(ctx, with(), out, errOut) })
	step("manifest clear-missing", func(out, errOut *bytes.Buffer) int {
		return manifestCmd(ctx, append([]string{"clear-missing", "--yes"}, with()...), strings.NewReader(""), out, errOut)
	})
	backupOut := step("backup", func(out, errOut *bytes.Buffer) int { return backupCmd(with(), out, errOut) })
	m := snapshotWrittenLine.FindStringSubmatch(backupOut)
	if m == nil {
		t.Fatalf("bridge backup printed no snapshot:\n%s", backupOut)
	}
	step("restore", func(out, errOut *bytes.Buffer) int {
		return restoreCmd(ctx, append(with("--yes"), m[1]), strings.NewReader(""), out, errOut)
	})
	step("variants move", func(out, errOut *bytes.Buffer) int {
		return variantsCmd(ctx, append([]string{"move"}, with("--to", filepath.Join(mnt, "variants"), "--confirm", "MOVE")...), out, errOut)
	})
	step("library add", func(out, errOut *bytes.Buffer) int {
		return libraryCmd(ctx, []string{"add", "--config", cfgPath, lib2}, out, errOut)
	})
	step("library remove", func(out, errOut *bytes.Buffer) int {
		return libraryCmd(ctx, []string{"remove", "--config", cfgPath, lib2}, out, errOut)
	})
	// A store a root CLI creates, before the service's first start.
	fresh := filepath.Join(base, "fresh")
	if err := os.MkdirAll(filepath.Join(fresh, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	chownTree(t, fresh, serviceUID, serviceUID)
	watched = append(watched, fresh)
	freshDB := manifest.DefaultDBPath(filepath.Join(fresh, "data"))
	step("store create", held("store create", freshDB, freshDB, freshDB+"-wal", freshDB+"-shm"))

	// Every command that writes must have written, or a writer that
	// stopped writing would pass by leaving nothing behind.
	wrote := map[string]bool{}
	scratch := false
	for _, c := range changes {
		wrote[c.cmd] = true
		if c.path == filepath.Join(tmp, "1-bit-bridge-render") {
			scratch = true
		}
	}
	for _, cmd := range []string{"scan", "store open", "upscale", "optimize", "render", "analyze",
		"upscale --force", "render --force", "analyze --force", "backup", "restore",
		"variants move", "library add", "library remove", "store create"} {
		if !wrote[cmd] {
			t.Errorf("%s created or replaced nothing, so it proves nothing about the owner", cmd)
		}
	}
	if !scratch {
		t.Errorf("no render made its scratch directory under the shared temp dir %s", tmp)
	}
	var wrong []string
	for _, c := range changes {
		if c.e.uid != serviceUID || c.e.gid != serviceUID {
			if r, err := filepath.Rel(base, c.path); err == nil && !strings.HasPrefix(r, "..") {
				c.path = r
			}
			wrong = append(wrong, c.String())
		}
	}
	t.Logf("variants moved across devices: %v (into %s)", crossDevice, mnt)
	if len(wrong) > 0 || t.Failed() {
		for _, c := range changes {
			t.Logf("%s", c)
		}
	}
	if len(wrong) > 0 {
		t.Fatalf("after the job CLIs ran as root, %d entries do not belong to uid %d:\n%s",
			len(wrong), serviceUID, strings.Join(wrong, "\n"))
	}
}
