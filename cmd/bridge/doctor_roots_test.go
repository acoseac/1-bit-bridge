package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// Backlog B650 through bridge doctor and the console's doctor: a library root
// whose mount does not answer is named within the doctor's bound, by the CLI
// and by the console's runner, which asks it once however often it runs, and
// the config's load, whose validation resolves each root where
// upscale.variantsDir or upscale.tempDir is set, is bounded beside it. No
// filesystem a test can make stops answering, so the root is stood in for
// where each step touches it (rootStall): the probe's stat and listing, fields
// of the RootProbes the run uses (doctor.NewRootProbesForTest), and
// config.Load's resolution of the roots (config.SetRootContainmentForTest).

// doctorRootsBound is how long the tests' runners wait for a work on a root.
const doctorRootsBound = 300 * time.Millisecond

// doctorRootsDeadline is how long a test waits for a run a root holds before
// it fails by its own deadline, never by hanging the suite: the run waited on
// the root with no end until B650.
const doctorRootsDeadline = 90 * time.Second

// doctorRunMargin is what a whole doctor run may take past the bound for its
// other checks: the host's service manager, ports and the like.
const doctorRunMargin = 20 * time.Second

// rootStall stands in for every touch of the roots it holds: the probe's stat
// and listing, and config.Load's resolution of the roots, none of which
// answers until letGo. It counts the probes begun of each root (its stats)
// and the loads that met it before it was let go.
type rootStall struct {
	held    map[string]bool
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	stats   map[string]int
	loads   map[string]int
}

func newRootStall(held ...string) *rootStall {
	s := &rootStall{held: map[string]bool{}, release: make(chan struct{}), stats: map[string]int{}, loads: map[string]int{}}
	for _, r := range held {
		s.held[r] = true
	}
	return s
}

func (s *rootStall) stat(p string) (fs.FileInfo, error) {
	s.mu.Lock()
	s.stats[p]++
	s.mu.Unlock()
	if s.held[p] {
		<-s.release
	}
	return os.Stat(p)
}

func (s *rootStall) readDir(p string) ([]fs.DirEntry, error) {
	if s.held[p] {
		<-s.release
	}
	return os.ReadDir(p)
}

// contain is Validate's containment check as a held root makes it: it waits,
// in the load that asks, until the root is let go.
func (s *rootStall) contain(candidate string, roots []string) string {
	for _, r := range roots {
		if !s.held[r] {
			continue
		}
		select {
		case <-s.release:
		default:
			s.mu.Lock()
			s.loads[r]++
			s.mu.Unlock()
			<-s.release
		}
		break
	}
	return fsutil.IsUnderAny(candidate, roots)
}

func (s *rootStall) letGo() { s.once.Do(func() { close(s.release) }) }

func (s *rootStall) statsOf(root string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats[root]
}

func (s *rootStall) loadsOf(root string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads[root]
}

// stallProbes is a runner's RootProbes over s, waiting bound, with Load's
// containment stood in for by s's for the rest of the test. Its cleanups let
// s's roots answer, wait for every work to end, and only then put the
// containment back, which a load still out reads.
func stallProbes(t *testing.T, s *rootStall, bound time.Duration) *doctor.RootProbes {
	t.Helper()
	p := doctor.NewRootProbesForTest(bound, s.stat, s.readDir)
	t.Cleanup(config.SetRootContainmentForTest(s.contain))
	t.Cleanup(func() {
		s.letGo()
		if !p.DrainForTest(30 * time.Second) {
			t.Error("a work on a root was still out 30s after its roots answered")
		}
	})
	return p
}

// standInCLIRoots is stallProbes made the CLI's works on the roots
// (cliRootProbes) for the rest of the test, put back once every work has
// ended. A test that calls it does not call t.Parallel.
func standInCLIRoots(t *testing.T, s *rootStall) *doctor.RootProbes {
	t.Helper()
	prev := cliRootProbes
	t.Cleanup(func() { cliRootProbes = prev })
	p := stallProbes(t, s, doctorRootsBound)
	cliRootProbes = p
	return p
}

