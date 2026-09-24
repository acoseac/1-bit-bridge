package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// nonTestGoFilesInPackage lists the package's production .go files by base
// name. A directory read rather than a hardcoded list: a new file that
// reintroduces the bug should be caught without anyone remembering to add it.
func nonTestGoFilesInPackage(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || goToolIgnores(n) {
			continue
		}
		out = append(out, n)
	}
	if len(out) < 20 {
		t.Fatalf("only %d production files found — the scan is not seeing the package", len(out))
	}
	return out
}

// readPackageFile returns one production file's source with // and /* */
// comments stripped, so the guard cannot fire on prose. This package's
// commentary names the very symbols it discusses — including, now, this one.
func readPackageFile(t *testing.T, base string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(base))
	if err != nil {
		t.Fatalf("read %s: %v", base, err)
	}
	return stripGoComments(string(b))
}

// stripGoComments removes block and line comments while respecting string and
// rune literals — Go raw strings contain both `//` and `/*` freely, and a
// naive strip would corrupt the code around them.
func stripGoComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	const (
		code = iota
		lineComment
		blockComment
		str
		rawStr
		runeLit
	)
	state := code
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case code:
			switch {
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				state = lineComment
				i++
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				state = blockComment
				i++
			case c == '"':
				state = str
				b.WriteByte(c)
			case c == '`':
				state = rawStr
				b.WriteByte(c)
			case c == '\'':
				state = runeLit
				b.WriteByte(c)
			default:
				b.WriteByte(c)
			}
		case lineComment:
			if c == '\n' {
				state = code
				b.WriteByte(c)
			}
		case blockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				state = code
				i++
			}
		case str, runeLit:
			// Contents dropped, delimiters kept. A production file whose
			// error text or log message happens to contain "config.Load("
			// is not a caller, and a guard that cries wolf gets deleted —
			// which is the outcome this exists to prevent.
			q := byte('"')
			if state == runeLit {
				q = '\''
			}
			if c == '\\' && i+1 < len(src) {
				i++
			} else if c == q {
				state = code
				b.WriteByte(c)
			}
		case rawStr:
			if c == '`' {
				state = code
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}

// The `--config` flag defaults to the EMPTY string on every subcommand.
// Sixteen files in this package therefore route through loadCLIConfig, which
// resolves that empty default per resolveConfigPath (explicit → ./bridge.yaml
// → the platform config dir). Two shared tails did not, and handed "" straight
// to config.Load — so `bridge token {list,rotate,expire,revoke}`,
// `bridge upscale` and `bridge optimize` all died with
//
//	config load failed: read config "": open : no such file or directory
//
// on a host with a perfectly good platform install, while their own flag help
// promised exactly the fallback they were not doing. `bridge token revoke` is
// the documented recovery path for an orphaned token, which makes it the one
// command an operator reaches for when something is already wrong.
//
// These tests assert on the CONFIG-RESOLUTION step reaching the platform
// install, not on each command succeeding outright: `bridge optimize` still
// legitimately refuses when `upscale.enabled` is false, and conflating the two
// refusals is how the original defect stayed readable as "expected".

// TestStripGoCommentsDropsProseAndLiterals pins the two things the guard's
// scanner has to get right, because both were review findings rather than
// hypotheticals.
//
// Comments: this package's commentary names the symbols it discusses — the very
// fix that prompted the guard is explained in a comment mentioning
// config.Load — so a raw text scan reports its own documentation as a caller.
//
// String contents: a production file whose error text or log message happens to
// contain the call spelling is not a caller either. A guard that cries wolf gets
// deleted, which is the outcome it exists to prevent. (Gemini, PR #853.)
func TestStripGoCommentsDropsProseAndLiterals(t *testing.T) {
	src := "package p\n" +
		"// we deliberately avoid config.Load( here\n" +
		"/* and config.Load( in a block comment */\n" +
		"var msg = \"use config.Load( carefully\"\n" +
		"var raw = `also config.Load( in a raw string`\n" +
		"func f() { realCall() }\n"
	got := stripGoComments(src)
	if strings.Contains(got, "config.Load(") {
		t.Errorf("stripGoComments left a config.Load( mention that is prose or literal text:\n%s", got)
	}
	if !strings.Contains(got, "realCall()") {
		t.Errorf("stripGoComments dropped actual code:\n%s", got)
	}

	// And it must NOT hide a real call — the guard is worthless if the
	// stripper eats the thing it is looking for.
	if !strings.Contains(stripGoComments("package p\nfunc f() { config.Load(x) }\n"), "config.Load(") {
		t.Error("stripGoComments removed a genuine call")
	}
}

