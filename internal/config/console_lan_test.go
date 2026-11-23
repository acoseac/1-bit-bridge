package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// lanSecret is planted in every refused value that can carry it, so a
// refusal that quotes its value is seen (backlog B989's R10, D6).
const lanSecret = "hunter2SECRETtoken"

// lanRefusalRows are the plan's §2.2 rows a value alone refuses, in its
// order, each with the words its refusal says.
var lanRefusalRows = []struct {
	name, value, words string
}{
	{"a name", "box-" + lanSecret + ".local", errLANShape},
	{"a secret pasted on the line", lanSecret, errLANShape},
	{"an address with a port", "192.168.1.20:7789", errLANShape},
	{"brackets", "[fd12:3456::7]", errLANShape},
	{"white space", " 192.168.1.20", errLANShape},
	{"a zone", "fe80::1%" + lanSecret, errLANZone},
	{"an IPv4-mapped address", "::ffff:192.168.1.20", errLANMapped},
	{"0.0.0.0", "0.0.0.0", errLANUnspec},
	{"::", "::", errLANUnspec},
	{"IPv4 loopback", "127.0.0.1", errLANLoopback},
	{"IPv6 loopback", "::1", errLANLoopback},
	{"IPv4 link-local", "169.254.10.20", errLANLinkLocal},
	{"IPv6 link-local", "fe80::1", errLANLinkLocal},
	{"Tailscale's IPv4 range", "100.100.1.1", errLANTailscale},
	{"Tailscale's IPv6 range", "fd7a:115c:a1e0::5", errLANTailscale},
	{"a public address", "8.8.8.8", errLANNotPrivate},
	{"multicast", "224.0.0.251", errLANNotPrivate},
	{"a public IPv6 address", "2001:db8::1", errLANNotPrivate},
	{"172.32/12's neighbour", "172.32.0.1", errLANNotPrivate},
}

// TestTheLANAddressIsRefusedForEachReasonWithoutItsValue holds Validate to
// the plan's §2.2 table: each row refused through Load in its own words,
// naming the field and never the value, nor the secret planted in it; and
// the two rows that ask more than the value, public mode and an adminAddress
// on port 0.
func TestTheLANAddressIsRefusedForEachReasonWithoutItsValue(t *testing.T) {
	for _, r := range lanRefusalRows {
		t.Run(r.name, func(t *testing.T) {
			path, _ := writeConfig(t, "libraryRoots:\n  - {{LIBRARY_ROOT}}\nadminLANAddress: "+yamlStr(r.value)+"\n")
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load took adminLANAddress %q", r.value)
			}
			if !strings.Contains(err.Error(), ConsoleLANField+": "+r.words) {
				t.Fatalf("refused %q, want %q", err, ConsoleLANField+": "+r.words)
			}
			if (strings.Contains(err.Error(), r.value) && !strings.Contains(r.words, r.value)) || strings.Contains(err.Error(), lanSecret) {
				t.Fatalf("the refusal %q names the value", err)
			}
		})
	}
	t.Run("an adminAddress on port 0", func(t *testing.T) {
		path, _ := writeConfig(t, "libraryRoots:\n  - {{LIBRARY_ROOT}}\nadminAddress: \"127.0.0.1:0\"\nadminLANAddress: \"192.168.1.20\"\n")
		_, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), ConsoleLANField+": "+errLANPortZero) {
			t.Fatalf("refused %v, want the port-0 refusal", err)
		}
		if strings.Contains(err.Error(), "192.168.1.20") {
			t.Fatalf("the refusal %q names the value", err)
		}
	})
	t.Run("public mode", func(t *testing.T) {
		path, _ := writeConfig(t, "libraryRoots:\n  - {{LIBRARY_ROOT}}\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		cfg.AdminLANAddress = "192.168.1.20"
		cfg.Deployment.Mode = "public"
		err = cfg.validateConsoleLAN()
		if err == nil || err.Error() != ConsoleLANField+": "+errLANPublic {
			t.Fatalf("refused %v, want the public-mode refusal", err)
		}
	})
}

