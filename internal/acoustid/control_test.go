package acoustid

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Live control harness — coverage and cost, measured with the REAL code path.
//
// Env-gated and skipped in CI. Run it by hand against a real library:
//
//	ACOUSTID_API_KEY=<key> \
//	BRIDGE_ACOUSTID_CONTROL_DIR=/path/to/audio \
//	go test ./internal/acoustid/ -run TestAcoustIDCoverageControl -v -count=1 -timeout 60m
//
// # Why this is a test and not a script
//
// The recall numbers behind the enrichment-matching work were first produced
// by a Python reimplementation of the folding logic that was not byte-identical
// to the shipped Go, so the numbers described the reimplementation rather than
// the bridge. Numbers measured with a lookalike are evidence about the
// lookalike. This drives the real Compute, the real Client and the real Accept,
// so the claim and the artefact cannot drift.
//
// # What it deliberately does NOT measure
//
// It does not compare the fingerprint's answer against a tag-derived one. That
// comparison — the one that estimates the FALSE-MATCH rate — needs the
// enricher's already-resolved MBIDs, so it belongs with the enricher wiring.
// Two things are worth stating in advance about it, because they decide how
// its number should be read:
//
//   - the sample would be biased OPTIMISTIC. Tracks that resolve confidently
//     from tags are mainstream, widely ripped and rich in AcoustID sources;
//     the fallback population is the opposite. Any agreement rate measured
//     that way is a LOWER bound, not "the false-match rate".
//   - it cannot see shared-error modes. If MusicBrainz itself is wrong, both
//     paths agree and the run reads green.
//
// What this harness answers instead is narrower and answerable now: of the
// audio you point it at, how much is eligible, how much does AcoustID know,
// how much survives the gate, what it costs, and — the two numbers that turn
// defaults into justified constants — the distribution of `sources` on
// accepted matches and of release-group counts per accepted recording.
//
// # No pass/fail bar on coverage
//
// Coverage depends on the upstream's data, not on this code, so a low number
// is information rather than a regression. The harness fails only on things
// that would be bugs here: a gate acceptance that carries no artist, or a
// decode that contradicts itself.
func TestAcoustIDCoverageControl(t *testing.T) {
	dir := os.Getenv("BRIDGE_ACOUSTID_CONTROL_DIR")
	if dir == "" {
		t.Skip("BRIDGE_ACOUSTID_CONTROL_DIR not set — live control, run by hand")
	}
	key := os.Getenv("ACOUSTID_API_KEY")
	if key == "" {
		t.Skip("ACOUSTID_API_KEY not set — live control, run by hand")
	}
	if _, err := Probe(context.Background()); err != nil {
		t.Skipf("fpcalc unavailable: %v", err)
	}

	files := collectAudio(t, dir, envInt(t, "BRIDGE_ACOUSTID_CONTROL_LIMIT", 200))
	if len(files) == 0 {
		t.Skipf("no audio files under %s", dir)
	}
	t.Logf("control set: %d file(s) under %s", len(files), dir)

	client := NewClient("", key, "1-bit-bridge-control/0 (+https://github.com/acoseac/1-bit-bridge)", nil)
	ctx := context.Background()

	var tally controlTally
	tally.init()

	for _, f := range files {
		runControlFile(ctx, t, client, f, &tally)
	}

	n := len(files)
	t.Logf("")
	t.Logf("=== coverage over %d file(s) ===", n)
	t.Logf("  ineligible (pre-decode)   %4d  %5.1f%%", tally.ineligible, pct(tally.ineligible, n))
	t.Logf("  decode failed             %4d  %5.1f%%", tally.decodeFailed, pct(tally.decodeFailed, n))
	t.Logf("  AcoustID knows nothing    %4d  %5.1f%%", tally.noMatch, pct(tally.noMatch, n))
	t.Logf("  lookup errored            %4d  %5.1f%%", tally.lookupFailed, pct(tally.lookupFailed, n))
	t.Logf("  refused by the gate       %4d  %5.1f%%", tally.rejected, pct(tally.rejected, n))
	t.Logf("  ACCEPTED                  %4d  %5.1f%%", tally.accepted, pct(tally.accepted, n))

	reasons, sourcesHist, rgCountHist := tally.reasons, tally.sourcesHist, tally.rgCountHist

	t.Logf("")
	t.Logf("=== why tracks were refused ===")
	for _, kv := range sortedReasons(reasons) {
		t.Logf("  %-22s %4d", kv.k, kv.v)
	}

	t.Logf("")
	t.Logf("=== `sources` on accepted matches ===")
	t.Logf("  (this is what turns minSources=%d from a default into a justified", minSources)
	t.Logf("   constant — if the mass sits at 1-2, the threshold is rejecting")
	t.Logf("   real matches; if it sits well above, it can afford to be stricter)")
	for _, kv := range sortedInts(sourcesHist) {
		t.Logf("  sources=%-4d %4d", kv.k, kv.v)
	}

	t.Logf("")
	t.Logf("=== release groups per accepted recording ===")
	t.Logf("  (the empirical case for never picking one: every row above 1 is a")
	t.Logf("   track whose album the fingerprint genuinely cannot determine)")
	for _, kv := range sortedInts(rgCountHist) {
		t.Logf("  groups=%-4d %4d", kv.k, kv.v)
	}

	t.Logf("")
	t.Logf("=== cost ===")
	// Divide by the number of files that ACTUALLY decoded / were looked up,
	// not by the whole set. Files filtered out before the decode contribute
	// nothing to totalDecode, so averaging over all of them would understate
	// the per-track cost — and understating it in the direction that makes
	// the feature look cheaper is exactly the error to avoid when this
	// number is what decides whether to run the sweep on a metered mount.
	t.Logf("  decoded %d file(s), looked up %d", tally.decodes, tally.lookups)
	t.Logf("  decode  %s total, %s per decode",
		tally.decode.Round(time.Millisecond), perOp(tally.decode, tally.decodes))
	t.Logf("  lookup  %s total, %s per lookup",
		tally.lookup.Round(time.Millisecond), perOp(tally.lookup, tally.lookups))
	t.Logf("  source bytes on disk  %.2f GiB total, %.1f MiB per decoded file",
		float64(tally.bytes)/(1<<30),
		float64(tally.bytes)/float64(max(tally.decodes, 1))/(1<<20))
	t.Logf("")
	t.Logf("  NOTE the byte figure is what the FILES weigh, not what a network")
	t.Logf("  mount fetched. On rclone the whole object is pulled per candidate")
	t.Logf("  at the default 128 MiB chunk size regardless of -length, so measure")
	t.Logf("  the real egress with `rclone rc vfs/stats` around this run.")
}

