package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/acoseac/1-bit-bridge/internal/config"
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
//     the addresses the other posture listened on. A kept endpoint that names
//     the port the rewrite moves the API off is kept, and named in a warning
//     (warnKeptEndpointsOnAMovedPort).
//   - libraryRoots, when the run names no --library, which only a public run
//     may do: a public install takes its roots later, in the console, and a
//     rewrite that emptied them left every track unplayable.
//   - libraryName, when the run names no --name, in either posture, since a
//     name, unlike an endpoint, names no address. It is the name the install
//     is served under: /v1/health gives it to a caller with no token too, and
//     every pairing QR carries it as the name a newly paired device takes for
//     its own. The rewrite gave it the host's name, the guess init makes for
//     a first install, which has nothing better to go on, and the prompt
//     offered that guess as its default. A config that gives no name, or a
//     blank one, has none to keep, and the run takes the host's as a first
//     install does: DefaultLibraryName, which Load serves such a config, is
//     a fallback nobody chose.
//   - listenAddress and adminAddress, when the run names neither address
//     flag and the posture does not change (addressesARewriteKeeps). A
//     posture change writes the new posture's ports. Naming either flag
//     writes that flag and the posture's address for the one it does not
//     name. A script that re-runs init --yes --force and expects :7788
//     keeps the old port instead. printKept lists an address only when a
//     first install of this posture would have written another.
//   - output, the whole block, in either posture (backlog B505). It says
//     what is beside the bridge on this box, the MPD and the player's DAC,
//     and init asks for none of it. The player's binding names a unit by a
//     serial that bridge output dac bind reads from the unit and never
//     prints, so only the unit can give it back; MPD's password is typed
//     from MPD's own configuration; MPD keeps the loopback route's URLs in
//     its queue, so a port the rewrite reset broke every queued track; and
//     output.mpd: off is the operator's opt-out, which the default, auto,
//     would turn back on. A setting that does not read, or that the
//     config's own check refuses, is not kept, and the run says which and
//     why (priorOutput, warnOutputNotKept).
//
// Everything else is this run's value or the default, which is the
// documented overwrite: features and cadences. The ports are kept under
// the rule above; init grades the addresses the rewrite saves.

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
	LibraryName     string                  `yaml:"libraryName"`
	CustomEndpoints []string                `yaml:"customEndpoints"`
	Deployment      config.DeploymentConfig `yaml:"deployment"`
	Demo            config.DemoConfig       `yaml:"demo"`
	// ListenAddress and AdminAddress are kept when the run names neither
	// address flag and the posture does not change. ListenAddress also says
	// which kept endpoint names the port a rewrite that does move the API
	// moves it off (warnKeptEndpointsOnAMovedPort).
	ListenAddress string `yaml:"listenAddress"`
	AdminAddress  string `yaml:"adminAddress,omitempty"`
}

// readPriorInstall reads the config at cfgPath as priorInstallFile, with its
// paths resolved as config.Load resolves them: against the file's directory,
// dataDir defaulting to the one beside it. The listen address defaults as
// Load defaults it, and so does the admin address, only for a loopback
// install. The name is as written, with no default: a rewrite keeps a name
// the config gives, and DefaultLibraryName, which Load serves a config that
// gives none, is a fallback nobody chose. A config that is not there is no
// install, and answers nil and no error.
func readPriorInstall(cfgPath string) (*priorInstallFile, error) {
	return priorInstallFrom(cfgPath, readConfigAsIs(cfgPath))
}

