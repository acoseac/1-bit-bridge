package updater

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// standInBinary puts the stand-in at a path of its own named name (".exe"
// added on Windows when exe), scripted by files, and returns its path.
func standInBinary(t *testing.T, name string, exe bool, files map[string]string) (path, script string) {
	t.Helper()
	script = candidateStandIn(t, files)
	if exe && runtime.GOOS == "windows" {
		name += ".exe"
	}
	return installCandidateCopy(t, filepath.Join(t.TempDir(), name)), script
}

func TestCheckConfigReadsTheCandidatesAnswerByItsExitCode(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "bridge.yaml")
	for _, tc := range []struct {
		name    string
		files   map[string]string
		verdict ConfigVerdict
		reasons []string
		detail  string
	}{
		{"valid", map[string]string{"check": "valid"}, ConfigValid, nil, ""},
		{"refused", map[string]string{"check": "refuse", "refusal": "line 3: one\nline 9: two"}, ConfigRefused,
			[]string{"line 3: one", "line 9: two"}, ""},
		{"a release that predates the check", map[string]string{"check": "old"}, ConfigCheckUnsupported, nil, ""},
		{"a settings file it could not read", map[string]string{"check": "exit:1"}, ConfigCheckFailed, nil, "status 1"},
		// A 2 without the flag package's refusal of the flag is a check
		// that failed, never the flag unknown: a panic exits 2 too.
		{"a 2 of another kind", map[string]string{"check": "exit:2"}, ConfigCheckFailed, nil, "status 2"},
		{"any other exit", map[string]string{"check": "exit:3"}, ConfigCheckFailed, nil, "status 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate, script := standInBinary(t, "bridge", true, tc.files)
			got := CheckConfig(context.Background(), candidate, cfg)
			if got.Verdict != tc.verdict {
				t.Fatalf("verdict = %v (%+v), want %v", got.Verdict, got, tc.verdict)
			}
			if strings.Join(got.Reasons, "\n") != strings.Join(tc.reasons, "\n") {
				t.Errorf("reasons = %q, want %q", got.Reasons, tc.reasons)
			}
			if !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("detail = %q, want it to say %q", got.Detail, tc.detail)
			}
			calls := candidateCalls(t, script)
			if want := "serve --" + ConfigCheckFlag + " --config " + cfg; len(calls) != 1 || calls[0] != want {
				t.Errorf("the candidate was run as %q, want once as %q", calls, want)
			}
		})
	}
}

func TestCheckConfigBoundsACandidateThatHangs(t *testing.T) {
	prev := configCheckTimeout
	configCheckTimeout = time.Second
	t.Cleanup(func() { configCheckTimeout = prev })
	candidate, _ := standInBinary(t, "bridge", true, map[string]string{"check": "hang"})
	start := time.Now()
	got := CheckConfig(context.Background(), candidate, filepath.Join(t.TempDir(), "bridge.yaml"))
	if got.Verdict != ConfigCheckFailed || !strings.Contains(got.Detail, "did not answer within") || got.Err != nil {
		t.Errorf("a hung check = %+v, want a failure past its bound", got)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("the hung check held its caller %s", took)
	}
}

func TestCheckConfigStoppedByItsContextCarriesTheCancellation(t *testing.T) {
	candidate, _ := standInBinary(t, "bridge", true, map[string]string{"check": "hang"})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	got := CheckConfig(ctx, candidate, filepath.Join(t.TempDir(), "bridge.yaml"))
	if got.Verdict != ConfigCheckFailed || got.Err == nil {
		t.Errorf("a check its context stopped = %+v, want a failure carrying the context's error", got)
	}
}