// TestAVariablesLANAddressIsRefusedAsTheFilesIs: the same rows set by
// BRIDGE_ADMIN_LAN_ADDRESS over a file without the key, refused in the same
// words with the variable named beside the field, and the value nowhere (D7).
func TestAVariablesLANAddressIsRefusedAsTheFilesIs(t *testing.T) {
	for _, r := range lanRefusalRows {
		if strings.TrimSpace(r.value) != r.value {
			continue // the variable's reader takes the value as it is
		}
		t.Run(r.name, func(t *testing.T) {
			t.Setenv(consoleLANEnv, r.value)
			path, _ := writeConfig(t, "libraryRoots:\n  - {{LIBRARY_ROOT}}\n")
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load took %s=%q", consoleLANEnv, r.value)
			}
			want := ConsoleLANField + ", set by " + consoleLANEnv + ": " + r.words
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("refused %q, want %q", err, want)
			}
			if (strings.Contains(err.Error(), r.value) && !strings.Contains(r.words, r.value)) || strings.Contains(err.Error(), lanSecret) {
				t.Fatalf("the refusal %q names the value", err)
			}
		})
	}
}

// TestTheLANClassIsThePrivateBlocksLessTailscales holds ConsoleLANClass,
// which Validate and the source policy share (the plan's §2.2, §4.1, I9).
func TestTheLANClassIsThePrivateBlocksLessTailscales(t *testing.T) {
	for _, c := range []struct {
		addr, class string
	}{
		{"10.0.0.0", "10.0.0.0/8"},
		{"10.255.255.255", "10.0.0.0/8"},
		{"172.16.0.1", "172.16.0.0/12"},
		{"172.31.255.254", "172.16.0.0/12"},
		{"192.168.0.1", "192.168.0.0/16"},
		{"192.168.255.255", "192.168.0.0/16"},
		{"fc00::1", "fc00::/7"},
		{"fdff:ffff::1", "fc00::/7"},
		{"fd12:3456::7", "fc00::/7"},
		{"::ffff:10.1.2.3", "10.0.0.0/8"},
		{"172.15.255.255", ""},
		{"172.32.0.0", ""},
		{"192.169.0.1", ""},
		{"11.0.0.1", ""},
		{"100.64.0.1", ""},
		{"100.127.255.254", ""},
		{"fd7a:115c:a1e0::1", ""},
		{"fd7a:115c:a1e0:ffff::1", ""},
		{"fe00::1", ""},
		{"2001:db8::1", ""},
		{"8.8.8.8", ""},
	} {
		got, ok := ConsoleLANClass(netip.MustParseAddr(c.addr))
		if c.class == "" {
			if ok {
				t.Errorf("ConsoleLANClass(%s) = %s, want none", c.addr, got)
			}
			continue
		}
		if !ok || got != netip.MustParsePrefix(c.class) {
			t.Errorf("ConsoleLANClass(%s) = %s, %v, want %s", c.addr, got, ok, c.class)
		}
	}
	for _, a := range []string{"100.64.0.1", "100.100.100.100", "fd7a:115c:a1e0::1", "::ffff:100.64.0.1"} {
		if !IsTailscaleAddr(netip.MustParseAddr(a)) {
			t.Errorf("IsTailscaleAddr(%s) = false", a)
		}
	}
}

