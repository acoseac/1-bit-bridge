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

// These tests hold the verdict to what the reading can tie to the MPD it
// dialled and to the output whose thread holds the card (B473 B3 round 2):
// mpd.conf read whole, shown to be the configuration that MPD runs with,
// the settings it reports read on both sides of the /proc read, the ALSA
// chain from a configuration read whole that nothing of MPD's own user
// overrides, and every step that reads a file ended by its own deadline.

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

// TestAConfigChangedAfterMPDStartedIsNotJudged: MPD reads mpd.conf once,
// when it starts, so a file edited since (software volume, then hardware,
// without a restart) describes a configuration MPD does not run with: the
// verdict says so, and claims nothing that file says. Once MPD has started
// again after the change, the file is what it read, and is judged.
func TestAConfigChangedAfterMPDStartedIsNotJudged(t *testing.T) {
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	clock := &manualClock{now: rigNow()}
	rig.cfg.Now, rig.cfg.AfterFunc = clock.Now, clock.AfterFunc
	hs, token, id := rig.serve()
	rig.writeConf(withOutputSetting(volumioConf(t, rig.music), "mixer_type", "software"))
	// Half the process's age ago: after it started, before this reading.
	edited := clock.now.Add(-mpdAge / 2)
	if err := os.Chtimes(rig.confPath, edited, edited); err != nil {
		t.Fatal(err)
	}
	_, got, raw := getReading(t, hs, token, id)
	wantVerdict(t, got, raw, signalpath.Withheld, signalpath.MPDConfigChanged, signalpath.ALSAChainUnproven)

	// MPD restarted: its process started 200 s before the reading, after
	// the edit.
	writeRigFile(t, rig.procRoot, "6551/stat", strings.Replace(rigStat, " 360000 ", " 700000 ", 1))
	clock.Advance(outputCacheTTL)
	_, got, raw = getReading(t, hs, token, id)
	wantVerdict(t, got, raw, signalpath.Withheld, signalpath.MPDSoftwareVolume)
}

// TestAConfigWhoseOutputsAreNotMPDsIsNotJudged: MPD numbers its outputs in
// its file's order, by the names and plugins the file gives them, so a file
// whose outputs are not the ones MPD reports is another configuration (a
// file MPD was not started with, or one rewritten since), and MPD's live
// outputs, not the file, say which outputs exist and are enabled.
func TestAConfigWhoseOutputsAreNotMPDsIsNotJudged(t *testing.T) {
	c := piCaptures[0]
	for _, tc := range []struct {
		name    string
		outputs mpd.Reply
		codes   []string
	}{
		{"an output the file does not define", liveOutputs([3]string{"alsa", "alsa", "1"}, [3]string{"multiroom", "fifo", "0"}, [3]string{"bt", "alsa", "0"}),
			[]string{signalpath.MPDConfigMismatch, signalpath.ALSAChainUnproven}},
		{"the outputs in another order", liveOutputs([3]string{"multiroom", "fifo", "0"}, [3]string{"alsa", "alsa", "1"}),
			[]string{signalpath.MPDConfigMismatch, signalpath.ALSAChainUnproven}},
		{"an output by another plugin", liveOutputs([3]string{"alsa", "pipewire", "1"}, [3]string{"multiroom", "fifo", "0"}),
			[]string{signalpath.MPDConfigMismatch, signalpath.ALSAChainUnproven}},
		// The file's enabled is not what MPD runs with: with no live output,
		// nothing is the thread's output.
		{"no output reported", replyOf(), []string{signalpath.OwnerNotOutput}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
			hs, token, id := rig.serve()
			rig.mpd.set(func(m *scriptedMPD) { m.outputs = tc.outputs })
			_, got, raw := getReading(t, hs, token, id)
			if got.State != "whole" {
				t.Fatalf("state %s %v\n%s", got.State, got.StateReasons, raw)
			}
			wantVerdict(t, got, raw, signalpath.Withheld, tc.codes...)
		})
	}
}

