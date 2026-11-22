package doctor

import (
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/consolelan"
)

// The console's home-network lines (backlog B989, the plan's §5.3), right
// after port-admin.
const (
	checkNameConsoleLAN         = "console-lan"
	checkNameConsoleLANRollback = "console-lan-rollback"
)

// adminPortCheckAt is port-admin's place in Run's list, after which the
// console's home-network lines go.
const adminPortCheckAt = 4

// ConsoleLANFacts is what a doctor knows of the console's home-network
// posture (the plan's §5.3): what the running bridge reports of its socket,
// which is the service's and its start's, and apart from it what bridge.yaml
// says now and what this process's own environment sets, each said as what it
// is, since a doctor run from a shell need not share the service's
// environment.
type ConsoleLANFacts struct {
	// Port is adminAddress's port, the home-network socket's.
	Port int
	// Running says a bridge serves this install now; Reported that it
	// said where its console answers, a bridge older than this change
	// saying nothing; State is what it said.
	Running  bool
	Reported bool
	State    consolelan.State
	// FileKey is bridge.yaml's adminLANAddress as the file reads now
	// (consolelan.FileKey*), and FileAddress its address where it holds one.
	FileKey     string
	FileAddress string
	// EnvSet says this process's environment sets BRIDGE_ADMIN_LAN_ADDRESS
	// to something; EnvAddress is its value where the field's check takes it.
	EnvSet     bool
	EnvAddress string
	// HeldNow says whether the box holds the address the next start would
	// bind, "yes" or "no", or "" where it was not read.
	HeldNow string
	// Console is the console's own doctor, whose process is the running
	// bridge and whose environment is the service's.
	Console bool
	// NotGraded is why the config was not read, "" where it was; neither
	// line runs then, as the port checks do not.
	NotGraded string
}

// consoleLANChecks are the two lines, each only where it has something to
// say.
func consoleLANChecks(d Deps) []Check {
	f := d.ConsoleLAN
	if f == nil || f.NotGraded != "" {
		return nil
	}
	var out []Check
	if c, ok := consoleLANLine(*f); ok {
		out = append(out, c)
	}
	if c, ok := consoleLANRollbackLine(*f); ok {
		out = append(out, c)
	}
	return out
}

// fileSays reports whether bridge.yaml holds the key now, empty or not.
func (f ConsoleLANFacts) fileSays() bool {
	return f.FileKey != "" && f.FileKey != consolelan.FileKeyAbsent
}

// consoleLANLine is console-lan, from the running bridge's report and, apart,
// what the file and this process's environment say.
func consoleLANLine(f ConsoleLANFacts) (Check, bool) {
	switch {
	case f.Running && !f.Reported:
		if !f.fileSays() && !f.EnvSet {
			return Check{}, false
		}
		return ok(checkNameConsoleLAN, "not checked: the running bridge does not say where its console answers"), true
	case f.Running:
		return runningConsoleLANLine(f)
	}
	// Not running: what the next start would do, from what this process sees.
	addr := f.EnvAddress
	source := "set by BRIDGE_ADMIN_LAN_ADDRESS in this shell's environment"
	if addr == "" && f.FileKey == consolelan.FileKeyAddress {
		addr, source = f.FileAddress, "set in bridge.yaml (adminLANAddress)"
	}
	if addr == "" {
		return Check{}, false
	}
	summary := "when the bridge runs, its console also answers on the home network at " + urlOfText(addr, f.Port) +
		", with no password"
	switch f.HeldNow {
	case "yes":
		summary += "; this box holds " + addr + " now"
	case "no":
		summary += "; this box does not hold " + addr + " now"
	}
	summary += "; " + source + ", and the service's environment, which may differ, is the one that counts"
	return warn(checkNameConsoleLAN, summary, "to keep the console on this machine, remove the adminLANAddress line from bridge.yaml "+
		"and leave BRIDGE_ADMIN_LAN_ADDRESS unset where the service is started; don't forward this port on a router"), true
}

