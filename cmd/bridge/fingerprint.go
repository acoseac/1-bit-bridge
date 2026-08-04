package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// fingerprintCmd implements `bridge fingerprint` — the manual diagnostic for
// the acoustic-fingerprinting fallback.
//
// It exists to answer two questions BEFORE the feature is wired into anything
// that writes:
//
//  1. Coverage — of the tracks text matching failed on, how many does AcoustID
//     actually know, and how many survive the acceptance gate?
//  2. Cost — how long does a decode take, and how many bytes does it pull? On a
//     network-backed library (rclone/B2) that second number is the one that
//     decides whether the feature is worth enabling there at all.
//
// It reads files directly rather than going through the config and the library
// resolver, so it needs no bridge config and can be pointed at anything.
//
// NOTHING here writes: not the manifest, not the artwork cache, not a single
// MBID. The gate's verdict is printed, never applied.
//
// Exit codes: 0 the run completed, 1 the run could not start (no key, no
// fpcalc, unwritable output), 2 usage error, 130 interrupted mid-batch
// (POSIX 128+SIGINT) so a script can tell an interrupted run from a complete
// one — matching `bridge analyze`.
//
// A file that cannot be read or decoded does NOT change the exit code. This is
// a diagnostic whose job is to report per-file outcomes, and pointing it at 200
// tracks of which 3 are unreadable is a successful run with three findings, not
// a failed one. Those files are reported with an `error` line (and an `error`
// field in --json), which is the surface to check.
func fingerprintCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fingerprint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	key := fs.String("key", "", "AcoustID application key (default: $ACOUSTID_API_KEY)")
	lengthSec := fs.Int("length", acoustid.DefaultLengthSeconds, "seconds of audio to fingerprint")
	noLookup := fs.Bool("no-lookup", false, "fingerprint only; skip the AcoustID call (no key needed)")
	prefixBytes := fs.Int64("prefix-bytes", 0,
		"pipe only the first N bytes to fpcalc instead of letting it read the file "+
			"(0 = off). Measures whether capping the read reduces what a network mount fetches.")
	asJSON := fs.Bool("json", false, "emit JSON instead of a human summary")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	paths := fs.Args()
	if len(paths) == 0 {
		fmt.Fprint(stderr, "usage: bridge fingerprint [flags] <file>...\n\n"+
			"Fingerprints one or more audio files and reports what AcoustID knows\n"+
			"about them, plus the acceptance gate's verdict. Writes nothing.\n\n")
		fs.PrintDefaults()
		return 2
	}

	info, client, code := prepareFingerprintRun(ctx, *key, *noLookup, stderr)
	if code != 0 {
		return code
	}

	reports, interrupted := runFingerprintFiles(ctx, client, paths,
		time.Duration(*lengthSec)*time.Second, *prefixBytes)

	// Print what we DID measure before reporting the interruption: on a long
	// run over a network-backed library those partial numbers are the
	// expensive part, and discarding them because of a Ctrl+C would mean
	// paying the egress again.
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(reports); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
	} else {
		printFingerprintReports(stdout, info, reports)
	}
	if interrupted {
		fmt.Fprintf(stderr, "\ninterrupted after %d of %d file(s)\n", len(reports), len(paths))
		return 130
	}
	return 0
}

// runFingerprintFiles walks the file list, pacing between AcoustID calls and
// stopping cleanly on cancellation. Returns what it managed to measure and
// whether it was cut short.
func runFingerprintFiles(ctx context.Context, client *acoustid.Client, paths []string,
	length time.Duration, prefixBytes int64) (reports []fingerprintReport, interrupted bool) {

	reports = make([]fingerprintReport, 0, len(paths))
	for _, p := range paths {
		if ctx.Err() != nil {
			return reports, true
		}
		// Pace before an actual REQUEST, not before every file. Many paths
		// never reach a lookup — ineligible container, decode failure, a gate
		// refusal before the network — and sleeping for those would pad the
		// run with intervals that paced nothing, which matters because this
		// command's other job is measuring wall-clock cost honestly. The
		// previous report's LookupMillis is the record of whether a request
		// actually went out.
		if client != nil && lastDidLookup(reports) {
			select {
			case <-time.After(client.MinInterval()):
			case <-ctx.Done():
				return reports, true
			}
		}
		reports = append(reports, fingerprintOne(ctx, client, p, length, prefixBytes))
		// Checked AFTER the append as well as before: cancellation during the
		// last (or only) file would otherwise fall out of the loop normally
		// and report a clean run, losing the 130 exit that tells a script the
		// batch was cut short.
		if ctx.Err() != nil {
			return reports, true
		}
	}
	return reports, false
}