// TestAConfigReadInPartIsNotJudged: what an include the bridge could not
// follow, or a line it could not parse, sets is not known (a mixer, a
// filter, a whole output), so a file read with a problem proves nothing
// it holds.
func TestAConfigReadInPartIsNotJudged(t *testing.T) {
	c := piCaptures[0]
	for name, extra := range map[string]string{
		"an include that is not there":          "include \"no-such.conf\"\n",
		"an include pattern, which MPD expands": "include_optional \"conf.d/*.conf\"\n",
		"a line that does not parse":            "audio_output_format \"48000:24:2\n",
	} {
		t.Run(name, func(t *testing.T) {
			rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
			writeRigFile(t, filepath.Dir(rig.confPath), "conf.d/soft.conf", "mixer_type \"software\"\n")
			hs, token, id := rig.serve()
			rig.writeConf(volumioConf(t, rig.music) + extra)
			_, got, raw := getReading(t, hs, token, id)
			wantVerdict(t, got, raw, signalpath.Withheld, signalpath.MPDConfigIncomplete, signalpath.ALSAChainUnproven)
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
// thread that holds the card is the one /proc names. That output's settings
// and chain are the ones judged: a second output on the card through a
// softvol PCM that holds it is not hidden behind the one whose chain the
// bridge resolves; a thread that is no enabled output's is not this MPD's
// stream; two outputs whose names are alike in 15 bytes cannot be told
// apart; a long name is cut as MPD cuts it; and an output of another
// plugin reaches the card some other way.
func TestTheOutputWhoseThreadHoldsTheCardIsTheOneJudged(t *testing.T) {
	c := piCaptures[0]
	soft := "audio_output {\n\ttype \"alsa\"\n\tname \"soft\"\n\tdevice \"softdev\"\n}\n"
	softvol := "pcm.softdev {\n    type softvol\n    slave.pcm \"volumioHw\"\n    control.name \"Soft\"\n}\n"
	twoOutputs := liveOutputs([3]string{"alsa", "alsa", "1"}, [3]string{"multiroom", "fifo", "0"}, [3]string{"soft", "alsa", "1"})
	for _, tc := range []struct {
		name    string
		conf    func(string) string
		asound  string
		outputs mpd.Reply
		thread  string
		value   string
		codes   []string
	}{
		{"the output whose chain is proven", func(s string) string { return s + soft }, softvol, twoOutputs, "output:alsa", signalpath.BitExact, nil},
		{"a softvol output on the same card", func(s string) string { return s + soft }, softvol, twoOutputs, "output:soft", signalpath.Withheld,
			[]string{signalpath.ALSAChainUnproven}},
		{"a thread that is no enabled output's", func(s string) string { return s }, "", volumioOutputs("1", "allowed_formats=", "dop=0"), "output:other", signalpath.Withheld,
			[]string{signalpath.OwnerNotOutput}},
		{"two names alike in 15 bytes", func(s string) string {
			return withName(s, "alsa-usb-front") + "audio_output {\n\ttype \"alsa\"\n\tname \"alsa-usb-rear\"\n\tdevice \"hw:CARD=Audio,DEV=0\"\n}\n"
		}, "", liveOutputs([3]string{"alsa-usb-front", "alsa", "1"}, [3]string{"multiroom", "fifo", "0"}, [3]string{"alsa-usb-rear", "alsa", "1"}),
			"output:alsa-usb", signalpath.Withheld, []string{signalpath.MPDOutputAmbiguous}},
		{"a long name, cut as MPD cuts it", func(s string) string { return withName(s, "USB DAC on card five") }, "",
			liveOutputs([3]string{"USB DAC on card five", "alsa", "1"}, [3]string{"multiroom", "fifo", "0"}), "output:USB DAC", signalpath.BitExact, nil},
		{"an output of another plugin", func(s string) string {
			return strings.Replace(s, "\t\ttype\t\t\"alsa\"", "\t\ttype\t\t\"ao\"", 1)
		}, "", liveOutputs([3]string{"alsa", "ao", "1"}, [3]string{"multiroom", "fifo", "0"}), "output:alsa", signalpath.Withheld,
			[]string{signalpath.ALSAChainUnproven}},
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
		})
	}
}

// TestAChainNothingProvesIsNotJudgedProven: a hw device's stages are known
// only while the ALSA configuration the bridge read whole redefines no hw
// PCM, and only while MPD's own user has no ALSA file in its home that
// alsa-lib loads after etc's; a home the bridge cannot tell about proves
// nothing either.
func TestAChainNothingProvesIsNotJudgedProven(t *testing.T) {
	c := piCaptures[0]
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, r *readingRig)
		value  string
		codes  []string
	}{
		{"hw with nothing redefined", func(t *testing.T, r *readingRig) {}, signalpath.BitExact, nil},
		{"a pcm.!hw in /etc/asound.conf", func(t *testing.T, r *readingRig) {
			appendAsound(t, r, "pcm.!hw {\n    type plug\n    slave.pcm \"volumioHw\"\n}\n")
		}, signalpath.Withheld, []string{signalpath.ALSAChainUnproven}},
		{"a ~/.asoundrc of MPD's user", func(t *testing.T, r *readingRig) {
			writeRigFile(t, filepath.Dir(r.procRoot), "home/volumio/.asoundrc", "pcm.!default { type softvol }\n")
		}, signalpath.Withheld, []string{signalpath.ALSAChainUnproven}},
		{"a uid passwd does not name", func(t *testing.T, r *readingRig) {
			writeRigFile(t, filepath.Dir(r.procRoot), "etc/passwd", "root:x:0:0:root:/root:/bin/bash\n")
		}, signalpath.Withheld, []string{signalpath.ALSAChainUnproven}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
			rig.writeConf(withDevice(volumioConf(t, rig.music), "hw:CARD=Audio,DEV=0"))
			hs, token, id := rig.serve()
			tc.change(t, rig)
			_, got, raw := getReading(t, hs, token, id)
			wantVerdict(t, got, raw, tc.value, tc.codes...)
		})
	}
}