// priorInstallFrom is readPriorInstall over a read of cfgPath already made,
// so initCmd can check later that the config is still as it read it
// (configChangedSinceRead).
func priorInstallFrom(cfgPath string, read configAsRead) (*priorInstallFile, error) {
	if errors.Is(read.err, fs.ErrNotExist) {
		return nil, nil
	}
	if read.err != nil {
		return nil, fmt.Errorf("read %s: %w", cfgPath, read.err)
	}
	var p priorInstallFile
	if err := yaml.Unmarshal(read.raw, &p); err != nil {
		// Without the file's text, as config.Load says it: refuseRewrite
		// prints this, and yaml.v3 quotes what it cannot decode.
		return nil, fmt.Errorf("parse %s: %w", cfgPath, config.ValueFreeYAMLError(err))
	}
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", cfgPath, err)
	}
	base := filepath.Dir(abs)
	if p.DataDir == "" {
		p.DataDir = config.DefaultDataDir
	}
	if p.ListenAddress == "" {
		p.ListenAddress = config.DefaultListenAddress
	}
	if p.AdminAddress == "" && !p.public() {
		p.AdminAddress = config.DefaultAdminAddress
	}
	p.DataDir = config.ResolvePath(base, p.DataDir)
	p.TLSCertPath = config.ResolvePath(base, p.TLSCertPath)
	p.TLSKeyPath = config.ResolvePath(base, p.TLSKeyPath)
	for i, root := range p.LibraryRoots {
		p.LibraryRoots[i] = config.ResolvePath(base, root)
	}
	return &p, nil
}

// configAsRead is the config file at a path as a run read it: its bytes, or
// the error the read gave, fs.ErrNotExist where there was no file.
type configAsRead struct {
	raw []byte
	err error
}

// readConfigAsIs reads the config at cfgPath as it is.
func readConfigAsIs(cfgPath string) configAsRead {
	raw, err := os.ReadFile(cfgPath)
	return configAsRead{raw: raw, err: err}
}

// sameAs says a and b read one config: the same bytes, or no file both
// times. A read that failed any other way cannot say the config is the one
// read, so it is a change. (A config the first read could not read never
// gets this far: refuseRewrite refuses it.)
func (a configAsRead) sameAs(b configAsRead) bool {
	if a.err != nil || b.err != nil {
		return errors.Is(a.err, fs.ErrNotExist) && errors.Is(b.err, fs.ErrNotExist)
	}
	return bytes.Equal(a.raw, b.raw)
}

// configChangedSinceRead refuses a run whose config is no longer the one it
// read at its start, and says so: asRead is that read. initCmd asks it right
// before Config.Save, after every prompt, refusal and check, so what is left
// between the check and the write is Save's own staging and rename.
//
// A run reads the config (what a rewrite keeps, readPriorInstall) and decides
// whether to keep or replace it before its preflight and its name prompt,
// which wait on the operator for as long as they take. A config another
// process wrote after that read, a second init or an edit or the console's
// save, is one the run neither read nor asked about, and writing over it
// drops what that process wrote. Until 2026-09-29 (backlog B61) "Overwrite?"
// came after both, so a config that appeared while they waited was asked
// about; moving the question ahead of the preflight widened the window to
// take them in, which CodeRabbit's security review of #1106 named. The
// check holds for a first install and a rewrite alike, and for a change the
// old order let through too (a rewrite keeps what it read at the start).
//
// It sat after the last prompt, before anything was written, until the same
// review's next round: the second port pass and the TLS load ran after it,
// and a change there was still written over. Right before Save, a refusal
// can follow a first install's TLS mint, which the next run loads, as it
// loads the pair a Save that fails leaves. Nothing but an interprocess lock
// would close the rename that remains, which #1043 declined for tokens.json.
func configChangedSinceRead(stderr io.Writer, cfgPath string, asRead configAsRead) bool {
	if asRead.sameAs(readConfigAsIs(cfgPath)) {
		return false
	}
	fmt.Fprintf(stderr, "the config at %s changed while this init ran, after it read it (another init, or an edit?).\n", cfgPath)
	fmt.Fprintln(stderr, "run init again to decide about the config as it is now; this run did not write it.")
	return true
}

// loopback says the file's posture is loopback: deployment.mode unset or
// "loopback". A mode the build does not know is neither posture.
func (p *priorInstallFile) loopback() bool {
	mode, err := p.Deployment.EffectiveMode()
	return err == nil && mode == config.DeploymentModeLoopback
}

