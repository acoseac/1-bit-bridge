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
// Exit codes: 0 clean, 1 runtime error, 2 usage error.
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

	if *key == "" {
		*key = os.Getenv("ACOUSTID_API_KEY")
	}
	if *key == "" && !*noLookup {
		fmt.Fprint(stderr, "No AcoustID key. Pass --key or set ACOUSTID_API_KEY.\n"+
			"Get a free application key at https://acoustid.org/new-application\n"+
			"Or pass --no-lookup to fingerprint without contacting AcoustID.\n")
		return 2
	}

	info, err := acoustid.Probe(ctx)
	if err != nil {
		if errors.Is(err, acoustid.ErrFpcalcMissing) {
			fmt.Fprintf(stderr, "%v\n\nInstall fpcalc (Chromaprint):\n", err)
			printFpcalcInstallHint(stderr)
		} else {
			fmt.Fprintf(stderr, "fpcalc precheck: %v\n", err)
		}
		return 1
	}

	var client *acoustid.Client
	if !*noLookup {
		userAgent := fmt.Sprintf("1-bit-bridge/%s (+https://github.com/acoseac/1-bit-bridge)",
			version.ServerVersion)
		client = acoustid.NewClient("", *key, userAgent, nil)
	}

	reports := make([]fingerprintReport, 0, len(paths))
	for i, p := range paths {
		if ctx.Err() != nil {
			break
		}
		// Pace between files exactly as the sweeper will — the politeness
		// contract is per-request, and a diagnostic that bursts is the one
		// most likely to get a key rate-limited.
		if i > 0 && client != nil {
			select {
			case <-time.After(client.MinInterval()):
			case <-ctx.Done():
				return 1
			}
		}
		reports = append(reports, fingerprintOne(ctx, client, p,
			time.Duration(*lengthSec)*time.Second, *prefixBytes))
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(reports); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
		return 0
	}
	printFingerprintReports(stdout, info, reports)
	return 0
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
	ID         string   `json:"id"`
	Score      float64  `json:"score"`
	Sources    int      `json:"sources"`
	Recordings int      `json:"recordings"`
	Artists    []string `json:"artists,omitempty"`
	Titles     []string `json:"titles,omitempty"`
}

func fingerprintOne(ctx context.Context, client *acoustid.Client, path string,
	length time.Duration, prefixBytes int64) fingerprintReport {

	rep := fingerprintReport{Path: path, Verdict: "skipped"}

	abs, err := filepath.Abs(path)
	if err != nil {
		rep.Err = err.Error()
		return rep
	}
	if st, err := os.Stat(abs); err == nil {
		rep.SizeBytes = st.Size()
	}

	// Extract through the SAME path the scanner uses, so the duration and DSD
	// flag the gate sees here are the ones it would see in production. Using
	// fpcalc's own decoded duration instead would make the decode-agreement
	// clause compare a value against itself.
	var track manifest.Track
	if err := manifest.Extract(abs, &track); err != nil {
		rep.Err = fmt.Sprintf("extract tags: %v", err)
		return rep
	}
	rep.Codec = track.Codec
	if track.Duration != nil {
		rep.DurationSec = *track.Duration
	}
	if track.IsDSD != nil {
		rep.IsDSD = *track.IsDSD
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
		HasLocalArtistWitness: track.Artist != "",
	}

	// A prefix read makes fpcalc report the PREFIX's length, so the
	// decode-agreement clause would fire on every file. Feed it the container
	// duration instead, and say so, rather than silently reporting a rejection
	// that is an artefact of the measurement.
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

	start = time.Now()
	results, err := client.Lookup(ctx, fp)
	rep.LookupMillis = time.Since(start).Milliseconds()
	if err != nil {
		if errors.Is(err, acoustid.ErrNoMatch) {
			rep.Verdict, rep.Reason = "reject", "no_results"
			return rep
		}
		rep.Err = err.Error()
		return rep
	}
	for _, r := range results {
		row := fingerprintResultRow{
			ID: r.ID, Score: r.Score, Sources: r.Sources, Recordings: len(r.Recordings),
		}
		for _, rec := range r.Recordings {
			if len(rec.Artists) > 0 {
				row.Artists = append(row.Artists, rec.Artists[0].Name)
			}
			row.Titles = append(row.Titles, rec.Title)
		}
		rep.Results = append(rep.Results, row)
	}

	in.Results = results
	decision, reason := acoustid.Accept(in)
	if reason != acoustid.ReasonNone {
		rep.Verdict, rep.Reason = "reject", string(reason)
		return rep
	}
	rep.Verdict = "accept"
	rep.ArtistMBID = decision.ArtistMBID
	rep.ArtistName = decision.ArtistName
	rep.RecordingMBID = decision.RecordingMBID
	rep.AlbumHint = decision.AlbumHint
	rep.AcoustID = decision.AcoustID
	return rep
}

