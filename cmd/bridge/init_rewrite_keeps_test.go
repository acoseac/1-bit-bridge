package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// `bridge init` run again over an install, overwriting its config: with
// --force, as below, or by answering "Overwrite?" with y.
//
// Measured on 2026-09-27 on a Mac with the real binary (main at daf8e6b5):
// the rewrite built bridge.yaml from baseConfig and the run's flags and
// carried nothing over from the config it replaced. A config naming
// tlsCertPath / tlsKeyPath lost both, and init minted a new pair in the data
// dir, so the fingerprint serve presents changed (04:AF:CB:… to 13:BE:B2:…)
// and the box printed the new one as "Stable across restarts". A config
// naming another dataDir was pointed back at <dir>/data, with a new pair
// there and the install's tokens left behind. A config that did not load for
// a misspelt key lost the pair it named the same way. customEndpoints
// vanished.
//
// Each test drives the real initCmd twice over one --dir and reads, after the
// second run, the exit code, the saved config, and the fingerprint `bridge
// serve` would present: the pair at the paths serve resolves from the saved
// config.

// TestInitRewriteKeepsTheTLSPairItsConfigNames is the reported case: an
// install whose config names its pair with tlsCertPath / tlsKeyPath, moved
// out of the data dir with nothing left in it, re-inited with --force.
//
// Run in both postures, the public one with the preflight on, which grades
// the pair the config names. Only the loopback run prints a fingerprint to
// pin, and it must be the one serve presents.
func TestInitRewriteKeepsTheTLSPairItsConfigNames(t *testing.T) {
	for _, posture := range rewritePostures {
		t.Run(posture.name, func(t *testing.T) {
			tmp := t.TempDir()
			cfgDir := filepath.Join(tmp, "cfg")
			rewrite := posture.setUp(t, cfgDir)
			certPath := filepath.Join(tmp, "pki", "bridge.crt")
			keyPath := filepath.Join(tmp, "pki", "bridge.key")
			movePair(t, filepath.Join(cfgDir, "data"), certPath, keyPath)
			appendToConfig(t, cfgDir, "tlsCertPath: "+certPath+"\ntlsKeyPath: "+keyPath+"\n")
			pinned := servedFingerprint(t, cfgDir)
			pair := readPair(t, certPath, keyPath)

			code, out := rewrite()
			defer logRunOnFailure(t, out)

			if code != 0 {
				t.Fatalf("the rewrite exited %d", code)
			}
			saved := loadInstallConfig(t, cfgDir)
			if saved.TLSCertPath != certPath || saved.TLSKeyPath != keyPath {
				t.Errorf("the saved config names %q / %q, want the pair the install serves, %q / %q",
					saved.TLSCertPath, saved.TLSKeyPath, certPath, keyPath)
			}
			if got := servedFingerprint(t, cfgDir); got != pinned {
				t.Errorf("serve would present %s, and every paired device pinned %s", got, pinned)
			}
			if readPair(t, certPath, keyPath) != pair {
				t.Error("the rewrite changed the pair the install serves")
			}
			if posture.printsFingerprint {
				assertPrintedFingerprint(t, out, pinned)
			}
			if _, err := os.Stat(filepath.Join(cfgDir, "data", servertls.CertFileName)); err == nil {
				t.Error("the rewrite minted a pair in the data dir, which nothing serves")
			}
			for _, want := range []string{"tlsCertPath", certPath} {
				if !strings.Contains(out, want) {
					t.Errorf("the rewrite does not say it kept %q", want)
				}
			}
		})
	}
}

// TestInitRewriteRefusesHalfATLSPair: a config naming one half of a pair,
// in the reported layout (the pair moved out of the data dir). It names a
// pair nothing can load, so a rewrite cannot keep it, and dropping the half
// it names mints a new pair in the data dir, which is the reported pin break
// by another route. Refused before anything is written, with the remedy.
func TestInitRewriteRefusesHalfATLSPair(t *testing.T) {
	for _, tc := range []struct{ named, missing string }{
		{"tlsCertPath", "tlsKeyPath"},
		{"tlsKeyPath", "tlsCertPath"},
	} {
		t.Run(tc.named+" alone", func(t *testing.T) {
			tmp := t.TempDir()
			cfgDir := filepath.Join(tmp, "cfg")
			rewrite := rewritePostures[0].setUp(t, cfgDir)
			paths := map[string]string{
				"tlsCertPath": filepath.Join(tmp, "pki", "bridge.crt"),
				"tlsKeyPath":  filepath.Join(tmp, "pki", "bridge.key"),
			}
			movePair(t, filepath.Join(cfgDir, "data"), paths["tlsCertPath"], paths["tlsKeyPath"])
			appendToConfig(t, cfgDir, tc.named+": "+paths[tc.named]+"\n")
			before := readConfigFile(t, cfgDir)
			pair := readPair(t, paths["tlsCertPath"], paths["tlsKeyPath"])

			code, out := rewrite()
			defer logRunOnFailure(t, out)

			if code == 0 {
				t.Error("a rewrite over half a TLS pair exited 0")
			}
			if readConfigFile(t, cfgDir) != before {
				t.Error("the refused rewrite changed the config")
			}
			if _, err := os.Stat(filepath.Join(cfgDir, "data", servertls.CertFileName)); err == nil {
				t.Error("the rewrite minted a pair in the data dir")
			}
			if readPair(t, paths["tlsCertPath"], paths["tlsKeyPath"]) != pair {
				t.Error("the rewrite changed the install's pair")
			}
			for _, want := range []string{
				"names " + tc.named + " without " + tc.missing,
				"add " + tc.missing,
				"the config was NOT changed",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("the refusal does not say %q", want)
				}
			}
		})
	}
}

