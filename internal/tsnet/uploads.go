package tsnet

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"

	"tailscale.com/envknob"
	"tailscale.com/ipn/ipnstate"
)

// NoLogsEnv is the variable Tailscale's code reads, live, to decide whether
// the node uploads its diagnostic logs (envknob.NoLogsNoSupport): the
// logtail transport the upstream start builds is a no-op while it is true,
// and so are the network flow logs a tailnet can ask for. Its name is
// upstream's (envknob.SetNoLogsNoSupport sets it), which
// TestTheNoLogsVariableIsUpstreams holds this constant to.
const NoLogsEnv = "TS_NO_LOGS_NO_SUPPORT"

// ErrNoLogsEnvUnreadable is NewServer's refusal of an environment whose
// NoLogsEnv is not a value strconv.ParseBool reads, which every reader of it
// in Tailscale's code ends the process on (envknob.Bool's log.Fatalf): the
// bridge says so in its own words, naming the variable and not its value,
// before any node is built.
var ErrNoLogsEnvUnreadable = errors.New("tsnet: " + NoLogsEnv + " in the bridge's environment is neither true nor false " +
	"as Tailscale reads it (1, t, true, 0, f or false, or those in capitals), and Tailscale's code stops the process on " +
	"such a value: set it to true, or remove it, and start again")

// Uploads is what the node's diagnostic-log uploads come to in this
// process: the config, and what holds them off where it allows them, kept
// from the policy NewServer applies (Server.Uploads reads it, live), so that
// the console's tile and a tailnet's refusal of the node can say it.
type Uploads struct {
	// Allowed is tailscale.uploadDiagnosticLogs: the config lets the node
	// upload.
	Allowed bool
	// EnvOff is whether the environment the process was launched with held
	// NoLogsEnv true, which keeps the uploads off whatever the config allows
	// until the process is launched without it.
	EnvOff bool
	// LatchedOff is whether this process's policy turned the uploads off
	// earlier, for a node built without the key, where the config allows
	// them now: the policy sets the variable again at every NewServer after
	// that, until the process exits, so the setting applies at the bridge's
	// next launch.
	LatchedOff bool
	// OtherOff is whether something else in this process turned the uploads
	// off where the config allows them: the variable reads true, and neither
	// the launch environment nor this process's policy set it (upstream's own
	// setter, which the coordination server's DisableLogTail message calls).
	OtherOff bool
}

// Off reports whether the node uploads nothing.
func (u Uploads) Off() bool { return !u.Allowed || u.EnvOff || u.LatchedOff || u.OtherOff }

// relaunch is what makes a change of the setting, or of the launch
// environment, apply once this process has turned the uploads off: a new
// launch of the bridge's process. The menu's "Start now", and bridge init's
// start, run serve in their own process, so stopping serve and starting it
// again from there is no new launch.
const relaunch = "quit the bridge, its menu included, and launch it again, or restart its service"

// HeldOff says what holds the uploads off where the config allows them, in
// the words the console's tile and a tailnet's refusal give it, or "" where
// the config does not allow them or nothing holds them off. The launch
// environment comes first, since a new launch alone does not undo it.
func (u Uploads) HeldOff() string {
	switch {
	case !u.Allowed:
		return ""
	case u.EnvOff:
		return NoLogsEnv + "=true in the environment the bridge was launched with keeps them off: remove it from " +
			"that environment, then " + relaunch
	case u.LatchedOff:
		return "this process turned them off when it built a node without the setting, and they stay off until it " +
			"exits: the setting applies at the bridge's next launch, so " + relaunch
	case u.OtherOff:
		return "something in this process turned them off (Tailscale's coordination server can), and a new launch " +
			"of the bridge may turn them back on: " + relaunch
	}
	return ""
}

// applyUploadPolicy is the bridge's diagnostic-log policy for the embedded
// node, applied to the process before the node is built: NoLogsEnv set to
// true through envknob unless allow, and, with allow, the environment left as
// it is until this process's policy has set the variable. An inherited true
// still keeps the uploads off then, and an inherited false never turns them
// on by itself, since a config that does not allow them overrides it. A
// value that is set and does not read as a boolean is ErrNoLogsEnvUnreadable,
// with the environment untouched; an empty one is unset, as envknob reads it.
//
// It runs once for each node a process builds (NewServer), on the caller's
// goroutine, before anything starts the node; the variable is not one of
// envknob's registered knobs, so no cache of it is read elsewhere, and every
// reader of it in Tailscale's code asks os.Getenv.
//
// Once it has set the variable, every later application sets it again,
// through envknob, before it reads it, whatever anything in the process has
// written there since, a false or a value that does not read as a boolean:
// the menu's "Start now", and bridge init's start before it, run serve in the
// process, and the menu runs it again after a stop, so a later node whose
// config allows the uploads starts with them off, and the setting applies at
// the bridge's next launch. It sets the variable again only there, at the
// next NewServer: between a write elsewhere in the process and that call,
// the variable reads as the write left it. Nothing is handed back (backlog
// B830), and upstream's own setter, which the coordination server's
// DisableLogTail message calls, also disables logtail for the rest of the
// process (logtail.Disable). What the process inherited is read once, at the
// first application whatever allow is, so that the words name the variable
// only where the launch environment held it.
func applyUploadPolicy(allow bool) (Uploads, error) {
	policyRecord.Lock()
	defer policyRecord.Unlock()
	if !policyRecord.recorded {
		policyRecord.inherited, policyRecord.recorded = os.Getenv(NoLogsEnv), true
	}
	envOff, err := noLogsOff(policyRecord.inherited)
	if err != nil {
		return Uploads{}, err
	}
	if policyRecord.set {
		envknob.SetNoLogsNoSupport()
	}
	now, err := noLogsOff(os.Getenv(NoLogsEnv))
	if err != nil {
		return Uploads{}, err
	}
	u := Uploads{Allowed: allow, EnvOff: envOff}
	if !allow {
		envknob.SetNoLogsNoSupport()
		policyRecord.set = true
		return u, nil
	}
	u.LatchedOff = policyRecord.set
	u.OtherOff = now && !envOff && !policyRecord.set
	return u, nil
}

