package main

// Shared CLI plumbing constants. Extracted so the `--config` flag's
// default value, usage string, and the "config load failed" / "open
// token store" error format strings live in one place — every
// `bridge <subcommand>` parses the same shape, and SonarCloud rule
// go:S1192 flagged the duplicates across cmd/bridge/main.go,
// cmd/bridge/token.go, and cmd/bridge/tsnet.go.
//
// These are intentionally NOT moved into internal/config — they're
// CLI-surface presentation strings (flag help text, stderr error
// formats), not part of the config package's API contract.
const (
	defaultConfigPath = "bridge.yaml"
	// configFlagUsage is a PROMISE, and the one that matters: the fallback
	// it describes is implemented by loadCLIConfig ALONE, so a command that
	// parses --config and then calls config.Load directly dies with
	// `read config ""` on any host where the operator did not pass the flag
	// — the defect TestNoSubcommandTailBypassesLoadCLIConfig sweeps for.
	//
	// It read "path to config file" and had ZERO users until #949, while 28
	// subcommands hardcoded the longer form below: the const was dead, under
	// a docblock claiming it had been extracted for go:S1192, and it carried
	// text that no longer described what the flag does. Wiring the 28 to the
	// SHORT string would have silently dropped the fallback from every
	// subcommand's help, so the value moved to the accurate one instead.
	configFlagUsage            = "path to config file (default: ./bridge.yaml, else the platform config dir)"
	configLoadFailedFormat     = "config load failed: %v\n"
	tokensFileName             = "tokens.json"
	openTokenStoreFailedFormat = "open token store: %v\n"
	// errWrapDetailFormat is the canonical Go error-wrap-plus-detail
	// format string used by mapUpdaterError to attach the underlying
	// error message to a sentinel via errors.Is.
	errWrapDetailFormat = "%w: %s"
)