// TestInitRewriteKeepsTheDataDirItsConfigNames: an install whose config keeps
// its state in another directory, where its tokens, its database, a public
// install's admin credentials and the default TLS pair live.
//
// The rewrite pointed dataDir back at <dir>/data and minted a pair there, so
// every device lost its bearer token and its pin at once. In the public
// posture #1038's "kept" credentials are the test: the store it keeps is the
// one in the data dir the install uses, not a new one beside the config.
func TestInitRewriteKeepsTheDataDirItsConfigNames(t *testing.T) {
	for _, posture := range rewritePostures {
		t.Run(posture.name, func(t *testing.T) {
			tmp := t.TempDir()
			cfgDir := filepath.Join(tmp, "cfg")
			rewrite := posture.setUp(t, cfgDir)
			state := filepath.Join(tmp, "state")
			if err := os.Rename(filepath.Join(cfgDir, "data"), state); err != nil {
				t.Fatal(err)
			}
			setConfigKey(t, cfgDir, "dataDir", "dataDir: "+state)
			tokens := []byte(`{"tokens":[{"id":"t1","hash":"not-a-real-hash"}]}`)
			if err := os.WriteFile(filepath.Join(state, tokensFileName), tokens, 0o600); err != nil {
				t.Fatal(err)
			}
			pinned := servedFingerprint(t, cfgDir)
			credentials, _ := os.ReadFile(filepath.Join(state, "adminauth.json"))

			code, out := rewrite()
			defer logRunOnFailure(t, out)

			if code != 0 {
				t.Fatalf("the rewrite exited %d", code)
			}
			if saved := loadInstallConfig(t, cfgDir); saved.DataDir != state {
				t.Errorf("the saved config keeps its state in %q, want %q", saved.DataDir, state)
			}
			if got := servedFingerprint(t, cfgDir); got != pinned {
				t.Errorf("serve would present %s, and every paired device pinned %s", got, pinned)
			}
			if got, err := os.ReadFile(filepath.Join(state, tokensFileName)); err != nil || string(got) != string(tokens) {
				t.Errorf("the install's tokens changed: %v", err)
			}
			if _, err := os.Stat(filepath.Join(cfgDir, "data")); err == nil {
				t.Error("the rewrite made a data dir beside the config, which the install does not use")
			}
			for _, want := range []string{"dataDir", state} {
				if !strings.Contains(out, want) {
					t.Errorf("the rewrite does not say it kept %q", want)
				}
			}
			if credentials != nil {
				if got, _ := os.ReadFile(filepath.Join(state, "adminauth.json")); string(got) != string(credentials) {
					t.Error("the rewrite changed the install's admin credentials")
				}
				if !strings.Contains(out, "Admin credentials — kept") {
					t.Error("the rewrite did not keep the install's admin account")
				}
			}
		})
	}
}

// TestInitRewriteKeepsThePairOfAConfigThatDoesNotLoad: #1027's row C, a
// config with a misspelt key, which the re-init exists to replace. It does
// not load, so nothing read it, and the pair it names was lost as in the
// reported case. A rewrite reads what it keeps from the file as written,
// without the unknown-key refusal.
func TestInitRewriteKeepsThePairOfAConfigThatDoesNotLoad(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	rewrite := rewritePostures[0].setUp(t, cfgDir)
	certPath := filepath.Join(tmp, "pki", "bridge.crt")
	keyPath := filepath.Join(tmp, "pki", "bridge.key")
	movePair(t, filepath.Join(cfgDir, "data"), certPath, keyPath)
	appendToConfig(t, cfgDir, "tlsCertPath: "+certPath+"\ntlsKeyPath: "+keyPath+"\n")
	pinned := servedFingerprint(t, cfgDir)
	appendToConfig(t, cfgDir, "libraryNmae: typo\n")
	if _, err := config.Load(filepath.Join(cfgDir, "bridge.yaml")); err == nil {
		t.Fatal("premise: the misspelt key did not stop the config loading")
	}

	code, out := rewrite()
	defer logRunOnFailure(t, out)

	if code != 0 {
		t.Fatalf("the rewrite exited %d", code)
	}
	if got := servedFingerprint(t, cfgDir); got != pinned {
		t.Errorf("serve would present %s, and every paired device pinned %s", got, pinned)
	}
	assertPrintedFingerprint(t, out, pinned)
}

