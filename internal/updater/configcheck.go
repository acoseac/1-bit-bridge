package updater

import "time"

// configCheckTimeout bounds one settings check, and versionProbeTimeout one
// `version` of a backup: a candidate that never answers must not park an
// install, nor a backup the early rollback. Each is generous next to the
// load it bounds (milliseconds), for a cold start of a 30 MiB binary from an
// SD card. Vars, not consts, solely so a test can shorten them; production
// code MUST NOT mutate them.
var (
	configCheckTimeout  = 30 * time.Second
	versionProbeTimeout = 10 * time.Second
)

// The settings check a candidate answers before it is installed (backlog
// B613): `<candidate> serve --validate-config --config <path>` loads the
// settings exactly as that version's serve would at its start, and writes
// nothing, opens no store and binds nothing (cmd/bridge, checkServeConfig).
//
// The flag, the exit codes and the refusal lines are a contract between this
// updater and every later release, each of which it runs as a candidate, so
// none of them may change.
const (
	// ConfigCheckFlag is serve's flag for the check.
	ConfigCheckFlag = "validate-config"
	// ConfigCheckValidExit says the candidate loads the settings.
	ConfigCheckValidExit = 0
	// ConfigCheckRefusedExit says it refuses them: sysexits.h's EX_CONFIG.
	ConfigCheckRefusedExit = 78
	// ConfigCheckFailedExit says it could not check them, a settings file
	// it could not read. Any exit but the two above reads the same way.
	ConfigCheckFailedExit = 1
	// ConfigRefusedPrefix begins each line of a refusal on the check's
	// standard error: one reason a line, naming no path and no value.
	ConfigRefusedPrefix = "config refused: "
)

// ConfigVerdict is what a candidate's settings check answered.
type ConfigVerdict int

const (
	// ConfigValid: the candidate loads the settings.
	ConfigValid ConfigVerdict = iota
	// ConfigRefused: the candidate refuses them, for the reasons it gave.
	ConfigRefused
	// ConfigCheckUnsupported: the candidate predates the check (its serve
	// does not define the flag), so it cannot say.
	ConfigCheckUnsupported
	// ConfigCheckFailed: the check did not answer: the candidate could not
	// be run, was stopped, ran past its time, or exited another way.
	ConfigCheckFailed
)

// ConfigCheck is a candidate's answer to the settings check.
type ConfigCheck struct {
	Verdict ConfigVerdict
	// Reasons are a refusal's lines, without their prefix, bounded and
	// made printable: what the console and `bridge update` show.
	Reasons []string
	// Detail says what failed, for a failed check, in words that name no
	// path: what Install's error carries.
	Detail string
	// Output is the start of what the candidate wrote to its standard
	// error, for the bridge log alone.
	Output string
	// Err is the context's error when the check was stopped by it.
	Err error
}