// TestTokenCmdResolvesPlatformConfigWithoutExplicitFlag pins the token tail.
func TestTokenCmdResolvesPlatformConfigWithoutExplicitFlag(t *testing.T) {
	_, platform := isolateConfigEnv(t)
	writeInstallAt(t, platform, "some-track.flac")

	var so, se bytes.Buffer
	code := tokenListCmd(nil, &so, &se)
	if code != 0 {
		t.Fatalf("token list exit %d with a platform install present and no --config; stderr:\n%s",
			code, se.String())
	}
	if strings.Contains(se.String(), `read config ""`) {
		t.Errorf("token list passed the flag's empty default straight to config.Load:\n%s", se.String())
	}
}

// TestTranscodeCmdResolvesPlatformConfigWithoutExplicitFlag pins the tail
// shared by `bridge upscale` and `bridge optimize`.
//
// The install written here leaves `upscale.enabled` false, so the command is
// expected to refuse — the assertion is that it refuses for the RIGHT reason,
// having found and read the platform config first.
func TestTranscodeCmdResolvesPlatformConfigWithoutExplicitFlag(t *testing.T) {
	_, platform := isolateConfigEnv(t)
	writeInstallAt(t, platform, "some-track.flac")

	var se bytes.Buffer
	_, code := bootstrapTranscodeCmd(context.Background(), &se, "", "", false)
	if code == 0 {
		t.Fatalf("expected a refusal (the fixture leaves upscale.enabled false), got 0")
	}
	if strings.Contains(se.String(), `read config ""`) {
		t.Errorf("the transcode tail passed the flag's empty default straight to config.Load:\n%s", se.String())
	}
	if !strings.Contains(se.String(), "disabled in bridge.yaml") {
		t.Errorf("expected the feature-gate refusal, meaning the platform config was found and read; got:\n%s",
			se.String())
	}
}

// TestDoctorResolvesTheWorkingDirectoryConfig pins `bridge doctor` to the
// lookup every other subcommand uses. It tried an explicit --config or the
// platform path and nothing between, so inside the Docker image (WORKDIR
// /data, config /data/bridge.yaml, no platform config) it graded an install
// with no config at all. With no data dir there was no server.pid, and both
// port checks FAILed against the bridge's own listeners. With no env
// overrides, audio-toolchain read "not enabled" beside
// BRIDGE_UPSCALE_ENABLED=true (#984).
//
// An install sits at BOTH locations here and the local one must win: that is
// resolveConfigPath's precedence, and what `bridge status` run from the same
// directory reads.
func TestDoctorResolvesTheWorkingDirectoryConfig(t *testing.T) {
	cwd, platform := isolateConfigEnv(t)
	writeInstallAt(t, platform, "platform-track.flac")
	writeInstallAt(t, cwd, "local-track.flac")
	// config.Load applies the env overrides, so a doctor that never loads the
	// file never sees them either.
	t.Setenv("BRIDGE_UPSCALE_ENABLED", "true")

	d := buildDoctorDeps("")

	wantData := filepath.Join(cwd, "data")
	if d.DataDir != wantData {
		t.Fatalf("DataDir = %q, want %q: doctor did not grade ./bridge.yaml", d.DataDir, wantData)
	}
	if want := filepath.Join(wantData, "server.pid"); d.OwnPIDFile != want {
		t.Errorf("OwnPIDFile = %q, want %q; without it the port checks cannot "+
			"recognise the bridge's own listeners", d.OwnPIDFile, want)
	}
	if !d.UpscaleEnabled {
		t.Error("UpscaleEnabled is false with BRIDGE_UPSCALE_ENABLED=true: the env " +
			"overrides config.Load applies never reached doctor")
	}
	// The config-dir check grades the directory the config came from, not the
	// platform dir it did not use. The report prints it, so it must be
	// absolute, where resolveConfigPath hands back the bare "bridge.yaml".
	if !filepath.IsAbs(d.ConfigDir) || !sameFile(t, d.ConfigDir, cwd) {
		t.Errorf("ConfigDir = %q, want the working directory %q as an absolute path", d.ConfigDir, cwd)
	}
	// And the report names the file it graded.
	if d.ConfigFile == nil || d.ConfigFile.LoadErr != nil ||
		!sameFile(t, d.ConfigFile.Path, filepath.Join(cwd, defaultConfigPath)) {
		t.Errorf("ConfigFile = %+v, want the local bridge.yaml, loaded", d.ConfigFile)
	}
}