// TestInitPreflightGradesThePairABrokenConfigNames: the preflight's view of
// the same config. config.Load cannot read it, and the preflight graded the
// default pair in init's data dir, which it answered ok about, "absent
// (init will mint)", while the rewrite kept the pair the file names. The
// data dir it points the pid file into is the file's too, the one the
// rewrite keeps, which the caller hands it.
func TestInitPreflightGradesThePairABrokenConfigNames(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bridge.yaml")
	certPath, keyPath := filepath.Join(dir, "pki", "bridge.crt"), filepath.Join(dir, "pki", "bridge.key")
	state := filepath.Join(dir, "state")
	body := "dataDir: " + state + "\ntlsCertPath: " + certPath + "\ntlsKeyPath: " + keyPath + "\nlibraryNmae: typo\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	prior, err := readPriorInstall(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	d := doctor.Deps{DataDir: prior.DataDir}
	withExistingInstallDeps(&d, cfgPath)

	if d.TLSCertPath != certPath || d.TLSKeyPath != keyPath {
		t.Errorf("the preflight grades %q / %q, want the pair the config names, %q / %q",
			d.TLSCertPath, d.TLSKeyPath, certPath, keyPath)
	}
	if want := filepath.Join(state, serverPIDFileName); d.OwnPIDFile != want || !d.OwnPIDPortsUnknown {
		t.Errorf("OwnPIDFile = %q (ports unknown %v), want %q, attribution only", d.OwnPIDFile, d.OwnPIDPortsUnknown, want)
	}
}

// TestInitRewriteOverAConfigItCannotParse: a file that is not YAML at all
// names nothing a rewrite can read, so it cannot keep the data dir, the pair,
// the endpoints or the roots, and it is refused before anything is written,
// whatever is in init's own data dir. It proceeded when a pair was there,
// until CodeRabbit's review of #1040: that pair being there does not make it
// the one the install serves. The remedy the refusal names, moving the file
// aside, works: init then runs as on a first install and keeps the pair it
// finds in its data dir.
func TestInitRewriteOverAConfigItCannotParse(t *testing.T) {
	const garbage = "tlsCertPath: [\n\tnot yaml\n"
	if err := yaml.Unmarshal([]byte(garbage), &map[string]any{}); err == nil {
		t.Fatal("premise: the fixture parses as YAML")
	}
	assertRefused := func(t *testing.T, cfgDir, before, out string, code int) {
		t.Helper()
		if code == 0 {
			t.Error("a rewrite over a config it cannot parse exited 0")
		}
		if readConfigFile(t, cfgDir) != before {
			t.Error("the refused rewrite changed the config")
		}
		for _, want := range []string{"the config was NOT changed", "move bridge.yaml aside"} {
			if !strings.Contains(out, want) {
				t.Errorf("the refusal does not say %q", want)
			}
		}
	}

	t.Run("the pair moved out of the data dir: refused", func(t *testing.T) {
		tmp := t.TempDir()
		cfgDir := filepath.Join(tmp, "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		certPath := filepath.Join(tmp, "pki", "bridge.crt")
		keyPath := filepath.Join(tmp, "pki", "bridge.key")
		movePair(t, filepath.Join(cfgDir, "data"), certPath, keyPath)
		appendToConfig(t, cfgDir, garbage)
		before := readConfigFile(t, cfgDir)
		pair := readPair(t, certPath, keyPath)

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		assertRefused(t, cfgDir, before, out, code)
		if _, err := os.Stat(filepath.Join(cfgDir, "data", servertls.CertFileName)); err == nil {
			t.Error("the refused rewrite minted a pair")
		}
		if readPair(t, certPath, keyPath) != pair {
			t.Error("the refused rewrite changed the install's pair")
		}
	})
	t.Run("a pair in the data dir: refused too", func(t *testing.T) {
		tmp := t.TempDir()
		cfgDir := filepath.Join(tmp, "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		certPath, keyPath := servertls.DefaultPaths(filepath.Join(cfgDir, "data"))
		pair := readPair(t, certPath, keyPath)
		appendToConfig(t, cfgDir, garbage)
		before := readConfigFile(t, cfgDir)

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		assertRefused(t, cfgDir, before, out, code)
		if readPair(t, certPath, keyPath) != pair {
			t.Error("the refused rewrite changed the pair in the data dir")
		}
	})
	t.Run("moved aside, as the refusal says: the data dir's pair kept", func(t *testing.T) {
		tmp := t.TempDir()
		cfgDir := filepath.Join(tmp, "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		pinned := servedFingerprint(t, cfgDir)
		appendToConfig(t, cfgDir, garbage)
		cfgPath := filepath.Join(cfgDir, "bridge.yaml")
		if err := os.Rename(cfgPath, cfgPath+".broken"); err != nil {
			t.Fatal(err)
		}

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("init exited %d with the broken config moved aside", code)
		}
		if got := servedFingerprint(t, cfgDir); got != pinned {
			t.Errorf("serve would present %s, and every paired device pinned %s", got, pinned)
		}
	})
}

// TestInitRewriteKeepsALoopbackInstallsCustomEndpoints: the endpoints an
// operator adds by hand or in Settings, which init never asks for. iOS
// replaces its alternates with /v1/health's endpoints on every successful
// fetch, so a rewrite that dropped one took that route from every paired
// device at its next health check.
//
// A --public rewrite still writes the domain's own endpoint in their place,
// and a loopback rewrite of a public install starts without the public ones,
// which name the domain and port the public posture listened on.
func TestInitRewriteKeepsALoopbackInstallsCustomEndpoints(t *testing.T) {
	const endpoint = "https://music.example.net:8443"
	t.Run("loopback over loopback: kept", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		appendToConfig(t, cfgDir, "customEndpoints:\n    - "+endpoint+"\n")

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		if got := loadInstallConfig(t, cfgDir).CustomEndpoints; !slices.Equal(got, []string{endpoint}) {
			t.Errorf("customEndpoints = %q, want [%s]", got, endpoint)
		}
		if !strings.Contains(out, endpoint) {
			t.Error("the rewrite does not say it kept the endpoint")
		}
	})
	t.Run("public over loopback: the domain's", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		lib := testLibrary(t)
		if code, out := loopbackInit(t, cfgDir, lib, "First"); code != 0 {
			t.Fatalf("the first init exited %d:\n%s", code, out)
		}
		appendToConfig(t, cfgDir, "customEndpoints:\n    - "+endpoint+"\n")
		ports := pickPublicInitPorts(t)

		code, out := publicInit(t, cfgDir, ports, "Rewritten", "--force")
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		want := []string{"https://localhost:" + strconv.Itoa(ports.api)}
		if got := loadInstallConfig(t, cfgDir).CustomEndpoints; !slices.Equal(got, want) {
			t.Errorf("customEndpoints = %q, want %q", got, want)
		}
	})
	t.Run("loopback over public: none", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		if code, out := publicInit(t, cfgDir, pickPublicInitPorts(t), "First", "--skip-doctor"); code != 0 {
			t.Fatalf("the first init exited %d:\n%s", code, out)
		}

		code, out := loopbackInit(t, cfgDir, testLibrary(t), "Rewritten", "--force")
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		if got := loadInstallConfig(t, cfgDir).CustomEndpoints; len(got) != 0 {
			t.Errorf("customEndpoints = %q, want none", got)
		}
	})
}

