package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/consolelan"
)

// bridge init's --admin-lan-address (backlog B989, the plan's §2.5 items 4
// and 5, §2.7).

// lanInitSecret is planted in refused values, so an echo is seen (D6).
const lanInitSecret = "hunter2SECRETinit"

// TestInitAddressFlagsErrorChecksTheLANAddress: the flag is Validate's own
// check against the admin address the run writes, the flag in place of the
// field, and public mode's refusal, none naming the value (R8, R10).
func TestInitAddressFlagsErrorChecksTheLANAddress(t *testing.T) {
	for _, c := range []struct {
		public           bool
		lan, admin, want string
	}{
		{false, "", "127.0.0.1:7789", ""},
		{false, "192.168.1.20", "127.0.0.1:7789", ""},
		{false, "fd12:3456::7", "", ""},
		{false, "8.8.8.8", "127.0.0.1:7789", "--admin-lan-address: not a private address"},
		{false, lanInitSecret, "127.0.0.1:7789", "--admin-lan-address: give this box's home-network IP address alone"},
		{false, "192.168.1.20", "127.0.0.1:0", "--admin-lan-address: adminAddress names port 0"},
		{true, "192.168.1.20", "0.0.0.0:7789", "--admin-lan-address: only a loopback install opens its console to the home network"},
	} {
		err := initLANAddressFlagError(c.public, c.lan, c.admin)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("(%v, %q, %q) refused: %v", c.public, c.lan, c.admin, err)
		case c.want != "" && (err == nil || !strings.HasPrefix(err.Error(), c.want)):
			t.Errorf("(%v, %q, %q) = %v, want %q", c.public, c.lan, c.admin, err, c.want)
		case err != nil && c.lan != "" && strings.Contains(err.Error(), c.lan):
			t.Errorf("the refusal %q names the value", err)
		}
	}
}

// lanInitArgs are a first install's flags with the posture.
func lanInitArgs(t *testing.T, cfgDir string, extra ...string) []string {
	t.Helper()
	return append([]string{"--yes", "--skip-doctor", "--dir", cfgDir, "--library", testLibrary(t),
		"--listen-address", "127.0.0.1:0", "--admin-address", "127.0.0.1:7811"}, extra...)
}

// lanKeyIn is the adminLANAddress key cfgDir's bridge.yaml holds now.
func lanKeyIn(t *testing.T, cfgDir string) config.LANKey {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cfgDir, "bridge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	k, err := config.LANKeyOf(raw)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestInitWritesTheLANAddressItIsGiven: the flag writes the field, the run
// says what it opens and costs before it saves, and its footer prints the
// LAN URL (§2.5 item 4, §5.4).
func TestInitWritesTheLANAddressItIsGiven(t *testing.T) {
	standInForInitHost(t)
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	code, out := runInit(t, lanInitArgs(t, cfgDir, "--admin-lan-address", "192.168.1.20")...)
	defer logRunOnFailure(t, out)
	if code != 0 {
		t.Fatalf("init exited %d", code)
	}
	if k := lanKeyIn(t, cfgDir); k != (config.LANKey{State: config.LANKeyAddress, Address: "192.168.1.20"}) {
		t.Fatalf("the config holds %+v", k)
	}
	for _, want := range []string{
		"Home network: http://192.168.1.20:7811/ (no password: anyone on your network can use it)",
		"with no password and over plain HTTP",
		"bridge 0.2.3 and earlier refuse the file",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("init did not say %q", want)
		}
	}
}

// TestInitRefusesALANAddressWithPublic: with --public the flag is refused
// before anything is written, exit 2, naming the flag and public mode and no
// value.
func TestInitRefusesALANAddressWithPublic(t *testing.T) {
	standInForInitHost(t)
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	code, out := runInit(t, "--yes", "--skip-doctor", "--dir", cfgDir, "--library", testLibrary(t), "--public",
		"--domain", "bridge.example.com", "--email", "ops@example.com", "--admin-lan-address", "192.168.1.20")
	if code != 2 {
		t.Fatalf("init exited %d, want 2:\n%s", code, out)
	}
	if !strings.Contains(out, "--admin-lan-address: only a loopback install opens its console to the home network") ||
		strings.Contains(out, "192.168.1.20") {
		t.Errorf("the refusal: %s", out)
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "bridge.yaml")); err == nil {
		t.Error("init wrote a config before its refusal")
	}
}

