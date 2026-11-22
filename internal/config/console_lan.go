package config

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/netip"

	"gopkg.in/yaml.v3"
)

// ConsoleLANField is the yaml key of the console's home-network address
// (backlog B989): a private IP address of this box, on which a loopback
// install also serves its console, at adminAddress's port, to devices on
// the network that address belongs to, with no login.
const ConsoleLANField = "adminLANAddress"

// consoleLANEnv is the variable the derivation gives ConsoleLANField
// (envName), named in a refusal of the value it set.
const consoleLANEnv = "BRIDGE_ADMIN_LAN_ADDRESS"

// The private classes a console's home-network address may be in, and a
// peer the console admits there (ConsoleLANClass): the three RFC 1918
// blocks and the IPv6 unique local addresses.
var consoleLANClasses = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
}

// tailscaleRanges are Tailscale's ranges, which no console address and no
// peer of the home-network socket is in, whatever a prefix says: the
// CGNAT block its IPv4 addresses come from, and its ULA prefix, which lies
// inside fc00::/7 (backlog B989's I9).
var tailscaleRanges = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// IsTailscaleAddr reports whether a, unmapped, lies in one of Tailscale's
// ranges (tailscaleRanges).
func IsTailscaleAddr(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	for _, p := range tailscaleRanges {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ConsoleLANClass is the private class a lies in, one of the three RFC 1918
// blocks or fc00::/7, and false for any other address, Tailscale's ranges
// included. Validate refuses an adminLANAddress outside every class, and the
// console's home-network socket admits a peer only inside its address's
// class, so the two refuse the same addresses.
func ConsoleLANClass(a netip.Addr) (netip.Prefix, bool) {
	a = a.Unmap().WithZone("")
	if IsTailscaleAddr(a) {
		return netip.Prefix{}, false
	}
	for _, p := range consoleLANClasses {
		if p.Contains(a) {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

// The refusals of a console's home-network address. Each names why, never
// the value (backlog B989's R10): what is refused can be a secret pasted
// onto the wrong line, and these reach the journal, bridge doctor, the
// updater's settings check and the console.
const (
	errLANPublic = "only a loopback install opens its console to the home network; public mode's console " +
		"has a login and binds where adminAddress says, so leave adminLANAddress unset"
	errLANShape = "give this box's home-network IP address alone, with no name, port or brackets: the console " +
		"listens there on adminAddress's port"
	errLANZone       = "an address with a zone is link-local, and a link-local address is not a home-network address"
	errLANMapped     = "write an IPv4 address in its IPv4 form"
	errLANUnspec     = "0.0.0.0 and :: listen on every network this box is on, the internet's included; give its address on the home network"
	errLANLoopback   = "a loopback address is this machine's alone, and the console answers there already (adminAddress)"
	errLANLinkLocal  = "a link-local address changes from link to link and is not a home-network address"
	errLANTailscale  = "these are Tailscale's ranges (and the first is carrier-grade NAT's), not the home network's"
	errLANNotPrivate = "not a private address: give this box's address on the home network, in 10.0.0.0/8, " +
		"172.16.0.0/12 or 192.168.0.0/16, or an IPv6 unique local address (fc00::/7)"
	errLANPortZero = "adminAddress names port 0, which the system picks at each start; give the console a fixed " +
		"port, which a device on the home network can open"
)

// checkConsoleLANAddr is the address half of ValidateConsoleLANAddress: the
// value as a home-network IP address of this box's, or why not, in the
// table's order (the plan's §2.2), the first row that applies refusing.
func checkConsoleLANAddr(value string) (netip.Addr, error) {
	a, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Addr{}, errors.New(errLANShape)
	}
	switch {
	case a.Zone() != "":
		return netip.Addr{}, errors.New(errLANZone)
	case a.Is4In6():
		return netip.Addr{}, errors.New(errLANMapped)
	case a.IsUnspecified():
		return netip.Addr{}, errors.New(errLANUnspec)
	case a.IsLoopback():
		return netip.Addr{}, errors.New(errLANLoopback)
	case a.IsLinkLocalUnicast():
		return netip.Addr{}, errors.New(errLANLinkLocal)
	case IsTailscaleAddr(a):
		return netip.Addr{}, errors.New(errLANTailscale)
	}
	if _, ok := ConsoleLANClass(a); !ok {
		return netip.Addr{}, errors.New(errLANNotPrivate)
	}
	return a, nil
}

// ValidateConsoleLANAddress checks value as a loopback install's
// adminLANAddress beside the admin address it writes or holds: an IP
// address alone, private, of no zone, mapping, loopback, link-local or
// Tailscale range, beside an admin address whose port is not 0. Its error
// names field, and why, and never the value. Validate checks the field with
// it, and bridge init its --admin-lan-address, so the file and the flag take
// one rule. Public mode's refusal is the caller's (errLANPublic).
func ValidateConsoleLANAddress(field, value, adminAddress string) error {
	if _, err := checkConsoleLANAddr(value); err != nil {
		return errors.New(field + ": " + err.Error())
	}
	if adminAddress == "" {
		adminAddress = DefaultAdminAddress
	}
	if _, port, err := net.SplitHostPort(adminAddress); err == nil && port == "0" {
		return errors.New(field + ": " + errLANPortZero)
	}
	return nil
}

// ConsoleLANPublicRefusal is public mode's refusal of the field (or of the
// flag named field), for bridge init.
func ConsoleLANPublicRefusal(field string) error {
	return errors.New(field + ": " + errLANPublic)
}

// consoleLANFieldWord is the field as a refusal names it: with the
// variable beside it where BRIDGE_ADMIN_LAN_ADDRESS set the leaf (backlog
// B989's D7), so a refusal says where the value came from and still never
// what it is.
func (c *Config) consoleLANFieldWord() string {
	if c.LANAddressSetByEnvironment() {
		return ConsoleLANField + ", set by " + consoleLANEnv
	}
	return ConsoleLANField
}

// validateConsoleLAN is Validate's check of AdminLANAddress (backlog B989).
func (c *Config) validateConsoleLAN() error {
	if c.AdminLANAddress == "" {
		return nil
	}
	if c.IsPublic() {
		return errors.New(c.consoleLANFieldWord() + ": " + errLANPublic)
	}
	return ValidateConsoleLANAddress(c.consoleLANFieldWord(), c.AdminLANAddress, c.AdminAddress)
}

// LANAddressSetByEnvironment reports whether BRIDGE_ADMIN_LAN_ADDRESS set
// AdminLANAddress at the load that made c (envSet).
func (c *Config) LANAddressSetByEnvironment() bool {
	for _, p := range c.envSet {
		if p == ConsoleLANField {
			return true
		}
	}
	return false
}

// LANKeyState is what a config file's adminLANAddress key holds, apart
// from what the load made of it (backlog B989's §2.6 and §2.7).
type LANKeyState string

const (
	// LANKeyAbsent is a file with no such key.
	LANKeyAbsent LANKeyState = "absent"
	// LANKeyEmpty is the key with an empty value or null: no listener, and
	// still a key every bridge before 0.2.4 refuses.
	LANKeyEmpty LANKeyState = "empty"
	// LANKeyAddress is the key holding an address the field's check takes.
	LANKeyAddress LANKeyState = "address"
	// LANKeyNotAnAddress is the key holding anything else, which nothing
	// repeats: it can be a secret pasted onto the wrong line.
	LANKeyNotAnAddress LANKeyState = "not_an_address"
)

// LANKey is a config file's adminLANAddress key as its bytes hold it.
// Address is set for LANKeyAddress alone.
type LANKey struct {
	State   LANKeyState
	Address string
}

// InFile reports whether the file holds the key at all, whatever its value:
// what every bridge before 0.2.4 refuses (its decoding takes no key it does
// not know).
func (k LANKey) InFile() bool {
	return k.State != "" && k.State != LANKeyAbsent
}

// lanKeyProbe is the one key LANKeyOf reads. A yaml.Node field is handed
// the node as it is, before yaml.v3 reads any value, so a key with an
// empty value or null is told from one that is not there.
type lanKeyProbe struct {
	Key yaml.Node `yaml:"adminLANAddress"`
}

// LANKeyOf reads a config file's adminLANAddress key from its bytes,
// without the loader's refusal of unknown keys and before any variable:
// whether the key is there, empty or not, and what it holds, which the
// value a load gives does not show (an empty value is no key once loaded,
// and a variable replaces the file's value). Aliases are resolved and merge
// keys taken as yaml.v3 takes them in the load itself (backlog B989's I1).
// An error is bytes that do not decode, which Load refuses too.
func LANKeyOf(raw []byte) (LANKey, error) {
	var probe lanKeyProbe
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&probe); err != nil {
		if errors.Is(err, io.EOF) {
			return LANKey{State: LANKeyAbsent}, nil
		}
		return LANKey{}, err
	}
	n := &probe.Key
	for depth := 0; n.Kind == yaml.AliasNode && n.Alias != nil && depth < 64; depth++ {
		n = n.Alias
	}
	switch {
	case n.Kind == 0:
		return LANKey{State: LANKeyAbsent}, nil
	case n.Kind != yaml.ScalarNode:
		return LANKey{State: LANKeyNotAnAddress}, nil
	case n.ShortTag() == "!!null" || n.Value == "":
		return LANKey{State: LANKeyEmpty}, nil
	}
	if _, err := checkConsoleLANAddr(n.Value); err != nil {
		return LANKey{State: LANKeyNotAnAddress}, nil
	}
	return LANKey{State: LANKeyAddress, Address: n.Value}, nil
}

// FileLANKey is the adminLANAddress key of the file the load that made c
// read, as its bytes held it (LANKeyOf), absent for a config no load made.
func (c *Config) FileLANKey() LANKey {
	if c.fileLANKey.State == "" {
		return LANKey{State: LANKeyAbsent}
	}
	return c.fileLANKey
}

// Where a console's home-network address came from, at the load that made a
// config (backlog B989's D7 and D14).
const (
	// ConsoleLANSourceFile is the file's key, no variable set.
	ConsoleLANSourceFile = "file"
	// ConsoleLANSourceEnvironment is BRIDGE_ADMIN_LAN_ADDRESS over a file
	// with no key.
	ConsoleLANSourceEnvironment = "environment"
	// ConsoleLANSourceBoth is the variable over a file that holds the key,
	// whatever the key holds.
	ConsoleLANSourceBoth = "both"
)

// ConsoleLANSource is where c's AdminLANAddress came from: "" for none, else
// one of ConsoleLANSourceFile, ConsoleLANSourceEnvironment and
// ConsoleLANSourceBoth.
func (c *Config) ConsoleLANSource() string {
	switch env, inFile := c.LANAddressSetByEnvironment(), c.FileLANKey().InFile(); {
	case c.AdminLANAddress == "":
		return ""
	case env && inFile:
		return ConsoleLANSourceBoth
	case env:
		return ConsoleLANSourceEnvironment
	default:
		return ConsoleLANSourceFile
	}
}