// TestInitPublicRewriteKeepsTheLibraryRootsWhenNoneIsNamed: a public install
// takes its roots later, in the console, and a public init needs no
// --library. So a public rewrite without one wrote `libraryRoots: []`, and
// every track on every device stopped playing. A --library still replaces
// them.
//
// The preflight grades only a root the run names. A kept root may be a
// mount that is not up, which public-mode serve tolerates, and
// checkLibraryRoots FAILs a missing root, so grading the kept ones refused
// the rewrite whenever the mount was down.
func TestInitPublicRewriteKeepsTheLibraryRootsWhenNoneIsNamed(t *testing.T) {
	for _, tc := range []struct {
		name string
		// unmounted removes the kept root before the rewrite.
		unmounted bool
		extra     func(other string) []string
		want      func(root, other string) []string
	}{
		{"no --library: kept", false,
			func(string) []string { return nil },
			func(root, _ string) []string { return []string{root} }},
		{"no --library, its mount down: kept", true,
			func(string) []string { return nil },
			func(root, _ string) []string { return []string{root} }},
		{"--library: replaced", false,
			func(other string) []string { return []string{"--library", other} },
			func(_, other string) []string { return []string{other} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			cfgDir := filepath.Join(tmp, "cfg")
			ports := pickPublicInitPorts(t)
			if code, out := publicInit(t, cfgDir, ports, "First", "--skip-doctor"); code != 0 {
				t.Fatalf("the first init exited %d:\n%s", code, out)
			}
			root, other := testLibrary(t), testLibrary(t)
			// What the console's POST /api/roots saves.
			setConfigKey(t, cfgDir, "libraryRoots", "libraryRoots:\n    - "+root)
			if tc.unmounted {
				if err := os.Remove(root); err != nil {
					t.Fatal(err)
				}
			}

			code, out := publicInit(t, cfgDir, ports, "Rewritten", append([]string{"--force"}, tc.extra(other)...)...)
			defer logRunOnFailure(t, out)

			if code != 0 {
				t.Fatalf("the rewrite exited %d", code)
			}
			if got, want := loadInstallConfig(t, cfgDir).LibraryRoots, tc.want(root, other); !slices.Equal(got, want) {
				t.Errorf("libraryRoots = %q, want %q", got, want)
			}
		})
	}
}