// rootsInstall writes base/bridge.yaml over two roots, healthy and dead, each
// a directory holding one file, with MPD off and, where sidecars,
// upscale.variantsDir and upscale.tempDir set outside both: the settings whose
// validation resolves every root. It returns the config's path and the roots.
func rootsInstall(t *testing.T, sidecars bool) (path, healthy, dead string) {
	t.Helper()
	base := t.TempDir()
	healthy, dead = filepath.Join(base, "Music"), filepath.Join(base, "NAS")
	for _, r := range []string{healthy, dead} {
		if err := os.MkdirAll(r, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r, "01.flac"), []byte{0}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	body := fmt.Sprintf("libraryRoots:\n  - %s\n  - %s\nlistenAddress: \"127.0.0.1:0\"\nadminAddress: \"127.0.0.1:0\"\ndataDir: %s\noutput:\n  mpd: off\n",
		healthy, dead, filepath.Join(base, "data"))
	if sidecars {
		body += fmt.Sprintf("upscale:\n  variantsDir: %s\n  tempDir: %s\n", filepath.Join(base, "variants"), filepath.Join(base, "scratch"))
	}
	path = filepath.Join(base, "bridge.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, healthy, dead
}

// doctorWithin runs fn on a goroutine of its own and waits
// doctorRootsDeadline for it, answering what it returned and how long it
// took: a test of a root that does not answer fails by its own deadline.
func doctorWithin[T any](t *testing.T, what string, fn func() T) (T, time.Duration) {
	t.Helper()
	done := make(chan T, 1)
	start := time.Now()
	go func() { done <- fn() }()
	select {
	case v := <-done:
		return v, time.Since(start)
	case <-time.After(doctorRootsDeadline):
		t.Fatalf("%s did not return within %s: a root that does not answer held it", what, doctorRootsDeadline)
		panic("unreachable")
	}
}

// doctorPrinted is what bridge doctor printed and its exit code.
type doctorPrinted struct {
	code int
	out  string
}

// bridgeDoctorOver runs bridge doctor over the config at path, bounded by the
// test's deadline.
func bridgeDoctorOver(t *testing.T, path string) (doctorPrinted, time.Duration) {
	t.Helper()
	return doctorWithin(t, "bridge doctor", func() doctorPrinted {
		var out, errOut strings.Builder
		code := doctorCmd([]string{"--config", path}, &out, &errOut)
		return doctorPrinted{code, stripANSI(out.String() + errOut.String())}
	})
}

// reportHint is the hint printed under check's line in a doctor report, or ""
// when there is none.
func reportHint(report, check string) string {
	lines := strings.Split(report, "\n")
	for i, l := range lines {
		if strings.Contains(l, " "+check+" ") && i+1 < len(lines) {
			if h := strings.TrimSpace(lines[i+1]); strings.HasPrefix(h, "↳ ") {
				return strings.TrimPrefix(h, "↳ ")
			}
			return ""
		}
	}
	return ""
}

// printedLine is a check's line as printReport prints it without colour,
// trimmed as reportLine trims it.
func printedLine(status doctor.Status, name, summary string) string {
	return strings.TrimSpace(stripANSI(fmt.Sprintf("%s %-18s  %s", badgeForStatus(status), name, summary)))
}

// reportNames is the name of every line of a printed doctor report, in order.
func reportNames(report string) []string {
	var names []string
	for _, l := range strings.Split(report, "\n") {
		if f := strings.Fields(l); len(f) >= 2 && strings.HasPrefix(f[0], "[") && strings.HasSuffix(f[0], "]") {
			names = append(names, f[1])
		}
	}
	return names
}

// consoleRun runs the console's doctor runner on ctx, bounded by the test's
// deadline, and returns its lines by name.
func consoleRun(t *testing.T, ctx context.Context, runner func(context.Context) *admin.DoctorReport) (map[string]admin.DoctorCheck, time.Duration) {
	t.Helper()
	rep, took := doctorWithin(t, "the console's doctor", func() *admin.DoctorReport { return runner(ctx) })
	lines := map[string]admin.DoctorCheck{}
	for _, c := range rep.Checks {
		lines[c.Name] = c
	}
	return lines, took
}

// TestBridgeDoctorNamesARootThatDoesNotAnswerAndGoesOn is B650's first case
// through the CLI's entry point: bridge doctor over a config whose second
// root's mount does not answer returns within the bound and a margin, fails
// library-roots naming that root as configured and not the other, exits 1,
// and prints every line it prints once the root answers. Red on main, whose
// library-roots stat'ed the root on the doctor's goroutine: bridge doctor
// printed nothing and never returned.
func TestBridgeDoctorNamesARootThatDoesNotAnswerAndGoesOn(t *testing.T) {
	path, healthy, dead := rootsInstall(t, false)
	s := newRootStall(dead)
	p := standInCLIRoots(t, s)
	got, took := bridgeDoctorOver(t, path)
	if limit := doctorRootsBound + doctorRunMargin; took > limit {
		t.Errorf("bridge doctor took %s, past the bound and a margin for its other checks (%s)", took, limit)
	}
	if got.code != 1 {
		t.Errorf("bridge doctor exited %d, want 1 for the fail", got.code)
	}
	want := fmt.Sprintf("library root %s did not answer within %s: its mount may be stalled", dead, doctorRootsBound)
	if line, hint := reportLine(got.out, "library-roots"), reportHint(got.out, "library-roots"); !strings.HasPrefix(line, "[FAIL]") || !strings.Contains(hint, want) {
		t.Errorf("library-roots = %q, %q; want a fail saying %q\n%s", line, hint, want, got.out)
	}
	if strings.Contains(got.out, healthy) {
		t.Errorf("the report names the root that answers:\n%s", got.out)
	}
	s.letGo()
	if !p.DrainForTest(30 * time.Second) {
		t.Fatal("the probe was still out once its root answered")
	}
	again, _ := bridgeDoctorOver(t, path)
	if names, healthyNames := reportNames(got.out), reportNames(again.out); strings.Join(names, " ") != strings.Join(healthyNames, " ") {
		t.Errorf("the report over a root that does not answer has the lines %v, and once it answers %v; want the same", names, healthyNames)
	}
	if line := reportLine(again.out, "library-roots"); !strings.HasPrefix(line, "[ok]") {
		t.Errorf("once the root answers, library-roots = %q, want ok", line)
	}
}

// TestBridgeDoctorBoundsTheLoadOfAConfigThatSetsItsSidecarDirectories is
// Codex's first note on the brief: with upscale.variantsDir and
// upscale.tempDir set, config.Load's validation resolves every root, so a dead
// mount held bridge doctor before any check ran. The load is bounded too, the
// roots probed beside it: config-file fails in the words of a load that did
// not finish, library-roots names the root, the lines that grade what the
// config sets say they were not checked, and the run takes the one bound. A
// config that sets neither loads without asking the root.
func TestBridgeDoctorBoundsTheLoadOfAConfigThatSetsItsSidecarDirectories(t *testing.T) {
	path, _, dead := rootsInstall(t, true)
	s := newRootStall(dead)
	standInCLIRoots(t, s)
	got, took := bridgeDoctorOver(t, path)
	if limit := doctorRootsBound + doctorRunMargin; took > limit {
		t.Errorf("bridge doctor took %s, past the bound and a margin (%s)", took, limit)
	}
	if got.code != 1 {
		t.Errorf("bridge doctor exited %d, want 1", got.code)
	}
	if line, want := reportLine(got.out, "config-file"), printedLine(doctor.Fail, "config-file", fmt.Sprintf("%s: its load did not finish within %s", path, doctorRootsBound)); line != want {
		t.Errorf("config-file = %q, want %q", line, want)
	}
	if hint := reportHint(got.out, "config-file"); !strings.Contains(hint, "upscale.variantsDir and upscale.tempDir") {
		t.Errorf("config-file's hint does not say what holds the load: %q", hint)
	}
	if hint := reportHint(got.out, "library-roots"); !strings.Contains(hint, fmt.Sprintf("library root %s did not answer within %s", dead, doctorRootsBound)) {
		t.Errorf("library-roots' hint = %q, want the root named\n%s", hint, got.out)
	}
	if line := reportLine(got.out, "port-api"); !strings.Contains(line, "not checked: the config that sets this port did not finish loading") {
		t.Errorf("port-api = %q", line)
	}
	if n := s.loadsOf(dead); n != 1 {
		t.Errorf("%d loads met the root, want one", n)
	}
}

// TestTheDoctorProbesTheRootsBesideItsLoadOfTheConfig: the roots a config
// names, read without touching them, are probed from the moment its load
// begins (loadDoctorConfig), so a load a root holds and the root's own probe
// cost a run one bound between them, and a load that does not finish hands
// the run those roots to name.
func TestTheDoctorProbesTheRootsBesideItsLoadOfTheConfig(t *testing.T) {
	path, healthy, dead := rootsInstall(t, true)
	s := newRootStall(dead)
	run := stallProbes(t, s, doctorRootsBound).BeginRun()
	type loaded struct {
		roots []string
		err   error
	}
	got, _ := doctorWithin(t, "the doctor's load of the config", func() loaded {
		_, roots, err := loadDoctorConfig(context.Background(), run, path)
		return loaded{roots, err}
	})
	var unfinished *doctor.UnfinishedLoadError
	if !errors.As(got.err, &unfinished) || strings.Join(got.roots, " ") != healthy+" "+dead {
		t.Fatalf("the load = %v with roots %q; want it unfinished, with the roots %q and %q", got.err, got.roots, healthy, dead)
	}
	for deadline := time.Now().Add(10 * time.Second); s.statsOf(healthy) != 1 || s.statsOf(dead) != 1; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("after the load, the roots were probed %d and %d times; want each once, beside the load",
				s.statsOf(healthy), s.statsOf(dead))
		}
	}
}