// lastDidLookup reports whether the most recent file actually reached
// AcoustID, which is what the pacing interval exists to space out.
func lastDidLookup(reports []fingerprintReport) bool {
	if len(reports) == 0 {
		return false
	}
	return reports[len(reports)-1].LookupMillis > 0
}

// prepareFingerprintRun resolves the API key, checks the toolchain, and builds
// the client. Returns a non-zero exit code when the run cannot proceed;
// a nil client means --no-lookup (fingerprint without contacting AcoustID).
func prepareFingerprintRun(ctx context.Context, key string, noLookup bool,
	stderr io.Writer) (acoustid.Info, *acoustid.Client, int) {

	if key == "" {
		key = os.Getenv("ACOUSTID_API_KEY")
	}
	if key == "" && !noLookup {
		fmt.Fprint(stderr, "No AcoustID key. Pass --key or set ACOUSTID_API_KEY.\n"+
			"Get a free application key at https://acoustid.org/new-application\n"+
			"Or pass --no-lookup to fingerprint without contacting AcoustID.\n")
		// 1, not 2: this is "the run could not start", the same class as a
		// missing fpcalc a few lines below — not a malformed command line.
		return acoustid.Info{}, nil, 1
	}

	info, err := acoustid.Probe(ctx)
	if err != nil {
		if errors.Is(err, acoustid.ErrFpcalcMissing) {
			fmt.Fprintf(stderr, "%v\n\nInstall fpcalc (Chromaprint):\n", err)
			printFpcalcInstallHint(stderr)
		} else {
			fmt.Fprintf(stderr, "fpcalc precheck: %v\n", err)
		}
		return acoustid.Info{}, nil, 1
	}
	if noLookup {
		return info, nil, 0
	}
	userAgent := fmt.Sprintf("1-bit-bridge/%s (+https://github.com/acoseac/1-bit-bridge)",
		version.ServerVersion)
	return info, acoustid.NewClient("", key, userAgent, nil), 0
}

// fingerprintReport is the per-file result. CLI-only — this is a diagnostic
// shape, not a wire contract, and nothing decodes it but a human or a script
// the operator wrote.
type fingerprintReport struct {
	Path string `json:"path"`
	Err  string `json:"error,omitempty"`

	// Container fields come from the bridge's own extractor, so the numbers
	// are the ones production would gate on.
	DurationSec float64 `json:"durationSec,omitempty"`
	Codec       string  `json:"codec,omitempty"`
	IsDSD       bool    `json:"isDSD,omitempty"`
	SizeBytes   int64   `json:"sizeBytes,omitempty"`

	DecodedSec   float64 `json:"decodedSec,omitempty"`
	DistinctB64  int     `json:"distinctB64,omitempty"`
	BytesRead    int64   `json:"bytesRead,omitempty"`
	DecodeMillis int64   `json:"decodeMillis,omitempty"`
	LookupMillis int64   `json:"lookupMillis,omitempty"`

	Results []fingerprintResultRow `json:"results,omitempty"`

	Verdict string `json:"verdict"`          // "accept" | "reject" | "skipped"
	Reason  string `json:"reason,omitempty"` // the gate clause that refused

	ArtistMBID    string `json:"artistMBID,omitempty"`
	ArtistName    string `json:"artistName,omitempty"`
	RecordingMBID string `json:"recordingMBID,omitempty"`
	AlbumHint     string `json:"albumHint,omitempty"`
	AcoustID      string `json:"acoustID,omitempty"`
}

type fingerprintResultRow struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
	// Recordings holds one entry per recording rather than parallel
	// artist/title/sources slices. An earlier shape kept three slices in step
	// by hand and drifted the moment a recording carried no artist, printing a
	// real artist name beside the wrong title. A struct per recording makes
	// that misalignment unrepresentable rather than merely tested-against.
	Recordings []fingerprintRecordingRow `json:"recordings,omitempty"`
}

