package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"net"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/packaging"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// baseConfig builds the minimal loopback-mode config shared by two
// writers: `bridge init` (as its base, before any --public mutations) and
// serve's --init-if-missing auto-init. Values are the loopback defaults;
// callers layer mode-specific fields on top. Kept as one helper so the two
// seed paths can't drift.
func baseConfig(roots []string, name, dataDir string) *config.Config {
	return &config.Config{
		LibraryRoots:    roots,
		ListenAddress:   config.DefaultListenAddress,
		AdminAddress:    config.DefaultAdminAddress,
		DataDir:         dataDir,
		ScanIntervalSec: config.DefaultScanIntervalSec,
		LibraryName:     name,
	}
}

// initDataDirFor is the data dir `bridge init` writes for a config in cfgDir:
// data, beside the config. A rewrite keeps the data dir an install's config
// names instead (init_rewrite.go). Before init has run, `bridge doctor` grades
// the TLS pair in it (buildDoctorDepsFor), the pair init keeps or mints.
func initDataDirFor(cfgDir string) string {
	return filepath.Join(cfgDir, "data")
}

// firstInstallName is the library name init gives an install that has none
// to keep, when the run names none: the host's name, or DefaultLibraryName
// on a host without one.
func firstInstallName() string {
	if h, _ := os.Hostname(); h != "" {
		return h
	}
	return config.DefaultLibraryName
}

// initAddresses is the API and admin address a run of `bridge init` writes:
// its defaults, or the addresses its flags name. It is the one definition of
// both. The preflight grades their ports wherever no install's config names
// its own, and initCmd builds the config from them, so the preflight cannot
// grade one port while the config gets another. Until 2026-09-28 the
// preflight graded 7788 and 7789 there whatever the run wrote, so another
// process on 7788 refused a public first install that would never bind it.
//
// A loopback run's defaults are config's: the API on :7788 and the admin
// console on 127.0.0.1:7789. A public run listens on :443, which ACME's
// TLS-ALPN-01 challenge needs. Its admin console's default depends on the TLS
// posture (CodeRabbit Major review post-PR-#295):
//
//   - The bridge terminating the console's TLS itself (no --admin-tls-proxy):
//     0.0.0.0:7789, so the operator's iOS management surface can reach it
//     from any interface. The TLS wrap via certManager is the trust boundary.
//   - --admin-tls-proxy: 127.0.0.1:7789. The reverse proxy talks to it on
//     loopback, and the bridge MUST NOT serve a plain-HTTP console on another
//     interface, which would leak session cookies and login credentials if
//     the firewall is mis-wired or the proxy is briefly down. The 0.0.0.0
//     default before that review was an unsafe shape.
//
// --listen-address and --admin-address win over the defaults, in either
// posture. initCmd refuses a value the config's own check refuses before it
// calls this (initAddressFlagsError), a loopback run's --admin-address
// included, which must name a loopback host as that install's adminAddress
// must. Until 2026-09-28 a loopback run read neither flag: it saved :7788 and
// 127.0.0.1:7789, without a word, whatever it was given, 0.0.0.0 included.
func initAddresses(public, proxy bool, listenFlag, adminFlag string) (listen, admin string) {
	listen, admin = config.DefaultListenAddress, config.DefaultAdminAddress
	if public {
		listen, admin = ":443", "0.0.0.0:7789"
		if proxy {
			admin = "127.0.0.1:7789"
		}
	}
	if listenFlag != "" {
		listen = listenFlag
	}
	if adminFlag != "" {
		admin = adminFlag
	}
	return listen, admin
}

// initAddressFlagsError is the refusal of a --listen-address or
// --admin-address the config's own validation would refuse in the posture
// this run writes, or nil. initCmd asks it before the preflight, which grades
// the port an address names and has none to grade for one that does not
// parse. Both must parse with a port (config.ValidateBindAddress), in either
// posture. A loopback run's admin address must also name a loopback host
// (config.ValidateLoopbackAddress), as that install's adminAddress must: its
// console has no login, so binding loopback is its whole trust boundary. A
// public run's console has a login and may bind any interface.
func initAddressFlagsError(public bool, listen, admin string) error {
	for _, f := range []struct{ flag, addr string }{
		{"--listen-address", listen},
		{"--admin-address", admin},
	} {
		if f.addr == "" {
			continue
		}
		if err := config.ValidateBindAddress(f.flag, f.addr); err != nil {
			return err
		}
	}
	if public || admin == "" {
		return nil
	}
	if err := config.ValidateLoopbackAddress("--admin-address", admin); err != nil {
		return fmt.Errorf("%w\nwithout --public the admin console has no login, so it listens on this machine only: "+
			"reach it from another over an SSH tunnel, or run init with --public for a console with a login", err)
	}
	return nil
}

// postureFlags are init's flags that describe a public install: --public, and
// the three that apply only with it.
type postureFlags struct {
	public        bool
	domain, email string
	proxy         bool
}

// publicOnly lists the flags given that apply only with --public, in the
// order init's help gives them.
func (f postureFlags) publicOnly() []string {
	var out []string
	if f.domain != "" {
		out = append(out, "--domain")
	}
	if f.email != "" {
		out = append(out, "--email")
	}
	if f.proxy {
		out = append(out, "--admin-tls-proxy")
	}
	return out
}

// configNotChanged is the line that closes a refusal init makes before it
// writes over an install's config, so the operator knows the install is as
// it was.
const configNotChanged = "the config was NOT changed."

// warnIgnoredPostureFlags says what init does with a posture flag this run
// does not write, and returns the exit code of a refusal, or 0. exists says a
// config is at cfgPath, replace that this run rewrites it (--yes --force, or
// an interactive yes), and prior is readPriorInstall's answer for it.
//
// --domain, --email and --admin-tls-proxy describe a public install, and a run
// without --public ignored them without a word until 2026-09-29 (backlog B61):
// a loopback first install given all three saved none of them, and a --yes
// --force rewrite of a public install given --domain and --admin-tls-proxy but
// not --public saved a loopback config, the endpoint every paired device dials
// dropped.
//
//   - A rewrite of a PUBLIC install is refused, exit 2, before anything is
//     graded or written. It is the run where the missing --public costs
//     something: the flags say the operator meant a public install, and the
//     rewrite would make this one loopback, dropping that endpoint.
//   - A first install, and a rewrite of a loopback install, warn and go on.
//     Each writes a working loopback install and loses nothing, so neither is
//     stopped over a flag that changes nothing it writes. A refusal of the
//     loopback rewrite was the first draft, and it would fail a script that
//     rewrites its config on every run with --yes --force, passing these
//     flags, on its second run, the first having only warned.
//   - A run that keeps the config says nothing more. Every flag it was given
//     goes unused, which its "keeping it" line says, and an idempotent `bridge
//     init --yes` re-run passing these flags must go on working as its first
//     run did.
//
// With --public and --admin-tls-proxy, --email goes unused too: the bridge
// then runs no ACME client, and --email is that client's contact address. The
// run warns, as a first install or a rewrite, since the config loses nothing.
// Every line names the flags and never their values: a --domain may carry a
// user name and password, which --public refuses without echoing (backlog
// B54).
func warnIgnoredPostureFlags(stderr io.Writer, f postureFlags, exists, replace bool, cfgPath string, prior *priorInstallFile) int {
	if exists && !replace {
		return 0
	}
	if f.public {
		if f.proxy && f.email != "" {
			fmt.Fprintln(stderr, "warning: --email is not used with --admin-tls-proxy, so this run ignores it: it is the "+
				"Let's Encrypt contact for the certificate the bridge obtains itself, and behind a TLS proxy it obtains none.")
		}
		return 0
	}
	given := f.publicOnly()
	if len(given) == 0 {
		return 0
	}
	names, verb, them := sentenceOfFlags(given)
	if !replace || prior == nil || !prior.public() {
		fmt.Fprintf(stderr, "warning: %s %s only with --public, so this run ignores %s and sets up a loopback install; "+
			"add --public for a public install.\n", names, verb, them)
		return 0
	}
	fmt.Fprintf(stderr, "%s %s only with --public, and this run would rewrite the public install at %s as a loopback "+
		"one, which ignores %s and drops the endpoint every paired device dials.\n", names, verb, cfgPath, them)
	fmt.Fprintf(stderr, "add --public to rewrite it as a public install, or leave %s out to make it a loopback one.\n", them)
	fmt.Fprintln(stderr, configNotChanged)
	return 2
}

// sentenceOfFlags joins flag names for a sentence ("--a", "--a and --b",
// "--a, --b and --c"), with the verb and the pronoun that agree with them.
func sentenceOfFlags(flags []string) (names, verb, pronoun string) {
	if len(flags) == 1 {
		return flags[0], "applies", "it"
	}
	return strings.Join(flags[:len(flags)-1], ", ") + " and " + flags[len(flags)-1], "apply", "them"
}

// portsThisInitWrites is what init prints under a preflight report whose
// port-api or port-admin check FAILed on the ports this run writes. The
// preflight grades those wherever no install's config names its own
// (initAddresses), and on a --yes --force rewrite of one whose config does,
// which grades only the ports it keeps (doctor.Deps.AbandonedPorts). Those
// lines are then about the run's choice, not only a verdict about an install
// that is there, and the check's own hint, written for an install whose
// bridge.yaml names the port, cannot name the flags that choose the run's.
func portsThisInitWrites() string {
	return "port-api and port-admin above grade the ports this init would write; " +
		"--listen-address and --admin-address choose others."
}

