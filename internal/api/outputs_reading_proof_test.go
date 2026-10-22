package api

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/mpd"
	"github.com/acoseac/1-bit-bridge/internal/mpd/conf"
	"github.com/acoseac/1-bit-bridge/internal/signalpath"
)

// These tests hold the verdict to what ties the stream at the card to the
// MPD the bridge dialled: the output whose thread holds the card, and what
// MPD reports read on both sides of the /proc read; and every step that
// reads a file to its own deadline (B473 B3 rounds 2 and 3). What mpd.conf
// and the ALSA files say is information, read for the warnings and the
// output shown, and no verdict waits on it.

// volumioNameLine is the name line of Volumio's alsa output.
var volumioNameLine = regexp.MustCompile(`(?m)^(\s*)name(\s+)"alsa"[ \t]*$`)

// withName renames Volumio's alsa output.
func withName(conf, name string) string {
	return volumioNameLine.ReplaceAllString(conf, `${1}name${2}"`+name+`"`)
}

// appendAsound adds text to the box's etc/asound.conf.
func appendAsound(t *testing.T, r *readingRig, text string) {
	t.Helper()
	etc := filepath.Join(filepath.Dir(r.procRoot), "etc", "asound.conf")
	b, err := os.ReadFile(etc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(etc, append(b, []byte("\n"+text)...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ownerThread renames the thread holding card 5 (the capture's 6584).
func ownerThread(t *testing.T, r *readingRig, name string) {
	t.Helper()
	writeRigFile(t, r.procRoot, "6584/status", "Name:\t"+name+"\nUmask:\t0022\nState:\tS (sleeping)\nTgid:\t6551\nPid:\t6584\nUid:\t1000\t1000\t1000\t1000\n")
}

// liveOutputs is an outputs reply: each output is name, plugin and
// enabled ("1" or "0"), in order.
func liveOutputs(outputs ...[3]string) mpd.Reply {
	var kv []string
	for i, o := range outputs {
		kv = append(kv, "outputid", itoa(i), "outputname", o[0], "plugin", o[1], "outputenabled", o[2])
	}
	return replyOf(kv...)
}

// TestMPDConfIsInformationNotProof: no verdict waits on mpd.conf being the
// configuration MPD runs with, which the bridge cannot show from outside
// it. A file whose outputs are not the ones MPD reports, or one read in
// part, still leaves the format the kernel shows the verdict, and what the
// file does say, of the parts read, is shown and warned of as read from
// mpd.conf.
func TestMPDConfIsInformationNotProof(t *testing.T) {
	c := piCaptures[0]
	normalized := func(t *testing.T, r *readingRig) string {
		return normalizationLine.ReplaceAllString(volumioConf(t, r.music), `volume_normalization "yes"`)
	}
	for _, tc := range []struct {
		name    string
		conf    func(t *testing.T, r *readingRig) string
		outputs mpd.Reply
	}{
		{"an output the file does not define", normalized,
			liveOutputs([3]string{"alsa", "alsa", "1"}, [3]string{"multiroom", "fifo", "0"}, [3]string{"bt", "alsa", "0"})},
		{"an output the file defines and MPD does not report", normalized, liveOutputs([3]string{"alsa", "alsa", "1"})},
		{"the outputs in another order", normalized,
			liveOutputs([3]string{"multiroom", "fifo", "0"}, [3]string{"alsa", "alsa", "1"})},
		{"an include that is not there", func(t *testing.T, r *readingRig) string {
			return normalized(t, r) + "include \"no-such.conf\"\n"
		}, volumioOutputs("1", "allowed_formats=", "dop=0")},
		{"an include pattern, which MPD expands", func(t *testing.T, r *readingRig) string {
			writeRigFile(t, filepath.Dir(r.confPath), "conf.d/soft.conf", "mixer_type \"software\"\n")
			return normalized(t, r) + "include_optional \"conf.d/*.conf\"\n"
		}, volumioOutputs("1", "allowed_formats=", "dop=0")},
		{"a line that does not parse", func(t *testing.T, r *readingRig) string {
			return normalized(t, r) + "audio_output_format \"48000:24:2\n"
		}, volumioOutputs("1", "allowed_formats=", "dop=0")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
			hs, token, id := rig.serve()
			rig.writeConf(tc.conf(t, rig))
			rig.mpd.set(func(m *scriptedMPD) { m.outputs = tc.outputs })
			_, got, raw := getReading(t, hs, token, id)
			if got.State != "whole" {
				t.Fatalf("state %s %v\n%s", got.State, got.StateReasons, raw)
			}
			wantVerdict(t, got, raw, signalpath.FormatPreserved)
			wantWarnings(t, got, raw, warning(signalpath.MPDNormalization, signalpath.FromMPDConf))
			if got.MPD.Config == nil || got.MPD.Config.VolumeNormalization != "yes" || got.MPD.Config.Output == nil || got.MPD.Config.Output.Device != "volumio" {
				t.Fatalf("the file as read: %+v\n%s", got.MPD.Config, raw)
			}
		})
	}
}

// TestASettingThatChangesAcrossTheReadSettles: the outputs (switched on or
// off, an attribute set with outputset), replay gain, the audio format,
// crossfade and MixRamp are read before the /proc read and after it, and a
// reading whose two reads disagree is settling: the setting the verdict
// would judge is not the one that held while the card was read.
func TestASettingThatChangesAcrossTheReadSettles(t *testing.T) {
	c := piCaptures[0]
	captured := capturedStatus(t, c.name)
	usual := volumioOutputs("1", "allowed_formats=", "dop=0")
	for _, tc := range []struct {
		name     string
		statuses []mpd.Reply
		outputs  []mpd.Reply
		gains    []mpd.Reply
	}{
		{name: "an output switched on", outputs: []mpd.Reply{usual, liveOutputs([3]string{"alsa", "alsa", "1"}, [3]string{"multiroom", "fifo", "1"})}},
		{name: "allowed_formats set", outputs: []mpd.Reply{usual, volumioOutputs("1", "allowed_formats=96000:16:*", "dop=0")}},
		{name: "replay gain switched on", gains: []mpd.Reply{replyOf("replay_gain_mode", "off"), replyOf("replay_gain_mode", "track")}},
		{name: "crossfade set", statuses: []mpd.Reply{captured, withPair(captured, "xfade", "5")}},
		{name: "another audio format", statuses: []mpd.Reply{captured, withPair(captured, "audio", "44100:24:2")}},
		{name: "MixRamp set", statuses: []mpd.Reply{captured, withPair(captured, "mixrampdelay", "2")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newReadingRig(t, c.name, c.file, c.track, captured)
			hs, token, id := rig.serve()
			rig.mpd.set(func(m *scriptedMPD) {
				m.statuses, m.nStatus = []mpd.Reply{captured}, 0
				if tc.statuses != nil {
					m.statuses = tc.statuses
				}
				m.outputsSeq, m.nOutputs = tc.outputs, 0
				m.gainSeq, m.nGain = tc.gains, 0
			})
			_, got, raw := getReading(t, hs, token, id)
			if got.State != "settling" || !slices.Equal(got.StateReasons, []string{signalpath.SettingsChanged}) {
				t.Fatalf("state %s %v\n%s", got.State, got.StateReasons, raw)
			}
			wantVerdict(t, got, raw, signalpath.Withheld, signalpath.SettingsChanged)
		})
	}
}

// TestTheOutputWhoseThreadHoldsTheCardIsTheOneJudged: MPD names each
// output's thread "output:" and the output's name, cut to 15 bytes, and the
// thread that holds the card is the one /proc names. That output is the one
// judged and warned of, and the one shown: a second output on the card
// through a softvol PCM that holds it is not hidden behind the one whose
// chain the bridge resolves; a thread that is no enabled output's, or one
// MPD reports no outputs beside, is not this MPD's stream; two outputs whose
// names are alike in 15 bytes cannot be told apart; a long name is cut as
// MPD cuts it; and an output of another plugin is still MPD's stream.
func TestTheOutputWhoseThreadHoldsTheCardIsTheOneJudged(t *testing.T) {
	c := piCaptures[0]
	soft := "audio_output {\n\ttype \"alsa\"\n\tname \"soft\"\n\tdevice \"softdev\"\n}\n"
	softvol := "pcm.softdev {\n    type softvol\n    slave.pcm \"volumioHw\"\n    control.name \"Soft\"\n}\n"
	twoOutputs := liveOutputs([3]string{"alsa", "alsa", "1"}, [3]string{"multiroom", "fifo", "0"}, [3]string{"soft", "alsa", "1"})
	softWarning := []ReadingWarning{warning(signalpath.ALSASoftvol, signalpath.FromALSAConf)}
	for _, tc := range []struct {
		name     string
		conf     func(string) string
		asound   string
		outputs  mpd.Reply
		thread   string
		value    string
		codes    []string
		warnings []ReadingWarning
		shown    string
	}{
		{"the output whose thread holds the card", func(s string) string { return s + soft }, softvol, twoOutputs, "output:alsa",
			signalpath.FormatPreserved, nil, nil, "alsa"},
		{"a softvol output on the same card", func(s string) string { return s + soft }, softvol, twoOutputs, "output:soft",
			signalpath.FormatPreserved, nil, softWarning, "soft"},
		{"a thread that is no enabled output's", func(s string) string { return s }, "", volumioOutputs("1", "allowed_formats=", "dop=0"), "output:other",
			signalpath.Withheld, []string{signalpath.OwnerNotOutput}, nil, "alsa"},
		// The file's enabled is not what MPD runs with: with no live output,
		// nothing is the thread's output.
		{"no output reported", func(s string) string { return s }, "", replyOf(), "output:alsa",
			signalpath.Withheld, []string{signalpath.OwnerNotOutput}, nil, ""},
		{"two names alike in 15 bytes", func(s string) string {
			return withName(s, "alsa-usb-front") + "audio_output {\n\ttype \"alsa\"\n\tname \"alsa-usb-rear\"\n\tdevice \"hw:CARD=Audio,DEV=0\"\n}\n"
		}, "", liveOutputs([3]string{"alsa-usb-front", "alsa", "1"}, [3]string{"multiroom", "fifo", "0"}, [3]string{"alsa-usb-rear", "alsa", "1"}),
			"output:alsa-usb", signalpath.Withheld, []string{signalpath.MPDOutputAmbiguous}, nil, ""},
		{"a long name, cut as MPD cuts it", func(s string) string { return withName(s, "USB DAC on card five") }, "",
			liveOutputs([3]string{"USB DAC on card five", "alsa", "1"}, [3]string{"multiroom", "fifo", "0"}), "output:USB DAC",
			signalpath.FormatPreserved, nil, nil, "USB DAC on card five"},
		{"an output of another plugin", func(s string) string {
			return strings.Replace(s, "\t\ttype\t\t\"alsa\"", "\t\ttype\t\t\"ao\"", 1)
		}, "", liveOutputs([3]string{"alsa", "ao", "1"}, [3]string{"multiroom", "fifo", "0"}), "output:alsa",
			signalpath.FormatPreserved, nil, nil, "alsa"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
			if tc.asound != "" {
				appendAsound(t, rig, tc.asound)
			}
			rig.writeConf(tc.conf(volumioConf(t, rig.music)))
			rig.mpd.outputs = tc.outputs
			ownerThread(t, rig, tc.thread)
			hs, token, id := rig.serve()
			_, got, raw := getReading(t, hs, token, id)
			if got.State != "whole" {
				t.Fatalf("state %s %v\n%s", got.State, got.StateReasons, raw)
			}
			wantVerdict(t, got, raw, tc.value, tc.codes...)
			wantWarnings(t, got, raw, tc.warnings...)
			shown := ""
			if got.MPD != nil && got.MPD.Config != nil && got.MPD.Config.Output != nil {
				shown = got.MPD.Config.Output.Name
			}
			if shown != tc.shown {
				t.Fatalf("the output shown is %q, want %q\n%s", shown, tc.shown, raw)
			}
		})
	}
}

// TestTheChainShownReachesThisCard: the chain shown for the output whose
// thread holds the card is the stages from its device to this card and PCM
// device; a device that reaches another card, or another PCM device of this
// one, shows no chain, since its stages are not the way to this card.
func TestTheChainShownReachesThisCard(t *testing.T) {
	c := piCaptures[0]
	for _, tc := range []struct {
		device string
		chain  []string
	}{
		{"volumio", []string{"empty", "plug", "hw"}},
		{"hw:CARD=Audio,DEV=0", []string{"hw"}},
		// The HDMI card, not the card whose substream the thread holds.
		{"hw:0,0", []string{}},
		// Another PCM device of this card.
		{"hw:CARD=Audio,DEV=1", []string{}},
	} {
		t.Run(tc.device, func(t *testing.T) {
			rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
			rig.writeConf(withDevice(volumioConf(t, rig.music), tc.device))
			hs, token, id := rig.serve()
			_, got, raw := getReading(t, hs, token, id)
			if got.MPD == nil || got.MPD.Config == nil || got.MPD.Config.Output == nil || got.MPD.Config.Output.Name != "alsa" {
				t.Fatalf("no output shown:\n%s", raw)
			}
			if chain := got.MPD.Config.Output.Chain; !slices.Equal(chain, tc.chain) || chain == nil {
				t.Fatalf("chain %v, want %v\n%s", chain, tc.chain, raw)
			}
		})
	}
}

// TestTheReadingShowsAnOutputMPDReportsEnabled: with nothing holding the
// card, the output a reading shows for it is the one enabled alsa output
// whose chain reaches it, enabled as MPD reports it, never as the file says.
func TestTheReadingShowsAnOutputMPDReportsEnabled(t *testing.T) {
	replies := volumioReplies(t)
	for _, tc := range []struct {
		enabled string
		shown   bool
	}{{"1", true}, {"0", false}} {
		rig := newReadingRig(t, "idle", "Album/01.flac", flacRow(44100, 16), replies["status"])
		rig.mpd.current = replies["currentsong"]
		rig.mpd.outputs = volumioOutputs(tc.enabled, "allowed_formats=", "dop=0")
		hs, token, id := rig.serve()
		_, got, raw := getReading(t, hs, token, id)
		if got.MPD == nil || got.MPD.Config == nil || (got.MPD.Config.Output != nil) != tc.shown {
			t.Fatalf("alsa enabled %s: the output shown %+v\n%s", tc.enabled, got.MPD, raw)
		}
	}
}

// stepRig is a rig whose reading's deadlines are short and whose whole
// reading's deadline is far off, so only a step's own deadline can end it
// in time, with a server to set them on.
func stepRig(t *testing.T) (*readingRig, *Server, string, string) {
	t.Helper()
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	clock := &manualClock{now: time.Now()}
	rig.cfg.Now, rig.cfg.AfterFunc = clock.Now, clock.AfterFunc
	srv, hs, token := newReadingServer(t, rig.cfg)
	id := firstOutputID(t, hs, token)
	srv.localOutputs.limits.build = 30 * time.Second
	return rig, srv, token, id
}

// readingWithin takes one reading through the server's handler and fails
// the test when it has not answered within bound.
func readingWithin(t *testing.T, srv *Server, id string, bound time.Duration) (OutputReading, time.Duration) {
	t.Helper()
	type answer struct {
		r   OutputReading
		err error
	}
	done := make(chan answer, 1)
	start := time.Now()
	go func() {
		r, _, err := srv.localOutputs.Reading(context.Background(), id)
		done <- answer{r, err}
	}()
	select {
	case a := <-done:
		if a.err != nil {
			t.Fatal(a.err)
		}
		return a.r, time.Since(start)
	case <-time.After(bound):
		t.Fatalf("the reading had not answered after %s", bound)
		return OutputReading{}, 0
	}
}

// TestAConfigReadPastItsDeadlineIsAnHonestUnknown: mpd.conf and its
// includes are read under their own deadline, on a goroutine of their own:
// a read that hangs (an include on a dead mount) leaves the reading with
// mpd.conf unread, shown as null, in time, and is not doubled by the next
// reading. No verdict waits on the file.
func TestAConfigReadPastItsDeadlineIsAnHonestUnknown(t *testing.T) {
	_, srv, _, id := stepRig(t)
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	var reads atomic.Int32
	srv.localOutputs.confFile = func(root, logical, host string) (conf.File, error) {
		reads.Add(1)
		<-stuck
		return conf.ReadFile(root, logical, host)
	}
	srv.localOutputs.limits.conf = 50 * time.Millisecond
	got, took := readingWithin(t, srv, id, 5*time.Second)
	if took < 50*time.Millisecond || took > time.Second {
		t.Fatalf("the reading took %s with a 50ms deadline on mpd.conf", took)
	}
	if got.State != "whole" || got.Verdict.Value != signalpath.FormatPreserved || len(got.Verdict.Warnings) != 0 {
		t.Fatalf("state %s verdict %+v", got.State, got.Verdict)
	}
	if got.MPD == nil || got.MPD.Config != nil {
		t.Fatalf("mpd %+v", got.MPD)
	}
	srv.localOutputs.mu.Lock()
	srv.localOutputs.reading.snap = nil
	srv.localOutputs.mu.Unlock()
	readingWithin(t, srv, id, 5*time.Second)
	if n := reads.Load(); n != 1 {
		t.Fatalf("mpd.conf was read %d times while one read hung", n)
	}
}

// TestAnALSAConfigurationReadPastItsDeadline: the ALSA configuration is
// read once a reading, under its own deadline: one that hangs ends the
// reading in time, an honest unknown (alsa_config_unread) that still
// reports MPD, and is not doubled.
func TestAnALSAConfigurationReadPastItsDeadline(t *testing.T) {
	_, srv, _, id := stepRig(t)
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	var reads atomic.Int32
	srv.localOutputs.alsaConfig = func(root string) alsa.PCMConfig {
		reads.Add(1)
		<-stuck
		return alsa.LoadPCMConfig(root)
	}
	srv.localOutputs.limits.alsa = 50 * time.Millisecond
	got, took := readingWithin(t, srv, id, 5*time.Second)
	if took < 50*time.Millisecond || took > time.Second {
		t.Fatalf("the reading took %s with a 50ms deadline on the ALSA configuration", took)
	}
	if got.State != "unknown" || !slices.Equal(got.StateReasons, []string{signalpath.ALSAConfigUnread}) || got.DAC != nil || got.MPD == nil {
		t.Fatalf("state %s %v dac %+v mpd %+v", got.State, got.StateReasons, got.DAC, got.MPD)
	}
	srv.localOutputs.mu.Lock()
	srv.localOutputs.reading.snap = nil
	srv.localOutputs.mu.Unlock()
	readingWithin(t, srv, id, 5*time.Second)
	if n := reads.Load(); n != 1 {
		t.Fatalf("the ALSA configuration was read %d times while one read hung", n)
	}
}

// TestAnALSAConfigurationNotHadLeavesEveryIdUnknown: which card an output
// id names can come from the ALSA configuration alone: Volumio's volumio
// PCM pointed at a card that is not USB (an I2S HAT, here the HDMI card) is
// listed by resolving that name. A reading whose ALSA configuration read ran
// past its deadline, or failed, cannot tell that id from one that names no
// output, so it answers every well-formed id unknown with
// alsa_config_unread, never a 404 for an output that is there.
func TestAnALSAConfigurationNotHadLeavesEveryIdUnknown(t *testing.T) {
	for name, read := range map[string]func(stuck <-chan struct{}) func(string) alsa.PCMConfig{
		"late": func(stuck <-chan struct{}) func(string) alsa.PCMConfig {
			return func(root string) alsa.PCMConfig { <-stuck; return alsa.LoadPCMConfig(root) }
		},
		"failed": func(<-chan struct{}) func(string) alsa.PCMConfig {
			return func(string) alsa.PCMConfig { panic("an ALSA configuration read that panics") }
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := piCaptures[0]
			rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
			etc := filepath.Join(filepath.Dir(rig.procRoot), "etc", "asound.conf")
			b, err := os.ReadFile(etc)
			if err != nil {
				t.Fatal(err)
			}
			hat := strings.Replace(string(b), `card "Audio"`, `card "vc4hdmi0"`, 1)
			if hat == string(b) {
				t.Fatal("the copied asound.conf names no card \"Audio\"")
			}
			if err := os.WriteFile(etc, []byte(hat), 0o644); err != nil {
				t.Fatal(err)
			}
			clock := &manualClock{now: time.Now()}
			rig.cfg.Now, rig.cfg.AfterFunc = clock.Now, clock.AfterFunc
			srv, hs, token := newReadingServer(t, rig.cfg)
			resp := outputsGet(t, hs, token, "/v1/outputs")
			body, raw := decodeOutputs(t, resp)
			if len(body.Outputs) != 1 || body.Outputs[0].DAC == nil || body.Outputs[0].DAC.Card != 0 {
				t.Fatalf("the outputs route does not list the card the PCM name resolves to: %s", raw)
			}
			id := body.Outputs[0].ID
			stuck := make(chan struct{})
			t.Cleanup(func() { close(stuck) })
			srv.localOutputs.alsaConfig = read(stuck)
			srv.localOutputs.limits.alsa = 50 * time.Millisecond
			got, found, err := srv.localOutputs.Reading(context.Background(), id)
			if err != nil || !found {
				t.Fatalf("an output that is there: found %v err %v", found, err)
			}
			if got.State != "unknown" || !slices.Equal(got.StateReasons, []string{signalpath.ALSAConfigUnread}) || got.DAC != nil || got.MPD == nil {
				t.Fatalf("state %s %v dac %+v mpd %+v", got.State, got.StateReasons, got.DAC, got.MPD)
			}
			wantVerdict(t, got, nil, signalpath.Withheld, signalpath.ALSAConfigUnread)
			if _, found, _ := srv.localOutputs.Reading(context.Background(), "abc"); found {
				t.Fatal("a malformed id was answered")
			}
		})
	}
}

// TestAProbeThatHangsEndsTheReadingInTime: the probe's inspection reads
// mpd.conf and the player's files, which a dead mount can hold for good,
// so it runs on a goroutine of its own and is waited for no longer than its
// deadline and a grace: the reading is then an honest unknown, in time, and
// the next probe starts no second inspection while the first runs.
func TestAProbeThatHangsEndsTheReadingInTime(t *testing.T) {
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	clock := &manualClock{now: time.Now()}
	rig.cfg.Now, rig.cfg.AfterFunc = clock.Now, clock.AfterFunc
	var hang atomic.Bool
	var inspects atomic.Int32
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	inspect := rig.cfg.Inspect
	rig.cfg.Inspect = func(ctx context.Context, opt mpd.Options) mpd.Report {
		if hang.Load() {
			inspects.Add(1)
			<-stuck
		}
		return inspect(ctx, opt)
	}
	srv, hs, token := newReadingServer(t, rig.cfg)
	id := firstOutputID(t, hs, token)
	srv.localOutputs.limits.build = 30 * time.Second
	// The probe's own cache: the next one inspects again.
	clock.Advance(outputProbeHit)
	hang.Store(true)
	got, took := readingWithin(t, srv, id, 5*time.Second)
	if took > 2*time.Second {
		t.Fatalf("the reading took %s behind an inspection that hangs", took)
	}
	// The probe left no endpoint, which the output ids are made from, so
	// the id is answered as an id /proc could not tell: unknown.
	if got.State != "unknown" || !slices.Equal(got.StateReasons, []string{signalpath.MPDTimeout}) || got.MPD != nil || got.DAC != nil {
		t.Fatalf("state %s %v mpd %+v dac %+v", got.State, got.StateReasons, got.MPD, got.DAC)
	}
	clock.Advance(outputCacheTTL)
	readingWithin(t, srv, id, 5*time.Second)
	if n := inspects.Load(); n != 1 {
		t.Fatalf("%d inspections ran while one hung", n)
	}
}

// TestAPanickingStepLeavesTheNextReadingWhole: a step that panics fails as
// a step (a /proc read that panics is /proc unreadable), a reading whose own
// code panics is an honest unknown (reading_failed), the panic is logged
// without its value, and the next reading is taken afresh and whole, on a
// session the panic did not take with it.
func TestAPanickingStepLeavesTheNextReadingWhole(t *testing.T) {
	const secret = "a-value-a-panic-carried"
	rec := loggingtest.Record(t)
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	clock := &manualClock{now: time.Now()}
	rig.cfg.Now, rig.cfg.AfterFunc = clock.Now, clock.AfterFunc
	var panicDial atomic.Bool
	dial := rig.cfg.Dial
	rig.cfg.Dial = func(ctx context.Context, endpoint, password string) (outputSession, error) {
		if panicDial.Load() {
			panic(secret)
		}
		return dial(ctx, endpoint, password)
	}
	srv, hs, token := newReadingServer(t, rig.cfg)
	id := firstOutputID(t, hs, token)

	read := rig.cards
	rig.cards = func() (alsa.Snapshot, error) { panic(secret) }
	got, _ := readingWithin(t, srv, id, 5*time.Second)
	if got.State != "unknown" || !slices.Equal(got.StateReasons, []string{signalpath.ProcUnreadable}) {
		t.Fatalf("a /proc read that panicked: state %s %v", got.State, got.StateReasons)
	}
	rig.cards = read

	srv.localOutputs.close()
	panicDial.Store(true)
	clock.Advance(outputCacheTTL)
	got, _ = readingWithin(t, srv, id, 5*time.Second)
	if got.State != "unknown" || !slices.Equal(got.StateReasons, []string{signalpath.ReadingFailed}) ||
		!slices.Equal(got.Verdict.Reasons, []string{signalpath.ReadingFailed}) || got.MPD != nil || got.DAC != nil {
		t.Fatalf("a reading that panicked: %+v", got)
	}
	panicDial.Store(false)
	clock.Advance(outputCacheTTL)
	got, _ = readingWithin(t, srv, id, 5*time.Second)
	if got.State != "whole" || got.Verdict.Value != signalpath.FormatPreserved {
		t.Fatalf("the reading after a panic: state %s %v verdict %+v", got.State, got.StateReasons, got.Verdict)
	}
	srv.localOutputs.mu.Lock()
	readings, flight := srv.localOutputs.readings, srv.localOutputs.reading.flight
	srv.localOutputs.mu.Unlock()
	if readings != 0 || flight != nil {
		t.Fatalf("readings %d flight %v after the panics", readings, flight)
	}
	var logged bool
	for _, line := range rec.All() {
		if strings.Contains(line, secret) {
			t.Fatalf("a log line carries the panic's value: %s", line)
		}
		logged = logged || strings.Contains(line, "a step panicked")
	}
	if !logged {
		t.Fatal("no panic was logged")
	}
}

// TestACommandThatPanicsDropsTheSession: a command that panics after it was
// written may leave MPD's reply unread, and that reply would answer the next
// command sent on the connection. So the shared session is closed and
// forgotten before the panic goes on: the reading is reading_failed, and the
// next one dials a session of its own.
func TestACommandThatPanicsDropsTheSession(t *testing.T) {
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	clock := &manualClock{now: time.Now()}
	rig.cfg.Now, rig.cfg.AfterFunc = clock.Now, clock.AfterFunc
	srv, hs, token := newReadingServer(t, rig.cfg)
	id := firstOutputID(t, hs, token)
	if n := rig.dials.Load(); n != 1 {
		t.Fatalf("the outputs body dialled %d sessions", n)
	}
	rig.mpd.set(func(m *scriptedMPD) { m.panics = map[string]bool{"currentsong": true} })
	got, _ := readingWithin(t, srv, id, 5*time.Second)
	if got.State != "unknown" || !slices.Equal(got.StateReasons, []string{signalpath.ReadingFailed}) {
		t.Fatalf("a command that panicked: state %s %v", got.State, got.StateReasons)
	}
	if n := rig.mpd.closed.Load(); n != 1 {
		t.Fatalf("the session a command panicked on was closed %d times, want once", n)
	}
	rig.mpd.set(func(m *scriptedMPD) { m.panics = nil })
	clock.Advance(outputCacheTTL)
	got, _ = readingWithin(t, srv, id, 5*time.Second)
	if got.State != "whole" {
		t.Fatalf("the reading after the panic: state %s %v", got.State, got.StateReasons)
	}
	if n := rig.dials.Load(); n != 2 {
		t.Fatalf("%d dials: the reading after the panic used the session it panicked on", n)
	}
}

// TestEveryMPDCommandIsHandedItsOwnDeadline: the dial and each command go
// out with the deadline each has, not the whole reading's: what is left of
// it when the command is sent is that limit, give or take the time to send.
func TestEveryMPDCommandIsHandedItsOwnDeadline(t *testing.T) {
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	var dialLeft atomic.Int64
	dial := rig.cfg.Dial
	rig.cfg.Dial = func(ctx context.Context, endpoint, password string) (outputSession, error) {
		if dl, ok := ctx.Deadline(); ok {
			dialLeft.Store(int64(time.Until(dl)))
		}
		return dial(ctx, endpoint, password)
	}
	srv, hs, token := newReadingServer(t, rig.cfg)
	id := firstOutputID(t, hs, token)
	srv.localOutputs.close()
	srv.localOutputs.limits.build = 30 * time.Second
	srv.localOutputs.limits.dial = 400 * time.Millisecond
	srv.localOutputs.limits.command = 600 * time.Millisecond
	readingWithin(t, srv, id, 5*time.Second)
	within := func(what string, left, limit time.Duration) {
		t.Helper()
		if left > limit || left < limit-250*time.Millisecond {
			t.Errorf("%s went out with %s left, want its own %s", what, left, limit)
		}
	}
	within("the dial", time.Duration(dialLeft.Load()), 400*time.Millisecond)
	for _, name := range []string{"status", "currentsong", "replay_gain_status", "outputs"} {
		lefts := rig.mpd.deadlinesLeft(name)
		if len(lefts) == 0 {
			t.Fatalf("%s was not sent", name)
		}
		for _, left := range lefts[len(lefts)-min(len(lefts), 2):] {
			within(name, left, 600*time.Millisecond)
		}
	}
}
