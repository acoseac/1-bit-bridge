package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/mpd"
)

func processStat(pid int, start string) string {
	// Fields 3 through 21, then field 22; comm may itself contain ')'.
	return fmt.Sprintf("%d (mpd ) worker) S %s%s 0\n", pid, strings.Repeat("0 ", 18), start)
}

func seedProcessEvidence(t *testing.T, r *readingRig, pid int) {
	t.Helper()
	dir := strconv.Itoa(pid)
	writeRigFile(t, r.procRoot, dir+"/stat", processStat(pid, "12345"))
	writeRigFile(t, r.procRoot, dir+"/mountinfo", "1 0 8:1 / / rw - ext4 /dev/root rw\n")
	writeRigFile(t, r.procRoot, "self/mountinfo", "2 0 8:1 / / rw - ext4 /dev/root rw\n")
}

func TestReadingRedactsEveryConfigRead(t *testing.T) {
	for _, secretSource := range []string{"configured", "include", "output password"} {
		t.Run(secretSource, func(t *testing.T) {
			r, root, pid := provenanceRig(t)
			secret := "old-secret"
			configured := withOutputSetting(volumioConf(t, r.music), "mixer_control", secret)
			switch secretSource {
			case "configured":
				configured = "password \"" + secret + "@read\"\n" + configured
			case "include":
				writeRigFile(t, filepath.Dir(r.confPath), "secrets.conf", "password \""+secret+"@read\"\n")
				configured = "include \"secrets.conf\"\n" + configured
			case "output password":
				r.cfg.Output = func() config.OutputConfig { return config.OutputConfig{MPD: "on", Password: secret} }
			}
			r.writeConf(configured)
			writeRigFile(t, root, "etc/old-secret.conf", "volume_normalization \"yes\"\n")
			writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/cmdline", "mpd\x00/etc/old-secret.conf\x00")
			got := provenanceReading(t, r)
			if got.DAC == nil || got.DAC.Mixer == nil {
				t.Fatal("missing mixer")
			}
			if got.DAC.Mixer.Control != "[redacted]" {
				t.Errorf("mixer control leaked: %q", got.DAC.Mixer.Control)
			}
			wantHint(t, got, "different", "/etc/[redacted].conf")
			b, _ := json.Marshal(got)
			if strings.Contains(string(b), secret) {
				t.Error("reading leaked a config secret")
			}
		})
	}
}

func TestReadingRejectsUnprovenProcess(t *testing.T) {
	for _, kind := range []string{"missing stat", "changed start", "not mpd", "different mount root", "different mount device", "missing mountinfo", "different root inode", "same root inode"} {
		t.Run(kind, func(t *testing.T) {
			r, root, pid := provenanceRig(t)
			dir := strconv.Itoa(pid)
			writeRigFile(t, root, "etc/actual.conf", volumioConf(t, r.music)+"\nvolume_normalization \"yes\"\n")
			args := "mpd\x00/etc/actual.conf\x00"
			if kind == "not mpd" {
				args = "some-daemon\x00/etc/actual.conf\x00"
			}
			writeRigFile(t, r.procRoot, dir+"/cmdline", args)
			switch kind {
			case "missing stat":
				if err := os.Remove(filepath.Join(r.procRoot, dir, "stat")); err != nil {
					t.Fatal(err)
				}
			case "different mount root":
				writeRigFile(t, r.procRoot, dir+"/mountinfo", "1 0 8:1 /container / rw - ext4 /dev/root rw\n")
			case "different mount device":
				writeRigFile(t, r.procRoot, dir+"/mountinfo", "1 0 8:2 / / rw - ext4 /dev/root rw\n")
			case "missing mountinfo":
				if err := os.Remove(filepath.Join(r.procRoot, dir, "mountinfo")); err != nil {
					t.Fatal(err)
				}
			case "different root inode", "same root inode":
				target := root
				if kind == "different root inode" {
					target = t.TempDir()
				}
				for name, dest := range map[string]string{dir + "/root": target, "self/root": root} {
					if err := os.Symlink(dest, filepath.Join(r.procRoot, name)); err != nil {
						t.Skip(err)
					}
				}
			}
			got := provenanceReading(t, r, func(o *localOutputs) {
				read := o.cmdline
				o.cmdline = func(proc, logical string, limit int64) ([]byte, error) {
					b, err := read(proc, logical, limit)
					if kind == "changed start" {
						writeRigFile(t, r.procRoot, dir+"/stat", processStat(pid, "67890"))
					}
					return b, err
				}
			})
			if kind == "same root inode" {
				wantConfigAndHint(t, got, "different", "/etc/actual.conf", "no")
			} else {
				wantConfigAndHint(t, got, "", "", "no")
			}
		})
	}
}