// portsARewriteAbandons are the ports of the install at the target path
// (installAPI, installAdmin, which its config names) that a rewrite writing
// runAPI and runAdmin binds in neither role, once each: what the preflight
// leaves ungraded on a --yes --force rewrite (doctor.Deps.AbandonedPorts). A
// port the rewrite keeps in the other role, the API moving onto the old admin
// port say, is not abandoned: the bridge binds it again after a restart. So
// every port the second pass grades, one the run writes, is outside the list.
func portsARewriteAbandons(installAPI, installAdmin, runAPI, runAdmin int) []int {
	var out []int
	for _, p := range []int{installAPI, installAdmin} {
		if p != runAPI && p != runAdmin && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// initCmd walks a first-time operator through the minimum answers needed
// to get a running bridge: config dir, library root, then writes
// bridge.yaml, mints the TLS cert, installs a launchd/systemd user unit,
// and prints the admin console URL so they can open it and pair.
//
// Idempotent: re-running on a populated config dir offers to keep or
// rewrite the existing bridge.yaml. A rewrite replaces the settings, not
// the install: it keeps the data dir and the TLS pair the config names —
// rotating the pair breaks every paired client's pin — and so a public
// install's admin account, which `bridge admin reset-password` rotates
// (init_rewrite.go says what else it keeps, and what it refuses to
// rewrite). Every refusal is decided before bridge.yaml is written.
func initCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgDirFlag := fs.String("dir", "", "config directory (default per-OS standard)")
	nonInteractive := fs.Bool("yes", false, "accept all defaults without prompting")
	fs.BoolVar(nonInteractive, "y", *nonInteractive, "alias for --yes")
	force := fs.Bool("force", false, "with --yes: overwrite an existing config (by default, --yes refuses to clobber); its data dir and TLS pair are kept")
	libraryRoot := fs.String("library", "", "library root path (required with --yes)")
	libraryName := fs.String("name", "", "library display name (default: the existing config's, or the hostname)")
	skipService := fs.Bool("no-service", false, "skip launchd/systemd install; run `bridge serve` yourself")
	skipDoctor := fs.Bool("skip-doctor", false, "don't run `bridge doctor` preflight before init (not recommended)")
	windowsService := fs.Bool("service", false, "Windows only: install as a Windows Service (requires admin); default is a Startup-folder launcher")
	// Windows-only: when init installs the Startup-folder launcher, the
	// .cmd only fires on *next* logon. Default behaviour with an
	// interactive prompt is to also spawn the server right now so the
	// operator can open the admin console without a logout. For
	// non-interactive runs (--yes) we keep the old "install but don't
	// spawn" default unless --start-now is set, so CI scripts that
	// rebuild a tempdir with `bridge init --yes` don't leave an orphan
	// server behind.
	startNow := fs.Bool("start-now", false, "Windows only: after install, spawn `bridge serve` detached so the admin console is reachable without a logout")
	// PR 5: public-VPS deployment posture flags.
	publicMode := fs.Bool("public", false, "configure as a public-VPS deployment (admin auth, no mDNS, no Tailscale by default)")
	publicDomain := fs.String("domain", "", "public hostname iOS clients dial (required with --public)")
	publicEmail := fs.String("email", "", "ACME contact email for Let's Encrypt (required with --public, unless --admin-tls-proxy)")
	// The two address flags apply in either posture (initAddresses).
	adminAddressFlag := fs.String("admin-address", "", "bind address for the admin console (default 127.0.0.1:7789; "+
		"with --public 0.0.0.0:7789, or 127.0.0.1:7789 with --admin-tls-proxy). Without --public it must be a "+
		"loopback address: that console has no login")
	listenAddressFlag := fs.String("listen-address", "", "bind address for the iOS-facing API "+
		"(default :7788; with --public :443, which ACME needs)")
	publicProxy := fs.Bool("admin-tls-proxy", false, "with --public: a reverse proxy (Caddy/nginx) fronts admin TLS — disables native ACME wrapping of the admin listener")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// A name the app's pairing parser would refuse is refused here, before
	// anything is written, as any other bad flag is: one that is not UTF-8
	// (`--name $'Caf\xe9'` from a Latin-1 terminal was saved as `!!binary`
	// and every QR's name=Caf%E9 was refused as a missing field) or one over
	// config.MaxLibraryNameLength. A blank one is no name, as below.
	if name := config.TrimLibraryName(*libraryName); name != "" {
		if err := config.CheckLibraryName(name); err != nil {
			fmt.Fprintf(stderr, "--name %v\n", err)
			return 2
		}
	}
	if *publicMode {
		if *publicDomain == "" {
			fmt.Fprintf(stderr, "--public requires --domain <fqdn>\n")
			return 2
		}
		// The domain becomes the endpoint every phone dials,
		// `https://<domain>`, in customEndpoints and in the autocert host,
		// and /v1/health (answering any caller) and every pairing QR publish
		// both. So a user name, password, query or fragment in it is
		// refused here, before anything is written, and not echoed (backlog
		// B54): the operator typed it, and a stored config that already
		// holds such an endpoint is published without them instead
		// (config.ValidateCustomEndpoints). So is a value that does not
		// parse as a URL's host at all: a password with a space in it is
		// no less a password, the prune drops such an endpoint without
		// reading it, and public mode builds the autocert host's URL from
		// the string itself (api's publicModeEndpoints). Trimmed first, as
		// Normalize trims the autocert host.
		typed := "https://" + strings.TrimSpace(*publicDomain)
		if _, err := url.Parse(typed); err != nil || config.HasCredentialParts(typed) {
			fmt.Fprintf(stderr, "--domain: must be the host name alone, with no user name, password, query or fragment "+
				"(/v1/health, which answers any caller, and every pairing QR publish the endpoint init writes from it)\n")
			return 2
		}
		if *publicEmail == "" && !*publicProxy {
			// Email is required ONLY when the bridge will run
			// autocert itself; reverse-proxy installs let the
			// proxy own ACME entirely.
			fmt.Fprintf(stderr, "--public requires --email <addr> (used for Let's Encrypt account registration)\n")
			return 2
		}
	}
	// An address the config's own check refuses is refused here, before the
	// preflight, which grades the port an address names and has no port to
	// grade for one that does not parse. Until 2026-09-28 it was refused only
	// at the validation before Save, after a preflight that had graded 7788 in
	// its place, and on a loopback run, which read neither flag, not at all.
	if err := initAddressFlagsError(*publicMode, *listenAddressFlag, *adminAddressFlag); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 2
	}
	// The addresses this run writes, which the preflight grades wherever no
	// install's config names its own, and on a --yes --force rewrite where
	// they replace its own (initAddresses).
	listenAddr, adminAddr := initAddresses(*publicMode, *publicProxy, *listenAddressFlag, *adminAddressFlag)

	cfgDir := *cfgDirFlag
	if cfgDir == "" {
		d, err := packaging.DefaultConfigDir()
		if err != nil {
			fmt.Fprintf(stderr, "resolve config dir: %v\n", err)
			return 1
		}
		cfgDir = d
	}
	// Always canonicalize to an absolute path — relative or ~-prefixed
	// inputs land verbatim in config.DataDir and the service templates,
	// where launchd / systemd have no cwd / no shell expansion, so the
	// service fails at start with a silent "no such file" from the
	// daemon's log. Resolving here keeps the rest of init trust-but-
	// verify-free.
	if absDir, err := filepath.Abs(expandHome(cfgDir)); err == nil {
		cfgDir = absDir
	} else {
		fmt.Fprintf(stderr, "resolve --dir path: %v\n", err)
		return 1
	}
	cfgPath := filepath.Join(cfgDir, "bridge.yaml")
	// The install already at cfgPath, as its file says (priorInstallFile). A
	// rewrite keeps its data dir, which the header, the preflight, the
	// credential store, the TLS pair and the service all use from here on.
	// With no config, or one init cannot read, the data dir is init's own.
	// The read is kept: a config that is no longer as read by the time this
	// run would write is refused (configChangedSinceRead).
	asRead := readConfigAsIs(cfgPath)
	prior, priorErr := priorInstallFrom(cfgPath, asRead)
	initDataDir := initDataDirFor(cfgDir)
	dataDir := initDataDir
	if prior != nil {
		dataDir = prior.DataDir
	}

	fmt.Fprintf(stdout, "1-bit-bridge — first-time setup\n\n")
	fmt.Fprintf(stdout, "  Config dir:  %s\n", cfgDir)
	fmt.Fprintf(stdout, "  Data dir:    %s\n", dataDir)
	fmt.Fprintf(stdout, "  Config file: %s\n\n", cfgPath)

	in := bufio.NewReader(stdin)

	// Collect the library root.
	//
	// Public mode (PR 5): library root is OPTIONAL. The
	// realistic VPS flow is `bridge init --public ...` first,
	// then the operator mounts their rclone B2 FUSE volume and
	// adds the root via the admin console once it's up. Forcing
	// the operator to pre-mount before init creates a chicken-
	// and-egg: rclone systemd units commonly use `After=
	// 1-bit-bridge.service` to read the bridge's config for
	// mount points. Either order should work without manual
	// gymnastics.
	// abs is the resolved library root, or empty for public-mode
	// installs that defer mount setup. The preflight still RUNS
	// without one — checkLibraryRoots answers "none configured
	// (init will prompt)" for an empty list, and every other check
	// is about the host and the install, not the library. It was
	// gated on `abs != ""` until the cert checks were wired in
	// (#951): a public-mode re-init — the exact flow that carries a
	// data directory to a new host — then printed nothing at all
	// about a certificate whose SANs no longer cover the endpoints
	// it advertises, which is the state those checks exist to catch
	// BEFORE devices pin it.
	var abs string
	switch {
	case *libraryRoot != "":
		// Value supplied via --library (interactive or --yes): validate
		// once and hard-error on a bad path (exit 1). Automation must pass
		// a real directory — re-prompting wouldn't help a non-interactive
		// caller.
		a, verr := resolveLibraryDir(*libraryRoot)
		if verr != nil {
			fmt.Fprintf(stderr, "%v\n", verr)
			return 1
		}
		abs = a
	case *publicMode:
		// Public-mode installs defer mount setup — no library root now.
	case *nonInteractive:
		fmt.Fprintf(stderr, "--yes requires --library <path>\n")
		return 2
	default:
		// Interactive: prompt with a bounded re-prompt loop so a single
		// paste typo doesn't abort the whole init (the operator retypes).
		a, code := promptLibraryDir(in, stdout, stderr)
		if code != 0 {
			return code
		}
		abs = a
	}
	// The roots this run saves. A run that names no library, which only a
	// public run may, keeps the install's: a public install takes its roots
	// later, in the console, and a rewrite that emptied them left every track
	// unplayable. The preflight grades only a root the run names, as it
	// always has. A kept public root may be a mount that is not up yet, which
	// public-mode serve tolerates, and the note above says why init must not
	// demand it: checkLibraryRoots FAILs a missing root, so grading the kept
	// ones refused a public rewrite whenever its mount was down.
	var roots, namedRoots []string
	rootsKept := false
	switch {
	case abs != "":
		roots = []string{abs}
		namedRoots = roots
	case prior != nil && len(prior.LibraryRoots) > 0:
		roots, rootsKept = prior.LibraryRoots, true
	}

	// Whether this run replaces the config at cfgPath, decided before the
	// preflight so the preflight knows what it grades. Non-interactive
	// (`--yes`) is the automation path, so it does not clobber an existing
	// config unless --force says it may: a CI job or packaging script that
	// reruns `bridge init --yes` would otherwise wipe an already-tuned
	// installation. An interactive run asks. A no keeps the config, common when
	// the operator is re-running init to reinstall the service against an
	// already-tuned one.
	//
	// The question came after the preflight and the name prompt until
	// 2026-09-29 (backlog B61). So an interactive run's preflight graded the
	// install's ports for a run that might go on to keep them, a stranger on a
	// port a yes would move off refused the run before it could ask, where
	// the same flags with --yes --force went through, and a no discarded the
	// name the operator had just typed. It comes after the library prompt,
	// which ends the run on a closed stdin rather than taking a default for a
	// question nobody answered.
	_, statErr := os.Stat(cfgPath)
	exists := statErr == nil
	replace := false
	if exists {
		if *nonInteractive {
			replace = *force
		} else {
			replace = confirm(in, stdout, "Config file exists. Overwrite?", false)
		}
	}
	keep := exists && !replace

	// A posture flag the run would not write (warnIgnoredPostureFlags). A
	// rewrite of a public install given one is refused here, before anything
	// is graded or written.
	if code := warnIgnoredPostureFlags(stderr, postureFlags{
		public: *publicMode, domain: *publicDomain, email: *publicEmail, proxy: *publicProxy,
	}, exists, replace, cfgPath, prior); code != 0 {
		return code
	}

	// Preflight. Run after library-path resolution so doctor sees the
	// real path the user chose, not a default. --skip-doctor bypasses
	// for the rare case where the operator knows better than the check.
	//
	// It used to bypass something else as well: an admin port bound by
	// the operator's OWN running bridge graded FAIL, because the Deps
	// built here carried no OwnPIDFile and checkPort's "is it us?"
	// ladder needs one. Re-running init against a live install — the
	// most ordinary reason to run it twice — therefore aborted, and this
	// comment named that as a reason to pass the flag. withExistingInstallDeps
	// reads the pid file and the real ports now, so the check answers. It
	// answers over a config that does not load too, from the pid file in
	// the data dir init writes, excusing only a port that bridge is seen
	// listening on.
	//
	// The ports graded are the install's where its config loads, and
	// otherwise the ones this run writes (initAddresses): on a first
	// install, or over a config that does not load, nothing says which
	// ports an install binds, and this run's are the ones its bridge will.
	// They were init's defaults there, 7788 and 7789, until 2026-09-28,
	// which a public run does not write: another process on 7788 (a second
	// bridge beside the operator's, say) refused a public first install over
	// a port it would never bind. A refusal on those ports says whose they
	// are, since it is not a verdict about an install (portsThisInitWrites).
	//
	// A rewrite of an install whose config loads, --yes --force or an
	// interactive yes, is certain by now, and the preflight grades only the
	// install's ports that rewrite keeps. One it moves off is left ungraded
	// (portsARewriteAbandons, doctor.Deps.AbandonedPorts): who holds a port
	// the new config does not name says nothing about whether the bridge can
	// start. Until 2026-09-28 it was graded, and a stranger on the old port,
	// the install's bridge stopped, refused a rewrite moving off it; an
	// interactive rewrite was refused so until 2026-09-29, when its
	// "Overwrite?" came after the preflight. The second pass below grades the
	// ports that rewrite writes in their place. A run that keeps the config
	// grades them all: they are the ports its bridge binds. #963 is unchanged
	// for everything else, the certificate and the data dir, which a rewrite
	// keeps.
	//
	// preflightDeps is kept for the SECOND port pass below: where the
	// config loads, the ports graded here are the install's CURRENT ones,
	// and a run that goes on to overwrite the config may be about to save
	// different ones. Where none loads they are this run's already.
	var preflightDeps doctor.Deps
	if !*skipDoctor {
		// Both addresses parse: the defaults do, and a flag's value was
		// refused above unless it passes the config's check.
		apiPort, _ := configuredPort(listenAddr)
		adminPort, _ := configuredPort(adminAddr)
		d := doctor.Deps{
			ConfigDir:    cfgDir,
			DataDir:      dataDir,
			LibraryRoots: namedRoots,
			APIPort:      apiPort,
			AdminPort:    adminPort,
		}
		installPorts := withExistingInstallDeps(&d, cfgPath)
		rewriting := installPorts && replace
		if rewriting {
			d.AbandonedPorts = portsARewriteAbandons(d.APIPort, d.AdminPort, apiPort, adminPort)
		}
		preflightDeps = d
		if report, code := ensureDoctorClean(stdout, d); code != 0 {
			// Every port graded is one this run writes, unless a config that
			// loads is being kept.
			if (!installPorts || rewriting) && report.PortFailed() {
				fmt.Fprintln(stdout)
				fmt.Fprintln(stdout, portsThisInitWrites())
			}
			fmt.Fprintln(stdout)
			fmt.Fprintln(stdout, "fix the fail(s) above, or re-run with --skip-doctor to bypass.")
			return 1
		}
	}

	// The library name this run saves. --name names it. A run that names
	// none keeps the name the install's config gives (init_rewrite.go), and a
	// first install, or one whose config gives none, takes the host's.
	// Interactively the prompt offers that name as its default, which Enter
	// takes: it offered the host's name over an install with one of its own
	// until 2026-09-27, so an operator who pressed Enter through a rewrite
	// replaced the name.
	//
	// Every name is trimmed as the app's pairing parser trims it
	// (config.TrimLibraryName), and one that is blank once trimmed is no
	// name: --name "  " is --name "", as an answer of spaces at the prompt
	// is an empty one. The flag was saved as given until 2026-09-27, and
	// Load served it so, padding and all, in /v1/health and in every
	// pairing QR, which the app refuses when the name is empty or padded.
	//
	// The install's name is kept as Load serves it (RepairLibraryName): a
	// config can hold one over the cap or one that is not UTF-8, which Load
	// repairs, and keeping the file's would offer at the prompt a name the
	// prompt then refuses when Enter takes it.
	//
	// A run that keeps the config asks for no name: it would discard it.
	firstName := firstInstallName()
	defaultName := firstName
	if prior != nil {
		if kept := config.RepairLibraryName(prior.LibraryName); kept != "" {
			defaultName = kept
		}
	}
	name := config.TrimLibraryName(*libraryName)
	if name == "" && !*nonInteractive && !keep {
		var code int
		if name, code = askLibraryName(in, stdout, stderr, defaultName); code != 0 {
			return code
		}
	}
	nameKept := false
	if name == "" {
		name = defaultName
		// printKept lists a kept name a first install would not have taken.
		// An interactive run showed it at the prompt.
		nameKept = *nonInteractive && name != firstName
	}

	// A run that keeps the config (the decision above) installs the service
	// against it and writes nothing.
	if keep {
		if *nonInteractive {
			fmt.Fprintf(stdout, "config file already exists at %s; keeping it\n", cfgPath)
			fmt.Fprintf(stdout, "pass --force to overwrite non-interactively\n")
		} else {
			fmt.Fprintf(stdout, "keeping existing config\n")
		}
		keepChoice := resolveLaunchChoice(in, stdout, *nonInteractive, *skipService, *windowsService, *startNow)
		return finishInit(in, *nonInteractive, stdout, stderr, cfgPath, dataDir, keepChoice)
	}

	// The config must still be the one this run read and decided about: the
	// preflight and the name prompt ran since, and another process can have
	// written it meanwhile (configChangedSinceRead).
	if configChangedSinceRead(stderr, cfgPath, asRead) {
		return 1
	}

	// Whether this run may overwrite the config at all, before anything is
	// written, the directories included: refuseRewrite.
	if refuseRewrite(stderr, cfgPath, prior, priorErr) {
		return 1
	}

	// 0o700 because both dirs hold private material: bridge.yaml
	// (TLS fingerprint, library paths), data/cert.key (TLS private
	// key), data/tokens.json (bearer-token hashes), data/bridge.db.
	// On POSIX this prevents cross-user reads on shared hosts; on
	// Windows the Go file mode is advisory only — protection there
	// relies on per-user-profile NTFS ACLs at %LOCALAPPDATA%, which
	// already block other standard users.
	//
	// MkdirAll preserves existing-dir mode, so re-runs over a 0o755
	// install would otherwise leave the broader perms in place. The
	// follow-up Chmod is what actually hardens upgrades. Chmod errors
	// are non-fatal: if perms can't be tightened (e.g. the dir is on
	// a filesystem that ignores POSIX modes, or Windows ACLs differ)
	// we still want init to succeed — surface a warning.
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "mkdir config dir: %v\n", err)
		return 1
	}
	if err := os.Chmod(cfgDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "warning: chmod config dir: %v\n", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "mkdir data dir: %v\n", err)
		return 1
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "warning: chmod data dir: %v\n", err)
	}

	cfg := baseConfig(roots, name, dataDir)
	// The addresses the preflight graded, where no install's config named
	// its own: one definition for both (initAddresses).
	cfg.ListenAddress, cfg.AdminAddress = listenAddr, adminAddr
	if *publicMode {
		// Public-mode YAML shape (PR 5). Defaults:
		//   listenAddress / adminAddress: initAddresses
		//   tailscale.mode: disabled  (applyDefaults sets this; explicit
		//                              for readability of saved YAML)
		//   mdns.enabled:   false     (no LAN to advertise on)
		//   customEndpoints: [the autocert domain]
		//   autocert: { enabled, domain, email } OR
		//             { domain } + AdminTLSTerminatedByProxy when --admin-tls-proxy
		cfg.Deployment.Mode = "public"
		cfg.Autocert.Domain = *publicDomain
		// customEndpoint: append the listen port unless it's
		// :443 (port 443 is the https default — iOS dials
		// "https://host" same as "https://host:443"). Keeps
		// the saved YAML compact for the standard ACME case.
		domainURL := "https://" + *publicDomain
		_, port, splitErr := net.SplitHostPort(cfg.ListenAddress)
		if splitErr == nil && port != "" && port != "443" {
			domainURL = "https://" + *publicDomain + ":" + port
		}
		cfg.CustomEndpoints = []string{domainURL}
		cfg.Tailscale.Mode = string(config.TailscaleModeDisabled)
		falseVal := false
		cfg.MDNS.Enabled = &falseVal
		if *publicProxy {
			cfg.Deployment.AdminTLSTerminatedByProxy = true
		} else {
			cfg.Autocert.Enabled = true
			cfg.Autocert.Email = *publicEmail
		}
	}
	// The TLS pair the install serves, and a loopback install's custom
	// endpoints. The data dir and the roots are in cfg already.
	endpointsKept := keepFromPrior(cfg, prior)
	if err := cfg.NormalizeAndValidate(); err != nil {
		fmt.Fprintf(stderr, "validate: %v\n", err)
		return 1
	}
	// The ports this run is about to SAVE, where the preflight graded
	// others: it ran before the keep-or-overwrite decision, against the
	// config already on disk when that config loads. For the certificate
	// that reading is right and deliberate — init does not rewrite the
	// cert, so the pair on disk IS the pair. The ports are the opposite:
	// a rewrite writes this run's (initAddresses), so an install on
	// :9090/:9091 was graded on 9090/9091 and then handed
	// :7788/127.0.0.1:7789 — and if something else holds 7789, the
	// operator learns it from a `bridge serve` that cannot bind, having
	// just been told the host was fine. Where no config loads, the
	// preflight graded this run's ports already, and nothing here differs.
	// On a --yes --force rewrite the preflight left the install's ports
	// this run moves off ungraded (doctor.Deps.AbandonedPorts), and this
	// pass grades what it writes in their place: d carries that list, and
	// it never names a port the run writes (portsARewriteAbandons).
	//
	// Before Save, so a refusal leaves the existing config intact, and
	// only over the ports that actually CHANGED: an unchanged one was
	// already graded by the preflight, correctly and with the pid file.
	//
	// And the pid file is CLEARED for the ones that did change.
	// checkPort's "is it us?" ladder answers ok or warn — never fail —
	// whenever our own recorded pid is alive and the owner probe could
	// not rule it out, which is right for a port the running bridge is
	// supposed to hold and wrong for one it is not: a live bridge binds
	// what ITS config says, so it cannot legitimately own a port that is
	// not in it. Left set, an occupied
	// new port on a host that cannot attribute it (a capability-bound
	// binary, a blocked probe) read as "our bridge is still running",
	// HasFail stayed false, and the config was saved anyway — the check
	// passing because the thing it guards is absent, one level in from
	// the defect this whole pass exists for (CodeRabbit on #970).
	//
	// Except where the install's config did not load
	// (OwnPIDPortsUnknown), where the pid file is kept for any port this
	// pass grades. No config says which ports that bridge binds, so a port
	// this run chooses may well be its own, as on a public re-init over a
	// broken public config, and cleared, the pid file would refuse the
	// bridge's own listeners. Kept, it excuses nothing but the recorded
	// bridge seen listening on the port, the one arm of the ladder a port
	// the run is choosing may be excused by. The preflight grades that
	// mode's ports from the one definition this config is built from, so
	// no port reaches this pass there today; until 2026-09-28 it graded
	// init's defaults, and a public re-init's own ports were graded here.
	if !*skipDoctor {
		d := preflightDeps
		apiPort, apiOK := configuredPort(cfg.ListenAddress)
		adminPort, adminOK := configuredPort(cfg.AdminAddress)
		apiChanged := apiOK && apiPort != d.APIPort
		adminChanged := adminOK && adminPort != d.AdminPort
		if apiChanged || adminChanged {
			d.APIPort, d.AdminPort = apiPort, adminPort
			if !d.OwnPIDPortsUnknown {
				d.OwnPIDFile = ""
			}
			report := doctor.RunPortChecks(context.Background(), d, apiChanged, adminChanged)
			if report.HasFail() {
				printReport(stdout, report)
				fmt.Fprintln(stdout)
				fmt.Fprintln(stdout, "these are the ports this init would write; the config was NOT changed.")
				fmt.Fprintln(stdout, "free them, pick others, or re-run with --skip-doctor to bypass.")
				return 1
			}
			printWarnings(stdout, report)
		}
	}
	// Every refusal is decided BEFORE Save, so a refusal leaves the install
	// as the run found it. Two of them are about files already in the data
	// dir, which an install keeps across a re-init: a public install's admin
	// credentials and every install's TLS pair. Both were asked AFTER Save,
	// so a store or a pair that could not be kept had the config rewritten
	// and then exited 1 about it, and a public re-init over an install with
	// an admin account exited 1 there on every run (row C of #1027's log
	// entry). What can still fail after Save is the credential mint's own
	// write and the service install, and neither is a verdict about the
	// install that is there.
	var adminAuth *adminauth.Store
	if *publicMode {
		store, ok := openInitAdminAuth(filepath.Join(dataDir, adminauth.FileName), stderr)
		if !ok {
			return 1
		}
		adminAuth = store
	}

	// Load the TLS pair, or on a first install mint it, so the fingerprint
	// is stable from the first serve onwards. A pair that is there is kept
	// as it is (LoadOrGenerate never rewrites one): rotating it breaks every
	// paired client's pin. One that is there and does not load, a cert with
	// no key beside it say, is a refusal. The preflight's tls-cert check
	// FAILs it as well, but only when the preflight runs.
	//
	// The pair is the one the saved config names, resolved as `bridge serve`
	// resolves it (resolveCertPaths), so the fingerprint printed below is the
	// one serve presents. It was DefaultPaths(dataDir) until 2026-09-27:
	// over a config naming its pair elsewhere, init minted a second pair in
	// the data dir and printed that one to pin.
	//
	// A first install's mint therefore lands before its config does. A Save
	// that then fails leaves a pair nothing has pinned, which the next run
	// loads.
	//
	// The mint picks up the broader SAN set so the cert covers every URL the
	// bridge will advertise from the very first serve. A re-init against an
	// existing cert emits the SAN-stale warning if the operator's
	// CustomEndpoints changed, which `bridge doctor`'s tls-cert-sans check
	// reports from the same gather.
	certPath, keyPath := resolveCertPaths(cfg)
	_, fp, err := servertls.LoadOrGenerateWithOptions(certPath, keyPath, certSANOptions(cfg))
	if err != nil {
		fmt.Fprintf(stderr, "TLS cert: %v\n", err)
		fmt.Fprintln(stderr, configNotChanged)
		return 1
	}

	if err := cfg.Save(cfgPath); err != nil {
		fmt.Fprintf(stderr, "save config: %v\n", err)
		return 1
	}
	printKept(stdout, cfg, initDataDir, rootsKept, nameKept, endpointsKept)
	if endpointsKept {
		warnKeptEndpointsOnAMovedPort(stderr, prior.ListenAddress, cfg.ListenAddress, cfg.CustomEndpoints)
	}

	if !*publicMode {
		// Box the fingerprint so it stands out from the surrounding
		// init narration. Operators have to copy this exact string
		// to the iOS side at pairing time; framing it makes the
		// "this is the bit you need" beat unmissable.
		//
		// NEVER truncate the fingerprint — a SHA-256 colon-separated
		// hex is 95 chars and operators copy it byte-for-byte to the
		// iOS pin. splitFingerprint splits on a colon boundary so
		// concatenating the halves yields the original verbatim.
		//
		// Public mode skips this entirely: iOS clients on the public
		// path validate the publicly-trusted LE cert via standard
		// ATS rather than pinning, so the fingerprint isn't load-
		// bearing for the pairing flow.
		first, second := splitFingerprint(fp)
		fmt.Fprint(stdout, "\n")
		fmt.Fprint(stdout, box("TLS fingerprint", []string{
			"Pin this on the iOS side. Stable across restarts:",
			"",
			"  " + first,
			"  " + second,
		}))
	}

	if *publicMode {
		if code := keepOrMintAdminCredentials(adminAuth, stdout, stderr); code != 0 {
			return code
		}
	}

	choice := resolveLaunchChoice(in, stdout, *nonInteractive, *skipService, *windowsService, *startNow)
	return finishInit(in, *nonInteractive, stdout, stderr, cfgPath, dataDir, choice)
}