func TestACandidateThatCannotRunIsAFailedCheckNamingNoPath(t *testing.T) {
	dir := t.TempDir()
	notAProgram := filepath.Join(dir, "bridge")
	if err := os.WriteFile(notAProgram, []byte("not a program"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string]string{
		"missing":       filepath.Join(dir, "gone", "bridge"),
		"not a program": notAProgram,
	} {
		t.Run(name, func(t *testing.T) {
			got := CheckConfig(context.Background(), candidate, filepath.Join(dir, "bridge.yaml"))
			if got.Verdict != ConfigCheckFailed || !strings.Contains(got.Detail, "could not be run") {
				t.Fatalf("check = %+v, want a failure to run", got)
			}
			if strings.Contains(got.Detail, dir) {
				t.Errorf("the detail names a path: %q", got.Detail)
			}
		})
	}
}

func TestARefusalsReasonsAreBoundedAndPrintable(t *testing.T) {
	var stderr strings.Builder
	stderr.WriteString("2026/11/02 INFO a log line that is not a reason\n")
	stderr.WriteString(ConfigRefusedPrefix + "line 1: a\x1b[31m colour\tand a tab\n")
	stderr.WriteString(ConfigRefusedPrefix + "line 2: bad \xff byte\n")
	stderr.WriteString(ConfigRefusedPrefix + strings.Repeat("é", 400) + "\n")
	for i := range 12 {
		stderr.WriteString(ConfigRefusedPrefix + "line " + strings.Repeat("9", i+1) + ": more\n")
	}
	got := refusalLines([]byte(stderr.String()))
	if len(got) != maxRefusalLines+1 || got[len(got)-1] != "and more" {
		t.Fatalf("%d reasons (%q), want %d and the note that more were given", len(got), got, maxRefusalLines+1)
	}
	if got[0] != "line 1: a [31m colour and a tab" || !strings.HasPrefix(got[1], "line 2: bad ") {
		t.Errorf("reasons = %q, want control characters made spaces and the bytes made valid", got[:2])
	}
	for _, r := range got {
		if len(r) > maxRefusalLine || !utf8.ValidString(r) || strings.ContainsAny(r, "\x1b\t\n\r") {
			t.Errorf("reason not bounded and printable: %q", r)
		}
	}
}

func TestProbeVersionNamesTheVersionTheBinarySays(t *testing.T) {
	for _, tc := range []struct {
		name, file, version string
		ok                  bool
	}{
		{"a backup", "bridge.bak", "0.2.1", true},
		{"a Windows backup, by its own name", "bridge.exe.bak", "0.2.1", true},
		{"a version line it cannot read", "bridge.bak", "two words", false},
		{"no version", "bridge.bak", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			if tc.version != "" {
				files["version"] = tc.version
			}
			bak, _ := standInBinary(t, tc.file, false, files)
			got, err := ProbeVersion(context.Background(), bak)
			if tc.ok != (err == nil) || (tc.ok && got != tc.version) {
				t.Errorf("ProbeVersion = %q, %v; want %q, ok %v", got, err, tc.version, tc.ok)
			}
		})
	}
}

