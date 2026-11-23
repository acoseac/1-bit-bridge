package consolelan

import (
	"net"
	"net/netip"
	"strconv"
)

// The home-network socket's states, as State.State names them.
const (
	StateOff       = "off"
	StateListening = "listening"
	StateWaiting   = "waiting"
)

// Where the address came from at the start (State.Source), as
// config.ConsoleLANSource records it.
const (
	SourceFile        = "file"
	SourceEnvironment = "environment"
	SourceBoth        = "both"
)

// What bridge.yaml's adminLANAddress key held at the start
// (State.FileKeyAtBoot), as config.LANKey reads it.
const (
	FileKeyAbsent       = "absent"
	FileKeyEmpty        = "empty"
	FileKeyAddress      = "address"
	FileKeyNotAnAddress = "not_an_address"
)

// The reasons a socket is not listening beside the holds' own, each a code
// the console, the banner and both doctors word through ReasonWords.
const (
	ReasonAddressNotOnBox  = "address_not_on_this_box"
	ReasonAddressOnTwo     = "address_on_two_interfaces"
	ReasonAddressNoNetwork = "address_has_no_network"
	ReasonAddressOnTunnel  = "address_on_a_tunnel"
	ReasonInterfaceDown    = "interface_down"
	ReasonPortInUse        = "port_in_use"
	ReasonPortNotPermitted = "port_not_permitted"
	ReasonBindFailed       = "bind_failed"
	ReasonStoppedServing   = "stopped_serving"
	ReasonStopping         = "stopping"
	ReasonReopening        = "reopening"
)

// State is the home-network socket as the serving process reports it, the
// "lan" of GET /api/console (the plan's §5.2): the socket's state and the
// facts the start recorded, never what bridge.yaml says now. Address is the
// address the start took, whichever source gave it; FileAddressAtBoot the
// file's own, present only where FileKeyAtBoot is "address", so a value the
// field's check refuses is never repeated.
type State struct {
	State             string `json:"state"`
	Address           string `json:"address,omitempty"`
	URL               string `json:"url,omitempty"`
	NameURL           string `json:"nameUrl,omitempty"`
	Reason            string `json:"reason"`
	Source            string `json:"source"`
	FileKeyAtBoot     string `json:"fileKeyAtBoot"`
	FileAddressAtBoot string `json:"fileAddressAtBoot,omitempty"`
	Network           string `json:"network,omitempty"`
	Interface         string `json:"interface,omitempty"`
	Hold              string `json:"hold,omitempty"`
}

// KeyInFile reports whether bridge.yaml held the key at the start, empty or
// not: what costs the rollback (the plan's §2.7).
func (st State) KeyInFile() bool {
	return st.FileKeyAtBoot != "" && st.FileKeyAtBoot != FileKeyAbsent
}

// Retried reports whether a waiting socket is tried again: every reason but
// not_supported_here, which nothing at run time changes.
func Retried(reason string) bool { return reason != ReasonNotSupportedHere }

// URLOf is the console's URL at a and port.
func URLOf(a netip.Addr, port int) string {
	return "http://" + net.JoinHostPort(a.String(), strconv.Itoa(port))
}

// ReasonWords are a reason's words for the console's line, the banner, the
// log and both doctors. a and port fill them; an iface names the interface
// a Windows refusal is of, where it has one.
func ReasonWords(reason, a string, port int, iface string) string {
	on := "the interface " + a + " is on"
	switch reason {
	case ReasonNotSupportedHere:
		return "this platform cannot hold the console to " + on
	case ReasonInterfacesUnreadable:
		return "this box's interfaces could not be read whole"
	case ReasonAddressNotOnBox:
		return a + " is not on this box"
	case ReasonAddressOnTwo:
		return a + " is on more than one interface"
	case ReasonAddressNoNetwork:
		return a + " has no network on its interface (its mask is no prefix)"
	case ReasonAddressOnTunnel:
		return a + " is on a loopback or point-to-point interface"
	case ReasonInterfaceDown:
		return on + " is down"
	case ReasonInterfaceNotStrongHost:
		who := "an interface"
		if iface != "" {
			who = iface
		}
		return who + " takes packets for other interfaces' addresses, sends from another interface's address, or forwards; " +
			"the console opens on the home network only where every interface keeps Windows' strong host model, its default"
	case ReasonHostModelUnreadable:
		return "Windows' interface table could not be read"
	case ReasonInterfaceUnknown:
		return on + " has no index"
	case ReasonRestrictionRefused:
		return "the kernel refused to hold the console to " + on + " (Linux before 5.7 needs CAP_NET_RAW)"
	case ReasonRestrictionUnconfirmed:
		return "the kernel did not confirm the console's hold on " + on
	case ReasonPortInUse:
		return "port " + strconv.Itoa(port) + " is taken on " + a
	case ReasonPortNotPermitted:
		return "this user may not listen on port " + strconv.Itoa(port)
	case ReasonStoppedServing:
		return "the home-network socket stopped serving"
	case ReasonStopping:
		return "the bridge is stopping"
	case ReasonBindFailed:
		return "the console could not listen on " + a + " at port " + strconv.Itoa(port)
	}
	return "the home-network socket is not open (" + reason + ")"
}

