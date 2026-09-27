//go:build !windows

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// serviceUID is the owner the root test hands the install to, standing in
// for the service user a system install runs as.
const serviceUID = 4242

// TestCLIRunAsRootKeepsTheInstallOwner drives the commands an operator runs
// with sudo beside a service install, as root, over an install that belongs
// to another uid, and requires every file to belong to that uid afterwards:
// `bridge pair` (tokens.json), `bridge admin reset-password` and
// `sign-out-everywhere` (adminauth.json), `bridge admin login-link` (the
// login-ticket sidecar), `bridge cert rotate` (the TLS pair) and a
// `bridge init --force` rewrite (bridge.yaml). Before fsutil.KeepOwner each
// of those left a root-owned 0600 file the service could not read. It needs
// root, so CI skips it; run it in a container as root (CLAUDE.md, dido):
// `go test ./cmd/bridge/ -run TestCLIRunAsRootKeepsTheInstallOwner -count=1`.
func TestCLIRunAsRootKeepsTheInstallOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: it runs the CLI as root over an install another uid owns")
	}
	cfgDir := t.TempDir()
	ports := pickPublicInitPorts(t)
	lib := testLibrary(t)
	if code, out := publicInit(t, cfgDir, ports, "Owned", "--library", lib, "--skip-doctor"); code != 0 {
		t.Fatalf("bridge init --public = %d:\n%s", code, out)
	}
	// The install now belongs to the service user, as `bridge init` run as
	// that user (or a deploy's chown) leaves it.
	if err := filepath.WalkDir(cfgDir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, serviceUID, serviceUID)
	}); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "bridge.yaml")

	run := func(name string, cmd func(stdin *strings.Reader, out, errOut *strings.Builder) int, stdin string) {
		t.Helper()
		var out, errOut strings.Builder
		if code := cmd(strings.NewReader(stdin), &out, &errOut); code != 0 {
			t.Fatalf("%s as root = %d\nstdout:\n%s\nstderr:\n%s", name, code, out.String(), errOut.String())
		}
	}
	run("bridge pair", func(_ *strings.Reader, out, errOut *strings.Builder) int {
		return pairCmd([]string{"--config", cfgPath, "--name", "phone"}, out, errOut)
	}, "")
	run("bridge admin reset-password", func(in *strings.Reader, out, errOut *strings.Builder) int {
		return adminCmd([]string{"reset-password", "--config", cfgPath, "--from-stdin"}, in, out, errOut)
	}, "correct horse battery staple\n")
	run("bridge admin login-link", func(in *strings.Reader, out, errOut *strings.Builder) int {
		return adminCmd([]string{"login-link", "--config", cfgPath}, in, out, errOut)
	}, "")
	run("bridge admin sign-out-everywhere", func(in *strings.Reader, out, errOut *strings.Builder) int {
		return adminCmd([]string{"sign-out-everywhere", "--config", cfgPath}, in, out, errOut)
	}, "")
	run("bridge cert rotate", func(in *strings.Reader, out, errOut *strings.Builder) int {
		return certCmd([]string{"rotate", "--config", cfgPath, "--yes"}, in, out, errOut)
	}, "")
	if code, out := publicInit(t, cfgDir, ports, "Owned", "--library", lib, "--skip-doctor", "--force"); code != 0 {
		t.Fatalf("bridge init --force as root = %d:\n%s", code, out)
	}

	// Every entry the commands wrote must still be the service user's. The
	// files named are the ones this test knows it wrote, so a writer that
	// stopped writing cannot pass by leaving nothing behind.
	for _, want := range []string{
		"bridge.yaml",
		filepath.Join("data", "tokens.json"),
		filepath.Join("data", "adminauth.json"),
	} {
		if _, err := os.Lstat(filepath.Join(cfgDir, want)); err != nil {
			t.Errorf("%s: %v (the commands above write it)", want, err)
		}
	}
	var wrong []string
	if err := filepath.WalkDir(cfgDir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		if st.Uid != serviceUID || st.Gid != serviceUID {
			rel, _ := filepath.Rel(cfgDir, p)
			wrong = append(wrong, rel)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(wrong) > 0 {
		t.Fatalf("after the CLI ran as root, these no longer belong to uid %d: %v", serviceUID, wrong)
	}
}