func TestConfigDigestFollowsTheFileAndTheBridgeVariables(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "bridge.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	digest := func() string {
		t.Helper()
		d, err := ConfigDigest(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	write("scanIntervalSec: 010\n")
	first := digest()
	if again := digest(); again != first {
		t.Fatal("the digest of unchanged settings changed")
	}
	t.Setenv("UNRELATED_TO_THE_BRIDGE", "x")
	t.Setenv("BRIDGE_UNSET_FOR_THE_TEST", "")
	if d := digest(); d != first {
		t.Error("a variable that is not the bridge's, or one set empty, changed the digest")
	}
	t.Setenv("BRIDGE_SCAN_INTERVAL_SEC", "10")
	withEnv := digest()
	if withEnv == first {
		t.Error("a BRIDGE_ variable the loader reads did not change the digest")
	}
	write("scanIntervalSec: 10\n")
	if d := digest(); d == withEnv {
		t.Error("an edit of the file did not change the digest")
	}
	if len(withEnv) != 64 {
		t.Errorf("the digest is not a SHA-256 in hex: %q", withEnv)
	}
	if _, err := ConfigDigest(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("the digest of a missing file answered")
	}
}

// TestTheSettingsDigestReadsTheBridgeVariablesAsThePlatformNamesThem: on
// Windows an environment name compares without regard to case, so config's
// overrides read a BRIDGE_ variable set as bridge_data_dir, and the digest
// must count it, under one spelling whichever was set. Elsewhere the name is
// the bytes, and bridge_data_dir is no setting.
func TestTheSettingsDigestReadsTheBridgeVariablesAsThePlatformNamesThem(t *testing.T) {
	environ := []string{"bridge_data_dir=/srv/data", "Bridge_Scan_Interval_Sec=10", "BRIDGE_LISTEN_ADDRESS=:7788",
		"BRIDGE_EMPTY=", "bridge_empty_too=", "PATH=/bin", "=C:=C:\\"}
	if got, want := bridgeEnvironment(environ, true),
		[]string{"BRIDGE_DATA_DIR=/srv/data", "BRIDGE_LISTEN_ADDRESS=:7788", "BRIDGE_SCAN_INTERVAL_SEC=10"}; !slices.Equal(got, want) {
		t.Errorf("names that fold case: %q, want %q", got, want)
	}
	if got, want := bridgeEnvironment(environ, false), []string{"BRIDGE_LISTEN_ADDRESS=:7788"}; !slices.Equal(got, want) {
		t.Errorf("names that are their bytes: %q, want %q", got, want)
	}

	cfg := filepath.Join(t.TempDir(), "bridge.yaml")
	if err := os.WriteFile(cfg, []byte("scanIntervalSec: 010\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := func(environ []string, foldCase bool) string {
		t.Helper()
		d, err := configDigest(cfg, environ, foldCase)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	lower, upper := []string{"bridge_data_dir=/srv/data"}, []string{"BRIDGE_DATA_DIR=/srv/data"}
	if digest(lower, true) == digest(nil, true) {
		t.Error("on Windows, a BRIDGE_ variable set in lower case did not change the digest")
	}
	if digest(lower, true) != digest(upper, true) {
		t.Error("on Windows, one variable set in two cases gave two digests")
	}
	if digest(lower, false) != digest(nil, false) {
		t.Error("elsewhere, a variable config does not read changed the digest")
	}
}

// TestTheSettingsDigestKeepsTheVariableTheOSReads: on Windows an environment
// block can hold one variable twice, in two cases, and config's overrides
// read it through os.LookupEnv, which asks GetEnvironmentVariable for the
// first entry its name matches whatever the case, in the order os.Environ
// lists them; an empty value there is an unset setting, and the second entry
// is never read. So the digest keeps the first entry of each name, folded,
// before it drops an empty one (backlog B613's round 3).
func TestTheSettingsDigestKeepsTheVariableTheOSReads(t *testing.T) {
	for _, tc := range []struct {
		name    string
		environ []string
		want    []string
	}{
		{"lower case first", []string{"bridge_data_dir=/a", "BRIDGE_DATA_DIR=/b"}, []string{"BRIDGE_DATA_DIR=/a"}},
		{"upper case first", []string{"BRIDGE_DATA_DIR=/b", "bridge_data_dir=/a"}, []string{"BRIDGE_DATA_DIR=/b"}},
		{"the first empty", []string{"BRIDGE_DATA_DIR=", "bridge_data_dir=/a"}, nil},
		{"the second empty", []string{"bridge_data_dir=/a", "BRIDGE_DATA_DIR="}, []string{"BRIDGE_DATA_DIR=/a"}},
		{"three spellings", []string{"Bridge_Data_Dir=/c", "bridge_data_dir=/a", "BRIDGE_DATA_DIR=/b"}, []string{"BRIDGE_DATA_DIR=/c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bridgeEnvironment(tc.environ, true); !slices.Equal(got, tc.want) {
				t.Errorf("bridgeEnvironment(%q) = %q, want %q", tc.environ, got, tc.want)
			}
		})
	}
	// Where names are their bytes, two cases are two variables.
	if got, want := bridgeEnvironment([]string{"BRIDGE_DATA_DIR=/b", "bridge_data_dir=/a"}, false),
		[]string{"BRIDGE_DATA_DIR=/b"}; !slices.Equal(got, want) {
		t.Errorf("names that are their bytes: %q, want %q", got, want)
	}
}

func TestHeldBackHoldsARefusedVersionOnlyWhileTheSettingsAreUnchanged(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfg, []byte("scanIntervalSec: 010\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := ConfigDigest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	save := func(st State) {
		t.Helper()
		if err := SaveState(dir, st); err != nil {
			t.Fatal(err)
		}
	}

	save(State{RejectedVersion: "0.2.0"})
	if h := HeldBack(dir, cfg, "0.2.0"); h.Version != "0.2.0" || h.Recorded || !strings.Contains(h.Reason, "rolled back") {
		t.Errorf("an operator's rollback = %+v, want it held with the rollback's words", h)
	}

	save(State{RejectedVersion: "0.2.0", RejectedReason: "0.2.0 refuses this bridge's settings: line 1: x", RejectedConfigDigest: digest})
	h := HeldBack(dir, cfg, "0.2.0")
	if h.Version != "0.2.0" || !h.Recorded || !strings.Contains(h.Reason, "line 1: x") || !strings.Contains(h.Reason, "settings change") {
		t.Errorf("a refusal of these settings = %+v, want it held with its reason", h)
	}
	if h := HeldBack(dir, cfg, "0.3.0"); h.Version != "" {
		t.Errorf("a newer version = %+v, want it admitted", h)
	}
	if h := HeldBack(dir, filepath.Join(dir, "missing.yaml"), "0.2.0"); h.Version == "" {
		t.Error("settings that cannot be read admitted the refused version")
	}
	if err := os.WriteFile(cfg, []byte("scanIntervalSec: 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if h := HeldBack(dir, cfg, "0.2.0"); h.Version != "" {
		t.Errorf("the settings fixed, the refused version = %+v, want it admitted", h)
	}

	save(State{RejectedVersion: "0.2.0", RejectedReason: "0.2.0 predates the settings check"})
	if h := HeldBack(dir, cfg, "0.2.0"); h.Version != "0.2.0" || !h.Recorded || strings.Contains(h.Reason, "settings change") {
		t.Errorf("a candidate that predates the check = %+v, want it held whatever the settings", h)
	}
	if h := HeldBack("", cfg, "0.2.0"); h.Version != "" {
		t.Errorf("no data dir = %+v, want nothing held", h)
	}
}

func TestEarlyRollbackProvesTheInstallFirst(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	armed := State{Status: "installing", TargetVersion: "0.2.3", PreviousVersion: "0.2.1", AttemptedAt: now.Add(-time.Minute), SwapStarted: true}
	for _, tc := range []struct {
		name string
		edit func(*State)
		ok   bool
		why  string
	}{
		{"an armed install of this version", func(*State) {}, true, ""},
		{"a v before the version", func(s *State) { s.TargetVersion = "v0.2.3" }, true, ""},
		{"no marker", func(s *State) { *s = State{} }, false, ""},
		{"an install of another version", func(s *State) { s.TargetVersion = "0.2.4" }, false, ""},
		{"an install already confirmed", func(s *State) { s.Status = "installed" }, false, ""},
		{"no swap", func(s *State) { s.SwapStarted = false }, false, "swap"},
		{"no previous version", func(s *State) { s.PreviousVersion = "" }, false, "no other version"},
		{"the previous version is this one", func(s *State) { s.PreviousVersion = "0.2.3" }, false, "no other version"},
		{"past the window", func(s *State) { s.AttemptedAt = now.Add(-recencyWindow - time.Second) }, false, "older"},
		{"just inside the window", func(s *State) { s.AttemptedAt = now.Add(-recencyWindow) }, true, ""},
		{"dated in the future", func(s *State) { s.AttemptedAt = now.Add(time.Hour) }, false, "future"},
		{"a step of the clock", func(s *State) { s.AttemptedAt = now.Add(30 * time.Second) }, true, ""},
		{"the data dir opened since", func(s *State) { s.DataDirOpenedBy = "0.2.3" }, false, "opened the data dir"},
		// The early rollback's own record, written just before its rename: a
		// start of this version that finds it was stopped before the backup
		// was back, and finishes the rollback.
		{"a rollback stopped before its rename", stoppedRollback, true, ""},
		{"a rollback stopped before its rename, past the window", func(s *State) {
			stoppedRollback(s)
			s.AttemptedAt = now.Add(-recencyWindow - time.Second)
		}, false, "older"},
		{"a rollback stopped before its rename, the data dir opened since", func(s *State) {
			stoppedRollback(s)
			s.DataDirOpenedBy = "0.2.3"
		}, false, "opened the data dir"},
		{"a hold alone", func(s *State) { *s = State{RejectedVersion: "0.2.3"} }, false, ""},
		{"an install already confirmed that says a rollback started", func(s *State) {
			s.Status, s.RollbackStarted = "installed", true
		}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := armed
			tc.edit(&st)
			ok, why := EarlyRollback(st, "0.2.3", now)
			if ok != tc.ok || !strings.Contains(why, tc.why) || (tc.why == "" && why != "") {
				t.Errorf("EarlyRollback = %v, %q; want %v, %q", ok, why, tc.ok, tc.why)
			}
		})
	}
}

// stoppedRollback makes an armed install's marker what the early rollback
// writes just before it puts the backup back: the hold, beside the install
// it undoes, with no Status, so the restored binary's boot keeps it.
func stoppedRollback(s *State) {
	s.Status, s.RollbackStarted = "", true
	s.RejectedVersion, s.RejectedReason, s.RejectedConfigDigest = s.TargetVersion, "it refused the settings", "digest"
}

func TestADataDirRecordIsArmedByAnInstallOfAnotherVersionThanTheBackups(t *testing.T) {
	armed := State{Status: "installing", TargetVersion: "0.2.3", PreviousVersion: "0.2.1", SwapStarted: true}
	for _, tc := range []struct {
		name    string
		edit    func(*State)
		current string
		want    bool
	}{
		{"the installed version", func(*State) {}, "0.2.3", true},
		{"a version the install did not name", func(*State) {}, "0.2.9", true},
		{"the backup's own version", func(*State) {}, "0.2.1", false},
		{"recorded already", func(s *State) { s.DataDirOpenedBy = "0.2.3" }, "0.2.3", false},
		{"no install armed", func(s *State) { s.Status = "installed" }, "0.2.3", false},
		{"no marker", func(s *State) { *s = State{} }, "0.2.3", false},
		{"a rollback stopped before its rename", stoppedRollback, "0.2.3", true},
		{"a rollback stopped before its rename, by the backup's version", stoppedRollback, "0.2.1", false},
		{"a hold alone", func(s *State) { *s = State{RejectedVersion: "0.2.3"} }, "0.2.3", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := armed
			tc.edit(&st)
			if got := ArmsDataDirRecord(st, tc.current); got != tc.want {
				t.Errorf("ArmsDataDirRecord = %v, want %v", got, tc.want)
			}
		})
	}
}
