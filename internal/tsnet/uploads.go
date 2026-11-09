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
// process, kept from the policy NewServer applies so that the refusal of a
// tailnet that requires the logs can say what holds them off.
type Uploads struct {
	// Allowed is tailscale.uploadDiagnosticLogs: the config lets the node
	// upload.
	Allowed bool
	// EnvOff is whether the environment the bridge inherited held NoLogsEnv
	// true, which keeps the uploads off whatever the config allows.
	EnvOff bool
}

// Off reports whether the node uploads nothing.
func (u Uploads) Off() bool { return !u.Allowed || u.EnvOff }

// applyUploadPolicy is the bridge's diagnostic-log policy for the embedded
// node, applied to the process before the node is built: NoLogsEnv set to
// true through envknob unless allow, and the inherited environment left as
// it is when allow. An inherited true still keeps the uploads off then, and
// an inherited false never turns them on by itself, since a config that
// does not allow them overrides it. A value that is set and does not read
// as a boolean is ErrNoLogsEnvUnreadable, with the environment untouched;
// an empty one is unset, as envknob reads it.
//
// It runs once for each node a process builds (NewServer), on the
// caller's goroutine, before anything starts the node; the variable is not
// one of envknob's registered knobs, so no cache of it is read elsewhere,
// and every reader of it in Tailscale's code asks os.Getenv.
//
// A process can build a node again after one it built: the menu's "Start
// now", and bridge init's start before it, run serve in the process, and the
// menu runs it again after a stop. The inherited value is then the one the
// policy found before it first set the variable (policyRecord), while the
// variable still holds the true it set, so that a consent saved between the
// two reaches the later node, which the policy hands the inherited value back
// to, and the refusal's words name the variable only where the process's
// environment holds the uploads off. A value anything else put there since
// (upstream's own setter, which the coordination server's DisableLogTail
// message calls) is the process's, and stays.
func applyUploadPolicy(allow bool) (Uploads, error) {
	policyRecord.Lock()
	defer policyRecord.Unlock()
	value, present := os.LookupEnv(NoLogsEnv)
	ours := policyRecord.set && present && value == "true"
	if ours {
		value, present = policyRecord.before, policyRecord.beforePresent
	} else {
		policyRecord.noLogsRecord = noLogsRecord{}
	}
	var envOff bool
	if value != "" {
		v, err := strconv.ParseBool(value)
		if err != nil {
			return Uploads{}, ErrNoLogsEnvUnreadable
		}
		envOff = v
	}
	switch {
	case !allow:
		if !policyRecord.set {
			policyRecord.noLogsRecord = noLogsRecord{set: true, before: value, beforePresent: present}
		}
		envknob.SetNoLogsNoSupport()
	case ours:
		putBackNoLogsEnv(value, present)
		policyRecord.noLogsRecord = noLogsRecord{}
	}
	return Uploads{Allowed: allow, EnvOff: envOff}, nil
}

// noLogsRecord is what applyUploadPolicy did to NoLogsEnv in this process:
// whether it set the variable true, and what the variable held before it
// first did (before, and whether it was set at all).
type noLogsRecord struct {
	set           bool
	before        string
	beforePresent bool
}

// policyRecord is the process's noLogsRecord, which every application of the
// policy reads and writes under its lock.
var policyRecord struct {
	sync.Mutex
	noLogsRecord
}

// putBackNoLogsEnv sets NoLogsEnv back to what the process inherited, through
// envknob, whose record of the knobs set follows it, and unset again where it
// was not set at all. Should the unset fail (it can only on Windows, for a
// name it refuses), the variable is empty, which every reader of it takes for
// unset.
func putBackNoLogsEnv(value string, present bool) {
	if present {
		envknob.Setenv(NoLogsEnv, value)
		return
	}
	envknob.Setenv(NoLogsEnv, "")
	_ = os.Unsetenv(NoLogsEnv)
}

// ForgetUploadPolicyForTest makes the policy forget what it did to NoLogsEnv
// in this process, so that a test case which sets the variable as a process
// would inherit it has it read that way (a test binary's cases share one
// process); restore puts the record back. Tests only.
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

// Error is the refusal's words.
func (e *LogsRequiredError) Error() string {
	var b strings.Builder
	b.WriteString("this tailnet requires Tailscale's logs, and the embedded node runs on it only with their uploads on: ")
	switch {
	case !e.Uploads.Allowed && e.Uploads.EnvOff:
		b.WriteString("set tailscale.uploadDiagnosticLogs: true (in the console's Settings, under Networking, or in " +
			"bridge.yaml), also remove " + NoLogsEnv + "=true from the bridge's environment, and restart the bridge")
	case !e.Uploads.Allowed:
		b.WriteString("set tailscale.uploadDiagnosticLogs: true (in the console's Settings, under Networking, or in " +
			"bridge.yaml) and restart the bridge")
	case e.Uploads.EnvOff:
		b.WriteString("tailscale.uploadDiagnosticLogs is true, and " + NoLogsEnv + "=true in the bridge's environment " +
			"holds them off: remove it from the bridge's environment and restart the bridge")
	default:
		b.WriteString("tailscale.uploadDiagnosticLogs is true, and something in this process turned them off after it " +
			"started (Tailscale's coordination server can): restart the bridge")
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
			return &LogsRequiredError{Uploads: s.uploads}
		}
	}
	return nil
}
