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
	defaultConfigPath          = "bridge.yaml"
	configFlagUsage            = "path to config file"
	configLoadFailedFormat     = "config load failed: %v\n"
	tokensFileName             = "tokens.json"
	openTokenStoreFailedFormat = "open token store: %v\n"
	// errWrapDetailFormat is the canonical Go error-wrap-plus-detail
	// format string used by mapUpdaterError to attach the underlying
	// error message to a sentinel via errors.Is.
	errWrapDetailFormat = "%w: %s"
)