type fingerprintRecordingRow struct {
	Artist string `json:"artist"`
	// ArtistMBID is what the GATE actually compares — headArtistConsensus
	// keys on the ID, never the name. Printing only the name hid that a
	// placeholder-credited recording was breaking consensus on clusters with
	// overwhelming support, so the diagnostic now shows what the decision is
	// made from.
	ArtistMBID string `json:"artistMbid,omitempty"`
	Title      string `json:"title"`
	Sources    int    `json:"sources"`
}

func fingerprintOne(ctx context.Context, client *acoustid.Client, path string,
	length time.Duration, prefixBytes int64) fingerprintReport {

	rep := fingerprintReport{Path: path, Verdict: "skipped"}

	abs, err := filepath.Abs(path)
	if err != nil {
		rep.Err = err.Error()
		return rep
	}
	artist, ok := readContainerFacts(abs, &rep)
	if !ok {
		return rep
	}

	// The cheap screen first — it is what production runs before spending a
	// decode, so a diagnostic that skipped it would misreport the cost.
	if reason := acoustid.CheckEligible(rep.DurationSec, rep.IsDSD); reason != acoustid.ReasonNone {
		rep.Verdict, rep.Reason = "reject", string(reason)
		return rep
	}

	start := time.Now()
	var fp acoustid.Fingerprint
	if prefixBytes > 0 {
		fp, err = acoustid.ComputeFromPrefix(ctx, abs, length, prefixBytes)
	} else {
		fp, err = acoustid.Compute(ctx, abs, length)
	}
	rep.DecodeMillis = time.Since(start).Milliseconds()
	if err != nil {
		rep.Err = err.Error()
		return rep
	}
	rep.DecodedSec = fp.Duration
	rep.DistinctB64 = fp.DistinctB64
	rep.BytesRead = fp.BytesRead

	in := acoustid.Input{
		DurationSec: rep.DurationSec,
		IsDSD:       rep.IsDSD,
		Fingerprint: fp,
		// The diagnostic assumes a usable artist tag when one is present. The
		// junk classification that production applies lives in the enricher
		// (it needs the match-folding vocabulary), so this is an
		// approximation — and it only affects which sources threshold applies.
		HasLocalArtistWitness: artist != "",
	}

	// A prefix read makes fpcalc report the PREFIX's length, so the
	// decode-agreement clause would fire on every file. Feed it the container
	// duration instead, rather than reporting a rejection that is an artefact
	// of the measurement.
	if prefixBytes > 0 {
		in.Fingerprint.Duration = rep.DurationSec
	}

	if reason := acoustid.CheckFingerprint(in); reason != acoustid.ReasonNone {
		rep.Verdict, rep.Reason = "reject", string(reason)
		return rep
	}
	if client == nil {
		rep.Verdict, rep.Reason = "skipped", "no_lookup"
		return rep
	}
	// in.Fingerprint, NOT fp: in prefix mode the two differ, and AcoustID
	// matches on the duration we send. Passing the raw fp would submit the
	// prefix's length and could produce false misses. (Inert for FLAC today,
	// where fpcalc reports the STREAMINFO duration regardless of how much it
	// decoded — but that is a per-format accident, not a guarantee.)
	lookupAndDecide(ctx, client, in, in.Fingerprint, &rep)
	return rep
}

// readContainerFacts fills the container-derived fields and returns the local
// artist tag.
//
// It extracts through the SAME path the scanner uses, so the duration and DSD
// flag the gate sees here are the ones it would see in production. Using
// fpcalc's own decoded duration instead would have the decode-agreement clause
// compare a value against itself.
func readContainerFacts(abs string, rep *fingerprintReport) (artist string, ok bool) {
	if st, err := os.Stat(abs); err == nil {
		rep.SizeBytes = st.Size()
	}
	var track manifest.Track
	if err := manifest.Extract(abs, &track); err != nil {
		rep.Err = fmt.Sprintf("extract tags: %v", err)
		return "", false
	}
	rep.Codec = track.Codec
	if track.Duration != nil {
		rep.DurationSec = *track.Duration
	}
	if track.IsDSD != nil {
		rep.IsDSD = *track.IsDSD
	}
	return track.Artist, true
}