// TestBridgeDoctorLoadsAConfigThatSetsNeitherDirectoryWithoutTheRoots: the
// other side of the bound on the load, its control: a config that sets
// neither upscale.variantsDir nor upscale.tempDir loads without touching a
// root, so a root that does not answer leaves every line that grades the
// config graded, and library-roots names it.
func TestBridgeDoctorLoadsAConfigThatSetsNeitherDirectoryWithoutTheRoots(t *testing.T) {
	path, _, dead := rootsInstall(t, false)
	s := newRootStall(dead)
	standInCLIRoots(t, s)
	got, _ := bridgeDoctorOver(t, path)
	if line, want := reportLine(got.out, "config-file"), printedLine(doctor.OK, "config-file", path); line != want {
		t.Errorf("config-file = %q, want %q", line, want)
	}
	if n := s.loadsOf(dead); n != 0 {
		t.Errorf("the load of a config that sets neither directory met the root %d times, want none", n)
	}
	if hint := reportHint(got.out, "library-roots"); !strings.Contains(hint, "library root "+dead+" did not answer") {
		t.Errorf("library-roots' hint = %q, want the root named", hint)
	}
}

// TestTheConsolesDoctorAsksARootThatDoesNotAnswerOnce is B650's third case
// through the console's runner (adminDoctorRunnerWith), whose Deps every run
// rebuilds: a second run while the first one's probe and load are still out
// says the root and the load still have not answered, and begins neither
// again. Red on main, whose first run never returned.
func TestTheConsolesDoctorAsksARootThatDoesNotAnswerOnce(t *testing.T) {
	path, _, dead := rootsInstall(t, true)
	s := newRootStall(dead)
	runner := adminDoctorRunnerWith(path, nil, nil, servingDACHost(hostTrees, nil), stallProbes(t, s, doctorRootsBound))
	first, _ := consoleRun(t, context.Background(), runner)
	if c := first["config-file"]; c.Status != "fail" || c.Summary != fmt.Sprintf("%s: its load did not finish within %s", path, doctorRootsBound) {
		t.Errorf("the first run's config-file = %+v", c)
	}
	if c := first["library-roots"]; c.Status != "fail" || !strings.Contains(c.Hint, fmt.Sprintf("library root %s did not answer within %s", dead, doctorRootsBound)) {
		t.Errorf("the first run's library-roots = %+v", c)
	}
	second, _ := consoleRun(t, context.Background(), runner)
	if c := second["config-file"]; c.Status != "fail" || !strings.Contains(c.Summary, "its load still has not finished") {
		t.Errorf("the second run's config-file = %+v, want the load still out", c)
	}
	if c := second["library-roots"]; c.Status != "fail" || !strings.Contains(c.Hint, fmt.Sprintf("library root %s still has not answered", dead)) {
		t.Errorf("the second run's library-roots = %+v, want the root still not answering", c)
	}
	if n := s.statsOf(dead); n != 1 {
		t.Errorf("two runs began %d probes of the root, want one", n)
	}
	if n := s.loadsOf(dead); n != 1 {
		t.Errorf("two runs began %d loads that met the root, want one", n)
	}
}