// runningConsoleLANLine is console-lan for a running bridge that reported
// its socket: the socket's state and its start's source, then what the file
// says now where it differs, and this shell's variable where it is not the
// running bridge's (D14).
func runningConsoleLANLine(f ConsoleLANFacts) (Check, bool) {
	st := f.State
	var summary, hint string
	switch st.State {
	case consolelan.StateListening:
		summary = "the console answers on the home network at " + st.URL + ", with no password and over plain HTTP: " +
			"anyone on that network can watch it and change the bridge's settings, music and devices"
		if st.Interface != "" {
			summary += "; held to " + st.Interface
			if st.Hold != "" {
				summary += " (" + st.Hold + ")"
			}
		}
		if where, clear := consolelan.SourceWords(st.Source, st.FileKeyAtBoot, st.FileAddressAtBoot); where != "" {
			summary += ". It is " + where
			hint = clear + "; don't forward this port on a router"
		}
	case consolelan.StateWaiting:
		summary = "the console answers on this machine only: " + consolelan.ReasonWords(st.Reason, st.Address, f.Port, st.Interface)
		if consolelan.Retried(st.Reason) {
			summary += "; tried again every 5 s"
		}
		if where, _ := consolelan.SourceWords(st.Source, st.FileKeyAtBoot, st.FileAddressAtBoot); where != "" {
			summary += ". It is " + where
		}
		hint = consolelan.ReasonHint(st.Reason, st.Interface)
	default:
		// Off at its start: only what changed since says anything.
	}
	var more []string
	if d := differsSinceStart(f); d != "" {
		more = append(more, d)
	}
	if !f.Console {
		envServed := st.Source == consolelan.SourceEnvironment || st.Source == consolelan.SourceBoth
		if f.EnvSet != envServed {
			more = append(more, "this shell's BRIDGE_ADMIN_LAN_ADDRESS is not the running bridge's: the service's environment is the one that counts")
		}
	}
	if summary == "" && len(more) == 0 {
		return Check{}, false
	}
	if summary == "" {
		summary = "the console answers on this machine only"
	}
	for _, m := range more {
		summary += "; " + m
	}
	if hint == "" {
		hint = "the running bridge keeps what it took at its start until it restarts"
	}
	return warn(checkNameConsoleLAN, summary, hint), true
}

// differsSinceStart says, where bridge.yaml's key reads otherwise now than at
// the running bridge's start, both and what the next start binds.
func differsSinceStart(f ConsoleLANFacts) string {
	st := f.State
	bootKey := st.FileKeyAtBoot
	if bootKey == "" {
		bootKey = consolelan.FileKeyAbsent
	}
	nowKey := f.FileKey
	if nowKey == "" {
		nowKey = consolelan.FileKeyAbsent
	}
	if bootKey == nowKey && st.FileAddressAtBoot == f.FileAddress {
		return ""
	}
	started := "the running bridge started with no adminLANAddress in bridge.yaml"
	switch bootKey {
	case consolelan.FileKeyAddress:
		started = "the running bridge started with bridge.yaml's adminLANAddress " + st.FileAddressAtBoot
	case consolelan.FileKeyEmpty:
		started = "the running bridge started with bridge.yaml's adminLANAddress empty"
	case consolelan.FileKeyNotAnAddress:
		started = "the running bridge started with a bridge.yaml adminLANAddress that does not read as an address"
	}
	if st.Address != "" && st.Source != "" {
		where, _ := consolelan.SourceWords(st.Source, st.FileKeyAtBoot, st.FileAddressAtBoot)
		started += ", serving " + st.Address + " " + where
	}
	now := "bridge.yaml holds no adminLANAddress now"
	switch nowKey {
	case consolelan.FileKeyAddress:
		now = "bridge.yaml's adminLANAddress is " + f.FileAddress + " now"
	case consolelan.FileKeyEmpty:
		now = "bridge.yaml's adminLANAddress is empty now"
	case consolelan.FileKeyNotAnAddress:
		now = "bridge.yaml's adminLANAddress does not read as an address now"
	}
	next := "the next start serves the console on this machine only"
	envServed := st.Source == consolelan.SourceEnvironment || st.Source == consolelan.SourceBoth
	switch {
	case envServed:
		next = "the next start binds BRIDGE_ADMIN_LAN_ADDRESS again where the service's environment still sets it, over bridge.yaml"
	case nowKey == consolelan.FileKeyAddress:
		next = "the next start binds " + f.FileAddress
	case nowKey == consolelan.FileKeyNotAnAddress:
		next = "the next start refuses bridge.yaml"
	}
	return started + "; " + now + "; " + next
}

// consoleLANRollbackLine is console-lan-rollback: while bridge.yaml holds the
// key now, empty or not, whatever the running bridge does (the plan's §2.7).
func consoleLANRollbackLine(f ConsoleLANFacts) (Check, bool) {
	if !f.fileSays() {
		return Check{}, false
	}
	holds := "bridge.yaml holds adminLANAddress"
	switch f.FileKey {
	case consolelan.FileKeyEmpty:
		holds = "bridge.yaml holds adminLANAddress, empty"
	case consolelan.FileKeyNotAnAddress:
		holds = "bridge.yaml holds adminLANAddress, which does not read as an address"
	}
	return warn(checkNameConsoleLANRollback,
		holds+": bridge 0.2.3 and earlier refuse this file, so an update's automatic rollback to one of them is not made "+
			"and one installed by hand will not start",
		"remove the whole adminLANAddress line before you go back to one; emptying it is not enough. "+
			"Where the posture is wanted, BRIDGE_ADMIN_LAN_ADDRESS costs no rollback"), true
}

// urlOfText is the console's URL at an address the field took, as text.
func urlOfText(addr string, port int) string {
	if strings.Contains(addr, ":") {
		addr = "[" + addr + "]"
	}
	return "http://" + addr + ":" + strconv.Itoa(port)
}