// public says the file's posture is public: deployment.mode "public".
func (p *priorInstallFile) public() bool {
	mode, err := p.Deployment.EffectiveMode()
	return err == nil && mode == config.DeploymentModePublic
}

// refuseRewrite decides whether this run may overwrite the config at
// cfgPath, before anything is written, and says why when it may not. prior
// and priorErr are readPriorInstall's answer for cfgPath.
//
//   - A config this user cannot read is read by the bridge's own user at
//     every start, so this run is the wrong user's, and it cannot tell which
//     data dir and pair it would keep.
//   - A file that does not parse names nothing the rewrite can read, so it
//     cannot keep the data dir, the pair, the endpoints or the roots: a
//     rewrite would reset all four, the defect this file exists to stop. It
//     proceeded when init's own data dir held a pair, until CodeRabbit's
//     review of #1040: that pair being there does not make it the one the
//     install serves, and the file may name another. A config that does not
//     load for a misspelt key does parse, and is kept from as usual. Moving
//     the file aside is the remedy it names: init then runs as on a first
//     install, and LoadOrGenerate keeps a pair it finds in the data dir.
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
func refuseRewrite(stderr io.Writer, cfgPath string, prior *priorInstallFile, priorErr error) bool {
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
		fmt.Fprintf(stderr, "%v\n", priorErr)
		fmt.Fprintln(stderr, "init cannot tell which data dir and TLS pair this install uses, so a rewrite could not keep them, and every device paired with it could have to pair again.")
		fmt.Fprintln(stderr, "fix the YAML, or move bridge.yaml aside to set this install up from scratch (init keeps a TLS pair it finds in its data dir).")
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
	fmt.Fprintln(stderr, configNotChanged)
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
			"its console would offer the controls its operator withholds (" + managedControlsSaid(p.Deployment) + ")"})
	}
	if len(p.Deployment.ManagedSettings) > 0 {
		out = append(out, postureKey{"deployment.managedSettings",
			"its console could change the settings its operator manages"})
	}
	return out
}

// managedControlsSaid names the managed controls d lists, for init's
// refusal: each name this build knows as it is, one of a fixed set, and each
// other entry by its place in the list with a fixed explanation
// (config.DeploymentConfig.UnknownManagedControls), never by what it holds,
// which can be anything, a password pasted onto the list's line included
// (B505's round 4: the entries were joined as written).
func managedControlsSaid(d config.DeploymentConfig) string {
	var known []string
	for _, m := range d.ManagedControls {
		if slices.Contains(config.KnownManagedControls(), m) && !slices.Contains(known, m) {
			known = append(known, m)
		}
	}
	said := strings.Join(known, ", ")
	if unknown := d.UnknownManagedControls(); len(unknown) > 0 {
		if said != "" {
			said += "; "
		}
		said += strings.Join(unknown, ", ") + ": not a control this build knows"
	}
	return said
}

// keepFromPrior sets on cfg what a rewrite keeps from prior that has not
// reached it already: the TLS pair, and a loopback install's custom
// endpoints on a loopback rewrite. The data dir, the roots and the name reach
// cfg through baseConfig: initCmd decides the first two before the
// preflight, which uses the data dir, and the name at its prompt. It reports
// whether it kept the endpoints, for printKept.
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

