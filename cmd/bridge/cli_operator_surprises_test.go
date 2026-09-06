package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dupes"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// --- the write gate ---------------------------------------------------------

// TestWriteGateRefusesWhileABridgeAnswers pins the gate `bridge restore` and
// `bridge manifest clear-missing` gained.
//
// Both mutate the store from a SECOND process — restore renames a fresh
// bridge.db over the open one after unlinking the live WAL, clear-missing runs
// a two-table DELETE — and Store.mu serialises writers within ONE process only
// (busy_timeout is a retry, not a serializer). Their sibling mutators
// (tryLibraryViaAdmin, enrichmentRetryCmd) already refuse this state; the two
// most destructive did not, on the strength of a docblock claiming there was
// "no PID file today", which stopped being true when pidfile.go landed.
func TestWriteGateRefusesWhileABridgeAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	cfg := &config.Config{AdminAddress: addr}
	var se bytes.Buffer
	if !refuseIfBridgeMayBeRunning(context.Background(), cfg, "restore", &se) {
		t.Error("the gate allowed a write while a bridge was answering on the admin port")
	}
	if !strings.Contains(se.String(), "a bridge is answering") {
		t.Errorf("no explanation on stderr:\n%s", se.String())
	}
}

// TestWriteGateAllowsWhenNothingIsListening is the other half — the gate must
// not be a blanket refusal, or the offline recovery paths it protects become
// unusable.
func TestWriteGateAllowsWhenNothingIsListening(t *testing.T) {
	cfg := &config.Config{AdminAddress: "127.0.0.1:" + closedLoopbackPort(t)}
	var se bytes.Buffer
	if refuseIfBridgeMayBeRunning(context.Background(), cfg, "restore", &se) {
		t.Errorf("the gate refused with nothing listening:\n%s", se.String())
	}
}

// TestWriteGateRefusesAnEphemeralAdminPortWithAnAccurateReason pins the port-0
// branch.
//
// probeBridge treats anything but connection-refused as "running" — deliberate,
// and the right default for a write gate. But `adminAddress: 127.0.0.1:0` names
// no fixed port to dial, so the probe cannot answer the question at all, and
// falling through to the default produced the message "a bridge is answering on
// 127.0.0.1:0" about an address where nothing answers and nothing can.
//
// It must still REFUSE — failing closed is right when liveness is unknowable —
// but say the true reason, because the two have different remedies.
func TestWriteGateRefusesAnEphemeralAdminPortWithAnAccurateReason(t *testing.T) {
	cfg := &config.Config{AdminAddress: "127.0.0.1:0"}
	var se bytes.Buffer
	if !refuseIfBridgeMayBeRunning(context.Background(), cfg, "restore", &se) {
		t.Fatal("an unprobeable admin address was allowed through; the gate must fail closed")
	}
	out := se.String()
	if strings.Contains(out, "a bridge is answering") {
		t.Errorf("the refusal claims a bridge answered on a port where none can:\n%s", out)
	}
	if !strings.Contains(out, "no fixed port") {
		t.Errorf("the refusal does not say why it could not tell:\n%s", out)
	}
}

// --- variants move --dry-run ------------------------------------------------

// TestVariantsMoveDryRunDoesNotCreateDestination pins the promise the flag
// makes: "list planned moves without touching files or DB". Creating the
// destination directory is touching the filesystem, and on a preview the
// operator may still be deciding where to point it.
func TestVariantsMoveDryRunDoesNotCreateDestination(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeInstallAt(t, dir, "Artist/Album/01.flac")
	dest := filepath.Join(dir, "not-yet-chosen")

	var so, se bytes.Buffer
	_ = variantsMoveCmd(context.Background(),
		[]string{"--config", cfgPath, "--to", dest, "--dry-run"}, &so, &se)

	if _, err := os.Stat(dest); err == nil {
		t.Errorf("--dry-run created %s; the flag promises not to touch the filesystem\nstdout:\n%s\nstderr:\n%s",
			dest, so.String(), se.String())
	}
}

// --- duplicates --json ------------------------------------------------------

// TestNarrowDupeReportAppliesTierFilters pins that --tier / --nested-only reach
// the JSON path.
//
// They used to be applied inside printDupeReport only, so `--json --tier X`
// emitted every tier. It read as working because --limit IS honoured (it is
// applied in buildDupeReport), so the output looked shaped by the flags.
func TestNarrowDupeReportAppliesTierFilters(t *testing.T) {
	full := &dupeReport{
		Scanned:     100,
		GroupsTotal: 3,
		Tiers: []dupeTierReport{
			{Tier: string(dupes.TierIdenticalAudio), Groups: 1},
			{Tier: string(dupes.TierDifferentFormat), Groups: 1},
			{Tier: string(dupes.TierSelfNested), Groups: 1},
		},
	}

	byTier := narrowDupeReport(full, &duplicatesOpts{tier: string(dupes.TierIdenticalAudio)})
	if len(byTier.Tiers) != 1 || byTier.Tiers[0].Tier != string(dupes.TierIdenticalAudio) {
		t.Errorf("--tier did not narrow the JSON report: %+v", byTier.Tiers)
	}

	nested := narrowDupeReport(full, &duplicatesOpts{nestedOnly: true})
	if len(nested.Tiers) != 1 || nested.Tiers[0].Tier != string(dupes.TierSelfNested) {
		t.Errorf("--nested-only did not narrow the JSON report: %+v", nested.Tiers)
	}

	// The library-wide counts stay put: they are the denominator a narrowed
	// report is read against, and rescoping them silently would make a
	// filtered report look like a whole-library one.
	if byTier.Scanned != 100 || byTier.GroupsTotal != 3 {
		t.Errorf("narrowing rewrote the library-wide counts: scanned=%d groupsTotal=%d",
			byTier.Scanned, byTier.GroupsTotal)
	}
	// And the input must not be mutated — printDupeReport renders the same
	// value, so an in-place filter would make the two paths differ by call
	// order.
	if len(full.Tiers) != 3 {
		t.Errorf("narrowDupeReport mutated its input: %d tiers left", len(full.Tiers))
	}

	unfiltered := narrowDupeReport(full, &duplicatesOpts{})
	if len(unfiltered.Tiers) != 3 {
		t.Errorf("no filter should mean no narrowing, got %d tiers", len(unfiltered.Tiers))
	}
}

