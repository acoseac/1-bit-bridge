package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/mpd"
	"github.com/acoseac/1-bit-bridge/internal/signalpath"
)

// provenanceRig reads only captured trees and uses the production reading entry
// point without a listener, so these regressions also run in a bind-free sandbox.
func provenanceRig(t *testing.T) (*readingRig, string, int) {
	t.Helper()
	r := newReadingRig(t, piCaptures[0].name, piCaptures[0].file, piCaptures[0].track, capturedStatus(t, piCaptures[0].name))
	r.cfg.Getenv = func(k string) string {
		if k == "HOME" {
			return "/home/pi"
		}
		return ""
	}
	snap, err := r.cards()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range snap.Cards {
		for _, d := range c.Devices {
			for _, sub := range d.Substreams {
				if sub.Owner != nil && sub.Owner.PID != nil && sub.Owner.Process != nil && *sub.Owner.Process == "mpd" {
					seedProcessEvidence(t, r, *sub.Owner.PID)
					return r, filepath.Dir(r.procRoot), *sub.Owner.PID
				}
			}
		}
	}
	t.Fatal("capture has no MPD process")
	return nil, "", 0
}

func provenanceReading(t *testing.T, r *readingRig, configure ...func(*localOutputs)) OutputReading {
	t.Helper()
	o := newLocalOutputs(r.cfg)
	t.Cleanup(func() { o.close() })
	for _, f := range configure {
		f(o)
	}
	var id string
	// Resolve the id from exactly the same injected cards as the reading uses.
	snap, err := r.cards()
	if err != nil {
		t.Fatal(err)
	}
	rep := r.cfg.Inspect(context.Background(), mpd.Options{})
	picks := dacsFrom(snap, rep, alsa.LoadPCMConfig(filepath.Dir(r.procRoot)).ResolveOutputDevice)
	if len(picks) != 1 {
		t.Fatalf("picks: %+v", picks)
	}
	id = outputID(o.identity, "127.0.0.1:6600", picks[0].dac.ID, picks[0].dac.USBID)
	got, found, err := o.Reading(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("reading found=%v err=%v", found, err)
	}
	return got
}