// stepRig is a rig whose reading's deadlines are short and whose whole
// reading's deadline is far off, so only a step's own deadline can end it
// in time, with a server to set them on.
func stepRig(t *testing.T) (*readingRig, *Server, string, string) {
	t.Helper()
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	clock := &manualClock{now: rigNow()}
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
// mpd.conf unread, in time, and is not doubled by the next reading.
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
	if got.State != "whole" || !slices.Equal(got.Verdict.Reasons, []string{signalpath.MPDConfigUnread, signalpath.ALSAChainUnproven}) {
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
// read once a reading, under its own deadline: one that hangs leaves no
// chain proven, the output still named by its card, and is not doubled.
func TestAnALSAConfigurationReadPastItsDeadline(t *testing.T) {
	_, srv, _, id := stepRig(t)
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	var reads atomic.Int32
	srv.localOutputs.alsaConfig = func(root string, uids []int) alsaContext {
		reads.Add(1)
		<-stuck
		return readALSAContext(root, uids)
	}
	srv.localOutputs.limits.alsa = 50 * time.Millisecond
	got, took := readingWithin(t, srv, id, 5*time.Second)
	if took < 50*time.Millisecond || took > time.Second {
		t.Fatalf("the reading took %s with a 50ms deadline on the ALSA configuration", took)
	}
	if got.State != "whole" || got.DAC == nil || got.DAC.Card != 5 || !slices.Equal(got.Verdict.Reasons, []string{signalpath.ALSAChainUnproven}) {
		t.Fatalf("state %s dac %+v verdict %+v", got.State, got.DAC, got.Verdict)
	}
	srv.localOutputs.mu.Lock()
	srv.localOutputs.reading.snap = nil
	srv.localOutputs.mu.Unlock()
	readingWithin(t, srv, id, 5*time.Second)
	if n := reads.Load(); n != 1 {
		t.Fatalf("the ALSA configuration was read %d times while one read hung", n)
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
	clock := &manualClock{now: rigNow()}
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
	clock := &manualClock{now: rigNow()}
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
	if got.State != "whole" || got.Verdict.Value != signalpath.BitExact {
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