// TestTheLANKeysPresenceIsReadApartFromItsValue holds LANKeyOf to the key's
// presence and the file's own value (the plan's §2.6, §2.7, D16): an empty
// value and null are a key, a value the field's check refuses is never
// repeated, and an alias and a merge key reach the key as the loader's own
// decode does (I1).
func TestTheLANKeysPresenceIsReadApartFromItsValue(t *testing.T) {
	for _, c := range []struct {
		name, doc string
		want      LANKey
	}{
		{"an empty file", "", LANKey{State: LANKeyAbsent}},
		{"no key", "libraryName: x\n", LANKey{State: LANKeyAbsent}},
		{"an empty value", "adminLANAddress: \"\"\n", LANKey{State: LANKeyEmpty}},
		{"no value", "adminLANAddress:\n", LANKey{State: LANKeyEmpty}},
		{"null", "adminLANAddress: null\n", LANKey{State: LANKeyEmpty}},
		{"tilde", "adminLANAddress: ~\n", LANKey{State: LANKeyEmpty}},
		{"an address", "adminLANAddress: 192.168.1.20\n", LANKey{State: LANKeyAddress, Address: "192.168.1.20"}},
		{"a ULA", "adminLANAddress: \"fd12:3456::7\"\n", LANKey{State: LANKeyAddress, Address: "fd12:3456::7"}},
		{"a secret", "adminLANAddress: " + lanSecret + "\n", LANKey{State: LANKeyNotAnAddress}},
		{"a refused address", "adminLANAddress: 8.8.8.8\n", LANKey{State: LANKeyNotAnAddress}},
		{"a list", "adminLANAddress: [192.168.1.20]\n", LANKey{State: LANKeyNotAnAddress}},
		{"a merge key", "<<: {adminLANAddress: 192.168.1.20}\n", LANKey{State: LANKeyAddress, Address: "192.168.1.20"}},
		{"a merge key's empty value", "<<: {adminLANAddress: \"\"}\n", LANKey{State: LANKeyEmpty}},
		{"an alias of an address", "libraryName: &lan 192.168.1.20\nadminLANAddress: *lan\n",
			LANKey{State: LANKeyAddress, Address: "192.168.1.20"}},
		{"an alias of a secret", "libraryName: &s " + lanSecret + "\nadminLANAddress: *s\n", LANKey{State: LANKeyNotAnAddress}},
		{"an alias of null", "libraryName: &n null\nadminLANAddress: *n\n", LANKey{State: LANKeyEmpty}},
		{"an alias of a mapping", "deployment: &m {}\nadminLANAddress: *m\n", LANKey{State: LANKeyNotAnAddress}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := LANKeyOf([]byte(c.doc))
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("LANKeyOf = %+v, want %+v", got, c.want)
			}
			if got.InFile() != (c.want.State != LANKeyAbsent) {
				t.Fatalf("InFile = %v for %s", got.InFile(), c.want.State)
			}
		})
	}
}

// TestAnAliasedLANAddressLoadsAndKeepsItsSource is I1 through the load: an
// address anchored on another line and referenced by adminLANAddress loads
// as the file's, alone (source file) and beside a variable that overrides it
// (source both, the file's own address kept apart from the effective one).
func TestAnAliasedLANAddressLoadsAndKeepsItsSource(t *testing.T) {
	doc := "libraryRoots:\n  - {{LIBRARY_ROOT}}\nlibraryName: &lan \"192.168.1.20\"\nadminLANAddress: *lan\n"
	t.Run("alone", func(t *testing.T) {
		path, _ := writeConfig(t, doc)
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AdminLANAddress != "192.168.1.20" || cfg.ConsoleLANSource() != ConsoleLANSourceFile {
			t.Fatalf("address %q, source %q", cfg.AdminLANAddress, cfg.ConsoleLANSource())
		}
		if k := cfg.FileLANKey(); k != (LANKey{State: LANKeyAddress, Address: "192.168.1.20"}) {
			t.Fatalf("the file's key = %+v", k)
		}
	})
	t.Run("with a variable over it", func(t *testing.T) {
		t.Setenv(consoleLANEnv, "192.168.1.30")
		path, _ := writeConfig(t, doc)
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AdminLANAddress != "192.168.1.30" || cfg.ConsoleLANSource() != ConsoleLANSourceBoth {
			t.Fatalf("address %q, source %q", cfg.AdminLANAddress, cfg.ConsoleLANSource())
		}
		if k := cfg.FileLANKey(); k != (LANKey{State: LANKeyAddress, Address: "192.168.1.20"}) {
			t.Fatalf("the file's key = %+v", k)
		}
	})
	t.Run("a variable over an empty key", func(t *testing.T) {
		t.Setenv(consoleLANEnv, "192.168.1.30")
		path, _ := writeConfig(t, "libraryRoots:\n  - {{LIBRARY_ROOT}}\nadminLANAddress: \"\"\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ConsoleLANSource() != ConsoleLANSourceBoth || cfg.FileLANKey().State != LANKeyEmpty {
			t.Fatalf("source %q, key %+v", cfg.ConsoleLANSource(), cfg.FileLANKey())
		}
	})
}