// openInitAdminAuth reads a public install's admin credential store for
// `bridge init`, which must happen before the run writes anything.
//
// A store that is there and does not load is a refusal. Minting over it
// would destroy whatever the file still holds, and it cannot be kept, so
// init stops, as `bridge serve` does on the same file. A store this user
// cannot read is the same refusal with a different remedy: the bridge's
// own user can read it.
func openInitAdminAuth(storePath string, stderr io.Writer) (*adminauth.Store, bool) {
	store, err := adminauth.OpenStore(storePath)
	if err == nil {
		return store, true
	}
	fmt.Fprintf(stderr, "adminauth: %v\n", err)
	if errors.Is(err, os.ErrPermission) {
		fmt.Fprintf(stderr, "this user cannot read the admin credentials at %s, which init keeps.\n", storePath)
		fmt.Fprintln(stderr, "run init as the user the bridge runs as.")
	} else {
		fmt.Fprintf(stderr, "the admin credentials at %s do not load, and init keeps an install's credentials rather than replacing them.\n", storePath)
		fmt.Fprintln(stderr, "restore that file from a backup, or move it aside and run this init again to mint new ones.")
	}
	fmt.Fprintln(stderr, configNotChanged)
	return nil, false
}

// keepOrMintAdminCredentials gives a public install its admin account, or
// keeps the one it has. The store was read before Save and nothing has
// written it since.
//
// Re-running init is not a request to rotate the password, any more than it
// is one to rotate the TLS cert, which init also keeps: `bridge admin
// reset-password` is the command for that. So an account that is there is
// kept, and the run says so and names it. Silence would leave an operator
// who expects the "shown ONCE" box looking for a password that was never
// made. MintInitial refuses a store that holds an account, and until
// 2026-09-27 that refusal ended every public re-init with exit 1, after the
// config had been rewritten.
//
// What is kept is an ACCOUNT, not a file: a store with none in it, no file
// or an empty one, is minted into, as on a first install.
func keepOrMintAdminCredentials(store *adminauth.Store, stdout, stderr io.Writer) int {
	if store.IsInitialised() {
		fmt.Fprint(stdout, "\n")
		fmt.Fprint(stdout, keptAdminCredentialsBox(store.Username()))
		return 0
	}
	// The mint's only failures are its own writes (a full disk, say), with
	// the config already saved. The same init run again finds the store
	// still empty and mints into it.
	plaintext, err := store.MintInitial("admin")
	if err != nil {
		fmt.Fprintf(stderr, "adminauth: %v\n", err)
		return 1
	}
	// The adminauth.Store persists only the bcrypt hash, so the plaintext
	// is reachable nowhere else once this box scrolls off-screen.
	fmt.Fprint(stdout, "\n")
	fmt.Fprint(stdout, mintedAdminCredentialsBox(store.Username(), plaintext))
	return 0
}