// TestOverlappingConsoleDoctorRunsShareOneProbeAndOneLoad: /api/doctor is not
// serialised, so the console's runs overlap; while a root does not answer
// they share its probe and the config's load, and each names the root.
func TestOverlappingConsoleDoctorRunsShareOneProbeAndOneLoad(t *testing.T) {
	path, _, dead := rootsInstall(t, true)
	s := newRootStall(dead)
	runner := adminDoctorRunnerWith(path, nil, nil, servingDACHost(hostTrees, nil), stallProbes(t, s, doctorRootsBound))
	const runs = 6
	reports := make(chan *admin.DoctorReport, runs)
	var wg sync.WaitGroup
	for range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reports <- runner(context.Background())
		}()
	}
	doctorWithin(t, "the overlapping runs", func() struct{} { wg.Wait(); return struct{}{} })
	close(reports)
	for rep := range reports {
		for _, c := range rep.Checks {
			if c.Name == "library-roots" && (c.Status != "fail" || !strings.Contains(c.Hint, "library root "+dead)) {
				t.Errorf("an overlapping run's library-roots = %+v", c)
			}
		}
	}
	if n, l := s.statsOf(dead), s.loadsOf(dead); n != 1 || l != 1 {
		t.Errorf("%d overlapping runs began %d probes and %d loads that met the root, want one of each", runs, n, l)
	}
}

