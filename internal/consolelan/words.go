package consolelan

import "net/netip"

const (
	StateOff       = "off"
	StateListening = "listening"
	StateWaiting   = "waiting"
)

const (
	SourceFile        = "file"
	SourceEnvironment = "environment"
	SourceBoth        = "both"
)

const (
	FileKeyAbsent       = "absent"
	FileKeyEmpty        = "empty"
	FileKeyAddress      = "address"
	FileKeyNotAnAddress = "not_an_address"
)

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
)

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

func (st State) KeyInFile() bool { return false }

func Retried(reason string) bool { return false }

func URLOf(a netip.Addr, port int) string { return "" }

func ReasonWords(reason, a string, port int, iface string) string { return "" }

func ReasonHint(reason, iface string) string { return "" }

func SourceWords(source, fileKey, fileAddr string) (where, clear string) { return "", "" }

const RollbackWords = ""

const EnvironmentRollbackWords = ""

const RollbackWarning = ""

const NoLoginWords = ""

func Line(st State, port int) string { return "" }