// warnKeptEndpointsOnAMovedPort warns about the kept custom endpoints that
// name the port the rewrite moves the API off: fromListen is the install's
// listen address, toListen the one the rewrite saves.
//
// A loopback rewrite of a loopback install keeps its customEndpoints and
// writes the run's ports (the list above), so a kept endpoint can name a port
// the bridge no longer binds, and /v1/health then advertises it beside the
// bridge's own addresses: an alternate every device puts into its failover
// rotation and fails on (measured with the real binary on 2026-09-29, backlog
// B61). The rewrite keeps the endpoint and says so, rather than dropping it or
// rewriting its port. The endpoint is the operator's word for what reaches
// this bridge, and its port need not be the bridge's: a router or proxy may
// forward it, and a forward from that port is right again once it is pointed
// at the new one (the rule on endpoints synthesised from the listen port, in
// CLAUDE.md, is the same fact from the other side).
func warnKeptEndpointsOnAMovedPort(stderr io.Writer, fromListen, toListen string, endpoints []string) {
	named, from, to := endpointsNamingAMovedPort(fromListen, toListen, endpoints)
	if len(named) == 0 {
		return
	}
	fmt.Fprintf(stderr, "warning: customEndpoints kept from the config name :%d, the port this rewrite moves the API off "+
		"(it listens on :%d now):\n", from, to)
	for _, e := range named {
		fmt.Fprintf(stderr, "  %s\n", e)
	}
	fmt.Fprintf(stderr, "where one reaches this bridge directly, a device tries it and fails over past it: change its port "+
		"in bridge.yaml, or re-run init with --listen-address on :%d to keep the API there. Where a router or proxy "+
		"forwards it, point that at :%d.\n", from, to)
}

// endpointsNamingAMovedPort returns the endpoints that name the port the API
// listens on at fromListen when toListen moves it to another, and the two
// ports. An endpoint names a port by its own, or by its scheme's (443 for
// https, 80 for http). It returns nothing where the port does not move, or
// where fromListen names no port a client can dial: one that does not parse,
// or 0, the port the system picks at each start.
func endpointsNamingAMovedPort(fromListen, toListen string, endpoints []string) (named []string, from, to int) {
	from, okFrom := configuredPort(fromListen)
	to, okTo := configuredPort(toListen)
	if !okFrom || !okTo || from == 0 || from == to {
		return nil, 0, 0
	}
	for _, e := range endpoints {
		if endpointPort(e) == from {
			named = append(named, e)
		}
	}
	return named, from, to
}

// endpointPort is the port an endpoint URL names: its own, or its scheme's
// default. 0 where it names none this can read.
func endpointPort(raw string) int {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0
		}
		return n
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return 443
	case "http":
		return 80
	}
	return 0
}

// keptFromHeading opens the list printKept prints.
const keptFromHeading = "kept from the config this run replaces:"

// addressesARewriteKeeps returns the listen and admin addresses a rewrite
// saves, and whether printKept lists each. It keeps the install's pair when
// the run replaces the config, names neither address flag and does not
// change posture. A named flag, a posture change, a first install and a run
// that keeps the file all leave the addresses the run already chose. An
// address is listed only when it was kept and a first install of this
// posture would have written another.
func addressesARewriteKeeps(listen, admin string, listenNamed, adminNamed, public, proxy, replace bool, prior *priorInstallFile) (string, string, bool, bool) {
	if !replace || prior == nil || prior.public() != public || listenNamed || adminNamed {
		return listen, admin, false, false
	}
	firstListen, firstAdmin := initAddresses(public, proxy, "", "")
	listenListed, adminListed := false, false
	if prior.ListenAddress != "" {
		listen = prior.ListenAddress
		listenListed = listen != firstListen
	}
	if prior.AdminAddress != "" {
		admin = prior.AdminAddress
		adminListed = admin != firstAdmin
	}
	return listen, admin, listenListed, adminListed
}