// TestDoctorReportsAWorkingDirectoryConfigThatDoesNotLoad is the case the
// lookup change made reachable (CodeRabbit on #985). A ./bridge.yaml that
// does not load now shadows a platform config that does, because it is the
// file `bridge status` and `bridge serve` run from the same directory read,
// and refuse. Dropped silently, as every unloadable config used to be, it
// left doctor grading defaults: "all clear" about ports the file does not
// name, while every other command exits 2. It must FAIL, which is also what
// makes `bridge doctor --config <path>` a check on a config edit before a
// restart.
func TestDoctorReportsAWorkingDirectoryConfigThatDoesNotLoad(t *testing.T) {
	cwd, platform := isolateConfigEnv(t)
	writeInstallAt(t, platform, "platform-track.flac")
	// config.Load decodes with KnownFields, so one misspelt key is a load
	// failure: the typo a hand edit makes.
	if err := os.WriteFile(filepath.Join(cwd, defaultConfigPath), []byte("libraryNmae: typo\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if d := buildDoctorDeps(""); d.DataDir != "" {
		t.Errorf("DataDir = %q: doctor graded a config other than the local one "+
			"every command run from here reads", d.DataDir)
	}

	var so, se bytes.Buffer
	if code := doctorCmd([]string{"--json"}, &so, &se); code != 1 {
		t.Errorf("doctor exited %d on a config that does not load, want 1; stderr:\n%s", code, se.String())
	}
	var rep jsonDoctorReport
	if err := json.Unmarshal(so.Bytes(), &rep); err != nil {
		t.Fatalf("decode the JSON report: %v\n%s", err, so.String())
	}
	for _, c := range rep.Checks {
		if c.Name != "config-file" {
			continue
		}
		if c.Status != string(doctor.Fail) || !strings.Contains(c.Summary, "libraryNmae") ||
			!strings.Contains(c.Summary, defaultConfigPath) {
			t.Errorf("config-file = %s %q, want a fail naming the local file and the key "+
				"that does not load", c.Status, c.Summary)
		}
		return
	}
	t.Fatalf("no config-file check in the report:\n%s", so.String())
}

// TestDoctorOnlyWarnsAboutAConfigItCannotRead: a permission failure is a
// fact about who ran doctor, not about the file (on the public-mode layout
// the operator is not the service user), so it warns where a config that
// does not load fails. It also checks the classification survives
// config.Load's own error wrapping, which a hand-built error cannot.
func TestDoctorOnlyWarnsAboutAConfigItCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not deny a read on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0000 file")
	}
	cwd, _ := isolateConfigEnv(t)
	cfgPath := writeInstallAt(t, cwd, "local-track.flac")
	if err := os.Chmod(cfgPath, 0); err != nil {
		t.Fatal(err)
	}

	c := findCheck(t, doctor.Run(context.Background(), buildDoctorDeps("")), "config-file")
	if c.Status != doctor.Warn || !strings.Contains(c.Summary, defaultConfigPath) {
		t.Errorf("config-file = %s %q on a config this user cannot read, want a warn naming it",
			c.Status, c.Summary)
	}
}

