package transcode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// The capability probe is fail-CLOSED, which is the opposite of ProbeSox's
// posture, so every arm is pinned explicitly: a real listing grants, a
// listing without the decoders refuses, and anything that is not a listing
// refuses too.

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(b)
}

// withoutLines drops every line containing any of the needles — used to
// build "a build without X" listings from the captured ones.
func withoutLines(text string, needles ...string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		drop := false
		for _, n := range needles {
			if strings.Contains(line, n) {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func TestParseFFmpegDecoders_Fixtures(t *testing.T) {
	for _, name := range []string{"ffmpeg-decoders-brew-8.1.txt", "ffmpeg-decoders-apt-6.1.1.txt"} {
		t.Run(name, func(t *testing.T) {
			decoders, known := parseFFmpegDecoders(readFixture(t, name))
			if !known {
				t.Fatal("a real listing must parse as known")
			}
			if len(decoders) < 400 {
				t.Errorf("only %d decoders parsed from a full listing; the row filter is too strict", len(decoders))
			}
			for _, want := range append([]string{dstDecoderName, "flac", "alac"}, dsdDecoderNames...) {
				found := false
				for _, d := range decoders {
					if d == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("decoder %q missing from the parsed list", want)
				}
			}
			for _, d := range decoders {
				if d == "=" || strings.HasPrefix(d, "-") {
					t.Errorf("legend or separator text leaked into the decoder list: %q", d)
				}
			}
			hasDSD, hasDST := ffmpegCapabilities(decoders)
			if !hasDSD || !hasDST {
				t.Errorf("capabilities from %s = dsd:%v dst:%v, want both true", name, hasDSD, hasDST)
			}
		})
	}
}

func TestFFmpegCapabilities_AllFourNotAny(t *testing.T) {
	three := dsdDecoderNames[:3]
	if dsd, _ := ffmpegCapabilities(three); dsd {
		t.Error("three of the four dsd_* decoders must NOT count as DSD-capable")
	}
	if dsd, dst := ffmpegCapabilities(dsdDecoderNames); !dsd || dst {
		t.Errorf("all four dsd_* without dst: dsd=%v dst=%v, want true/false", dsd, dst)
	}
	if dsd, dst := ffmpegCapabilities([]string{"dst"}); dsd || !dst {
		t.Errorf("dst alone: dsd=%v dst=%v, want false/true", dsd, dst)
	}
	if dsd, dst := ffmpegCapabilities(nil); dsd || dst {
		t.Error("an empty list grants nothing")
	}
}

func TestParseFFmpegDecoders_RefusesWhatIsNotAListing(t *testing.T) {
	cases := map[string]string{
		"empty":                "",
		"a version banner":     "ffmpeg version 8.1.2 Copyright (c) 2000-2026 the FFmpeg developers\nbuilt with Apple clang",
		"an error line":        "Unrecognized option 'decoders'.\nError splitting the argument list: Option not found",
		"the legend only":      "Decoders:\n V..... = Video\n A..... = Audio\n S..... = Subtitle\n",
		"separator but no row": "Decoders:\n V..... = Video\n ------\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			decoders, known := parseFFmpegDecoders(text)
			if known || len(decoders) != 0 {
				t.Errorf("parseFFmpegDecoders(%q) = %v, known=%v; must be unknown with no decoders", name, decoders, known)
			}
		})
	}
}

func TestParseFFmpegDecoders_RowsBeforeTheSeparatorDoNotCount(t *testing.T) {
	text := " A....D dsd_lsbf   DSD\n ------\n A....D flac   FLAC\n"
	decoders, known := parseFFmpegDecoders(text)
	if !known || len(decoders) != 1 || decoders[0] != "flac" {
		t.Errorf("got %v known=%v; only rows after the separator are decoders", decoders, known)
	}
}

// withFakeFFmpeg installs an ffmpeg whose `-decoders` output is `listing`
// and whose exit code is `exitCode`, mirroring withFakeSox. ffprobe is
// pretended present too (the DSD pipe needs both).
func withFakeFFmpeg(t *testing.T, listing string, exitCode int) *atomic.Int32 {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("test uses /bin/sh which isn't available on this platform")
	}
	origFF, origFP, origCmd := ffmpegLookPath, ffprobeLookPath, ffmpegProbeCommand
	resetFFmpegSnapshotForTest()
	t.Cleanup(func() {
		ffmpegLookPath, ffprobeLookPath, ffmpegProbeCommand = origFF, origFP, origCmd
		resetFFmpegSnapshotForTest()
	})
	ffmpegLookPath = func() (string, error) { return "/fake/ffmpeg", nil }
	ffprobeLookPath = func() (string, error) { return "/fake/ffprobe", nil }
	spawns := &atomic.Int32{}
	ffmpegProbeCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		spawns.Add(1)
		if name != "/fake/ffmpeg" {
			t.Errorf("probe spawned %q, want the resolved /fake/ffmpeg", name)
		}
		if strings.Join(args, " ") != "-hide_banner -decoders" {
			t.Errorf("probe args = %q, want -hide_banner -decoders", args)
		}
		script := "printf '%s' '" + strings.ReplaceAll(listing, "'", "'\\''") + "'"
		if exitCode != 0 {
			script += "; exit " + strings.TrimSpace(itoaSox(exitCode))
		}
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
	return spawns
}