type controlFile struct {
	path string
	size int64
}

// controlTally accumulates the run's counters and histograms.
type controlTally struct {
	ineligible   int
	decodeFailed int
	lookupFailed int
	noMatch      int
	accepted     int
	rejected     int

	// decodes / lookups count the operations actually performed, so the
	// averages divide by the right denominator. Files filtered out before the
	// decode contribute nothing to `decode`, and averaging those over the whole
	// set would understate per-track cost.
	decodes int
	lookups int

	decode time.Duration
	lookup time.Duration
	bytes  int64

	reasons     map[RejectReason]int
	sourcesHist map[int]int
	rgCountHist map[int]int
}

func (c *controlTally) init() {
	c.reasons = map[RejectReason]int{}
	c.sourcesHist = map[int]int{}
	c.rgCountHist = map[int]int{}
}

// runControlFile takes one file all the way through the real pipeline and
// records where it landed.
func runControlFile(ctx context.Context, t *testing.T, client *Client, f controlFile, tally *controlTally) {
	t.Helper()

	durationSec, isDSD, ok := containerFacts(f.path)
	if !ok {
		// Without a container duration there is nothing to gate against; in
		// production this is the FLAC-only consequence of where Track.Duration
		// is populated.
		tally.ineligible++
		tally.reasons[ReasonUnknownDuration]++
		return
	}
	if r := CheckEligible(durationSec, isDSD); r != ReasonNone {
		tally.ineligible++
		tally.reasons[r]++
		return
	}

	start := time.Now()
	fp, err := Compute(ctx, f.path, DefaultLengthSeconds*time.Second)
	tally.decode += time.Since(start)
	tally.decodes++
	tally.bytes += f.size
	if err != nil {
		tally.decodeFailed++
		return
	}

	in := Input{
		DurationSec:           durationSec,
		IsDSD:                 isDSD,
		Fingerprint:           fp,
		HasLocalArtistWitness: true, // the harness has no tags; assume the easier bar
	}
	if r := CheckFingerprint(in); r != ReasonNone {
		tally.rejected++
		tally.reasons[r]++
		return
	}

	// Pace only ACTUAL lookups. Sleeping once per file would pad the harness's
	// own wall-clock with delays that paced nothing — most files never reach
	// here (non-FLAC sources are ineligible, and decode failures and gate
	// rejections return earlier), and wall-clock cost is what this measures.
	if tally.lookups > 0 {
		time.Sleep(client.MinInterval())
	}
	start = time.Now()
	results, err := client.Lookup(ctx, fp)
	tally.lookup += time.Since(start)
	tally.lookups++
	if err != nil {
		if errors.Is(err, ErrNoMatch) {
			tally.noMatch++
			tally.reasons[ReasonNoResults]++
			return
		}
		tally.lookupFailed++
		t.Logf("lookup %s: %v", filepath.Base(f.path), err)
		return
	}

	in.Results = results
	decision, reason := Accept(in)
	if reason != ReasonNone {
		tally.rejected++
		tally.reasons[reason]++
		return
	}

	tally.accepted++
	// Bugs in THIS code, unlike coverage, are failures.
	if decision.ArtistMBID == "" {
		t.Errorf("%s: accepted with no artist MBID", filepath.Base(f.path))
	}
	tally.sourcesHist[decision.Sources]++
	tally.rgCountHist[countSurvivorReleaseGroups(in)]++
}