// TestTheConfigExampleSaysWhatTheKeyCostsTheRollback: the example's block for
// the field carries §2.7's sentence (D8).
func TestTheConfigExampleSaysWhatTheKeyCostsTheRollback(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "bridge.yaml.example"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(strings.ReplaceAll(string(raw), "#", " ")), " ")
	for _, want := range []string{
		"adminLANAddress",
		"While this key is in the file, even empty, bridge 0.2.3 and earlier refuse the file: an update's automatic rollback to one of them is not made, and one installed by hand will not start.",
		"Remove the whole line before you go back to one.",
		"BRIDGE_ADMIN_LAN_ADDRESS instead costs no rollback: earlier versions ignore it and keep the console on this machine.",
		"no password",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the example does not say %q", want)
		}
	}
}

// TestALANAddressTakesEachPrivateClass: an address of each private class
// loads (a green control: a class left out of ConsoleLANClass refuses it).
func TestALANAddressTakesEachPrivateClass(t *testing.T) {
	for _, a := range []string{"10.1.2.3", "172.16.5.4", "172.31.0.9", "192.168.1.20", "fd12:3456::7", "fc00::9"} {
		path, _ := writeConfig(t, "libraryRoots:\n  - {{LIBRARY_ROOT}}\nadminLANAddress: "+yamlStr(a)+"\n")
		cfg, err := Load(path)
		if err != nil {
			t.Errorf("Load refused %s: %v", a, err)
			continue
		}
		if cfg.AdminLANAddress != a {
			t.Errorf("loaded %q, want %q", cfg.AdminLANAddress, a)
		}
	}
}

// TestNoLANAddressWritesNoKey: a config without the posture saves no
// adminLANAddress key, read by unmarshalling and looking for the key, so a
// file the bridge writes stays one 0.2.3 loads (R8, the rollback).
func TestNoLANAddressWritesNoKey(t *testing.T) {
	path, _ := writeConfig(t, "libraryRoots:\n  - {{LIBRARY_ROOT}}\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := LockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(lock); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	lock.Unlock()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["adminLANAddress"]; ok {
		t.Fatalf("a config without the posture saved an adminLANAddress key:\n%s", raw)
	}
}

// TestTheLANAddressIsSetByItsVariable: BRIDGE_ADMIN_LAN_ADDRESS is derived
// from the field as every leaf's variable is, sets the leaf, and the load
// records it (envSet), which the plan's source is read from (§2.3).
func TestTheLANAddressIsSetByItsVariable(t *testing.T) {
	t.Setenv("BRIDGE_ADMIN_LAN_ADDRESS", "192.168.1.30")
	path, _ := writeConfig(t, "libraryRoots:\n  - {{LIBRARY_ROOT}}\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminLANAddress != "192.168.1.30" {
		t.Fatalf("the variable set %q", cfg.AdminLANAddress)
	}
	if !slices.Contains(cfg.envSet, "adminLANAddress") {
		t.Fatalf("the load recorded %v, without adminLANAddress", cfg.envSet)
	}
}