// --- enrichment retry positional args ---------------------------------------

// TestEnrichmentRetryRejectsPositionalPath pins the guard on the widening this
// command can silently do.
//
// flag.Parse stops at the first non-flag argument, so
// `bridge enrichment retry Artist/Album` parses cleanly with --path EMPTY — and
// an empty scope means the WHOLE LIBRARY. The operator asked for one album and
// would get an enriched_at reset across everything, which is a whole-library
// delta to every paired device.
func TestEnrichmentRetryRejectsPositionalPath(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeInstallAt(t, dir, "Artist/Album/01.flac")

	var so, se bytes.Buffer
	code := enrichmentRetryCmd(context.Background(),
		[]string{"--config", cfgPath, "Artist/Album"}, &so, &se)

	if code != 2 {
		t.Errorf("exit %d for a positional path; want 2 (refused) — an unflagged path "+
			"silently widens the retry to the whole library\nstdout:\n%s", code, so.String())
	}
	if !strings.Contains(se.String(), "--path") {
		t.Errorf("the refusal does not point at the flag form:\n%s", se.String())
	}
}

// --- enrichment retry clears fingerprint suppression -------------------------

// TestEnrichmentRetryOfflineClearsFingerprintSuppression pins the half of the
// "Retry missing" job the offline path was skipping.
//
// The command's own header calls itself "the scripted equivalent of the admin
// console's Retry missing button". Both admin paths additionally clear the
// persisted AcoustID markers; this one only reset enriched_at, leaving every
// acoustid_nomatch_* / acoustid_veto_* marker standing for up to 30 days.
//
// Those markers cover exactly the population an operator runs this for — the
// tracks the text ladder already failed on, which is what fingerprinting
// exists to rescue — so the command reported "re-queued N tracks" while
// silently excluding them.
func TestEnrichmentRetryOfflineClearsFingerprintSuppression(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfgPath := writeInstallAt(t, dir, "Artist/Album/01.flac")

	// Plant both marker kinds — they are cleared by ONE statement but READ
	// separately, so a test covering only one would pass against a half-fix.
	store, err := manifest.OpenStore(manifest.DefaultDBPath(filepath.Join(dir, "data")))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAcoustIDNoMatch(ctx, "Artist/Album/01.flac", 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAcoustIDTagVeto(ctx, "Artist/Album/02.flac", 1, 1, "Some Artist"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var so, se bytes.Buffer
	if code := enrichmentRetryCmd(ctx, []string{"--config", cfgPath}, &so, &se); code != 0 {
		t.Fatalf("retry exit %d\nstdout:\n%s\nstderr:\n%s", code, so.String(), se.String())
	}

	reopened, err := manifest.OpenStore(manifest.DefaultDBPath(filepath.Join(dir, "data")))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	nm, err := reopened.FreshAcoustIDNoMatches(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	tv, err := reopened.FreshAcoustIDTagVetoes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nm) != 0 || len(tv) != 0 {
		t.Errorf("offline retry left fingerprint suppression standing: %d no-match, %d veto — "+
			"the tracks this command exists to rescue stay sidelined\nstdout:\n%s",
			len(nm), len(tv), so.String())
	}
}

// --- writeJSONIndent --------------------------------------------------------

// TestWriteJSONIndentKeepsErrorsOffTheJSONStream pins the stdout-purity rule
// all three --json surfaces depend on (`status`, `doctor --json`,
// `cert info --json`). The encoder may already have flushed a partial object,
// and appending prose to that yields neither valid JSON nor a readable message.
func TestWriteJSONIndentKeepsErrorsOffTheJSONStream(t *testing.T) {
	var out, errOut bytes.Buffer
	// A channel cannot be marshalled — a reliable encoder failure.
	if code := writeJSONIndent(&out, &errOut, map[string]any{"bad": make(chan int)}); code == 0 {
		t.Fatal("expected a non-zero code from an unmarshalable value")
	}
	if strings.Contains(out.String(), "encode JSON") {
		t.Errorf("the error was written into the JSON stream:\n%q", out.String())
	}
	if !strings.Contains(errOut.String(), "encode JSON") {
		t.Errorf("the error did not reach stderr:\n%q", errOut.String())
	}
	// Whatever landed on stdout must still be parseable-or-empty, never prose.
	if s := strings.TrimSpace(out.String()); s != "" {
		if err := json.Unmarshal([]byte(s), new(any)); err != nil {
			t.Errorf("stdout is neither empty nor valid JSON: %q", s)
		}
	}
}