// collectAudio walks dir for files the bridge would consider audio, capped at
// limit. Sorted so two runs over the same tree measure the same set.
func collectAudio(t *testing.T, dir string, limit int) []controlFile {
	t.Helper()
	exts := map[string]bool{
		".flac": true, ".mp3": true, ".m4a": true, ".wav": true,
		".aiff": true, ".aif": true, ".ogg": true, ".opus": true,
	}
	var out []controlFile
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Swallow errors BELOW the root only (an unreadable subtree should
			// not abort a long run). A bad root must surface: otherwise a typo
			// in BRIDGE_ACOUSTID_CONTROL_DIR reads as "no audio files here" and
			// the control silently skips, which looks identical to a clean run
			// that found nothing.
			if p == dir {
				return err
			}
			return nil
		}
		if d.IsDir() || !exts[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, controlFile{path: p, size: info.Size()})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// containerFacts reports the duration and DSD flag the gate needs.
//
// It reads the FLAC STREAMINFO directly rather than importing
// internal/manifest, because internal/manifest is a heavyweight dependency
// (the whole store) for two numbers, and a test-only import of it here would
// invert the layering this package deliberately avoids. Non-FLAC inputs report
// !ok, which mirrors production: Track.Duration is only populated for FLAC and
// DSF today, so those are the files the feature can gate.
func containerFacts(path string) (durationSec float64, isDSD bool, ok bool) {
	if !strings.EqualFold(filepath.Ext(path), ".flac") {
		return 0, false, false
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false, false
	}
	defer f.Close()

	// "fLaC" magic + a 4-byte METADATA_BLOCK_HEADER + the 34-byte STREAMINFO.
	var buf [42]byte
	if _, err := f.Read(buf[:]); err != nil {
		return 0, false, false
	}
	if string(buf[0:4]) != "fLaC" {
		return 0, false, false
	}
	// STREAMINFO body starts at 8. Sample rate is 20 bits at bit offset 80 of
	// the body; total samples is 36 bits starting 8 bits later.
	b := buf[8:]
	sampleRate := uint32(b[10])<<12 | uint32(b[11])<<4 | uint32(b[12])>>4
	totalSamples := uint64(b[13]&0x0F)<<32 | uint64(b[14])<<24 |
		uint64(b[15])<<16 | uint64(b[16])<<8 | uint64(b[17])
	if sampleRate == 0 || totalSamples == 0 {
		return 0, false, false
	}
	return float64(totalSamples) / float64(sampleRate), false, true
}

// countSurvivorReleaseGroups counts the distinct release groups across the
// recordings the gate ACTUALLY decided on.
//
// It re-runs the gate's own selection rather than walking the raw cluster,
// because Accept filters recordings by duration first: counting every
// recording AcoustID returned would include ones the gate rejected and inflate
// the histogram — which would undercut the very claim the histogram exists to
// support (that a fingerprint often cannot determine the album). Being
// in-package, the test can call the unexported stages directly, so the number
// reported is by construction the number the gate saw.
func countSurvivorReleaseGroups(in Input) int {
	top, reason := selectResult(in)
	if reason != ReasonNone {
		return 0
	}
	seen := map[string]struct{}{}
	for _, rec := range recordingsMatchingDuration(top.Recordings, in.DurationSec) {
		for _, rg := range rec.ReleaseGroups {
			if rg.ID != "" {
				seen[rg.ID] = struct{}{}
			}
		}
	}
	return len(seen)
}

func envInt(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		t.Fatalf("%s=%q must be a positive integer", key, v)
	}
	return n
}

func pct(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return 100 * float64(part) / float64(whole)
}

// perOp averages over the operations actually performed, not the whole file
// set — see the cost block for why the denominator matters.
func perOp(d time.Duration, n int) time.Duration {
	if n == 0 {
		return 0
	}
	return (d / time.Duration(n)).Round(time.Millisecond)
}

type reasonCount struct {
	k RejectReason
	v int
}

func sortedReasons(m map[RejectReason]int) []reasonCount {
	out := make([]reasonCount, 0, len(m))
	for k, v := range m {
		out = append(out, reasonCount{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].v != out[j].v {
			return out[i].v > out[j].v
		}
		return out[i].k < out[j].k
	})
	return out
}

type intCount struct{ k, v int }

func sortedInts(m map[int]int) []intCount {
	out := make([]intCount, 0, len(m))
	for k, v := range m {
		out = append(out, intCount{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].k < out[j].k })
	return out
}