// TestInitNeverEchoesARefusedLANAddress: init's real path, its whole output
// read, for each refusal of a secret-shaped value and for the flag with
// --public: exit 2, and the value nowhere (D6).
func TestInitNeverEchoesARefusedLANAddress(t *testing.T) {
	for _, v := range []string{lanInitSecret, "box-" + lanInitSecret + ".local", "fe80::1%" + lanInitSecret, lanInitSecret + ":7789"} {
		for _, public := range []bool{false, true} {
			standInForInitHost(t)
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			args := lanInitArgs(t, cfgDir, "--admin-lan-address", v)
			if public {
				args = []string{"--yes", "--skip-doctor", "--dir", cfgDir, "--library", testLibrary(t), "--public",
					"--domain", "bridge.example.com", "--email", "ops@example.com", "--admin-lan-address", v}
			}
			code, out := runInit(t, args...)
			if code != 2 {
				t.Errorf("value %q, public %v: init exited %d, want 2", v, public, code)
			}
			if strings.Contains(out, lanInitSecret) {
				t.Errorf("value %q, public %v: init echoed it:\n%s", v, public, out)
			}
		}
	}
}

// saveWatch is a stderr whose writes record, as the rollback sentence is
// written, what bridge.yaml holds then, and a save seam that records its
// call after them.
type saveWatch struct {
	path       string
	buf        bytes.Buffer
	atSentence []byte
	sentence   bool
	saved      bool
	beforeSave bool
}

func (w *saveWatch) Write(p []byte) (int, error) {
	if !w.sentence && strings.Contains(string(p), "bridge 0.2.3 and earlier refuse the file") {
		w.sentence = true
		w.atSentence, _ = os.ReadFile(w.path)
		w.beforeSave = !w.saved
	}
	return w.buf.Write(p)
}