// printKept lists, after the rewrite is saved, what it kept that a first
// install would not have: a data dir other than initDataDir (init's own,
// beside the config), a pair the config names, the roots when the run named
// none, the name when it named none and a first install would have taken
// another (nameKept), the listen and admin addresses when the run named
// neither and they are not the ones a first install of this posture writes
// (listenKept, adminKept), the custom endpoints. An operator who rewrote a
// config should not have to diff it to learn that these stayed, and a
// rewrite that gives an install init made the answers it was made with
// prints nothing.
//
// Both sides of the data dir comparison are absolute, so it compares
// directories and not spellings: initCmd makes cfgDir absolute before it
// derives initDataDir from it, and a kept data dir is readPriorInstall's,
// which resolves the file's value against the config's directory.
// NormalizeAndValidate resolves no path. (Gemini on #1040, twice, assumed a
// relative cfgDir.)
func printKept(w io.Writer, cfg *config.Config, initDataDir string, rootsKept, nameKept, endpointsKept, listenKept, adminKept bool) {
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
	if nameKept {
		add("libraryName", cfg.LibraryName)
	}
	if endpointsKept {
		add("customEndpoints", cfg.CustomEndpoints...)
	}
	if listenKept {
		add("listenAddress", cfg.ListenAddress)
	}
	if adminKept {
		add("adminAddress", cfg.AdminAddress)
	}
	// A first install writes no output block, so one in cfg was kept.
	for _, l := range keptOutputLines(cfg.Output) {
		add(l[0], l[1])
	}
	if len(lines) == 0 {
		return
	}
	// Sixteen columns, or the longest key's (output.loopbackPort is 19).
	width := 16
	for _, l := range lines {
		width = max(width, len(l[0]))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, keptFromHeading)
	for _, l := range lines {
		fmt.Fprintf(w, "  %-*s %s\n", width, l[0], l[1])
	}
	fmt.Fprintln(w, "every other setting is this run's or the default.")
}

