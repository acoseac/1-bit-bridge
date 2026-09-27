package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/acoseac/1-bit-bridge/internal/config"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// What `bridge init` keeps when it overwrites a config: with --force, or
// "Overwrite?" answered y.
//
// An overwrite replaces the install's settings with the run's answers and
// the defaults, as its flag and its prompt say. It does not replace the
// install. It used to: the rewrite built bridge.yaml from baseConfig and the
// flags alone, so a config naming its own TLS pair or its own data dir lost
// it, init minted a new pair, and every paired device lost its pin, its
// bearer token or both (measured 2026-09-27). What a rewrite keeps is decided
// by who could give a value back if it were lost:
//
//   - dataDir, tlsCertPath and tlsKeyPath, always. The data dir holds
//     tokens.json, the database, a public install's admin account and the
//     default TLS pair, and the pair is what every device pinned. Neither can
//     be handed back to a device without pairing it again.
//   - customEndpoints, on a loopback rewrite of a loopback install. init never
//     asks for them, and iOS replaces its alternates with /v1/health's
//     endpoints on every successful fetch, so dropping one took that route
//     from every device at its next health check. A --public rewrite writes
//     the domain's endpoint in their place, as it always has, and a rewrite
//     that changes posture starts from the new posture's: the old list names
//     the addresses the other posture listened on.
//   - libraryRoots, when the run names no --library, which only a public run
//     may do: a public install takes its roots later, in the console, and a
//     rewrite that emptied them left every track unplayable.
//
// Everything else is this run's value or the default, which is the
// documented overwrite: features, cadences, the ports (init grades the ports
// it saves rather than keeping the old ones), the library name.

// priorInstallFile is what the config already at init's path says about the
// install it describes: the fields a rewrite keeps, and those that decide
// whether it may rewrite at all.
//
// Read from the FILE, not through config.Load. Load applies the BRIDGE_*
// overrides, and a rewrite that kept Load's values would write the
// environment of whoever ran init into the file, which the serve auto-init
// refuses to do for the same reason (writeAutoInitConfig). And read without
// Load's unknown-key refusal or its validation, so a config that does not
// load for a misspelt key (#1027's row C, the config re-running init exists
// to replace) still gives up what the rewrite keeps. A struct of its own
// rather than config.Config, so a type error in a field it does not keep
// cannot cost the ones it does. Its yaml keys and types are config.Config's
// (TestPriorInstallTagsAreConfigs).
type priorInstallFile struct {
	DataDir         string                  `yaml:"dataDir"`
	TLSCertPath     string                  `yaml:"tlsCertPath"`
	TLSKeyPath      string                  `yaml:"tlsKeyPath"`
	LibraryRoots    []string                `yaml:"libraryRoots"`
	CustomEndpoints []string                `yaml:"customEndpoints"`
	Deployment      config.DeploymentConfig `yaml:"deployment"`
	Demo            config.DemoConfig       `yaml:"demo"`
}

// readPriorInstall reads the config at cfgPath as priorInstallFile, with its
// paths resolved as config.Load resolves them: against the file's directory,
// dataDir defaulting to the one beside it. A config that is not there is no
// install, and answers nil and no error.
func readPriorInstall(cfgPath string) (*priorInstallFile, error) {
	raw, err := os.ReadFile(cfgPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", cfgPath, err)
	}
	var p priorInstallFile
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", cfgPath, err)
	}
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", cfgPath, err)
	}
	base := filepath.Dir(abs)
	if p.DataDir == "" {
		p.DataDir = config.DefaultDataDir
	}
	p.DataDir = config.ResolvePath(base, p.DataDir)
	p.TLSCertPath = config.ResolvePath(base, p.TLSCertPath)
	p.TLSKeyPath = config.ResolvePath(base, p.TLSKeyPath)
	for i, root := range p.LibraryRoots {
		p.LibraryRoots[i] = config.ResolvePath(base, root)
	}
	return &p, nil
}