func TestProbeFFmpeg_SeamPresent(t *testing.T) {
	withFakeFFmpeg(t, readFixture(t, "ffmpeg-decoders-brew-8.1.txt"), 0)
	info, err := ProbeFFmpeg(context.Background())
	if err != nil {
		t.Fatalf("ProbeFFmpeg: %v", err)
	}
	if !info.Available() || info.Path != "/fake/ffmpeg" {
		t.Errorf("Available=%v Path=%q", info.Available(), info.Path)
	}
	if !info.DecodersKnown || !info.HasDSD || !info.HasDST {
		t.Errorf("known=%v dsd=%v dst=%v, want all true", info.DecodersKnown, info.HasDSD, info.HasDST)
	}
}

func TestProbeFFmpeg_SeamNoDSD(t *testing.T) {
	listing := withoutLines(readFixture(t, "ffmpeg-decoders-apt-6.1.1.txt"), " dsd_lsbf ", " dsd_msbf ")
	withFakeFFmpeg(t, listing, 0)
	info, err := ProbeFFmpeg(context.Background())
	if err != nil {
		t.Fatalf("a parseable listing is not an error: %v", err)
	}
	if !info.DecodersKnown {
		t.Fatal("the listing must still parse as known")
	}
	if info.HasDSD {
		t.Error("two of the four dsd_* decoders were dropped; HasDSD must be false")
	}
	if !info.HasDST {
		t.Error("dst was left in and must still be reported")
	}
}

func TestProbeFFmpeg_SeamNoDST(t *testing.T) {
	listing := withoutLines(readFixture(t, "ffmpeg-decoders-brew-8.1.txt"), " dst ")
	withFakeFFmpeg(t, listing, 0)
	info, err := ProbeFFmpeg(context.Background())
	if err != nil {
		t.Fatalf("ProbeFFmpeg: %v", err)
	}
	if !info.HasDSD || info.HasDST {
		t.Errorf("dsd=%v dst=%v, want true/false — the two capabilities are independent", info.HasDSD, info.HasDST)
	}
}

func TestProbeFFmpeg_SeamMissing(t *testing.T) {
	withFFmpeg(t, false)
	info, err := ProbeFFmpeg(context.Background())
	if !errors.Is(err, ErrFFmpegMissing) {
		t.Errorf("want ErrFFmpegMissing, got %v", err)
	}
	if info.Available() || info.HasDSD || info.DecodersKnown {
		t.Errorf("a missing toolchain grants nothing: %+v", info)
	}
	if got := strings.Join(info.MissingBinaries, ","); got != "ffmpeg,ffprobe" {
		t.Errorf("MissingBinaries = %q, want both named in stable order", got)
	}
}