// ReasonHint is what to do about a reason, for the doctor's hint.
func ReasonHint(reason, iface string) string {
	switch reason {
	case ReasonNotSupportedHere:
		return "keep the console on this machine, or give adminLANAddress an IPv4 address of the home network (macOS holds no IPv6 address to its interface)"
	case ReasonInterfacesUnreadable:
		return "the bridge reads the interfaces again every 5 s; if this stays, see the bridge's log"
	case ReasonAddressNotOnBox:
		return "give the box this address (a fixed address, or a DHCP reservation), or set adminLANAddress to an address it holds"
	case ReasonAddressOnTwo:
		return "keep the address on one interface"
	case ReasonAddressNoNetwork:
		return "give the address its network's mask on its interface"
	case ReasonAddressOnTunnel:
		return "give adminLANAddress the box's address on its home-network interface"
	case ReasonInterfaceDown:
		return "bring the interface up; the bridge tries again every 5 s"
	case ReasonInterfaceNotStrongHost:
		who := "that interface"
		if iface != "" {
			who = iface
		}
		return "turn weak host send, weak host receive and forwarding off on " + who + ", or keep the console on this machine"
	case ReasonHostModelUnreadable:
		return "the bridge reads the table again every 5 s; if this stays, see the bridge's log"
	case ReasonRestrictionRefused:
		return "run a Linux kernel of 5.7 or later, or give the service CAP_NET_RAW, or keep the console on this machine"
	case ReasonPortInUse:
		return "stop what listens on that port at that address, or move adminAddress's port"
	case ReasonPortNotPermitted:
		return "give adminAddress a port of 1024 or more"
	}
	return "see the bridge's log"
}

// SourceWords are where the address came from at the start, and what
// clears it, in one wording for both doctors, the console's line and the
// banner (the plan's §2.6). For "both", the file's key at the start says
// what unsetting the variable alone does; fileAddr is the file's own
// address where the key held one.
func SourceWords(source, fileKey, fileAddr string) (where, clear string) {
	switch source {
	case SourceFile:
		return "set in bridge.yaml (adminLANAddress)",
			"to turn it off, remove the adminLANAddress line from bridge.yaml, the key itself, and restart"
	case SourceEnvironment:
		return "set by BRIDGE_ADMIN_LAN_ADDRESS in the bridge's environment",
			"to turn it off, unset BRIDGE_ADMIN_LAN_ADDRESS where the service is started " +
				"(the systemd unit's Environment= or EnvironmentFile=, launchd's EnvironmentVariables, " +
				"the container's -e or Compose's environment:) and restart; a variable set to the empty string sets nothing"
	case SourceBoth:
		where = "set by BRIDGE_ADMIN_LAN_ADDRESS in the bridge's environment, over bridge.yaml's adminLANAddress"
		clear = "to turn it off, unset BRIDGE_ADMIN_LAN_ADDRESS where the service is started and remove the adminLANAddress line from bridge.yaml, then restart"
		switch fileKey {
		case FileKeyAddress:
			clear += "; unsetting the variable alone opens the console at bridge.yaml's address"
			if fileAddr != "" {
				clear += ", " + fileAddr + ","
			}
			clear += " at the next start"
		case FileKeyEmpty:
			clear += "; unsetting the variable alone turns the console off the home network at the next start; " +
				"bridge.yaml's empty adminLANAddress line still costs the rollback: remove it"
		case FileKeyNotAnAddress:
			clear += "; unsetting the variable alone makes the next start refuse bridge.yaml: its adminLANAddress does not read as an address"
		}
		return where, clear
	}
	return "", ""
}

// RollbackWords is the rollback the key costs, for the console's line and
// the banner (the plan's §2.7).
const RollbackWords = "bridge 0.2.3 and earlier refuse this bridge.yaml while it holds the adminLANAddress line, even empty: " +
	"remove the line before you go back to one"

// EnvironmentRollbackWords is the variable's case, said where the source is
// the environment alone.
const EnvironmentRollbackWords = "set by BRIDGE_ADMIN_LAN_ADDRESS: bridge 0.2.3 and earlier ignore that variable, " +
	"so a rollback to one of them starts with the console on this machine only"

// RollbackWarning is what bridge init says before it saves a file with the
// key, and the config example's sentence (the plan's §2.7, D15).
const RollbackWarning = "While this key is in the file, even empty, bridge 0.2.3 and earlier refuse the file: " +
	"an update's automatic rollback to one of them is not made, and one installed by hand will not start. " +
	"Remove the whole line before you go back to one. BRIDGE_ADMIN_LAN_ADDRESS instead costs no rollback: " +
	"earlier versions ignore it and keep the console on this machine."

// NoLoginWords is what the posture opens, said by bridge init before it
// saves and in the doctor's line.
const NoLoginWords = "the console then also answers on the home network, with no password and over plain HTTP: " +
	"anyone on that network can watch it and change the bridge's settings, music and devices"

// Line is the console's menu line for st (the plan's §5.1), "" for none:
// the posture's words, the source's and what clears it, and the rollback
// sentence where the file held the key at the start.
func Line(st State, port int) string {
	var out string
	switch st.State {
	case StateListening:
		out = "Open on your home network at " + st.URL + ", with no password: anyone on that network can use this console."
	case StateWaiting:
		out = "Not open on your home network yet: " + ReasonWords(st.Reason, st.Address, port, st.Interface)
		if Retried(st.Reason) {
			out += " (tried again every 5 s)"
		}
		out += ". Until then this console answers on this machine only."
	}
	if out != "" {
		if where, clear := SourceWords(st.Source, st.FileKeyAtBoot, st.FileAddressAtBoot); where != "" {
			out += " It is " + where + "; " + clear + "."
		}
		if st.Source == SourceEnvironment && !st.KeyInFile() {
			out += " " + upperFirst(EnvironmentRollbackWords) + "."
		}
	}
	if st.KeyInFile() {
		if out != "" {
			out += " "
		}
		out += upperFirst(RollbackWords) + "."
	}
	return out
}

func upperFirst(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}