func printFingerprintReports(w io.Writer, info acoustid.Info, reports []fingerprintReport) {
	fmt.Fprintf(w, "fpcalc %s (%s)\n\n", nonEmptyOr(info.Version, "unknown version"), info.Path)

	var accepted, rejected, failed int
	var totalDecode, totalLookup time.Duration
	var totalBytes int64

	for _, r := range reports {
		fmt.Fprintf(w, "%s\n", r.Path)
		if r.Err != "" {
			failed++
			fmt.Fprintf(w, "  error       %s\n\n", r.Err)
			continue
		}
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

		if r.DecodeMillis > 0 {
			totalDecode += time.Duration(r.DecodeMillis) * time.Millisecond
			fmt.Fprintf(w, "  fingerprint %s  entropy %d/64  decoded %s",
				formatMillis(r.DecodeMillis), r.DistinctB64, formatSeconds(r.DecodedSec))
			if r.BytesRead > 0 {
				totalBytes += r.BytesRead
				fmt.Fprintf(w, "  read %s", formatBytes(r.BytesRead))
			}
			fmt.Fprintln(w)
		}
		if r.LookupMillis > 0 {
			totalLookup += time.Duration(r.LookupMillis) * time.Millisecond
			fmt.Fprintf(w, "  lookup      %s  %d result(s)\n",
				formatMillis(r.LookupMillis), len(r.Results))
		}
		for i, res := range r.Results {
			fmt.Fprintf(w, "    [%d] score %.2f  sources %d  recordings %d\n",
				i+1, res.Score, res.Sources, res.Recordings)
			for j := range res.Titles {
				artist := "?"
				if j < len(res.Artists) {
					artist = res.Artists[j]
				}
				fmt.Fprintf(w, "        %s — %s\n", artist, res.Titles[j])
			}
		}

		switch r.Verdict {
		case "accept":
			accepted++
			fmt.Fprintf(w, "  verdict     ACCEPT\n")
			fmt.Fprintf(w, "    artist    %s (%s)\n", r.ArtistName, r.ArtistMBID)
			if r.RecordingMBID != "" {
				fmt.Fprintf(w, "    recording %s\n", r.RecordingMBID)
			} else {
				fmt.Fprintf(w, "    recording (ambiguous — not written)\n")
			}
			if r.AlbumHint != "" {
				fmt.Fprintf(w, "    album cue %q\n", r.AlbumHint)
			} else {
				fmt.Fprintf(w, "    album cue (several release groups — falls back to the local tag)\n")
			}
		case "reject":
			rejected++
			fmt.Fprintf(w, "  verdict     REJECT (%s)\n", r.Reason)
		default:
			fmt.Fprintf(w, "  verdict     skipped (%s)\n", nonEmptyOr(r.Reason, "n/a"))
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "%d file(s): %d accepted, %d rejected, %d errored\n",
		len(reports), accepted, rejected, failed)
	if totalDecode > 0 {
		fmt.Fprintf(w, "decode %s total", totalDecode.Round(time.Millisecond))
		if totalLookup > 0 {
			fmt.Fprintf(w, ", lookup %s total", totalLookup.Round(time.Millisecond))
		}
		if totalBytes > 0 {
			fmt.Fprintf(w, ", %s fed to fpcalc", formatBytes(totalBytes))
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