// TestACancelledConsoleDoctorRunSaysItWasStopped: a run whose request goes
// away before a root's bound says the root was not checked because the run
// was stopped, never that it did not answer, and comes back at once.
func TestACancelledConsoleDoctorRunSaysItWasStopped(t *testing.T) {
	const bound = 2 * time.Second
	path, _, dead := rootsInstall(t, false)
	s := newRootStall(dead)
	runner := adminDoctorRunnerWith(path, nil, nil, servingDACHost(hostTrees, nil), stallProbes(t, s, bound))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	lines, took := consoleRun(t, ctx, runner)
	if took >= bound {
		t.Errorf("the stopped run took %s, want it back before the bound (%s)", took, bound)
	}
	want := fmt.Sprintf("library root %s was not checked: the doctor's run was stopped before it answered", dead)
	if c := lines["library-roots"]; c.Status != "warn" || c.Summary != "1 root(s) not checked" || c.Hint != want {
		t.Errorf("the stopped run's library-roots = %+v, want a warn saying %q", c, want)
	}
	for _, c := range lines {
		if strings.Contains(c.Summary+c.Hint, "did not answer") || strings.Contains(c.Summary+c.Hint, "still has not") {
			t.Errorf("the stopped run's %s says the root is stuck: %+v", c.Name, c)
		}
	}
}

// TestTheConsolesDoctorReadsARecoveredRootAsHealthy: once a root that did not
// answer answers, its probe ends with nothing waiting for it, and the next run
// probes it afresh and reads it healthy, and loads the config afresh.
func TestTheConsolesDoctorReadsARecoveredRootAsHealthy(t *testing.T) {
	path, _, dead := rootsInstall(t, true)
	s := newRootStall(dead)
	p := stallProbes(t, s, doctorRootsBound)
	runner := adminDoctorRunnerWith(path, nil, nil, servingDACHost(hostTrees, nil), p)
	if lines, _ := consoleRun(t, context.Background(), runner); lines["library-roots"].Status != "fail" {
		t.Fatalf("library-roots = %+v, want the root not answering", lines["library-roots"])
	}
	s.letGo()
	if !p.DrainForTest(30 * time.Second) {
		t.Fatal("the probe and the load were still out once the root answered")
	}
	lines, _ := consoleRun(t, context.Background(), runner)
	if c := lines["library-roots"]; c.Status != "ok" || c.Summary != "2 root(s) reachable" {
		t.Errorf("once the root answers, library-roots = %+v, want both reachable", c)
	}
	if c := lines["config-file"]; c.Status != "ok" || c.Summary != path {
		t.Errorf("once the root answers, config-file = %+v, want it loaded", c)
	}
	if n := s.statsOf(dead); n != 2 {
		t.Errorf("the root was probed %d times, want twice", n)
	}
}