// TestDoctorFailsANamedConfigThatIsNotThere (CodeRabbit on #985, round 2):
// `--config` naming a file that does not exist, the typo'd path of a
// validate-before-restart run, used to read "none found", ok, and grade
// defaults, so doctor could exit 0 having validated nothing. An operator
// who names a file asserts it exists, so it FAILs, as loadCLIConfig refuses
// it for every other subcommand. The no-flag pre-init run stays ok (the
// nothing-found subtest above) and so does the menu's pre-setup row
// (TestMenuDoctorGradesTheMenusOwnConfig).
func TestDoctorFailsANamedConfigThatIsNotThere(t *testing.T) {
	cwd, _ := isolateConfigEnv(t)
	// A good config sits in the working directory, so a fallback to it
	// would pass for the right answer. It must not be graded instead.
	writeInstallAt(t, cwd, "local-track.flac")
	// In a directory that is not there either, the shape of a typo'd path.
	named := filepath.Join(t.TempDir(), "typo", "bridge.yml")

	var so, se bytes.Buffer
	if code := doctorCmd([]string{"--json", "--config", named}, &so, &se); code != 1 {
		t.Errorf("doctor exited %d for a --config that is not there, want 1; stderr:\n%s", code, se.String())
	}
	// config-dir used to MkdirAll the named file's directory, reporting it ok
	// beside "does not exist" (CodeRabbit on #985, round 3). A diagnostic
	// must not leave that behind.
	if _, err := os.Stat(filepath.Dir(named)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("doctor left %s behind for a --config that is not there (stat: %v)", filepath.Dir(named), err)
	}
	var rep jsonDoctorReport
	if err := json.Unmarshal(so.Bytes(), &rep); err != nil {
		t.Fatalf("decode the JSON report: %v\n%s", err, so.String())
	}
	for _, c := range rep.Checks {
		if c.Name != "config-file" {
			continue
		}
		if c.Status != string(doctor.Fail) || !strings.Contains(c.Summary, named) ||
			!strings.Contains(c.Summary, "does not exist") {
			t.Errorf("config-file = %s %q, want a fail naming %s as not there", c.Status, c.Summary, named)
		}
		return
	}
	t.Fatalf("no config-file check in the report:\n%s", so.String())
}

// TestDoctorWarnsWhenANamedConfigCannotBeReached (CodeRabbit on #985, round
// 2): resolveConfigPath folds EVERY stat error into "not found", so a named
// config under a directory this user cannot traverse (a service-owned 0700
// config dir) read as "none found", ok. That is a permission failure, and
// it gets the permission WARN, like a file this user cannot read.
func TestDoctorWarnsWhenANamedConfigCannotBeReached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not deny traversal on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root traverses a mode-0000 directory")
	}
	_, _ = isolateConfigEnv(t)
	locked := filepath.Join(t.TempDir(), "locked")
	named := writeInstallAt(t, locked, "local-track.flac")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	// Registered after t.TempDir, so it runs first: RemoveAll cannot
	// descend into a directory with no permissions.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	c := findCheck(t, doctor.Run(context.Background(), buildDoctorDeps(named)), "config-file")
	if c.Status != doctor.Warn || !strings.Contains(c.Summary, named) {
		t.Errorf("config-file = %s %q for a named config this user cannot reach, want a warn naming it",
			c.Status, c.Summary)
	}
}

// TestDoctorConfigDirFollowsTheResolvedConfig pins the config-dir check's
// subject when the working directory holds no config. That check CREATES the
// directory it is handed, and `--fix` chmods it, so an answer that slid to
// the working directory would act on wherever doctor happened to be run from.
func TestDoctorConfigDirFollowsTheResolvedConfig(t *testing.T) {
	t.Run("a platform config grades the platform dir", func(t *testing.T) {
		_, platform := isolateConfigEnv(t)
		writeInstallAt(t, platform, "platform-track.flac")
		d := buildDoctorDeps("")
		if d.ConfigDir != platform {
			t.Errorf("ConfigDir = %q, want %q", d.ConfigDir, platform)
		}
		if want := filepath.Join(platform, "data"); d.DataDir != want {
			t.Errorf("DataDir = %q, want %q: the platform config was not read", d.DataDir, want)
		}
	})
	t.Run("no config anywhere grades the platform dir, where init writes", func(t *testing.T) {
		_, platform := isolateConfigEnv(t)
		d := buildDoctorDeps("")
		if d.ConfigDir != platform {
			t.Errorf("ConfigDir = %q, want the platform dir %q", d.ConfigDir, platform)
		}
		// And the report says where it looked, as absolute paths.
		if d.ConfigFile == nil {
			t.Fatal("ConfigFile is nil: the report cannot say where it looked")
		}
		tried := d.ConfigFile.Tried
		if len(tried) != 2 || !filepath.IsAbs(tried[0]) || filepath.Base(tried[0]) != defaultConfigPath ||
			tried[1] != filepath.Join(platform, defaultConfigPath) {
			t.Errorf("ConfigFile.Tried = %q, want ./bridge.yaml made absolute, then the platform path", tried)
		}
	})
	t.Run("an explicit relative path grades its directory, made absolute", func(t *testing.T) {
		_, _ = isolateConfigEnv(t)
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		got := buildDoctorDeps(filepath.Join("elsewhere", "bridge.yaml")).ConfigDir
		if want := filepath.Join(wd, "elsewhere"); got != want {
			t.Errorf("ConfigDir = %q, want %q", got, want)
		}
	})
	t.Run("no config and no platform dir grades nothing", func(t *testing.T) {
		_, _ = isolateConfigEnv(t)
		// isolateConfigEnv's cleanup restores the production lookup.
		defaultConfigDirFn = func() (string, error) { return "", errors.New("no home directory") }
		if got := buildDoctorDeps("").ConfigDir; got != "" {
			t.Errorf("ConfigDir = %q; want empty, which the check reports as a warn, "+
				"not the working directory graded on behalf of a config that is not there", got)
		}
	})
}