func wantProvenance(t *testing.T, got OutputReading, source, path, normalization string) {
	t.Helper()
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		MPD struct {
			Provenance struct {
				Source string `json:"source"`
				Path   string `json:"path"`
			} `json:"configProvenance"`
		} `json:"mpd"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.MPD.Provenance.Source != source || wire.MPD.Provenance.Path != path {
		t.Errorf("configProvenance=%+v, want source=%q path=%q", wire.MPD.Provenance, source, path)
	}
	if got.MPD == nil || got.MPD.Config == nil || got.MPD.Config.VolumeNormalization != normalization {
		t.Fatalf("config=%+v, want normalization=%q", got.MPD, normalization)
	}
	if normalization == "yes" {
		found := false
		for _, w := range got.Verdict.Warnings {
			if w.Code == signalpath.MPDNormalization && w.Source == signalpath.FromMPDConf {
				found = true
			}
		}
		if !found {
			t.Errorf("no mpd_conf normalization warning: %+v", got.Verdict)
		}
	}
}

func TestReadingUsesMPDProcessConfig(t *testing.T) {
	for _, args := range []string{"mpd\x00--no-daemon\x00/etc/actual.conf\x00", "mpd\x00/etc/actual.conf\x00--stderr\x00", "mpd\x00--verbose=" + strings.Repeat("p", (64<<10)-len("mpd\x00--verbose=\x00/etc/actual.conf\x00")) + "\x00/etc/actual.conf\x00"} {
		t.Run(fmt.Sprintf("argv_%d_bytes", len(args)), func(t *testing.T) {
			r, root, pid := provenanceRig(t)
			writeRigFile(t, root, "etc/actual.conf", volumioConf(t, r.music)+"\nvolume_normalization \"yes\"\n")
			writeRigFile(t, r.procRoot, filepath.Join(strconv.Itoa(pid), "cmdline"), args)
			calls := 0
			got := provenanceReading(t, r, func(o *localOutputs) {
				read := o.cmdline
				o.cmdline = func(proc, logical string, limit int64) ([]byte, error) {
					calls++
					if proc != r.procRoot || logical != "/"+strconv.Itoa(pid)+"/cmdline" || limit != 64<<10 {
						t.Errorf("cmdline root=%q path=%q limit=%d", proc, logical, limit)
					}
					return read(proc, logical, limit)
				}
			})
			if calls != 1 {
				t.Errorf("cmdline read %d times, want once", calls)
			}
			wantProvenance(t, got, "process_cmdline", "/etc/actual.conf", "yes")
		})
	}
}

func TestReadingUsesMPDDefaultConfig(t *testing.T) {
	for _, selected := range []string{"home/pi/.config/mpd/mpd.conf", "home/pi/.mpdconf", "home/pi/.mpd/mpd.conf", "etc/mpd.conf"} {
		t.Run(selected, func(t *testing.T) {
			r, root, pid := provenanceRig(t)
			if err := os.MkdirAll(filepath.Join(root, "home/pi/.config"), 0755); err != nil {
				t.Fatal(err)
			}
			writeRigFile(t, root, selected, volumioConf(t, r.music)+"\nvolume_normalization \"yes\"\n")
			// A later candidate must not win just because /etc is familiar.
			if selected != "etc/mpd.conf" {
				writeRigFile(t, root, "etc/mpd.conf", volumioConf(t, r.music))
			}
			writeRigFile(t, r.procRoot, filepath.Join(strconv.Itoa(pid), "cmdline"), "mpd\x00--no-daemon\x00")
			wantProvenance(t, provenanceReading(t, r), "mpd_default", "/"+selected, "yes")
		})
	}
}

func TestReadingFallsBackFromUnusableCmdline(t *testing.T) {
	cases := map[string]string{
		"unreadable": "", "oversized": "mpd\x00--verbose=" + strings.Repeat("p", (64<<10)+1-len("mpd\x00--verbose=\x00/etc/actual.conf\x00")) + "\x00/etc/actual.conf\x00",
		"unterminated": "mpd\x00/etc/actual.conf", "empty argument": "mpd\x00\x00/etc/actual.conf\x00",
		"unknown option": "mpd\x00--unknown\x00/etc/actual.conf\x00", "extra positional": "mpd\x00/etc/actual.conf\x00/other\x00",
		"relative path": "mpd\x00actual.conf\x00", "outside root": "mpd\x00/etc/outside.conf\x00",
		"not a file": "mpd\x00/etc/directory.conf\x00",
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			r, root, pid := provenanceRig(t)
			writeRigFile(t, root, "etc/actual.conf", "volume_normalization \"yes\"\n")
			if err := os.MkdirAll(filepath.Join(root, "etc/directory.conf"), 0755); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside.conf")
			if err := os.WriteFile(outside, []byte("volume_normalization \"yes\"\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "etc/outside.conf")); err != nil && name == "outside root" {
				t.Skip(err)
			}
			if args != "" {
				writeRigFile(t, r.procRoot, filepath.Join(strconv.Itoa(pid), "cmdline"), args)
			}
			wantProvenance(t, provenanceReading(t, r), "configured_fallback", r.confPath, "no")
		})
	}
}

func TestReadingWithoutAnMPDOwnerUsesConfiguredFallback(t *testing.T) {
	for _, owner := range []string{"foreign", "none", "unmatched thread"} {
		t.Run(owner, func(t *testing.T) {
			r, root, pid := provenanceRig(t)
			writeRigFile(t, root, "etc/actual.conf", "volume_normalization \"yes\"\n")
			writeRigFile(t, r.procRoot, filepath.Join(strconv.Itoa(pid), "cmdline"), "mpd\x00/etc/actual.conf\x00")
			read := r.cards
			r.cards = func() (alsa.Snapshot, error) {
				snap, err := read()
				for i := range snap.Cards {
					for j := range snap.Cards[i].Devices {
						for k := range snap.Cards[i].Devices[j].Substreams {
							s := &snap.Cards[i].Devices[j].Substreams[k]
							if s.Owner != nil {
								switch owner {
								case "none":
									s.Owner = nil
								case "foreign":
									v := "other"
									s.Owner.Process = &v
								case "unmatched thread":
									v := "output:other"
									s.Owner.Thread = &v
								}
							}
						}
					}
				}
				return snap, err
			}
			calls := 0
			got := provenanceReading(t, r, func(o *localOutputs) {
				o.cmdline = func(string, string, int64) ([]byte, error) { calls++; return nil, fs.ErrPermission }
			})
			if calls != 0 {
				t.Errorf("read cmdline %d times without an MPD owner", calls)
			}
			wantProvenance(t, got, "configured_fallback", r.confPath, "no")
		})
	}
}

func TestReadingRejectsAnUnusableProcCmdlineFile(t *testing.T) {
	for _, kind := range []string{"directory", "outside link", "permission denied"} {
		t.Run(kind, func(t *testing.T) {
			r, root, pid := provenanceRig(t)
			writeRigFile(t, root, "etc/actual.conf", volumioConf(t, r.music)+"\nvolume_normalization \"yes\"\n")
			name := filepath.Join(r.procRoot, strconv.Itoa(pid), "cmdline")
			switch kind {
			case "directory":
				if err := os.Mkdir(name, 0755); err != nil {
					t.Fatal(err)
				}
			case "outside link":
				outside := filepath.Join(t.TempDir(), "cmdline")
				if err := os.WriteFile(outside, []byte("mpd\x00/etc/actual.conf\x00"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, name); err != nil {
					t.Skip(err)
				}
			case "permission denied":
				if runtime.GOOS == "windows" || os.Geteuid() == 0 {
					t.Skip("requires POSIX permissions and a non-root user")
				}
				if err := os.WriteFile(name, []byte("mpd\x00/etc/actual.conf\x00"), 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(name, 0644) })
			}
			wantProvenance(t, provenanceReading(t, r), "configured_fallback", r.confPath, "no")
		})
	}
}

func TestReadingDefaultConfigRespectsXDGPrecedence(t *testing.T) {
	for _, xdgExists := range []bool{false, true} {
		t.Run(fmt.Sprint(xdgExists), func(t *testing.T) {
			r, root, pid := provenanceRig(t)
			writeRigFile(t, r.procRoot, filepath.Join(strconv.Itoa(pid), "environ"), "HOME=/home/pi\x00XDG_CONFIG_HOME=/xdg\x00")
			r.cfg.Getenv = func(k string) string {
				switch k {
				case "HOME":
					return "/home/pi"
				case "XDG_CONFIG_HOME":
					return "/xdg"
				}
				return ""
			}
			writeRigFile(t, root, "home/pi/.config/mpd/mpd.conf", volumioConf(t, r.music)+"\nvolume_normalization \"yes\"\n")
			selected := "/home/pi/.config/mpd/mpd.conf"
			if xdgExists {
				if err := os.MkdirAll(filepath.Join(root, "xdg"), 0755); err != nil {
					t.Fatal(err)
				}
				selected = "/home/pi/.mpdconf"
			}
			writeRigFile(t, root, "home/pi/.mpdconf", volumioConf(t, r.music)+"\nvolume_normalization \"yes\"\n")
			writeRigFile(t, r.procRoot, filepath.Join(strconv.Itoa(pid), "cmdline"), "mpd\x00")
			wantProvenance(t, provenanceReading(t, r), "mpd_default", selected, "yes")
		})
	}
}

func TestReadingConfigProvenanceOnTheWire(t *testing.T) {
	r, root, pid := provenanceRig(t)
	writeRigFile(t, root, "etc/actual.conf", volumioConf(t, r.music)+"\nvolume_normalization \"yes\"\n")
	writeRigFile(t, r.procRoot, filepath.Join(strconv.Itoa(pid), "cmdline"), "mpd\x00/etc/actual.conf\x00")
	store := outputsToken(t)
	token, _, err := store.Mint("phone")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(&config.Config{LibraryRoots: []string{r.root}}, store, nil, "test").WithLocalOutputs(r.cfg)
	t.Cleanup(srv.localOutputs.close)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, rr.Code, rr.Body.String())
		}
		return rr
	}
	var outputs OutputsResponse
	if err := json.Unmarshal(get("/v1/outputs").Body.Bytes(), &outputs); err != nil {
		t.Fatal(err)
	}
	if len(outputs.Outputs) != 1 {
		t.Fatalf("outputs=%+v", outputs)
	}
	var got OutputReading
	if err := json.Unmarshal(get("/v1/outputs/"+outputs.Outputs[0].ID+"/reading").Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	wantProvenance(t, got, "process_cmdline", "/etc/actual.conf", "yes")
}

func TestReadingProcessConfigDoesNotChangeTheVerdictInputs(t *testing.T) {
	r, root, pid := provenanceRig(t)
	r.mpd.set(func(m *scriptedMPD) {
		m.outputs = volumioOutputs("1")
		m.statuses = []mpd.Reply{withPair(capturedStatus(t, piCaptures[0].name), "audio", "dsd64:2")}
	})
	// With no live dop, use the process file for the veto; keep row mapping.
	r.writeConf(regexp.MustCompile(`(?m)^\s*dop\s+"no"`).ReplaceAllString(volumioConf(t, r.music), `dop "yes"`))
	writeRigFile(t, root, "etc/actual.conf", volumioConf(t, "/not-the-library")+"\nvolume_normalization \"yes\"\n")
	writeRigFile(t, r.procRoot, filepath.Join(strconv.Itoa(pid), "cmdline"), "mpd\x00/etc/actual.conf\x00")
	o := newLocalOutputs(r.cfg)
	t.Cleanup(o.close)
	snap := o.takeReading(context.Background())
	if !snap.confRead || snap.conf.VolumeNormalization != "yes" {
		t.Fatal("process config not selected")
	}
	pick := &snap.picks[0]
	sub, _ := pickSubstream(snap.cards, pick)
	hs := holders(snap, sub)
	if len(hs) != 1 {
		t.Fatal("no holder")
	}
	if _, live := hs[0].Attribute("dop"); live {
		t.Fatal("fixture must have no live dop")
	}
	if got := holderOutput(snap, hs[0]).DoP; got {
		t.Error("DoP veto used configured yes, want process no")
	}
	if snap.row == nil || snap.row.track.Codec != "FLAC" {
		t.Errorf("process config changed row mapping: %+v", snap.row)
	}
}