// priorOutput reads the output block of the config a rewrite replaces (raw,
// the bytes the run read): what the rewrite keeps of it, and a line for each
// setting, or for the block, it does not keep.
//
// A file Load's decoder takes whole gives up Load's own block: raw decoded
// by Load's own decode (config.DecodeFile: the decoder set the same way,
// KnownFields, and a whole number written as a fraction refused) into
// config.Config, before Load's defaults, its BRIDGE_* overrides, its paths
// and its checks (outputAsLoadDecodes). So no merge key, alias, tag or
// encoded key is read one way here and another by Load, and neither is
// anything the decoder counts across the document (B505's round 4: round 3
// decoded the block on its own, and yaml.v3 counts aliases document-wide, so
// a block whose merges expand aliases tenfold a level beside 1,000 library
// roots, which Load takes, was refused as "excessive aliasing" and lost its
// password; round 2's resolver of its own disagreed with the decoder on
// encoded keys, on a merge past 64 blocks and on an alias key). Of that
// block, the settings config.OutputConfig.WithoutRefused refuses are the
// ones Load refuses it for: each is left out, and the rest kept as decoded.
//
// A file the decoder refuses, anywhere, is one Load refuses, and of its
// block the rewrite keeps only what is certain: the block as the decoder
// decodes it on its own, when it does (outputAsLoaded; what the decoder
// counts outside the block can only make it refuse more, never read a value
// otherwise); else, of a block whose keys are plain text, each written once,
// beside no merge key, alias or tag among the keys, each setting decoded on
// its own (outputRecovered); else nothing, and a line that says so. So is a
// block the file writes twice, which Load refuses whatever each holds.
//
// Each line names the setting and the line of its key, or the block and its
// line, never a value. A player block never fails: one this release cannot
// read is kept as the file holds it, as Load keeps it, and the player stays
// off. Read on its own, never as a field of priorInstallFile: a type error in
// the block fails that struct's whole decode, and refuseRewrite would then
// refuse the rewrite over a block it can do without.
func priorOutput(raw []byte) (kept config.OutputConfig, notKept []string) {
	if block, ok := outputAsLoadDecodes(raw); ok {
		kept, refused := block.WithoutRefused()
		node := outputNode(raw)
		for _, p := range refused {
			notKept = append(notKept, outputRefusal(p, node))
		}
		return kept, notKept
	}
	keyLines := outputKeyLines(raw)
	if len(keyLines) > 1 {
		return config.OutputConfig{}, []string{fmt.Sprintf("output (lines %s): it is written twice, which Load refuses, so none of it is kept",
			lineList(keyLines))}
	}
	var file struct {
		Output outputAsLoaded `yaml:"output"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	// Every other key of the file is one this struct does not have, and the
	// decoder's error says so: what matters is what outputAsLoaded recorded.
	_ = dec.Decode(&file)
	o := file.Output
	switch {
	case !o.found:
		// No block, or a null one: Load decodes none either.
		return config.OutputConfig{}, nil
	case o.node == nil:
		return config.OutputConfig{}, []string{"output: it does not load, so none of it is kept"}
	case o.err == nil:
		kept, refused := o.block.WithoutRefused()
		for _, p := range refused {
			notKept = append(notKept, outputRefusal(p, o.node))
		}
		return kept, notKept
	}
	at := o.node.Line
	if len(keyLines) == 1 {
		at = keyLines[0]
	}
	return outputRecovered(o.node, at)
}

// outputAsLoadDecodes is the output block of raw as config.Load decodes it,
// when Load's decode takes the whole file: Load's own, config.DecodeFile,
// into config.Config, the type Load decodes into, taken before anything
// Load does after the decode. ok is false when the decode refuses any of the
// file, a fraction in a whole-number setting included (backlog B528). An
// empty file decodes as no block.
func outputAsLoadDecodes(raw []byte) (block config.OutputConfig, ok bool) {
	var cfg config.Config
	if err := config.DecodeFile(raw, &cfg); err != nil && !errors.Is(err, io.EOF) {
		return config.OutputConfig{}, false
	}
	return cfg.Output, true
}

// outputNode is the node the decoder hands the file's output field, through
// a top-level merge or an alias as it reads them, and nil for no block or a
// null one: where the lines of a block's own keys are, for the warnings. It
// decodes no value, so it counts no alias the block expands.
func outputNode(raw []byte) *yaml.Node {
	var file struct {
		Output nodeOf `yaml:"output"`
	}
	_ = yaml.Unmarshal(raw, &file)
	return file.Output.node
}

// outputAsLoaded is the output block as Load's decoder decodes it on its
// own: the node the decoder hands it, through an alias, and
// config.OutputConfig decoded from that node by the same decoder, with the
// error of that decode alone, nil for a block the decoder takes. found is
// false for a file with no block, or a null one, which Load decodes as none.
// It is what priorOutput reads of a file the decoder refuses elsewhere.
type outputAsLoaded struct {
	found bool
	node  *yaml.Node
	block config.OutputConfig
	err   error
}

// UnmarshalYAML is yaml.v3's older Unmarshaler, whose unmarshal decodes a
// value with the decoder at hand, Load's settings included, and returns the
// errors of that value alone, a refusal the decoder panics with included. A
// whole number the block writes as a fraction decodes as its whole part, and
// Load's decode refuses it (config.CheckWholeNumbers), so the block is one
// the decoder refuses here too (backlog B528). It never fails the file's
// decode.
func (o *outputAsLoaded) UnmarshalYAML(unmarshal func(any) error) error {
	o.found = true
	var n nodeOf
	_ = unmarshal(&n)
	o.node = n.node
	o.err = unmarshal(&o.block)
	if o.err == nil && o.node != nil {
		o.err = config.CheckWholeNumbers(o.node, &o.block)
	}
	return nil
}

// nodeOf holds the node yaml.v3 decodes into it.
type nodeOf struct{ node *yaml.Node }

// UnmarshalYAML takes the node.
func (n *nodeOf) UnmarshalYAML(v *yaml.Node) error {
	n.node = v
	return nil
}

// outputRecovered is what a rewrite keeps of n, an output block yaml.v3
// does not decode, whose key is on line at. A block whose keys are each
// plain text (plainKey) gives up each setting a key names once, when the
// setting's value decodes on its own and config.OutputConfig.WithoutRefused
// keeps it, and a line for each key it does not: one output does not take,
// one written more than once, a value that does not read or is refused. Any
// other block gives up nothing, and one line that says so: a merge key, an
// alias or a tag among the keys is the decoder's to resolve, and the decoder
// refused the block, so which setting a value is for is not certain.
func outputRecovered(n *yaml.Node, at int) (kept config.OutputConfig, notKept []string) {
	if n.Kind != yaml.MappingNode {
		return config.OutputConfig{}, []string{fmt.Sprintf("output (line %d): it is not a block of output settings, so none of it is kept", at)}
	}
	lines := map[string][]int{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		if !plainKey(k) {
			return config.OutputConfig{}, []string{fmt.Sprintf("output (line %d): it does not load, and a key of it is a merge key (<<), an alias, "+
				"tagged or not plain text, so which setting a value is for is not certain: none of it is kept", at)}
		}
		lines[k.Value] = append(lines[k.Value], k.Line)
	}
	said := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		setting, known := outputSettingNamed(k.Value)
		switch {
		case !known:
			notKept = append(notKept, fmt.Sprintf("a key of output (line %d): it is not an output setting", k.Line))
			continue
		case len(lines[k.Value]) > 1:
			if !said[k.Value] {
				said[k.Value] = true
				notKept = append(notKept, fmt.Sprintf("%s (lines %s): it is written more than once in one block, so no value of it is kept",
					setting, lineList(lines[k.Value])))
			}
			continue
		}
		// A fraction where a whole number goes decodes as its whole part,
		// and does not read as Load reads it (config.CheckWholeNumbers).
		var one config.OutputConfig
		pair := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{k, v}}
		if err := pair.Decode(&one); err != nil || config.CheckWholeNumbers(pair, &one) != nil {
			notKept = append(notKept, fmt.Sprintf("%s (line %d): it does not read as that setting", setting, k.Line))
			continue
		}
		one, refused := one.WithoutRefused()
		for _, p := range refused {
			notKept = append(notKept, fmt.Sprintf("%s (line %d): %s", p.Setting, k.Line, p.Why))
		}
		// The one setting the pair holds, into what is kept. Each comes once,
		// so one left at its zero (an explicit "" or null) is its value.
		from, into := reflect.ValueOf(one), reflect.ValueOf(&kept).Elem()
		for f := 0; f < from.NumField(); f++ {
			if from.Type().Field(f).IsExported() && !from.Field(f).IsZero() {
				into.Field(f).Set(from.Field(f))
			}
		}
	}
	return kept, notKept
}

// plainKey reports whether k is a key that names its setting with
// certainty: text with no tag written on it, which yaml.v3 reads as the text
// itself. A merge key (<<) resolves to !!merge, an alias is no scalar, and a
// key that resolves to a number, a boolean, a null or a time is read as one.
func plainKey(k *yaml.Node) bool {
	return k.Kind == yaml.ScalarNode && k.Style&yaml.TaggedStyle == 0 && k.Tag == "!!str"
}

// outputSettingNamed is output.<name> when name is a key of
// config.OutputConfig's own, and false for any other key, which is never
// quoted.
func outputSettingNamed(name string) (string, bool) {
	t := reflect.TypeOf(config.OutputConfig{})
	for f := 0; f < t.NumField(); f++ {
		if tag, _, _ := strings.Cut(t.Field(f).Tag.Get("yaml"), ","); tag != "" && tag == name {
			return "output." + tag, true
		}
	}
	return "", false
}

// outputRefusal is the line for a setting of a block yaml.v3 decodes that
// Validate refuses (config.OutputProblem, whose reason carries no value): the
// setting and the line of the block's own key that writes it, or that it was
// merged when none does.
func outputRefusal(p *config.OutputProblem, block *yaml.Node) string {
	if line := settingKeyLine(block, strings.TrimPrefix(p.Setting, "output.")); line > 0 {
		return fmt.Sprintf("%s (line %d): %s", p.Setting, line, p.Why)
	}
	return fmt.Sprintf("%s (from a merged block): %s", p.Setting, p.Why)
}

// settingKeyLine is the line of the key of block, the output block as the
// decoder handed it, that names name as the decoder reads a key, and 0 when
// none of the block's own keys does. In a block yaml.v3 decodes, one key of
// its own at most names a setting, and its value is the one decoded: a key
// of the block wins over a merged one.
func settingKeyLine(block *yaml.Node, name string) int {
	if block == nil || block.Kind != yaml.MappingNode {
		return 0
	}
	for i := 0; i+1 < len(block.Content); i += 2 {
		var key string
		if block.Content[i].Decode(&key) == nil && key == name {
			return block.Content[i].Line
		}
	}
	return 0
}

// outputKeyLines lists the lines of the keys of the file's top level that
// name output, as the decoder reads a key. Two are a block written twice,
// once encoded (a key written twice as it is, refuseRewrite refuses): the
// decoder takes the first and Load refuses the config, so neither is certain
// to be the block the operator meant.
func outputKeyLines(raw []byte) []int {
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	top := doc.Content[0]
	var lines []int
	for i := 0; i+1 < len(top.Content); i += 2 {
		var key string
		if top.Content[i].Decode(&key) == nil && key == "output" {
			lines = append(lines, top.Content[i].Line)
		}
	}
	return lines
}

// lineList is lines as a warning says them: "5", "5 and 9", "5, 9 and 12".
func lineList(lines []int) string {
	s := make([]string, len(lines))
	for i, l := range lines {
		s[i] = strconv.Itoa(l)
	}
	if len(s) < 2 {
		return strings.Join(s, "")
	}
	return strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
}

// keptOutputLines lists, for printKept, the output settings a rewrite kept:
// each key, with its value where it is no secret. MPD's password and the
// player's serial are never shown, nor anything the player's binding names
// but its kind.
func keptOutputLines(o config.OutputConfig) [][2]string {
	var lines [][2]string
	if o.MPD != "" {
		lines = append(lines, [2]string{"output.mpd", o.Mode()})
	}
	if o.Endpoint != "" {
		lines = append(lines, [2]string{"output.endpoint", o.Endpoint})
	}
	if o.Conf != "" {
		lines = append(lines, [2]string{"output.conf", o.Conf})
	}
	if o.Password != "" {
		lines = append(lines, [2]string{"output.password", "set (not shown)"})
	}
	if o.LoopbackPort != nil {
		lines = append(lines, [2]string{"output.loopbackPort", strconv.Itoa(*o.LoopbackPort)})
	}
	if !o.Player.IsZero() {
		lines = append(lines, [2]string{"output.player", keptPlayerSummary(o.Player)})
	}
	return lines
}

// keptPlayerSummary says what a kept player block asks for: whether the
// player is enabled, and the binding's kind when it is one this release
// knows.
func keptPlayerSummary(p config.PlayerConfig) string {
	if p.Unread() != nil {
		return "kept as written (it does not read, so the player is off)"
	}
	s := "not enabled"
	if p.Enabled {
		s = "enabled"
	}
	switch b := p.Device.Binding; {
	case b == config.BindingSerial || b == config.BindingUniqueModel || b == config.BindingPort:
		return s + ", bound by " + b
	case p.Device.Configured():
		return s + ", with a binding this release refuses"
	}
	return s + ", no DAC bound"
}

// warnOutputNotKept names, once the rewrite is saved, the output settings it
// could not keep (priorOutput), so none is dropped without a word.
func warnOutputNotKept(w io.Writer, notKept []string) {
	if len(notKept) == 0 {
		return
	}
	fmt.Fprintln(w, "warning: output settings in the config this run replaced do not read, so the rewrite did not keep them:")
	for _, s := range notKept {
		fmt.Fprintf(w, "  %s\n", s)
	}
	fmt.Fprintln(w, "the config did not load with them either. Set them again in bridge.yaml if you want them.")
}