func TestProbeFFmpeg_SeamUnparseableFailsClosed(t *testing.T) {
	withFakeFFmpeg(t, "ffmpeg version 8.1.2\nbuilt with clang\n", 0)
	info, err := ProbeFFmpeg(context.Background())
	if err == nil {
		t.Error("output that is not a decoder listing must be reported as an error so the doctor can name it")
	}
	if info.DecodersKnown || info.HasDSD || info.HasDST {
		t.Errorf("fail CLOSED: %+v", info)
	}
	if !info.Available() {
		t.Error("binary presence is a separate fact and stays true")
	}
}

func TestProbeFFmpeg_SeamNonZeroExitWithOutputStillParses(t *testing.T) {
	// A wrapper that prints the listing and then exits 1 (seen with some
	// distro shims) — the listing is what matters.
	withFakeFFmpeg(t, readFixture(t, "ffmpeg-decoders-apt-6.1.1.txt"), 1)
	info, err := ProbeFFmpeg(context.Background())
	if err != nil {
		t.Fatalf("a recognised listing wins over the exit code: %v", err)
	}
	if !info.HasDSD {
		t.Error("HasDSD must be true from the apt listing")
	}
}

func TestProbeFFmpeg_SeamEmptyOutputIsAnError(t *testing.T) {
	withFakeFFmpeg(t, "", 1)
	info, err := ProbeFFmpeg(context.Background())
	if err == nil || info.DecodersKnown {
		t.Errorf("empty output must be an error with nothing known: err=%v info=%+v", err, info)
	}
}

func TestFFmpegSnapshot_CachesAndFailsClosed(t *testing.T) {
	spawns := withFakeFFmpeg(t, readFixture(t, "ffmpeg-decoders-brew-8.1.txt"), 0)
	first := FFmpegSnapshot()
	second := FFmpegSnapshot()
	if !first.HasDSD || !second.HasDSD {
		t.Fatalf("snapshot must carry the probe result: %+v / %+v", first, second)
	}
	if n := spawns.Load(); n != 1 {
		t.Errorf("two snapshot reads spawned %d probes, want exactly 1 (cached)", n)
	}

	// Binary presence is re-checked live: the cache must not mask a
	// toolchain that vanished from PATH.
	ffmpegLookPath = func() (string, error) { return "", errors.New("gone") }
	if got := FFmpegSnapshot(); got.Available() || got.HasDSD {
		t.Errorf("after ffmpeg vanished the snapshot still granted: %+v", got)
	}
	if n := spawns.Load(); n != 1 {
		t.Errorf("an absent binary must not be probed (spawns=%d)", n)
	}
}

func TestFFmpegSnapshot_UnparseableProbeGrantsNothing(t *testing.T) {
	withFakeFFmpeg(t, "garbage\n", 0)
	got := FFmpegSnapshot()
	if !got.Available() {
		t.Error("the binaries are present")
	}
	if got.DecodersKnown || got.HasDSD || got.HasDST {
		t.Errorf("an unparseable probe must grant nothing: %+v", got)
	}
}

// TestCanDecodeViaRefusesDSDWithoutTheDecoders is the gate-level twin of the
// route table: the four call sites of CanDecodeVia must answer "no" for a
// DSD source on a host whose ffmpeg cannot decode it, and "yes" once it can.
func TestCanDecodeViaRefusesDSDWithoutTheDecoders(t *testing.T) {
	stock := func() (SoxInfo, error) { return soxThatReads("flac", "wav"), nil }

	withFakeFFmpeg(t, withoutLines(readFixture(t, "ffmpeg-decoders-brew-8.1.txt"), " dsd_"), 0)
	if CanDecodeVia(stock, "/l/a.dsf") {
		t.Error("ffmpeg present but built without dsd_* must refuse a DSD source")
	}
	if !CanDecodeVia(stock, "/l/a.m4a") {
		t.Error("the same ffmpeg still decodes ALAC — the two families are judged separately")
	}

	withFakeFFmpeg(t, readFixture(t, "ffmpeg-decoders-brew-8.1.txt"), 0)
	if !CanDecodeVia(stock, "/l/a.dsf") || !CanDecodeVia(stock, "/l/a.dff") {
		t.Error("with the decoders present both DSD containers are decodable")
	}
	withFFmpeg(t, false)
	if CanDecodeVia(stock, "/l/a.dsf") {
		t.Error("without ffmpeg a DSD source must be refused")
	}
}