// loopback says the file's posture is loopback: deployment.mode unset or
// "loopback". A mode the build does not know is neither posture.
func (p *priorInstallFile) loopback() bool {
	mode, err := p.Deployment.EffectiveMode()
	return err == nil && mode == config.DeploymentModeLoopback
}

// refuseRewrite decides whether this run may overwrite the config at
// cfgPath, before anything is written, and says why when it may not. prior
// and priorErr are readPriorInstall's answer for cfgPath, and dataDir is the
// data dir the run keeps: prior's, or init's own when there is no prior.
//
//   - A config this user cannot read is read by the bridge's own user at
//     every start, so this run is the wrong user's, and it cannot tell which
//     data dir and pair it would keep.
//   - A file that does not parse names nothing the rewrite can keep, so init
//     keeps its own layout and the pair in its own data dir, as #1027 assumes
//     for the pid file. Where there is no pair there, the rewrite would MINT
//     one, and the config it cannot read may name the pair every device
//     pinned anywhere, so it refuses.
//   - A config naming one half of a TLS pair names a pair nothing can load,
//     and dropping the half it names would serve the data dir's pair, or mint
//     one, which no device may have pinned: the reported pin break by another
//     route. Kept, the half failed validation, a refusal too, but one that
//     said neither what to do nor that nothing had changed.
//   - A demo bridge's config and a managed tenant's are written by other
//     tooling, in postures init never writes: a rewrite made the demo an
//     ordinary bridge, dropping the token every shipped app carries, and a
//     tenant an unmanaged one, handing the controls its operator withholds to
//     whoever holds a console session.
func refuseRewrite(stdout, stderr io.Writer, cfgPath, dataDir string, prior *priorInstallFile, priorErr error) bool {
	var postures []postureKey
	if prior != nil {
		postures = prior.madeElsewhere()
	}
	switch {
	case errors.Is(priorErr, fs.ErrPermission):
		fmt.Fprintf(stderr, "%v\n", priorErr)
		fmt.Fprintf(stderr, "this user cannot read the config at %s, so init cannot tell which data dir and TLS pair a rewrite would keep.\n", cfgPath)
		fmt.Fprintln(stderr, "run init as the user the bridge runs as.")
	case priorErr != nil:
		certPath, keyPath := servertls.DefaultPaths(dataDir)
		if pathExists(certPath) || pathExists(keyPath) {
			fmt.Fprintf(stdout, "%v\n", priorErr)
			fmt.Fprintf(stdout, "the file names nothing this rewrite can read, so it keeps the TLS pair in %s, where init keeps it.\n", dataDir)
			return false
		}
		fmt.Fprintf(stderr, "%v\n", priorErr)
		fmt.Fprintf(stderr, "init cannot tell which TLS pair this install serves, and there is none at %s.\n", certPath)
		fmt.Fprintln(stderr, "a rewrite would mint a new one, and every device paired with this install would have to pair again.")
		fmt.Fprintln(stderr, "fix the YAML, or move bridge.yaml aside to set this install up from scratch.")
	case prior != nil && (prior.TLSCertPath == "") != (prior.TLSKeyPath == ""):
		named, missing := "tlsCertPath", "tlsKeyPath"
		if prior.TLSCertPath == "" {
			named, missing = missing, named
		}
		fmt.Fprintf(stderr, "the config at %s names %s without %s, so init cannot tell which TLS pair this install serves.\n", cfgPath, named, missing)
		fmt.Fprintf(stderr, "add %s, or remove %s to use the pair in the data dir.\n", missing, named)
	case len(postures) > 0:
		var keys, costs []string
		for _, m := range postures {
			keys, costs = append(keys, m.key), append(costs, m.cost)
		}
		fmt.Fprintf(stderr, "the config at %s sets %s, a posture bridge init never writes.\n",
			cfgPath, strings.Join(keys, " and "))
		fmt.Fprintf(stderr, "a rewrite would make it an ordinary bridge: %s.\n", strings.Join(costs, ", and "))
		fmt.Fprintln(stderr, "change it by hand, or with the tooling that manages this bridge.")
	default:
		return false
	}
	fmt.Fprintln(stderr, "the config was NOT changed.")
	return true
}