// lookupAndDecide performs the AcoustID call and records the gate's verdict.
func lookupAndDecide(ctx context.Context, client *acoustid.Client,
	in acoustid.Input, fp acoustid.Fingerprint, rep *fingerprintReport) {

	start := time.Now()
	results, err := client.Lookup(ctx, fp)
	rep.LookupMillis = time.Since(start).Milliseconds()
	if err != nil {
		if errors.Is(err, acoustid.ErrNoMatch) {
			rep.Verdict, rep.Reason = "reject", "no_results"
			return
		}
		rep.Err = err.Error()
		return
	}
	rep.Results = buildResultRows(results)

	in.Results = results
	decision, reason := acoustid.Accept(in)
	if reason != acoustid.ReasonNone {
		rep.Verdict, rep.Reason = "reject", string(reason)
		return
	}
	rep.Verdict = "accept"
	rep.ArtistMBID = decision.ArtistMBID
	rep.ArtistName = decision.ArtistName
	rep.RecordingMBID = decision.RecordingMBID
	rep.AlbumHint = decision.AlbumHint
	rep.AcoustID = decision.AcoustID
}

// fingerprintTotals accumulates the run-level numbers the cost measurement
// wants: how many files landed where, and what they took to produce.
type fingerprintTotals struct {
	accepted, rejected, failed int
	decode, lookup             time.Duration
	bytes                      int64
}

// buildResultRows flattens AcoustID results into the diagnostic's display rows.
//
// One struct per recording, carrying its own artist, title and source count —
// `sources` is a per-recording field in AcoustID's response, and pairing it
// with its recording here is what lets the report show which link in a cluster
// is well attested and which is a lone submission.
func buildResultRows(results []acoustid.Result) []fingerprintResultRow {
	rows := make([]fingerprintResultRow, 0, len(results))
	for _, r := range results {
		row := fingerprintResultRow{ID: r.ID, Score: r.Score}
		for _, rec := range r.Recordings {
			artist, artistMBID := "?", ""
			if len(rec.Artists) > 0 {
				artist, artistMBID = rec.Artists[0].Name, rec.Artists[0].ID
			}
			row.Recordings = append(row.Recordings, fingerprintRecordingRow{
				Artist: artist, ArtistMBID: artistMBID, Title: rec.Title, Sources: rec.Sources,
			})
		}
		rows = append(rows, row)
	}
	return rows
}

func printFingerprintReports(w io.Writer, info acoustid.Info, reports []fingerprintReport) {
	fmt.Fprintf(w, "fpcalc %s (%s)\n\n", nonEmptyOr(info.Version, "unknown version"), info.Path)
	var totals fingerprintTotals
	for _, r := range reports {
		printOneFingerprintReport(w, r, &totals)
	}
	printFingerprintTotals(w, len(reports), totals)
}

func printOneFingerprintReport(w io.Writer, r fingerprintReport, totals *fingerprintTotals) {
	fmt.Fprintf(w, "%s\n", r.Path)
	if r.Err != "" {
		totals.failed++
		fmt.Fprintf(w, "  error       %s\n\n", r.Err)
		return
	}
	printFingerprintContainer(w, r)
	printFingerprintTiming(w, r, totals)
	printFingerprintResults(w, r)
	printFingerprintVerdict(w, r, totals)
	fmt.Fprintln(w)
}

func printFingerprintContainer(w io.Writer, r fingerprintReport) {
	fmt.Fprintf(w, "  container   %s", formatSeconds(r.DurationSec))
	if r.Codec != "" {
		fmt.Fprintf(w, "  %s", r.Codec)
	}
	if r.IsDSD {
		fmt.Fprint(w, "  DSD")
	}
	if r.SizeBytes > 0 {
		fmt.Fprintf(w, "  %s", formatBytes(r.SizeBytes))
	}
	fmt.Fprintln(w)
}