// TestInitRewriteKeepsTheLibraryNameWhenNoneIsNamed: the name an install is
// served under, in /v1/health (to a caller with no token too), its Bonjour
// record and the name= of every pairing QR, which a newly paired device takes
// for its own. A rewrite that named none replaced it with the host's name, the
// guess a first install takes: measured on 2026-09-27 with the real binary,
// `My Library` became `Macbook.local` in both postures, in the file and in
// what /v1/health and a new pairing URL said, and the run did not mention it.
// A --name still replaces it.
//
// A config that gives no name, or a blank one, has none to keep, and the
// rewrite takes the host's as a first install does, as it always has. The
// DefaultLibraryName config.Load serves such a config is a fallback nobody
// chose, and a rewrite that kept it would list it as kept from a config that
// never held it (Gemini, consulted on the first draft, which did). The run
// lists a kept name only where a first install would have taken another, so
// the rewrite of an install named for its host says nothing.
func TestInitRewriteKeepsTheLibraryNameWhenNoneIsNamed(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skipf("premise: this host has a name, which a first install takes (%v)", err)
	}
	for _, posture := range rewritePostures {
		for _, tc := range []struct {
			name string
			// first names the first init. nameLine, when set, then replaces
			// the line naming it in its config (a hand-written config may give
			// no name, or a blank one), and served is what config.Load then
			// serves.
			first, nameLine, served string
			// rewrite names the rewrite; empty passes no --name.
			rewrite string
			want    string
			// listed says the run lists the name among what it kept.
			listed bool
		}{
			{name: "no --name: kept",
				first: "My Library", want: "My Library", listed: true},
			{name: "--name: replaced",
				first: "My Library", rewrite: "Jazz Archive", want: "Jazz Archive"},
			{name: "a config giving no name: the host's, as a first install takes",
				first: "My Library", nameLine: "# no libraryName", served: config.DefaultLibraryName, want: host},
			{name: "a config giving a blank one: the host's too",
				first: "My Library", nameLine: `libraryName: "  "`, served: "  ", want: host},
			{name: "named for its host: kept, with nothing to say",
				want: host},
		} {
			t.Run(posture.name+"/"+tc.name, func(t *testing.T) {
				cfgDir := filepath.Join(t.TempDir(), "cfg")
				rewrite := posture.install(t, cfgDir, tc.first)
				if tc.nameLine != "" {
					setConfigKey(t, cfgDir, "libraryName", tc.nameLine)
					if got := loadInstallConfig(t, cfgDir).LibraryName; got != tc.served {
						t.Fatalf("premise: the edited config is served as %q, want %q", got, tc.served)
					}
				}

				code, out := rewrite(tc.rewrite)
				defer logRunOnFailure(t, out)

				if code != 0 {
					t.Fatalf("the rewrite exited %d", code)
				}
				if got := loadInstallConfig(t, cfgDir).LibraryName; got != tc.want {
					t.Errorf("libraryName = %q, want %q", got, tc.want)
				}
				listed, ok := keptValue(out, "libraryName")
				switch {
				case tc.listed && (!ok || listed != tc.want):
					t.Errorf("the run lists libraryName as %q (listed %v), want %q", listed, ok, tc.want)
				case !tc.listed && strings.Contains(out, keptFromHeading):
					t.Error("the run lists what it kept, and it kept nothing a first install would not have written")
				}
			})
		}
	}
}