// postureKey is a key only the tooling running a demo bridge or a managed
// tenant writes, with what a rewrite that dropped it would cost.
type postureKey struct{ key, cost string }

// madeElsewhere lists the postureKeys the file sets.
func (p *priorInstallFile) madeElsewhere() []postureKey {
	var out []postureKey
	if p.Demo.Enabled {
		out = append(out, postureKey{"demo.enabled",
			"the demo's pinned token would stop working in every shipped app"})
	}
	if len(p.Deployment.ManagedControls) > 0 {
		out = append(out, postureKey{"deployment.managedControls",
			"its console would offer the controls its operator withholds (" +
				strings.Join(p.Deployment.ManagedControls, ", ") + ")"})
	}
	if len(p.Deployment.ManagedSettings) > 0 {
		out = append(out, postureKey{"deployment.managedSettings",
			"its console could change the settings its operator manages"})
	}
	return out
}

// keepFromPrior sets on cfg what a rewrite keeps from prior that has not
// reached it already: the TLS pair, and a loopback install's custom
// endpoints on a loopback rewrite. The data dir and the roots reach cfg
// through baseConfig, since initCmd decides both before the preflight, which
// uses the data dir. It reports whether it kept the endpoints, for
// printKept.
func keepFromPrior(cfg *config.Config, prior *priorInstallFile) (endpointsKept bool) {
	if prior == nil {
		return false
	}
	cfg.TLSCertPath, cfg.TLSKeyPath = prior.TLSCertPath, prior.TLSKeyPath
	if cfg.IsPublic() || !prior.loopback() || len(prior.CustomEndpoints) == 0 {
		return false
	}
	cfg.CustomEndpoints = slices.Clone(prior.CustomEndpoints)
	return true
}

// keptFromHeading opens the list printKept prints.
const keptFromHeading = "kept from the config this run replaces:"

// printKept lists, after the rewrite is saved, what it kept that a first
// install would not have: a data dir other than initDataDir (init's own,
// beside the config), a pair the config names, the roots when the run named
// none, the custom endpoints. An operator who rewrote a config should not
// have to diff it to learn that these stayed, and the common rewrite, of an
// install init made, prints nothing.
//
// Both sides of the data dir comparison are absolute, so it compares
// directories and not spellings: initCmd makes cfgDir absolute before it
// derives initDataDir from it, and a kept data dir is readPriorInstall's,
// which resolves the file's value against the config's directory.
// NormalizeAndValidate resolves no path. (Gemini on #1040, twice, assumed a
// relative cfgDir.)
func printKept(w io.Writer, cfg *config.Config, initDataDir string, rootsKept, endpointsKept bool) {
	var lines [][2]string
	add := func(key string, values ...string) {
		for i, v := range values {
			if i > 0 {
				key = ""
			}
			lines = append(lines, [2]string{key, v})
		}
	}
	if cfg.DataDir != initDataDir {
		add("dataDir", cfg.DataDir)
	}
	if cfg.TLSCertPath != "" {
		add("tlsCertPath", cfg.TLSCertPath)
	}
	if cfg.TLSKeyPath != "" {
		add("tlsKeyPath", cfg.TLSKeyPath)
	}
	if rootsKept {
		add("libraryRoots", cfg.LibraryRoots...)
	}
	if endpointsKept {
		add("customEndpoints", cfg.CustomEndpoints...)
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, keptFromHeading)
	for _, l := range lines {
		fmt.Fprintf(w, "  %-16s %s\n", l[0], l[1])
	}
	fmt.Fprintln(w, "every other setting is this run's or the default.")
}

// pathExists says whether something is at path.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
