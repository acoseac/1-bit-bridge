//go:build !windows

package supervision

import (
	"os"
	"strings"
)

// isSupervisedForOS checks the macOS / Linux supervisor environment
// variables. A meaningful value means we're running under a
// supervisor that will relaunch us after os.Exit (launchd KeepAlive
// / systemd Restart=always, both of which the bridge's `init`
// command writes into the unit files it ships). Otherwise →
// unsupervised; the admin UI must not promise auto-relaunch.
//
// Tested via `supervision_unix_test.go` which sets / unsets the
// vars in-process — no subprocess fork required.
func isSupervisedForOS() bool {
	// macOS launchd sets XPC_SERVICE_NAME for plist-installed jobs:
	// the value is the job's Label (e.g. com.acoseac.onebit.bridge)
	// for managed daemons / agents. BUT the entire macOS user
	// session is itself a launchd subtree, so every child of every
	// shell inherits a non-empty XPC_SERVICE_NAME from the
	// ancestor process — typically the literal string "0", which
	// launchd documents as "you have a launchd ancestor but you
	// are not a managed job." Treat "0" as unsupervised; only a
	// real Label-string answers "yes, launchd will relaunch me on
	// exit." (Bug found in the local Mac dev fixture immediately
	// after PR #124 merged: nohup-launched bridge had
	// XPC_SERVICE_NAME=0 inherited from the shell, IsSupervised()
	// returned true, admin UI lied that "Restart now" would
	// auto-relaunch — the very lie that PR was supposed to retire.
	// Reference: Apple's documented "0" sentinel from the launchd
	// source / BPSystemStartup chapter on launchd jobs.)
	if xpc := strings.TrimSpace(os.Getenv("XPC_SERVICE_NAME")); xpc != "" && xpc != "0" {
		return true
	}
	// systemd sets INVOCATION_ID for services it spawns. The
	// documented signal — companion `JOURNAL_STREAM` is set under
	// the same condition but `INVOCATION_ID` is the canonical
	// "systemd service" probe per `systemd.exec(5)`. systemd has
	// no analogous "0" sentinel — INVOCATION_ID is either an
	// actual UUID or unset.
	if os.Getenv("INVOCATION_ID") != "" {
		return true
	}
	return false
}