// TestTheConsolesDoctorHoldsItsProbesToALimitAcrossConfigChurn is Codex's
// fourth note on the brief through the console's runner, which loads the
// config afresh at every run: a dead root replaced by another, sixteen times,
// leaves sixteen probes out, and the next run begins none for the next root,
// which it says it did not check and why, while the config's load, which
// touches no root here, is never held back; a root removed and added again
// finds its probe still out.
func TestTheConsolesDoctorHoldsItsProbesToALimitAcrossConfigChurn(t *testing.T) {
	const bound = 100 * time.Millisecond
	const limit = 16
	base := t.TempDir()
	roots := make([]string, limit+1)
	for i := range roots {
		roots[i] = filepath.Join(base, fmt.Sprintf("NAS-%02d", i))
		if err := os.MkdirAll(roots[i], 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(base, "bridge.yaml")
	setRoot := func(root string) {
		body := fmt.Sprintf("libraryRoots:\n  - %s\nlistenAddress: \"127.0.0.1:0\"\nadminAddress: \"127.0.0.1:0\"\ndataDir: %s\noutput:\n  mpd: off\n",
			root, filepath.Join(base, "data"))
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := newRootStall(roots...)
	runner := adminDoctorRunnerWith(path, nil, nil, servingDACHost(hostTrees, nil), stallProbes(t, s, bound))
	for _, r := range roots[:limit] {
		setRoot(r)
		lines, _ := consoleRun(t, context.Background(), runner)
		if c := lines["library-roots"]; c.Status != "fail" || !strings.Contains(c.Hint, "library root "+r+" did not answer within") {
			t.Fatalf("library-roots over %s = %+v, want the root not answering", r, c)
		}
	}
	last := roots[limit]
	setRoot(last)
	lines, _ := consoleRun(t, context.Background(), runner)
	want := fmt.Sprintf("library root %s was not checked: %d earlier probes of the library roots have not answered yet, and the doctor begins no more until one does", last, limit)
	if c := lines["library-roots"]; c.Status != "warn" || c.Hint != want {
		t.Errorf("past the limit, library-roots = %+v, want a warn saying %q", c, want)
	}
	if c := lines["config-file"]; c.Status != "ok" {
		t.Errorf("past the probes' limit, config-file = %+v, want the load, which touches no root, made", c)
	}
	if n := s.statsOf(last); n != 0 {
		t.Errorf("past the limit, %d probes of a new root began, want none", n)
	}
	setRoot(roots[0])
	lines, _ = consoleRun(t, context.Background(), runner)
	if c := lines["library-roots"]; c.Status != "fail" || !strings.Contains(c.Hint, "still has not answered") {
		t.Errorf("a root added again: library-roots = %+v, want its probe still out", c)
	}
	if n := s.statsOf(roots[0]); n != 1 {
		t.Errorf("a root removed and added again was probed %d times, want once", n)
	}
}

// TestTheChecklistSaysABoxThatDoesNotAnswerWasNotRead: on a player box the
// checklist's outputs body proves each root's path rule, whose stat of a root
// that does not answer held bridge doctor and the console's doctor with no
// end. Its read is bounded (outputsReadBound), and a body not read in time is
// a box that could not be read, the Player line saying so.
func TestTheChecklistSaysABoxThatDoesNotAnswerWasNotRead(t *testing.T) {
	prev := outputsReadBound
	outputsReadBound = doctorRootsBound
	t.Cleanup(func() { outputsReadBound = prev })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	src := outputChecklistSource{
		Active: func(context.Context) bool { return true },
		Outputs: func(context.Context) (api.OutputsResponse, error) {
			<-release
			return api.OutputsResponse{}, nil
		},
		Roots: func() []string { return []string{"/srv/music"} },
	}
	checks, took := doctorWithin(t, "the checklist", func() []doctor.Check { return checklistDoctorChecks(src)(context.Background()) })
	if took > doctorRootsBound+doctorRunMargin {
		t.Errorf("the checklist took %s", took)
	}
	if len(checks) != 4 || checks[0].Name != "player" || checks[0].Status != doctor.Fail ||
		!strings.Contains(checks[0].Summary, fmt.Sprintf("it did not answer within %s", doctorRootsBound)) {
		t.Errorf("the checklist = %+v, want the Player line saying the box did not answer", checks)
	}
}

// TestBridgeDoctorPrintsHealthyRootsAsBefore is B650's guard through the CLI:
// over roots that all answer, with the library watcher on, the lines that
// touch them print what main printed for this fixture, word for word (the
// same test is green on main's tree).
func TestBridgeDoctorPrintsHealthyRootsAsBefore(t *testing.T) {
	path, _, _ := rootsInstall(t, true)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, "libraryWatch:\n  enabled: true\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runDoctor(t, "--config", path)
	for _, want := range []string{
		printedLine(doctor.OK, "config-file", path),
		printedLine(doctor.OK, "library-roots", "2 root(s) reachable"),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report has no line %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "not checked: the folders") || strings.Contains(out, "did not answer") {
		t.Errorf("a healthy library reads as one that did not answer:\n%s", out)
	}
}