func printFingerprintTiming(w io.Writer, r fingerprintReport, totals *fingerprintTotals) {
	if r.DecodeMillis > 0 {
		totals.decode += time.Duration(r.DecodeMillis) * time.Millisecond
		fmt.Fprintf(w, "  fingerprint %s  entropy %d/64  decoded %s",
			formatMillis(r.DecodeMillis), r.DistinctB64, formatSeconds(r.DecodedSec))
		if r.BytesRead > 0 {
			totals.bytes += r.BytesRead
			fmt.Fprintf(w, "  read %s", formatBytes(r.BytesRead))
		}
		fmt.Fprintln(w)
	}
	if r.LookupMillis > 0 {
		totals.lookup += time.Duration(r.LookupMillis) * time.Millisecond
		fmt.Fprintf(w, "  lookup      %s  %d result(s)\n",
			formatMillis(r.LookupMillis), len(r.Results))
	}
}

func printFingerprintResults(w io.Writer, r fingerprintReport) {
	for i, res := range r.Results {
		fmt.Fprintf(w, "    [%d] score %.2f  %d recording(s)\n", i+1, res.Score, len(res.Recordings))
		for _, rec := range res.Recordings {
			fmt.Fprintf(w, "        %-4d src  %s — %s\n", rec.Sources, rec.Artist, rec.Title)
		}
	}
}

func printFingerprintVerdict(w io.Writer, r fingerprintReport, totals *fingerprintTotals) {
	switch r.Verdict {
	case "accept":
		totals.accepted++
		fmt.Fprint(w, "  verdict     ACCEPT\n")
		fmt.Fprintf(w, "    artist    %s (%s)\n", r.ArtistName, r.ArtistMBID)
		if r.RecordingMBID != "" {
			fmt.Fprintf(w, "    recording %s\n", r.RecordingMBID)
		} else {
			fmt.Fprint(w, "    recording (ambiguous — not written)\n")
		}
		if r.AlbumHint != "" {
			fmt.Fprintf(w, "    album cue %q\n", r.AlbumHint)
		} else {
			fmt.Fprint(w, "    album cue (several release groups — falls back to the local tag)\n")
		}
	case "reject":
		totals.rejected++
		fmt.Fprintf(w, "  verdict     REJECT (%s)\n", r.Reason)
	default:
		fmt.Fprintf(w, "  verdict     skipped (%s)\n", nonEmptyOr(r.Reason, "n/a"))
	}
}

func printFingerprintTotals(w io.Writer, n int, t fingerprintTotals) {
	fmt.Fprintf(w, "%d file(s): %d accepted, %d rejected, %d errored\n",
		n, t.accepted, t.rejected, t.failed)
	if t.decode > 0 {
		fmt.Fprintf(w, "decode %s total", t.decode.Round(time.Millisecond))
		if t.lookup > 0 {
			fmt.Fprintf(w, ", lookup %s total", t.lookup.Round(time.Millisecond))
		}
		if t.bytes > 0 {
			fmt.Fprintf(w, ", %s fed to fpcalc", formatBytes(t.bytes))
		}
		fmt.Fprintln(w)
	}
	fmt.Fprint(w, "\nNothing was written — this command only reports.\n")
}

// printFpcalcInstallHint mirrors printSoxInstallHint's per-OS shape. Kept in
// step with the same hints in internal/doctor and the deployment runbook.
func printFpcalcInstallHint(w io.Writer) {
	switch runtime.GOOS {
	case "darwin":
		fmt.Fprint(w, "  brew install chromaprint\n")
	case "windows":
		fmt.Fprint(w, "  Download the official chromaprint-fpcalc zip from\n"+
			"    https://acoustid.org/chromaprint\n"+
			"  and put fpcalc.exe somewhere on PATH.\n")
	default:
		fmt.Fprint(w, "  Debian/Ubuntu:  sudo apt install libchromaprint-tools\n"+
			"  Fedora:         sudo dnf install chromaprint-tools\n"+
			"  Arch:           sudo pacman -S chromaprint\n"+
			"  Alpine:         apk add chromaprint\n"+
			"\n  NOTE the binary lives in the -tools package on Debian/Ubuntu,\n"+
			"  not in libchromaprint1 — same split as sox and libsox-fmt-all.\n")
	}
}

func nonEmptyOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func formatSeconds(s float64) string {
	if s <= 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d:%02d (%.1fs)", int(s)/60, int(s)%60, s)
}

func formatMillis(ms int64) string {
	return (time.Duration(ms) * time.Millisecond).Round(time.Millisecond).String()
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