func TestReadingUsesOnlyTheBoundCardsOwner(t *testing.T) {
	for _, kind := range []string{"other card only", "other card also", "two on bound card", "other PCM on bound card"} {
		t.Run(kind, func(t *testing.T) {
			r, root, pid := provenanceRig(t)
			writeRigFile(t, root, "etc/actual.conf", volumioConf(t, r.music)+"\nvolume_normalization \"yes\"\n")
			writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/cmdline", "mpd\x00/etc/actual.conf\x00")
			otherPID := pid + 100
			seedProcessEvidence(t, r, otherPID)
			writeRigFile(t, r.procRoot, strconv.Itoa(otherPID)+"/cmdline", "mpd\x00/etc/actual.conf\x00")
			read := r.cards
			r.cards = func() (alsa.Snapshot, error) {
				snap, err := read()
				for i := range snap.Cards {
					if snap.Cards[i].Index != 5 {
						continue
					}
					card := snap.Cards[i]
					// Make independent slices before changing the copied owner.
					card.Devices = append([]alsa.Device(nil), card.Devices...)
					card.Devices[0].Substreams = append([]alsa.Substream(nil), card.Devices[0].Substreams...)
					sub := card.Devices[0].Substreams[0]
					owner := *sub.Owner
					owner.PID = &otherPID
					sub.Owner = &owner
					if kind == "other PCM on bound card" {
						dev := card.Devices[0]
						dev.Device = 1
						dev.Substreams = []alsa.Substream{sub}
						snap.Cards[i].Devices = append(snap.Cards[i].Devices, dev)
					} else if kind == "two on bound card" {
						snap.Cards[i].Devices[0].Substreams = append(snap.Cards[i].Devices[0].Substreams, sub)
					} else {
						card.Index = 9
						card.ID = "other"
						card.Devices[0].Substreams[0] = sub
						snap.Cards = append(snap.Cards, card)
						if kind == "other card only" {
							snap.Cards[i].Devices[0].Substreams[0].Owner = nil
						}
					}
					break
				}
				return snap, err
			}
			got := provenanceReading(t, r)
			if kind == "other card also" {
				wantConfigAndHint(t, got, "different", "/etc/actual.conf", "no")
			} else {
				wantConfigAndHint(t, got, "", "", "no")
			}
		})
	}
}

func TestReadingMPDOptionGrammar(t *testing.T) {
	cases := map[string]string{
		"trailing flag": "/etc/actual.conf\x00--stderr", "stdout": "--stdout\x00/etc/actual.conf",
		"equals": "--verbose=anything\x00/etc/actual.conf", "help alias": "-?\x00/etc/actual.conf",
		"no config": "--no-config\x00/etc/actual.conf", "no config equals": "--no-config=yes",
		"terminator": "--\x00/etc/actual.conf", "dot": "/etc/./actual.conf", "dotdot": "/etc/../etc/actual.conf", "double slash": "/etc//actual.conf",
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			r, root, pid := provenanceRig(t)
			writeRigFile(t, root, "etc/actual.conf", volumioConf(t, r.music)+"\nvolume_normalization \"yes\"\n")
			writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/cmdline", "mpd\x00"+args+"\x00")
			got := provenanceReading(t, r)
			switch name {
			case "no config", "no config equals":
				wantConfigAndHint(t, got, "no_config", "", "no")
			case "terminator", "dot", "dotdot", "double slash":
				wantConfigAndHint(t, got, "", "", "no")
			default:
				wantConfigAndHint(t, got, "different", "/etc/actual.conf", "no")
			}
		})
	}
}

func TestReadingRejectsCmdlineTruncatedAtANUL(t *testing.T) {
	r, root, pid := provenanceRig(t)
	writeRigFile(t, root, "etc/actual.conf", volumioConf(t, r.music)+"\nvolume_normalization \"yes\"\n")
	// The first cap bytes form valid argv, ending in NUL; one extra operand follows.
	prefix := "mpd\x00--verbose=" + strings.Repeat("x", (64<<10)-len("mpd\x00--verbose=\x00/etc/actual.conf\x00")) + "\x00/etc/actual.conf\x00"
	writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/cmdline", prefix+"/extra\x00")
	wantConfigAndHint(t, provenanceReading(t, r), "", "", "no")
}