// TestInitInteractiveRewriteOffersTheInstallsName: the name prompt's default
// is what Enter takes, and over an install with a name of its own it offered
// the host's, so an operator who pressed Enter through a rewrite replaced the
// name (measured: `Library display name [Macbook.local]` over `My Library`).
// It offers the install's name, which a typed one still replaces. A first
// install has no name to keep and is offered the host's, as before.
func TestInitInteractiveRewriteOffersTheInstallsName(t *testing.T) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("stdin is a terminal, where init would offer to start the bridge after the run")
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skipf("premise: this host has a name, which a first install is offered (%v)", err)
	}
	for _, tc := range []struct {
		name string
		// first names an install the run rewrites; empty runs a first install.
		first string
		typed string
		offer string
		want  string
	}{
		{"Enter keeps the install's name", "My Library", "", "My Library", "My Library"},
		{"a typed name replaces it", "My Library", "Jazz Archive", "My Library", "Jazz Archive"},
		{"a first install is offered the host's", "", "", host, host},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			lib := testLibrary(t)
			// The library, the name, and "Overwrite?" where there is a config.
			answers := lib + "\n" + tc.typed + "\n"
			if tc.first != "" {
				if code, out := loopbackInit(t, cfgDir, lib, tc.first); code != 0 {
					t.Fatalf("the first init exited %d:\n%s", code, out)
				}
				answers += "y\n"
			}

			var stdout, stderr strings.Builder
			code := initCmd([]string{"--no-service", "--skip-doctor", "--dir", cfgDir},
				strings.NewReader(answers), &stdout, &stderr)
			out := stripANSI("--- stdout ---\n" + stdout.String() + "\n--- stderr ---\n" + stderr.String())
			defer logRunOnFailure(t, out)

			if code != 0 {
				t.Fatalf("the run exited %d", code)
			}
			if want := "Library display name [" + tc.offer + "]"; !strings.Contains(out, want) {
				t.Errorf("the prompt does not offer %q", want)
			}
			if got := loadInstallConfig(t, cfgDir).LibraryName; got != tc.want {
				t.Errorf("libraryName = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestInitRewriteKeepsTheFileNotTheEnvironment: what a rewrite keeps comes
// from the file as written. config.Load applies BRIDGE_* overrides, and a
// rewrite that kept Load's values would write the environment of whoever ran
// init into the file, which the serve auto-init refuses to do for the same
// reason (writeAutoInitConfig): the next change to that environment would
// then not reach the bridge. The rewrite names no name, so the name is one of
// the values it keeps.
func TestInitRewriteKeepsTheFileNotTheEnvironment(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	rewrite := rewritePostures[0].install(t, cfgDir, "First")
	const endpoint = "https://music.example.net:8443"
	appendToConfig(t, cfgDir, "customEndpoints:\n    - "+endpoint+"\n")
	envData := filepath.Join(tmp, "env-data")
	t.Setenv("BRIDGE_DATA_DIR", envData)
	t.Setenv("BRIDGE_CUSTOM_ENDPOINTS", "https://env.example.test:9443")
	t.Setenv("BRIDGE_LIBRARY_NAME", "Env Name")

	code, out := rewrite("")
	defer logRunOnFailure(t, out)

	if code != 0 {
		t.Fatalf("the rewrite exited %d", code)
	}
	var written struct {
		DataDir         string   `yaml:"dataDir"`
		CustomEndpoints []string `yaml:"customEndpoints"`
		LibraryName     string   `yaml:"libraryName"`
	}
	if err := yaml.Unmarshal([]byte(readConfigFile(t, cfgDir)), &written); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfgDir, "data"); written.DataDir != want {
		t.Errorf("the file says dataDir %q, want %q", written.DataDir, want)
	}
	if !slices.Equal(written.CustomEndpoints, []string{endpoint}) {
		t.Errorf("the file says customEndpoints %q, want [%s]", written.CustomEndpoints, endpoint)
	}
	if written.LibraryName != "First" {
		t.Errorf("the file says libraryName %q, want %q", written.LibraryName, "First")
	}
	if _, err := os.Stat(envData); err == nil {
		t.Error("the rewrite wrote into the environment's data dir")
	}
}

// TestInitRewriteOfTheDefaultLayoutWritesWhatItAlwaysDid is the positive
// control: an install init made, with nothing added, rewritten. Its pair is
// in the data dir the config names by default, so the saved config names no
// pair, the fingerprint is the one it had, and the run has nothing to say
// about what it kept.
func TestInitRewriteOfTheDefaultLayoutWritesWhatItAlwaysDid(t *testing.T) {
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	rewrite := rewritePostures[0].setUp(t, cfgDir)
	pinned := servedFingerprint(t, cfgDir)

	code, out := rewrite()
	defer logRunOnFailure(t, out)

	if code != 0 {
		t.Fatalf("the rewrite exited %d", code)
	}
	file := readConfigFile(t, cfgDir)
	for _, key := range []string{"tlsCertPath", "tlsKeyPath", "customEndpoints"} {
		if strings.Contains(file, key) {
			t.Errorf("the saved config has %s:\n%s", key, file)
		}
	}
	if saved := loadInstallConfig(t, cfgDir); saved.DataDir != filepath.Join(cfgDir, "data") {
		t.Errorf("dataDir = %q, want the default", saved.DataDir)
	}
	if got := servedFingerprint(t, cfgDir); got != pinned {
		t.Errorf("serve would present %s, and every paired device pinned %s", got, pinned)
	}
	if strings.Contains(out, keptFromHeading) {
		t.Error("the rewrite lists what it kept, and it kept nothing init would not write")
	}
}

// TestInitRefusesToRewriteAConfigItDoesNotMake: a demo bridge's config and a
// managed tenant's are written by other tooling, and init writes neither
// posture. A rewrite turned the demo into an ordinary public bridge, which
// drops the token every shipped app carries, and a tenant into an unmanaged
// one, which hands its operator's controls (restart, updates, roots) to
// whoever holds a console session. Refused before anything is written. The
// keep path writes nothing and is not refused.
func TestInitRefusesToRewriteAConfigItDoesNotMake(t *testing.T) {
	for _, tc := range []struct{ name, block, says string }{
		{"demo", "demo:\n    enabled: true\n", "demo.enabled"},
		{"managed controls", "deployment:\n    managedControls:\n        - restart\n", "deployment.managedControls"},
		{"managed settings", "deployment:\n    managedSettings:\n        - libraryName\n", "deployment.managedSettings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			rewrite := rewritePostures[0].setUp(t, cfgDir)
			appendToConfig(t, cfgDir, tc.block)
			before := readConfigFile(t, cfgDir)

			code, out := rewrite()
			defer logRunOnFailure(t, out)

			if code == 0 {
				t.Error("the rewrite exited 0")
			}
			if readConfigFile(t, cfgDir) != before {
				t.Error("the refused rewrite changed the config")
			}
			for _, want := range []string{tc.says, "the config was NOT changed"} {
				if !strings.Contains(out, want) {
					t.Errorf("the refusal does not say %q", want)
				}
			}

			// The keep path: --yes without --force leaves the config as it is.
			// Its own variables: the deferred log above is the refused run's
			// output, and reusing out would make it this run's instead.
			if keepCode, keepOut := loopbackInit(t, cfgDir, testLibrary(t), "Kept"); keepCode != 0 {
				t.Errorf("a run that keeps the config exited %d:\n%s", keepCode, keepOut)
			}
		})
	}
}

// TestInitRefusesToRewriteAConfigThisUserCannotRead: a config this user may
// not read is one the bridge's own user reads at every start, so this run is
// the wrong user's, and it cannot tell which data dir and pair the rewrite
// would keep.
func TestInitRefusesToRewriteAConfigThisUserCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file mode does not deny its owner a read on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	rewrite := rewritePostures[0].setUp(t, cfgDir)
	cfgPath := filepath.Join(cfgDir, "bridge.yaml")
	if err := os.Chmod(cfgPath, 0); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	code, out := rewrite()
	defer logRunOnFailure(t, out)

	if code == 0 {
		t.Error("the rewrite exited 0 over a config this user cannot read")
	}
	if after, err := os.Stat(cfgPath); err != nil || !os.SameFile(info, after) || after.ModTime() != info.ModTime() {
		t.Errorf("the refused rewrite replaced the config: %v", err)
	}
	for _, want := range []string{"run init as the user the bridge runs as", "the config was NOT changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not say %q", want)
		}
	}
}