// noLogsOff is a NoLogsEnv value as Tailscale's code reads it
// (envknob.Bool: strconv.ParseBool, empty false), or ErrNoLogsEnvUnreadable
// for one it would stop the process on.
func noLogsOff(v string) (bool, error) {
	if v == "" {
		return false, nil
	}
	off, err := strconv.ParseBool(v)
	if err != nil {
		return false, ErrNoLogsEnvUnreadable
	}
	return off, nil
}

// noLogsRecord is what the process's policy knows of NoLogsEnv: whether it
// has read what the process inherited, that value, and whether the policy
// has set the variable, which it then sets again at every application until
// the process exits.
type noLogsRecord struct {
	recorded  bool
	inherited string
	set       bool
}

// policyRecord is the process's noLogsRecord, which every application of the
// policy reads and writes under its lock.
var policyRecord struct {
	sync.Mutex
	noLogsRecord
}

// Uploads is the policy's status for this node, read now: what NewServer's
// policy found, and, where the config allows the uploads and that found
// nothing holding them off, whether something in this process has turned
// them off since (the coordination server's DisableLogTail message can,
// while the node runs). It reads the environment and nothing else.
func (s *Server) Uploads() Uploads {
	u := s.uploads
	if u.Allowed && !u.Off() {
		if now, err := noLogsOff(os.Getenv(NoLogsEnv)); err == nil && now {
			u.OtherOff = true
		}
	}
	return u
}

// ForgetUploadPolicyForTest makes the policy forget what it knows of
// NoLogsEnv in this process, so that a test case starts as a process that
// has built no node and reads the variable as one it inherited (a test
// binary's cases share one process); restore puts the record back. Tests
// only.
func ForgetUploadPolicyForTest() (restore func()) {
	policyRecord.Lock()
	saved := policyRecord.noLogsRecord
	policyRecord.noLogsRecord = noLogsRecord{}
	policyRecord.Unlock()
	return func() {
		policyRecord.Lock()
		policyRecord.noLogsRecord = saved
		policyRecord.Unlock()
	}
}

// logsRequiredText is the part of upstream's refusal of a node that does not
// upload its logs, on a tailnet that requires them, that the bridge
// recognises it by: ipnlocal sets it as the node's local-log health warning
// and sends it as the error that ends tsnet's Up ("tsnet.Up: backend:
// <message>"). Matched as a substring, and only this part: the rest of
// upstream's message is advice for tailscaled's command line.
// TestTheLogsRequirementIsUpstreamsWords pins it to the tailscale.com source
// the build resolves, so a dependency bump that changes it fails there.
const logsRequiredText = "tailnet requires logging to be enabled"

// isLogsRequired reports whether msg carries upstream's refusal.
func isLogsRequired(msg string) bool { return strings.Contains(msg, logsRequiredText) }

// LogsRequiredError is a tailnet's refusal of the node because it does not
// upload Tailscale's diagnostic logs, in the bridge's words, at a start (the
// error Start returns, which Up's carried) or while the node runs (its
// health; Server.LogsRequired). It says what to change, from what held the
// uploads off in this process; the bridge never turns them on itself.
type LogsRequiredError struct {
	Uploads Uploads
	err     error // upstream's, nil for a running node's health
}

// Error is the refusal's words: what holds the uploads off in this process
// (Uploads.HeldOff), or the setting, and that a new launch of the bridge is
// what applies a change, never a restart of serve in its process.
func (e *LogsRequiredError) Error() string {
	var b strings.Builder
	b.WriteString("this tailnet requires Tailscale's logs, and the embedded node runs on it only with their uploads on: ")
	const set = "set tailscale.uploadDiagnosticLogs: true (in the console's Settings, under Networking, or in bridge.yaml)"
	switch held := e.Uploads.HeldOff(); {
	case !e.Uploads.Allowed && e.Uploads.EnvOff:
		b.WriteString(set + ", remove " + NoLogsEnv + "=true from the environment the bridge was launched with, then " +
			relaunch)
	case !e.Uploads.Allowed:
		b.WriteString(set + ", then " + relaunch + ": the setting applies at the bridge's next launch")
	case held != "":
		b.WriteString("tailscale.uploadDiagnosticLogs is true, but " + held)
	default:
		b.WriteString("tailscale.uploadDiagnosticLogs is true, and something in this process turned the uploads off " +
			"after the node started (Tailscale's coordination server can), and a new launch of the bridge may turn " +
			"them back on: " + relaunch)
	}
	b.WriteString("; the node then uploads Tailscale's diagnostic logs to Tailscale. The bridge never turns the uploads on itself")
	return b.String()
}

// Unwrap is upstream's error, for a refused start.
func (e *LogsRequiredError) Unwrap() error { return e.err }

// LogsRequired is the bridge's words for a running node the tailnet has
// refused because it uploads no logs, which upstream reports in st's health
// (and with the node's backend Stopped), or nil where st reports no such
// refusal.
func (s *Server) LogsRequired(st *ipnstate.Status) error {
	if st == nil {
		return nil
	}
	for _, h := range st.Health {
		if isLogsRequired(h) {
			return &LogsRequiredError{Uploads: s.Uploads()}
		}
	}
	return nil
}