// mintedAdminCredentialsBox is the box a public init prints the password it
// minted in, the only time that password is shown.
func mintedAdminCredentialsBox(username, plaintext string) string {
	return box("Admin credentials — shown ONCE", []string{
		"Save these now. The plaintext is stored nowhere.",
		"",
		"  Username:  " + username,
		"  Password:  " + plaintext,
		"",
		"Rotate with:  bridge admin reset-password",
	})
}

// keptAdminCredentialsBox is the box a public re-init prints in place of
// minted credentials, when the install already has an account.
func keptAdminCredentialsBox(username string) string {
	return box("Admin credentials — kept", []string{
		"This install already has an admin account, and",
		"re-running init leaves it as it is.",
		"",
		"  Username:  " + username,
		"",
		"Rotate with:  bridge admin reset-password",
	})
}

// launchChoice bundles the three orthogonal knobs that control how
// init leaves the bridge behind: whether to skip the
// launchd/systemd/Startup install entirely (manual mode), whether to
// install the Windows Service via SCM instead of the Startup-folder
// .cmd, and whether to spawn `bridge serve` detached right now so the
// admin console is reachable without a logout. Only the last one is
// new on Windows — macOS launchd / Linux systemd already start the
// unit during install.
type launchChoice struct {
	skipService bool
	useService  bool
	spawnNow    bool
}