// TestPriorInstallReadsWhatLoadReads pins readPriorInstall to config.Load for
// a config that loads: the same data dir, pair, roots, endpoints and name,
// with the relative paths resolved against the config's directory, not the
// working directory. The fields come from a struct of their own, so a yaml
// tag renamed on config.Config would otherwise leave it reading a key nothing
// writes.
func TestPriorInstallReadsWhatLoadReads(t *testing.T) {
	for _, env := range []string{"BRIDGE_DATA_DIR", "BRIDGE_LIBRARY_ROOTS", "BRIDGE_CUSTOM_ENDPOINTS", "BRIDGE_LIBRARY_NAME"} {
		t.Setenv(env, "")
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bridge.yaml")
	body := "libraryRoots:\n    - music\n" +
		"dataDir: state\n" +
		"tlsCertPath: pki/bridge.crt\ntlsKeyPath: pki/bridge.key\n" +
		"libraryName: Jazz Archive\n" +
		"customEndpoints:\n    - https://music.example.net:8443\n" +
		"deployment:\n    managedSettings:\n        - libraryName\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())

	prior, err := readPriorInstall(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct {
		name      string
		got, want any
	}{
		{"dataDir", prior.DataDir, loaded.DataDir},
		{"tlsCertPath", prior.TLSCertPath, loaded.TLSCertPath},
		{"tlsKeyPath", prior.TLSKeyPath, loaded.TLSKeyPath},
		{"libraryRoots", prior.LibraryRoots, loaded.LibraryRoots},
		{"libraryName", prior.LibraryName, loaded.LibraryName},
		{"customEndpoints", prior.CustomEndpoints, loaded.CustomEndpoints},
		{"deployment.managedSettings", prior.Deployment.ManagedSettings, loaded.Deployment.ManagedSettings},
	} {
		if !reflect.DeepEqual(f.got, f.want) {
			t.Errorf("%s: readPriorInstall %q, config.Load %q", f.name, f.got, f.want)
		}
	}

	// And a missing dataDir is the default one, beside the config.
	if err := os.WriteFile(cfgPath, []byte("libraryName: X\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if prior, err = readPriorInstall(cfgPath); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "data"); prior.DataDir != want {
		t.Errorf("dataDir = %q with none in the file, want %q", prior.DataDir, want)
	}
}

// TestPriorInstallTagsAreConfigs checks every field readPriorInstall decodes
// against the config.Config field of the same name: the same type and the
// same yaml key.
func TestPriorInstallTagsAreConfigs(t *testing.T) {
	var check func(prior, cfg reflect.Type, path string)
	check = func(prior, cfg reflect.Type, path string) {
		for i := 0; i < prior.NumField(); i++ {
			pf := prior.Field(i)
			cf, ok := cfg.FieldByName(pf.Name)
			if !ok {
				t.Errorf("%s%s: config.Config has no such field", path, pf.Name)
				continue
			}
			pKey := strings.Split(pf.Tag.Get("yaml"), ",")[0]
			cKey := strings.Split(cf.Tag.Get("yaml"), ",")[0]
			if pKey != cKey {
				t.Errorf("%s%s: yaml key %q, config.Config says %q", path, pf.Name, pKey, cKey)
			}
			if pf.Type.Kind() == reflect.Struct && pf.Type != cf.Type {
				check(pf.Type, cf.Type, path+pf.Name+".")
			} else if pf.Type != cf.Type {
				t.Errorf("%s%s: type %s, config.Config says %s", path, pf.Name, pf.Type, cf.Type)
			}
		}
	}
	check(reflect.TypeOf(priorInstallFile{}), reflect.TypeOf(config.Config{}), "")
}

// rewritePosture runs the two inits of a rewrite test in one posture.
type rewritePosture struct {
	name string
	// install runs the first init over cfgDir, named name, and returns the
	// second: the same install's init again, with --force, the name it is
	// given and any extra flags. An empty name passes no --name.
	install func(t *testing.T, cfgDir, name string) func(name string, extra ...string) (int, string)
	// printsFingerprint says whether the run shows the fingerprint to pin.
	printsFingerprint bool
}

// setUp is install named First, with a rewrite named Rewritten, for the
// tests of what a rewrite keeps other than the name.
func (p rewritePosture) setUp(t *testing.T, cfgDir string) func(extra ...string) (int, string) {
	t.Helper()
	rewrite := p.install(t, cfgDir, "First")
	return func(extra ...string) (int, string) {
		return rewrite("Rewritten", extra...)
	}
}

// rewritePostures are the two installs init makes. The loopback one runs
// without the preflight, which grades 7788 / 7789 and fails where something
// holds them, and the public one with it, on ports picked free.
var rewritePostures = []rewritePosture{
	{
		name: "loopback",
		install: func(t *testing.T, cfgDir, name string) func(string, ...string) (int, string) {
			t.Helper()
			lib := testLibrary(t)
			if code, out := loopbackInit(t, cfgDir, lib, name); code != 0 {
				t.Fatalf("the first init exited %d:\n%s", code, out)
			}
			return func(name string, extra ...string) (int, string) {
				return loopbackInit(t, cfgDir, lib, name, append([]string{"--force"}, extra...)...)
			}
		},
		printsFingerprint: true,
	},
	{
		name: "public, with the preflight",
		install: func(t *testing.T, cfgDir, name string) func(string, ...string) (int, string) {
			t.Helper()
			ports := pickPublicInitPorts(t)
			if code, out := publicInit(t, cfgDir, ports, name, "--skip-doctor"); code != 0 {
				t.Fatalf("the first init exited %d:\n%s", code, out)
			}
			return func(name string, extra ...string) (int, string) {
				return publicInit(t, cfgDir, ports, name, append([]string{"--force"}, extra...)...)
			}
		},
	},
}

// loopbackInit runs `bridge init --yes --no-service --skip-doctor` over
// cfgDir with the given library and name (no --name when it is empty), and
// any extra flags. It returns the exit code and both streams.
func loopbackInit(t *testing.T, cfgDir, lib, name string, extra ...string) (int, string) {
	t.Helper()
	args := []string{
		"--yes", "--no-service", "--skip-doctor",
		"--dir", cfgDir, "--library", lib,
	}
	if name != "" {
		args = append(args, "--name", name)
	}
	args = append(args, extra...)
	var out, errOut strings.Builder
	code := initCmd(args, strings.NewReader(""), &out, &errOut)
	return code, stripANSI("--- stdout ---\n" + out.String() + "\n--- stderr ---\n" + errOut.String())
}

// testLibrary makes an empty library directory.
func testLibrary(t *testing.T) string {
	t.Helper()
	lib := filepath.Join(t.TempDir(), "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	return lib
}

// servedFingerprint is the fingerprint `bridge serve` would present for the
// install at cfgDir: the pair at the paths serve resolves from its config,
// with resolveCertPaths, serve's own call. Serve loads that pair when both
// files are there and mints a new one when neither is, so a pair that is not
// there answers "(minted)", which no pinned fingerprint equals.
func servedFingerprint(t *testing.T, cfgDir string) string {
	t.Helper()
	certPath, keyPath := resolveCertPaths(loadInstallConfig(t, cfgDir))
	if _, err := os.Stat(keyPath); errors.Is(err, fs.ErrNotExist) {
		return "(minted)"
	}
	info, err := servertls.Inspect(certPath)
	if errors.Is(err, fs.ErrNotExist) {
		return "(minted)"
	}
	if err != nil {
		t.Fatal(err)
	}
	return info.Fingerprint
}

// keptValue returns the value the run's list of what it kept gives key, and
// whether the list has key at all. A key with several values gives the first.
func keptValue(out, key string) (string, bool) {
	_, list, ok := strings.Cut(out, keptFromHeading+"\n")
	if !ok {
		return "", false
	}
	for _, line := range strings.Split(list, "\n") {
		entry, ok := strings.CutPrefix(line, "  ")
		if !ok {
			break
		}
		if value, ok := strings.CutPrefix(entry, key+" "); ok {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

// assertPrintedFingerprint fails unless the run's fingerprint box shows
// want, the fingerprint a device pins.
func assertPrintedFingerprint(t *testing.T, out, want string) {
	t.Helper()
	first, second := splitFingerprint(want)
	if !strings.Contains(out, first) || !strings.Contains(out, second) {
		t.Errorf("the run's fingerprint box does not show %s, the fingerprint serve presents", want)
	}
}

// loadInstallConfig loads the config at cfgDir as serve would.
func loadInstallConfig(t *testing.T, cfgDir string) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join(cfgDir, "bridge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// readConfigFile returns the bytes of the config at cfgDir.
func readConfigFile(t *testing.T, cfgDir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cfgDir, "bridge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// appendToConfig adds lines to the config at cfgDir, as an operator's edit.
func appendToConfig(t *testing.T, cfgDir, lines string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(cfgDir, "bridge.yaml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(lines); err != nil {
		t.Fatal(err)
	}
}

// setConfigKey replaces the line that sets a top-level key in the config at
// cfgDir with block.
func setConfigKey(t *testing.T, cfgDir, key, block string) {
	t.Helper()
	body := readConfigFile(t, cfgDir)
	line := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `:.*$`)
	if !line.MatchString(body) {
		t.Fatalf("the config sets no %s:\n%s", key, body)
	}
	body = line.ReplaceAllLiteralString(body, block)
	if err := os.WriteFile(filepath.Join(cfgDir, "bridge.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// movePair moves the TLS pair out of dataDir to certPath and keyPath.
func movePair(t *testing.T, dataDir, certPath, keyPath string) {
	t.Helper()
	fromCert, fromKey := servertls.DefaultPaths(dataDir)
	for _, dir := range []string{filepath.Dir(certPath), filepath.Dir(keyPath)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for from, to := range map[string]string{fromCert: certPath, fromKey: keyPath} {
		if err := os.Rename(from, to); err != nil {
			t.Fatal(err)
		}
	}
}

// readPair returns the pair's two files' bytes, together.
func readPair(t *testing.T, certPath, keyPath string) string {
	t.Helper()
	var b strings.Builder
	for _, p := range []string{certPath, keyPath} {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
	}
	return b.String()
}