// TestMenuDoctorGradesTheMenusOwnConfig pins the launcher's doctor row to the
// platform path the rest of the menu acts on. The row is offered only before
// that install exists, beside the Setup wizard that writes it. A bare `bridge
// doctor` tries ./bridge.yaml first, so run from a directory holding its own
// config it would grade that file instead of the directory Setup is about to
// write to.
func TestMenuDoctorGradesTheMenusOwnConfig(t *testing.T) {
	cwd, platform := isolateConfigEnv(t)
	writeInstallAt(t, cwd, "local-track.flac")
	s := menuState{cfgPath: filepath.Join(platform, defaultConfigPath)}

	var so, se bytes.Buffer
	if code := actDoctor(context.Background(), nil, &so, &se, s); code != -1 {
		t.Fatalf("actDoctor returned %d, want -1 (stay in the menu)", code)
	}
	line := func(check string) string {
		for _, l := range strings.Split(so.String(), "\n") {
			if strings.Contains(l, " "+check+" ") {
				return l
			}
		}
		return ""
	}
	if l := line("config-dir"); !strings.Contains(l, platform) {
		t.Errorf("the menu's doctor graded %q, want the platform dir %s that Setup writes to; "+
			"report:\n%s\nstderr:\n%s", l, platform, so.String(), se.String())
	}
	// And the platform config's absence is the pre-setup state this row is
	// for, not the FAIL a --config naming a missing file gets.
	if l := line("config-file"); !strings.Contains(l, "[ok]") || !strings.Contains(l, "none found") {
		t.Errorf("the menu's config-file line is %q, want ok \"none found\"; report:\n%s", l, so.String())
	}
}

// TestNoSubcommandTailBypassesLoadCLIConfig is the class guard. Both defects
// above were one call each, in a shared tail, invisible from the subcommand
// that owned the flag — so a fix that only corrects today's two sites leaves
// the next one to be found in production.
//
// The allowlist is the set of sites that legitimately call config.Load with an
// ALREADY-RESOLVED concrete path (loadCLIConfig's own body, plus the three
// callers that resolve first and pass the result). Anything else handing a
// flag value to config.Load is the bug this file exists for.
func TestNoSubcommandTailBypassesLoadCLIConfig(t *testing.T) {
	allowed := map[string]bool{
		// loadCLIConfig itself — the one place that MAY call config.Load
		// with a path it just resolved.
		"configpath.go": true,
		// These never hand config.Load the flag's empty default: init.go
		// writes the file then reads it back; doctor.go resolves through
		// resolveConfigPath, the same lookup as loadCLIConfig, and loads
		// only a path it found, because it must REPORT a missing config
		// (it runs before `bridge init`) or one that does not load, both
		// of which loadCLIConfig makes an error; menu.go holds
		// packaging.IsInitialized()'s platform path.
		"init.go":   true,
		"doctor.go": true,
		"menu.go":   true,
	}
	for _, f := range nonTestGoFilesInPackage(t) {
		if allowed[f] {
			continue
		}
		body := readPackageFile(t, f)
		if strings.Contains(body, "config.Load(") {
			t.Errorf("%s calls config.Load directly. A subcommand's --config flag defaults to "+
				"the empty string, so config.Load(*configPath) resolves nothing and the command "+
				"fails on any host where the operator did not pass --config explicitly. Use "+
				"loadCLIConfig, which implements the ./bridge.yaml-then-platform fallback the "+
				"flag help already promises. If this site genuinely holds an already-resolved "+
				"concrete path, add it to the allowlist above with the reason.", f)
		}
	}
}