// resolveLaunchChoice merges flag-driven and prompt-driven input into a
// single choice. On Windows, when the user is interactive and didn't
// pre-pick via a flag, we show a 3-option picker so they don't end up
// with the old "init exits, server isn't running, admin URL 404s"
// experience. Non-interactive runs (--yes) keep today's flag semantics
// so scripts that call `bridge init --yes --no-service` don't get a
// surprise detached server.
func resolveLaunchChoice(in *bufio.Reader, stdout io.Writer, nonInteractive, skipService, useService, startNow bool) launchChoice {
	flagSet := skipService || useService
	if runtime.GOOS == "windows" && !nonInteractive && !flagSet {
		return promptLaunchMode(in, stdout)
	}
	return launchChoice{
		skipService: skipService,
		useService:  useService,
		spawnNow:    startNow,
	}
}

// promptLaunchMode is the Windows-only 3-option picker. Mode 1 (the
// recommended default) installs the Startup-folder launcher AND spawns
// the server right now so init doesn't leave the admin console dark.
// Mode 2 installs the SCM service (which starts itself). Mode 3 is
// "I'll run `bridge serve` myself" — used by operators who want zero
// residue.
func promptLaunchMode(in *bufio.Reader, stdout io.Writer) launchChoice {
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "How should the bridge start up on this machine?")
	fmt.Fprintln(stdout, "  [1] Launch when I log in  (recommended — Startup-folder launcher)")
	fmt.Fprintln(stdout, "  [2] Always-on Windows Service  (requires admin; survives logout)")
	fmt.Fprintln(stdout, "  [3] Only when I start it manually  (I'll run `bridge serve` myself)")
	for {
		// `ask` itself appends `[def]:` to the prompt, so the prompt
		// must NOT already contain `[1]` — otherwise the rendered
		// line is `Choose [1] [1]:` (the original transcript bug).
		choice := ask(in, stdout, "Choose", "1")
		switch strings.TrimSpace(choice) {
		case "1", "":
			return launchChoice{spawnNow: true}
		case "2":
			return launchChoice{useService: true}
		case "3":
			return launchChoice{skipService: true}
		}
		fmt.Fprintln(stdout, "  (enter 1, 2, or 3)")
	}
}

