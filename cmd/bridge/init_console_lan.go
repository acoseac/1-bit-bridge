package main

import (
	"fmt"
	"io"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/consolelan"
)

// initLANFlag is --admin-lan-address as init's refusals name it.
const initLANFlag = "--admin-lan-address"

// initLANAddressFlagError is init's refusal of --admin-lan-address, or nil:
// with --public, refused as public mode's console has its login; otherwise
// Validate's own check against the admin address the run writes
// (config.ValidateConsoleLANAddress), the flag in place of the field. Neither
// names the value (D6, R10).
func initLANAddressFlagError(public bool, lan, admin string) error {
	if lan == "" {
		return nil
	}
	if public {
		return fmt.Errorf("%s: only a loopback install opens its console to the home network; "+
			"--public gives the console a login and binds where --admin-address says, so leave %s out", initLANFlag, initLANFlag)
	}
	return config.ValidateConsoleLANAddress(initLANFlag, lan, admin)
}

// lanAddressARewriteKeeps is the console's home-network address a run saves
// (the plan's §2.5, item 5): the flag's when it is named; else the install's
// where the rewrite keeps its addresses (a run that replaces the config, the
// same posture, neither address flag named) and the kept value passes its
// check against the admin address the run writes; else none, with one line
// saying why for an install that had one. listed says printKept lists it.
// A run that keeps the config whole writes nothing and never reaches this.
func lanAddressARewriteKeeps(flag string, listenNamed, adminNamed, public, replace bool, prior *priorInstallFile, admin string) (lan string, listed bool, note string) {
	if flag != "" {
		return flag, false, ""
	}
	if !replace || prior == nil || prior.AdminLANAddress == "" {
		return "", false, ""
	}
	switch {
	case public:
		return "", false, "public mode's console has a login: adminLANAddress is not kept"
	case prior.public():
		return "", false, ""
	case listenNamed || adminNamed:
		return "", false, "this run writes the console's addresses: add " + initLANFlag + " to open it on the home network again"
	}
	if config.ValidateConsoleLANAddress(config.ConsoleLANField, prior.AdminLANAddress, admin) != nil {
		return "", false, "the config this run replaces holds an adminLANAddress line its check refuses: it is not kept"
	}
	return prior.AdminLANAddress, true, ""
}

// warnLANBeforeSave is what init says before it saves a config that opens the
// console on the home network (the plan's §2.7, D15): what it opens, with no
// login, and the rollback the key costs with its recovery.
func warnLANBeforeSave(w io.Writer, a string, port int) {
	fmt.Fprintf(w, "adminLANAddress: %s also opens the console at %s; %s.\n", a, consoleLANURLText(a, port), consolelan.NoLoginWords)
	fmt.Fprintf(w, "adminLANAddress: %s\n", consolelan.RollbackWarning)
}