func TestReadingWithoutConfigOrArgvOmitsHint(t *testing.T) {
	for _, kind := range []string{"missing", "empty path"} {
		t.Run(kind, func(t *testing.T) {
			r, _, _ := provenanceRig(t)
			if err := os.Remove(r.confPath); err != nil {
				t.Fatal(err)
			}
			if kind == "empty path" {
				r.confPath = ""
			}
			got := provenanceReading(t, r)
			if got.MPD == nil || got.MPD.Config != nil || got.MPD.ConfigHint != nil {
				t.Fatalf("unread config=%+v", got.MPD)
			}
			b, _ := json.Marshal(got.MPD)
			var wire map[string]json.RawMessage
			_ = json.Unmarshal(b, &wire)
			if _, present := wire["configHint"]; present {
				t.Error("unread provenance is present")
			}
		})
	}
}

func TestReadingRedactsOtherEmittedStrings(t *testing.T) {
	r, root, pid := provenanceRig(t)
	r.writeConf("password \"old-secret@read\"\n" + volumioConf(t, r.music))
	writeRigFile(t, root, "etc/actual.conf", volumioConf(t, r.music))
	writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/cmdline", "mpd\x00/etc/actual.conf\x00")
	o := newLocalOutputs(r.cfg)
	t.Cleanup(o.close)
	s := o.takeReading(context.Background())
	s.row.path = "old-secret/song.flac"
	s.row.track.Codec = "old-secret"
	s.picks[0].dac.ID = "old-secret"
	id := outputID(o.identity, s.endpoint, s.picks[0].dac.ID, s.picks[0].dac.USBID)
	got, found := o.readingView(s, id)
	if !found {
		t.Fatal("missing view")
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "old-secret") {
		t.Error("non-MPD fields leaked config secret")
	}
}

func TestReadingCachesHintPerCard(t *testing.T) {
	r, root, pid := provenanceRig(t)
	r.writeConf("password \"second-secret@read\"\n" + withOutputSetting(volumioConf(t, r.music), "mixer_control", "second-secret"))
	now := r.cfg.Now()
	r.cfg.Now = func() time.Time { return now }
	otherPID := pid + 100
	seedProcessEvidence(t, r, otherPID)
	writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/cmdline", "mpd\x00/etc/first.conf\x00")
	writeRigFile(t, r.procRoot, strconv.Itoa(otherPID)+"/cmdline", "mpd\x00/etc/second.conf\x00")
	writeRigFile(t, root, "etc/first.conf", withOutputSetting(volumioConf(t, r.music), "mixer_control", "second-secret")+"\nvolume_normalization \"yes\"\n")
	writeRigFile(t, root, "etc/second.conf", "password \"second-secret@read\"\n"+volumioConf(t, r.music))
	read := r.cards
	r.cards = func() (alsa.Snapshot, error) {
		snap, err := read()
		for _, c := range snap.Cards {
			if c.Index == 5 {
				c.Index = 9
				c.ID = "other"
				c.Devices = append([]alsa.Device(nil), c.Devices...)
				c.Devices[0].Substreams = append([]alsa.Substream(nil), c.Devices[0].Substreams...)
				owner := *c.Devices[0].Substreams[0].Owner
				owner.PID = &otherPID
				c.Devices[0].Substreams[0].Owner = &owner
				snap.Cards = append(snap.Cards, c)
				break
			}
		}
		return snap, err
	}
	inspect := r.cfg.Inspect
	r.cfg.Inspect = func(ctx context.Context, opt mpd.Options) mpd.Report {
		rep := inspect(ctx, opt)
		rep.Outputs = []mpd.Output{{Name: "alsa", Type: "alsa", Device: "hw:5,0", Enabled: "yes"}, {Name: "alsa", Type: "alsa", Device: "hw:9,0", Enabled: "yes"}}
		return rep
	}
	o := newLocalOutputs(r.cfg)
	t.Cleanup(o.close)
	s, err := o.readingSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.picks) != 2 {
		t.Fatalf("picks=%+v", s.picks)
	}
	for _, pick := range s.picks {
		id := outputID(o.identity, s.endpoint, pick.dac.ID, pick.dac.USBID)
		got, found, err := o.Reading(context.Background(), id)
		if err != nil || !found {
			t.Fatalf("reading: %v %v", found, err)
		}
		wantPath := "/etc/first.conf"
		if pick.dac.Card == 9 {
			wantPath = "/etc/second.conf"
		}
		wantConfigAndHint(t, got, "different", wantPath, "no")
		b, _ := json.Marshal(got)
		if strings.Contains(string(b), "second-secret") {
			t.Error("configured secret leaked")
		}
	}
	if r.reads.Load() != 1 {
		t.Errorf("cache read cards %d times", r.reads.Load())
	}
}