// finishInit installs the service (or tells the user how to run manually)
// and prints the admin console URL. Separated from the main init path so
// the "keep existing config" branch can reach it without rebuilding the
// Config struct.
//
// The admin URL + browser open always runs at the end, even on the
// "skip service" and "unsupported OS" paths — a successful init is
// useless to the operator if they don't know where to point their
// browser. The browser-open is best-effort (no stderr on headless
// machines), so the cost of always attempting it is zero.
func finishInit(in *bufio.Reader, nonInteractive bool, stdout, stderr io.Writer, cfgPath, dataDir string, choice launchChoice) int {
	// Load the config once up-front so the admin-address probe (Windows
	// auto-start path) and the browser-open at the end both use the
	// operator-configured bind address, not the hard-coded default.
	// `spawnNowOrWarn` previously probed `config.DefaultAdminAddress`
	// directly — on a non-default admin_address, the "already running"
	// check always missed, the second process tried to bind the
	// configured port, and the log filled with port-bind errors.
	adminAddr := config.DefaultAdminAddress
	var loadedCfg *config.Config
	if cfg, err := config.Load(cfgPath); err == nil {
		loadedCfg = cfg
		if cfg.AdminAddress != "" {
			adminAddr = cfg.AdminAddress
		}
	}

	// `printAdmin` is called once per mode with per-mode copy. On
	// Windows we also pass whether the server is expected to be live
	// right now — if it is, we poll briefly before opening the browser
	// so the user doesn't hit "site can't be reached" on a fast machine
	// that outran the cmd.exe handoff.
	//
	// Two URLs in play (kept distinct on purpose):
	//   - browseURL: what the operator types into a browser. Derived
	//     from `operatorAdminURL` so a public-mode install prints
	//     `https://<domain>[:port]/` (autocert direct-TLS) or
	//     `https://<domain>/` (reverse-proxy), NOT the literal bind
	//     target like `http://0.0.0.0:7789/` which is dial-broken
	//     from any other host. Loopback installs keep the historical
	//     `http://<adminAddress>/` shape.
	//   - probeURL semantics live on `adminAddr` directly: the
	//     listen-port probe before browser-open uses the bind
	//     target because that's where the local listener actually
	//     binds (proxy/autocert layer wraps it on top). Auto-
	//     opening a browser on the server's local desktop only
	//     fires when the bridge is starting in this process tree
	//     (Windows skip-service "open in cmd.exe" path); on a
	//     headless VPS this branch never runs.
	//
	// CodeRabbit/Gemini followup post-PR-#296.
	printAdmin := func(serverIsLive bool) {
		// Loopback installs default to http; the helper picks
		// https for public modes regardless of the override slot.
		browseURL := operatorAdminURL(loadedCfg, "http")
		if serverIsLive {
			if host, port, ok := splitHostPort(adminAddr); ok {
				_ = packaging.WaitForListen(host, port, 2*time.Second)
			}
		}
		fmt.Fprintf(stdout, "\nAdmin console: %s\n", browseURL)
		if serverIsLive {
			openInBrowser(browseURL)
		}
		fmt.Fprintf(stdout, "\nDone. Open the admin console to add library folders and pair iOS devices.\n")
	}

	// Resolve the running-binary path once up-front. Used by every
	// downstream branch (skipService handoff, service-install fallback,
	// future-launch hint). os.Executable can fail in unusual environments
	// (deleted binary mid-run, embedded test) — fall back to argv[0] so
	// later prints still surface a useful command. EvalSymlinks resolves
	// /usr/local/bin/bridge → /opt/homebrew/Cellar/... so the printed
	// command points at the real binary, not the launcher symlink.
	binary, err := os.Executable()
	if err != nil || binary == "" {
		binary = os.Args[0]
	}
	if resolved, lerr := filepath.EvalSymlinks(binary); lerr == nil {
		binary = resolved
	}

	if choice.skipService || (runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows") {
		// Interactive operators get a "Start it now in this terminal?"
		// prompt so they don't have to copy-paste a path-laden command
		// (the original PowerShell footgun: PS doesn't search CWD,
		// `bridge serve` returns CommandNotFound). Non-interactive
		// (--yes / piped stdin) gets the shell-aware handoff text
		// only — no prompt, preserves automation behavior.
		//
		// Stdin MUST be a real TTY before we prompt. confirm() falls
		// back to its default on a read error, so a piped/closed
		// stdin (e.g. `bridge init < /dev/null`) would otherwise
		// silently auto-start the server with no real consent —
		// flagged on PR review.
		fmt.Fprintln(stdout)
		if !nonInteractive && stdinIsTerminal(in) && confirm(in, stdout, "Start the bridge now in this terminal?", true) {
			// Per-invocation signal scope: Ctrl+C cancels just this
			// serve session, returns control to init's caller. We
			// derive from context.Background() because init wasn't
			// passed a parent ctx; future PRs that thread one in can
			// wire it here without breaking this contract.
			serveCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			fmt.Fprint(stdout, paint(cBrightCyan, "\nStarting the bridge — Ctrl+C to stop.\n\n"))
			return runServe(serveCtx, serveOpts{configPath: cfgPath}, stdout, stderr)
		}
		fmt.Fprint(stdout, shellHandoff(binary, cfgPath))
		printAdmin(false)
		return 0
	}

	// --service on non-Windows is a usage error; the flag only makes
	// sense with SCM. Call it out loudly so the user doesn't think
	// they got a Windows-Service-equivalent on their Mac.
	if choice.useService && runtime.GOOS != "windows" {
		fmt.Fprintf(stderr, "--service is a Windows-only flag; ignored on %s\n", runtime.GOOS)
		choice.useService = false
	}

	logPath, err := packaging.DefaultLogPath()
	if err != nil {
		fmt.Fprintf(stderr, "log path: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		fmt.Fprintf(stderr, "mkdir log dir: %v\n", err)
		return 1
	}
	params := packaging.Params{
		BinaryPath: binary,
		ConfigPath: cfgPath,
		WorkingDir: dataDir,
		LogPath:    logPath,
	}
	// serverIsLive tracks whether the final printAdmin should open a
	// browser: true if SCM started the service or we just spawned it
	// detached; false if only the Startup-folder launcher landed and it
	// won't fire until next logon (which today can't happen — spawnNow
	// is the default — but is the compatibility path for
	// `--yes` callers).
	var serverIsLive bool
	var unitPath string
	if choice.useService {
		// SCM install. Requires admin; fails otherwise with a clear
		// "access denied" from the SCM layer.
		unitPath, err = packaging.InstallWindowsService(params)
		if err != nil {
			fmt.Fprintf(stderr, "Windows Service install: %v\n", err)
			fmt.Fprintf(stderr, "If the error mentions \"access\" or \"denied\", re-run init from an elevated PowerShell.\n")
			fmt.Fprintf(stderr, "Otherwise fall back to the Startup-folder install: bridge init (no --service flag).\n")
			return 1
		}
		fmt.Fprintf(stdout, "Windows Service installed: %s\n", unitPath)
		serverIsLive = true // SCM's Start() fired during Install.
	} else {
		// We're in the non-service branch — `choice.useService` is
		// false (option 2 is handled in the if-branch above), and
		// `choice.skipService` already returned at line 378. On
		// Windows that means "Startup-folder launcher" regardless of
		// `spawnNow`: both interactive option-1 (`spawnNow == true`)
		// AND non-interactive `bridge init --yes` (no `--service`
		// flag, `spawnNow == false`) are operator-explicit "no SCM"
		// requests. `Install`'s SCM-first auto-elevation would
		// silently install as a Windows Service when running elevated
		// — the resulting status line ("background service (SCM)")
		// wouldn't match the operator's choice. `InstallStartup`
		// bypasses the SCM tier. On macOS/Linux it returns ("", nil)
		// and we fall back to `Install` (launchd / systemd user unit)
		// — those platforms have only one install mode anyway.
		// (CodeRabbit / Gemini on PR #73 — first cut tied this to
		// `choice.spawnNow` and missed the non-interactive default.)
		if runtime.GOOS == "windows" {
			unitPath, err = packaging.InstallStartup(params)
		} else {
			unitPath, err = packaging.Install(params)
		}
		if err != nil {
			fmt.Fprintf(stderr, "service install: %v\n", err)
			fmt.Fprintln(stderr, "You can still run the bridge manually:")
			fmt.Fprint(stderr, shellHandoff(binary, cfgPath))
			return 1
		}
		fmt.Fprintf(stdout, "Service installed at:\n  %s\n", unitPath)
		// macOS launchd `bootstrap` / Linux systemctl `enable --now`
		// started the daemon as part of install. On Windows the Startup
		// .cmd only runs at next logon, so we spawn a detached child
		// here if the user asked for Mode 1.
		switch runtime.GOOS {
		case "darwin", "linux":
			serverIsLive = true
		case "windows":
			if choice.spawnNow {
				serverIsLive = spawnNowOrWarn(stdout, stderr, binary, cfgPath, logPath, adminAddr)
			}
		}
	}
	fmt.Fprintf(stdout, "Logs:\n  %s\n", logPath)

	printFutureLaunchHint(stdout, choice, binary, cfgPath)
	printAdmin(serverIsLive)
	return 0
}

// spawnNowOrWarn tries to fire up a detached `bridge serve` right now.
// Returns true on success. On failure, warns the operator and falls
// back to the "next logon" path — init still exits cleanly because the
// launcher .cmd is already on disk.
//
// Skips the spawn if something is already listening on the admin port
// — re-running init while the SCM service (or a previous detached
// launcher) is up shouldn't produce a port-bind error buried in the
// log. `adminAddr` comes from the loaded config (caller passes
// `cfg.AdminAddress`); falls back to 127.0.0.1:7789 only if the addr
// isn't host:port parseable.
//
// A configured port of 0 (the OS-picks-an-ephemeral-port mode) skips
// the probe instead of taking that fallback. There is no address to
// dial — the port this bridge will bind is not known until it binds —
// so asking "is 7789 already taken?" answers about a listener the
// operator never asked for, and a yes suppresses an auto-start that
// would have worked.
func spawnNowOrWarn(stdout, stderr io.Writer, binary, cfgPath, logPath, adminAddr string) bool {
	host, port, probe := autoStartProbeTarget(adminAddr)
	if probe && packaging.IsListening(host, port) {
		fmt.Fprintf(stdout, "A bridge is already running on %s:%d; skipping auto-start.\n", host, port)
		return true
	}
	if err := packaging.SpawnDetached(binary, cfgPath, logPath); err != nil {
		fmt.Fprintf(stderr, "auto-start failed: %v\n", err)
		fmt.Fprintln(stderr, "Launcher is installed — the bridge will start at next logon. Or run:")
		fmt.Fprint(stderr, shellHandoff(binary, cfgPath))
		return false
	}
	return true
}

// autoStartProbeTarget says what spawnNowOrWarn should probe before it
// starts a detached bridge, and whether to probe at all.
//
// One parse, not two: net.SplitHostPort + Atoi answers all three
// questions — host, port, and whether there is an address to dial — and
// calling splitHostPort beside configuredPort ran both over the same
// string (Gemini on #970).
//
// An address that does not parse takes the documented
// 127.0.0.1:7789 fallback. A parsed port of 0 does NOT: that is the
// OS-picks-an-ephemeral-port mode, the port this bridge will bind is not
// known until it binds, and asking "is 7789 already taken?" answers
// about a listener the operator never asked for — a yes there suppresses
// an auto-start that would have worked.
func autoStartProbeTarget(adminAddr string) (host string, port int, probe bool) {
	h, p, err := splitHostPortRaw(adminAddr)
	if err != nil {
		return "127.0.0.1", 7789, true
	}
	return h, p, p != 0
}

// stdinIsTerminal reports whether the bridge process's stdin is
// connected to a real terminal. Used to gate interactive prompts in
// init so a piped or closed stdin (e.g. `bridge init < /dev/null`,
// CI pipelines) doesn't accept the prompt's default by reading EOF
// out of confirm(). Takes a non-nil *bufio.Reader as a sanity check —
// when callers pass nil they explicitly mean "no interactivity".
func stdinIsTerminal(in *bufio.Reader) bool {
	if in == nil {
		return false
	}
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// splitFingerprint splits a TLS fingerprint string into two halves
// for two-line display inside a frameWidth-bounded box. Splits at a
// colon boundary near the midpoint so the line break visually
// anchors on a separator rather than inside a hex pair.
//
// Invariant: first + second == input. NEVER truncates — operators
// copy this byte-for-byte to the iOS pairing UI, and a missing
// character silently breaks pairing for every paired client.
//
// When the input contains no colons (a pathological format change
// upstream), splits at the midpoint instead. Still concatenable.
func splitFingerprint(fp string) (first, second string) {
	mid := len(fp) / 2
	for mid < len(fp) && fp[mid] != ':' {
		mid++
	}
	if mid < len(fp) {
		// mid points at a colon; include it on the first line so
		// the second line starts with a fresh hex byte.
		return fp[:mid+1], fp[mid+1:]
	}
	return fp[:len(fp)/2], fp[len(fp)/2:]
}

// futureLaunchHeader is the section header printed for each per-mode
// launch-hint branch in printFutureLaunchHint. Extracted so the literal
// lives in one place across the five `runtime.GOOS` switch arms.
const futureLaunchHeader = "How it'll start in the future:"

// printFutureLaunchHint tells the operator how the bridge is going to
// come up next time — the asymmetry we're fixing is that Windows init
// used to leave them with a dead port and no explanation. Per-mode
// copy; macOS / Linux get a shorter note since their service managers
// make this self-evident.
func printFutureLaunchHint(stdout io.Writer, choice launchChoice, binary, cfgPath string) {
	fmt.Fprintln(stdout)
	switch runtime.GOOS {
	case "windows":
		switch {
		case choice.useService:
			fmt.Fprintln(stdout, futureLaunchHeader)
			fmt.Fprintln(stdout, "  • Automatically at boot (Windows Service, delayed-start)")
			fmt.Fprintln(stdout, "  • Survives logout — always on")
			fmt.Fprintf(stdout, "  • To stop: `sc stop %s` from an elevated shell\n", packaging.ServiceLabel)
		case choice.spawnNow:
			fmt.Fprintln(stdout, futureLaunchHeader)
			fmt.Fprintln(stdout, "  • Automatically when you log in (Startup-folder launcher)")
			fmt.Fprintf(stdout, "  • To stop now: close the minimized \"1-bit-bridge\" window, or End Task in Task Manager\n")
			fmt.Fprintln(stdout, "  • To start manually any time:")
			fmt.Fprint(stdout, shellHandoff(binary, cfgPath))
		default:
			fmt.Fprintln(stdout, futureLaunchHeader)
			fmt.Fprintln(stdout, "  • Automatically when you next log in (Startup-folder launcher)")
			fmt.Fprintln(stdout, "  • To start right now:")
			fmt.Fprint(stdout, shellHandoff(binary, cfgPath))
		}
	case "darwin":
		fmt.Fprintln(stdout, futureLaunchHeader)
		fmt.Fprintln(stdout, "  • Automatically at login (launchd user agent, already running)")
		fmt.Fprintf(stdout, "  • To stop: `launchctl bootout gui/$UID ~/Library/LaunchAgents/%s.plist`\n", packaging.ServiceLabel)
	case "linux":
		fmt.Fprintln(stdout, futureLaunchHeader)
		fmt.Fprintln(stdout, "  • Automatically at login (systemd user unit, already running)")
		fmt.Fprintf(stdout, "  • To stop: `systemctl --user stop %s.service`\n", packaging.ServiceLabel)
	}
}

// --- helpers ---

func ask(r *bufio.Reader, w io.Writer, prompt, def string) string {
	if def != "" {
		fmt.Fprintf(w, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(w, "%s: ", prompt)
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return def
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func confirm(r *bufio.Reader, w io.Writer, prompt string, defYes bool) bool {
	hint := "[y/N]"
	if defYes {
		hint = "[Y/n]"
	}
	fmt.Fprintf(w, "%s %s: ", prompt, hint)
	line, err := r.ReadString('\n')
	if err != nil {
		return defYes
	}
	line = strings.TrimSpace(strings.ToLower(line))
	if line == "" {
		return defYes
	}
	return line == "y" || line == "yes"
}

// withExistingInstallDeps points `bridge init`'s preflight at the
// install that is ALREADY at cfgPath: the certificate it serves, the SAN
// gather (so tls-cert-sans runs at all), its deployment posture, and the
// ports and pid file it listens with.
//
// `bridge init` is re-run far more often than it is run: reinstalling
// the service, rewriting a hand-edited config, moving a data directory
// to a new host. Its preflight built Deps from the prompts alone, so on
// every one of those runs the two cert checks graded
// `<cfgDir>/data/server.{crt,key}` — which is not where an install with
// an explicit `tlsCertPath` keeps its pair — and tls-cert-sans skipped
// itself entirely, because a nil CertSANs is a silent ok. That skip is
// the wrong way round: the SAN check's own docblock says it belongs in
// a preflight rather than only in the startup path, and `bridge init`
// IS the preflight command.
//
// JUDGEMENT CALL — this grades the PRE-init state, deliberately. The
// preflight runs before init writes the config, so the values here are
// the ones on disk NOW, not the ones init is about to save. For the
// cert that is the only coherent reading: the pair graded is the one the
// install serves, and a rewrite keeps it, and the data dir it may live in
// (init_rewrite.go), and loads it rather than minting one. That was true only of a pair in init's own
// data dir until 2026-09-27: a rewrite dropped the config's tlsCertPath,
// tlsKeyPath and dataDir, and minted a new pair.
// For the SAN want-set it is right wherever the rewrite keeps
// `customEndpoints`, the one input that moves the answer: init never
// prompts for them, and a loopback rewrite of a loopback install keeps
// them, so there the old value IS the new value. Until 2026-09-27 this
// paragraph said they survived every rewrite, and they survived none. A
// --public rewrite writes the domain's endpoint in their place, and a
// rewrite that changes posture starts from the new posture's, so for
// those two the want-set graded here is the one being replaced, and
// `bridge doctor` after the run grades the saved one. A first install
// has no config to read and keeps the existing skip: nothing is stale on
// a host whose first mint has not happened yet, and grading a narrower
// want-set than `bridge serve` builds would be a comparison presented as
// authoritative that was never made.
//
// Everything both checks say about this state is warn-level by design
// (neither a stale SAN set nor a clock-skewed NotBefore is a reason to
// refuse to initialise), which is why ensureDoctorClean surfaces warns.
//
// A config that is there and does not load (a misspelt key, say) is the
// re-init that exists to replace it, often while the install's bridge is
// still serving. config.Load cannot read it, so the preflight grades the
// ports the caller seeded d with, the ones this run writes (initAddresses),
// as for a first install, and the pair the file names as written
// (readPriorInstall), the one the rewrite keeps. It graded the default pair
// in init's data dir until 2026-09-27, and over a config naming its own
// answered ok, "absent (init will mint)". But the ports are not the only
// fact here: `bridge serve` records its pid in the data dir, the one
// d.DataDir names (the file's, which the rewrite keeps, or init's own where
// the file cannot be read), so the bridge this run replaces is known without
// the config. Without it, that bridge's own listeners read as another
// process's, both port checks FAILed, and the re-init refused (measured on
// 2026-09-25, #1022's log entry). Its ports are unknown, though, so only the
// probe seeing it listen on a port excuses that port (doctor's
// checkChosenPort), never its being alive: an install that had moved off the
// defaults has a live bridge on its own ports while another process may hold
// the one init writes.
//
// It reports whether the install's config loaded, which is whether the
// ports in d are now the install's. Where it did not, they are still the
// caller's.
func withExistingInstallDeps(d *doctor.Deps, cfgPath string) (loaded bool) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		// A first install has nothing at cfgPath and stays as it is. With
		// no data dir there is no pid file to point at.
		if !errors.Is(err, fs.ErrNotExist) && d.DataDir != "" {
			d.OwnPIDFile = filepath.Join(d.DataDir, serverPIDFileName)
			d.OwnPIDPortsUnknown = true
			if prior, perr := readPriorInstall(cfgPath); perr == nil && prior != nil &&
				prior.TLSCertPath != "" && prior.TLSKeyPath != "" {
				d.TLSCertPath, d.TLSKeyPath = prior.TLSCertPath, prior.TLSKeyPath
			}
		}
		return false
	}
	d.TLSCertPath, d.TLSKeyPath = resolveCertPaths(cfg)
	// The same helper `bridge serve`, `bridge cert rotate` and `bridge
	// doctor` gather from, so the preflight's verdict is a claim about
	// what a rotation would mint rather than a second opinion about it.
	d.CertSANs = func(context.Context) servertls.GenerateOptions {
		return certSANOptions(cfg)
	}
	// And the posture, from the same file, for the same reason the paths
	// come from it: this is the install that is THERE. checkTLSCertSANs
	// skips on a managed bridge because the control plane owns rotation
	// and the tenant reaches the console over the autocert domain — the
	// only remedy that check names is a shell command they cannot run.
	// Without this, a re-init of a hosted install printed exactly that
	// advice. `bridge doctor` has always set it (buildDoctorDeps); this
	// helper copied the cert fields beside it and not this one, so the
	// two commands graded the same host differently.
	d.Managed = cfg.Deployment.IsManaged()
	// The PORTS this install actually listens on. 7788/7789 are the
	// DEFAULTS, and the preflight hard-coded them: a public-mode install
	// on :443 was graded against two ports nothing was using, which are
	// free, so `port-api: ok` — a check passing because the thing it
	// guards is absent. `bridge doctor` has read them from the config
	// since it learned to (buildDoctorDeps); this helper copied the cert
	// fields beside them and not these, so the two commands graded the
	// same host differently.
	if port, ok := configuredPort(cfg.ListenAddress); ok {
		d.APIPort = port
	}
	if port, ok := configuredPort(cfg.AdminAddress); ok {
		d.AdminPort = port
	}
	// And the pid file `bridge serve` writes while it runs, without
	// which checkPort cannot run its "is that bound port US?" ladder at
	// all — an empty OwnPIDFile skips it, and a bound admin port then
	// grades FAIL. That is not hypothetical: it is the situation the
	// --skip-doctor comment at the call site describes as the reason
	// the flag exists, i.e. the defect was being worked around rather
	// than fixed, in the command whose whole job here is to grade the
	// install that is there.
	d.OwnPIDFile = filepath.Join(cfg.DataDir, "server.pid")
	return true
}

// maxLibraryPrompts bounds the interactive library-path re-prompt loop so a
// non-TTY stdin that slipped past the menu's TTY gate can't spin forever.
// askLibraryName's loop takes the same bound.
const maxLibraryPrompts = 5

// askLibraryName asks for the library's display name, offering def, and asks
// again when the answer is one the app's pairing parser would refuse
// (config.CheckLibraryName), where `--name` exits 2: by the prompt the library
// is chosen and the preflight has run, and a run should not end over the
// name. The answer is trimmed as the parser trims it, and one blank once
// trimmed is returned as "", no name, which the caller turns into def.
//
// Enter takes def, and so does a closed stdin (ask returns it for both), so
// the loop cannot spin on a stream that has ended. def is not checked: it is
// the host's name or the name the install is served under, and a host name
// the parser would refuse is repaired where the config is saved
// (config.RepairLibraryName, in Normalize). Returns (name, 0), or ("", 2)
// after maxLibraryPrompts refused answers.
func askLibraryName(in *bufio.Reader, stdout, stderr io.Writer, def string) (string, int) {
	for attempt := 0; attempt < maxLibraryPrompts; attempt++ {
		answer := ask(in, stdout, "Library display name", def)
		name := config.TrimLibraryName(answer)
		if name == "" || answer == def {
			return name, 0
		}
		err := config.CheckLibraryName(name)
		if err == nil {
			return name, 0
		}
		fmt.Fprintln(stderr, paint(ansiRed, "✗ the name "+err.Error()))
	}
	fmt.Fprintf(stderr, "Too many invalid attempts. Aborting.\n")
	return "", 2
}

// resolveLibraryDir expands a leading ~, makes the path absolute, and
// verifies it's an existing directory. Returns the absolute path or an
// error describing why the path is unusable. Shared by the --library flag's
// single-shot validation and the interactive re-prompt loop.
func resolveLibraryDir(libRoot string) (string, error) {
	a, err := filepath.Abs(expandHome(libRoot))
	if err != nil {
		return "", fmt.Errorf("resolve library path: %v", err)
	}
	info, err := os.Stat(a)
	if err != nil {
		return "", fmt.Errorf("library path: %v", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", a)
	}
	return a, nil
}

// askLine is an EOF-aware single-line prompt. Unlike ask(), it surfaces
// io.EOF (alongside any data read so far) so the caller's re-prompt loop can
// tell "user closed stdin" (Ctrl+D) from "user pressed Enter on an empty
// line" — conflating them would let a closed pipe spin the retry loop. No
// default value: the loop decides what an empty line means.
func askLine(r *bufio.Reader, w io.Writer, prompt string) (string, error) {
	fmt.Fprintf(w, "%s: ", prompt)
	line, err := r.ReadString('\n')
	return strings.TrimSpace(line), err
}

// promptLibraryDir interactively asks for the library folder, re-prompting
// on an invalid path instead of aborting the whole init — a paste typo is
// the common failure and forcing a full re-run for it is a papercut. Returns
// (abs, 0) on success, or ("", code) to abort. Every abort uses exit 2 (the
// same code as the non-interactive "--yes requires --library" failure) so a
// piped-stdin run fails identically whether or not --yes is set:
//
//   - empty Enter: "library path is required".
//   - closed / unreadable stdin with no input (Ctrl+D, or any other read
//     error): "input closed; aborting." — returned immediately, never
//     spinning to the retry cap.
//   - a non-empty path that arrived together with a read error (typed then
//     Ctrl+D, no newline) is still validated; only if it's also invalid do
//     we abort (no more input to retry with).
//   - exhausted after maxLibraryPrompts invalid tries.
func promptLibraryDir(in *bufio.Reader, stdout, stderr io.Writer) (string, int) {
	for attempt := 0; attempt < maxLibraryPrompts; attempt++ {
		line, err := askLine(in, stdout, "Library folder to expose (absolute path)")
		// Any non-nil error (io.EOF on a closed pipe, or a genuine read
		// fault) means the stream won't yield more — treat it as terminal so
		// a broken reader can't spin the loop to its cap.
		noMoreInput := err != nil
		if line == "" {
			if noMoreInput {
				fmt.Fprintf(stderr, "\ninput closed; aborting.\n")
			} else {
				fmt.Fprintf(stderr, "library path is required\n")
			}
			return "", 2
		}
		abs, verr := resolveLibraryDir(line)
		if verr == nil {
			return abs, 0
		}
		fmt.Fprintln(stderr, paint(ansiRed, "✗ "+verr.Error()))
		if noMoreInput {
			// The bad path came with a closed/broken stream — no more input
			// to retry with, so abort rather than spin to the cap.
			return "", 2
		}
	}
	fmt.Fprintf(stderr, "Too many invalid attempts. Aborting.\n")
	return "", 2
}

// expandHome expands a leading "~" to $HOME, because operators paste
// paths copied from terminals and shells usually handle that themselves.
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	// Accept the native Windows separator too. An operator typing
	// `~\Music` at the init prompt would otherwise fall through
	// unexpanded, and the subsequent filepath.Abs treats "~" as a
	// literal directory name relative to the CWD — producing a library
	// root like C:\Users\me\~\Music that silently doesn't exist.
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(home, p[2:])
	}
	return p
}

// openInBrowser tries to pop the admin URL in the operator's browser.
// Best-effort — ignore errors so headless machines don't surface a
// confusing failure on a successful init.
//
// Windows uses `cmd /c start "" <url>`: the empty first quoted
// argument is the window title that `start` expects when its first
// positional is itself a quoted string (a URL counts, on paths with
// spaces). Without the empty title, `start` treats the URL as the
// title and silently does nothing.
func openInBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("cmd.exe", "/c", "start", "", url)
	default:
		return
	}
	_ = cmd.Start()
}
