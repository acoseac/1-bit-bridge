package doctor

import "github.com/acoseac/1-bit-bridge/internal/consolelan"

// Names-only shim (backlog B989's tests-first commit).

const (
	checkNameConsoleLAN         = "console-lan"
	checkNameConsoleLANRollback = "console-lan-rollback"
)

type ConsoleLANFacts struct {
	Port        int
	Running     bool
	Reported    bool
	State       consolelan.State
	FileKey     string
	FileAddress string
	EnvSet      bool
	EnvAddress  string
	HeldNow     string
	Console     bool
	NotGraded   string
}

// consoleLANChecks says nothing.
func consoleLANChecks(d Deps) []Check { return nil }