// TestInitWarnsOfTheLostRollbackBeforeItSaves: the rollback sentence is
// written before the save, while bridge.yaml is absent on a first install
// and holds its original bytes on a --yes --force rewrite that keeps the key
// (D15, F7, F12).
func TestInitWarnsOfTheLostRollbackBeforeItSaves(t *testing.T) {
	run := func(t *testing.T, cfgDir string, args []string) *saveWatch {
		t.Helper()
		standInForInitHost(t)
		w := &saveWatch{path: filepath.Join(cfgDir, "bridge.yaml")}
		prev := initHost.save
		initHost.save = func(stderr io.Writer, cfgPath string, asRead configAsRead, cfg *config.Config) int {
			w.saved = true
			return saveInitConfig(stderr, cfgPath, asRead, cfg)
		}
		t.Cleanup(func() { initHost.save = prev })
		var out bytes.Buffer
		if code := initCmd(args, strings.NewReader(""), &out, w); code != 0 {
			t.Fatalf("init exited %d:\n%s\n%s", code, out.String(), w.buf.String())
		}
		if !w.sentence || !w.beforeSave || !w.saved {
			t.Fatalf("sentence %v, before the save %v, saved %v:\n%s", w.sentence, w.beforeSave, w.saved, w.buf.String())
		}
		return w
	}
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	first := run(t, cfgDir, lanInitArgs(t, cfgDir, "--admin-lan-address", "192.168.1.20"))
	if first.atSentence != nil {
		t.Fatalf("a first install's sentence came with bridge.yaml already written:\n%s", first.atSentence)
	}
	original, err := os.ReadFile(filepath.Join(cfgDir, "bridge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	again := run(t, cfgDir, []string{"--yes", "--force", "--skip-doctor", "--dir", cfgDir, "--library", testLibrary(t)})
	if !bytes.Equal(again.atSentence, original) {
		t.Fatalf("the rewrite's sentence came with bridge.yaml holding\n%s\nwant its original bytes\n%s", again.atSentence, original)
	}
	if k := lanKeyIn(t, cfgDir); k.Address != "192.168.1.20" {
		t.Fatalf("the rewrite did not keep the address: %+v", k)
	}
}

// firstLANInstall is a first install with the posture, at 127.0.0.1:7811.
func firstLANInstall(t *testing.T) string {
	t.Helper()
	standInForInitHost(t)
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	code, out := runInit(t, lanInitArgs(t, cfgDir, "--admin-lan-address", "192.168.1.20")...)
	if code != 0 {
		t.Fatalf("first install exited %d:\n%s", code, out)
	}
	return cfgDir
}

// TestARewriteKeepsTheLANAddressWithTheAdminAddress: a rewrite that keeps
// the addresses keeps adminLANAddress, and lists it (§2.5 item 5, R4).
func TestARewriteKeepsTheLANAddressWithTheAdminAddress(t *testing.T) {
	cfgDir := firstLANInstall(t)
	code, out := runInit(t, "--yes", "--force", "--skip-doctor", "--dir", cfgDir, "--library", testLibrary(t))
	defer logRunOnFailure(t, out)
	if code != 0 {
		t.Fatalf("rewrite exited %d", code)
	}
	if k := lanKeyIn(t, cfgDir); k.Address != "192.168.1.20" {
		t.Fatalf("the rewrite holds %+v", k)
	}
	if !strings.Contains(out, "adminLANAddress  192.168.1.20") {
		t.Errorf("the rewrite did not list the kept address")
	}
}

// TestARewriteThatNamesAnAddressFlagSaysTheLANAddressIsNotKept: a run that
// names an address flag writes the console's addresses, so the field is not
// kept, and one line says why.
func TestARewriteThatNamesAnAddressFlagSaysTheLANAddressIsNotKept(t *testing.T) {
	cfgDir := firstLANInstall(t)
	code, out := runInit(t, "--yes", "--force", "--skip-doctor", "--dir", cfgDir, "--library", testLibrary(t),
		"--admin-address", "127.0.0.1:7812")
	if code != 0 {
		t.Fatalf("rewrite exited %d:\n%s", code, out)
	}
	if k := lanKeyIn(t, cfgDir); k.InFile() {
		t.Fatalf("the rewrite kept %+v", k)
	}
	if !strings.Contains(out, "this run writes the console's addresses: add --admin-lan-address to open it on the home network again") {
		t.Errorf("no line says why:\n%s", out)
	}
}

// TestARewriteToPublicSaysTheLANAddressIsNotKept: public mode's console has
// a login, so a rewrite to public mode keeps no adminLANAddress, and says so.
func TestARewriteToPublicSaysTheLANAddressIsNotKept(t *testing.T) {
	cfgDir := firstLANInstall(t)
	code, out := runInit(t, "--yes", "--force", "--skip-doctor", "--dir", cfgDir, "--library", testLibrary(t), "--public",
		"--domain", "bridge.example.com", "--email", "ops@example.com")
	if code != 0 {
		t.Fatalf("the rewrite to public mode exited %d:\n%s", code, out)
	}
	if k := lanKeyIn(t, cfgDir); k.InFile() {
		t.Fatalf("the rewrite to public mode kept %+v", k)
	}
	if !strings.Contains(out, "public mode's console has a login: adminLANAddress is not kept") {
		t.Errorf("no line says why:\n%s", out)
	}
}

// TestARewriteDropsAKeptLANAddressTheCheckRefuses: a kept value the check
// refuses against the admin address the rewrite writes is not kept, named by
// its line and never its value.
func TestARewriteDropsAKeptLANAddressTheCheckRefuses(t *testing.T) {
	standInForInitHost(t)
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	lib := testLibrary(t)
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "libraryRoots:\n  - " + lib + "\nadminAddress: \"127.0.0.1:0\"\nadminLANAddress: \"192.168.1.20\"\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "bridge.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runInit(t, "--yes", "--force", "--skip-doctor", "--dir", cfgDir, "--library", lib)
	if code != 0 {
		t.Fatalf("rewrite exited %d:\n%s", code, out)
	}
	if k := lanKeyIn(t, cfgDir); k.InFile() {
		t.Fatalf("the rewrite kept %+v", k)
	}
	if !strings.Contains(out, "holds an adminLANAddress line its check refuses: it is not kept") {
		t.Errorf("no line says why:\n%s", out)
	}
	_ = consolelan.RollbackWords
}
