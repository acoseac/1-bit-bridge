package config

import (
	"net/netip"
)

// Names-only shim (backlog B989's tests-first commit): every name the tests
// read, each body inert until the change gives it its own.

const ConsoleLANField = "adminLANAddress"

const consoleLANEnv = "BRIDGE_ADMIN_LAN_ADDRESS"

func IsTailscaleAddr(a netip.Addr) bool { return false }

func ConsoleLANClass(a netip.Addr) (netip.Prefix, bool) { return netip.Prefix{}, false }

const (
	errLANPublic     = ""
	errLANShape      = ""
	errLANZone       = ""
	errLANMapped     = ""
	errLANUnspec     = ""
	errLANLoopback   = ""
	errLANLinkLocal  = ""
	errLANTailscale  = ""
	errLANNotPrivate = ""
	errLANPortZero   = ""
)

func ValidateConsoleLANAddress(field, value, adminAddress string) error { return nil }

func ConsoleLANPublicRefusal(field string) error { return nil }

func (c *Config) validateConsoleLAN() error { return nil }

func (c *Config) LANAddressSetByEnvironment() bool { return false }

type LANKeyState string

const (
	LANKeyAbsent       LANKeyState = "absent"
	LANKeyEmpty        LANKeyState = "empty"
	LANKeyAddress      LANKeyState = "address"
	LANKeyNotAnAddress LANKeyState = "not_an_address"
)

type LANKey struct {
	State   LANKeyState
	Address string
}

func (k LANKey) InFile() bool { return false }

func LANKeyOf(raw []byte) (LANKey, error) { return LANKey{State: LANKeyAbsent}, nil }

func (c *Config) FileLANKey() LANKey { return LANKey{State: LANKeyAbsent} }

const (
	ConsoleLANSourceFile        = "file"
	ConsoleLANSourceEnvironment = "environment"
	ConsoleLANSourceBoth        = "both"
)

func (c *Config) ConsoleLANSource() string { return "" }
