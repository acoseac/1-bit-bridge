package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// `bridge doctor` that finds no config is the pre-init report: config-file
// says "none found; the checks below use defaults", and the port checks grade
// 7788 and 7789, the ports `bridge init` writes (#1022). tls-cert graded
// nothing there: it warned "no data dir set" on every such run (measured with
// the real binary on 2026-09-29, backlog B61: "16 ok, 1 warn, 0 fail", the
// warn tls-cert's), while `bridge init`, run next, keeps or mints the pair in
// the data dir it writes beside the config, and its preflight FAILs a broken
// one there. So a pre-init "all clear." could be followed by an init that
// refuses, the launcher's doctor row included, which exists to preview the
// Setup wizard's preflight (#1023).

// TestDoctorBeforeInitGradesThePairInitWouldKeep: with no config found,
// tls-cert grades the pair in the data dir `bridge init` writes beside the
// platform config dir, in `bridge doctor` and in the launcher's row, and a
// broken pair there FAILs where init's preflight refuses it.
func TestDoctorBeforeInitGradesThePairInitWouldKeep(t *testing.T) {
	for _, tc := range []struct {
		name string
		// plant puts what the case needs in dataDir, init's data dir.
		plant func(t *testing.T, dataDir string)
		// want is what tls-cert's line must say.
		want []string
		// initRefuses says `bridge init` there refuses on tls-cert.
		initRefuses bool
	}{
		{"nothing there", func(*testing.T, string) {}, []string{"[ok]", "absent (init will mint)"}, false},
		{"a pair", func(t *testing.T, dataDir string) {
			certPath, keyPath := servertls.DefaultPaths(dataDir)
			if err := servertls.Generate(certPath, keyPath, "doctor.example.test"); err != nil {
				t.Fatal(err)
			}
		}, []string{"[ok]", "present, expires in"}, false},
		{"half a pair", func(t *testing.T, dataDir string) {
			certPath, keyPath := servertls.DefaultPaths(dataDir)
			if err := servertls.Generate(certPath, keyPath, "doctor.example.test"); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(keyPath); err != nil {
				t.Fatal(err)
			}
		}, []string{"[FAIL]", "partial state"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, platform := isolateConfigEnv(t)
			dataDir := filepath.Join(platform, "data")
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				t.Fatal(err)
			}
			tc.plant(t, dataDir)

			for _, run := range []struct {
				name   string
				report func() string
			}{
				{"bridge doctor", func() string { return runDoctor(t) }},
				{"the launcher's doctor row", func() string {
					var out, errOut strings.Builder
					runDoctorReport(buildDoctorDepsFor(filepath.Join(platform, "bridge.yaml"), true), false, false, &out, &errOut)
					return stripANSI(out.String() + errOut.String())
				}},
			} {
				checkReportLineSays(t, run.name, run.report(), "tls-cert", tc.want)
			}

			// What init does over the same data dir, with its preflight.
			code, out := runInit(t, "--yes", "--no-service", "--dir", platform, "--library", testLibrary(t),
				"--listen-address", loopbackAddr(freeLoopbackPort(t)), "--admin-address", loopbackAddr(freeLoopbackPort(t)))
			if refused := initRefusedOnTLSCert(code, out); refused != tc.initRefuses {
				t.Errorf("bridge init exited %d (refused on tls-cert: %v), want a refusal: %v:\n%s",
					code, refused, tc.initRefuses, out)
			}
		})
	}
}

// checkReportLineSays checks that out, the report `who` printed, has a line
// for check that says every string in want, and logs the report when it does
// not.
func checkReportLineSays(t *testing.T, who, out, check string, want []string) {
	t.Helper()
	line := reportLine(out, check)
	says := true
	for _, w := range want {
		if !strings.Contains(line, w) {
			t.Errorf("%s: %s says %q, want %q", who, check, line, w)
			says = false
		}
	}
	if !says {
		t.Logf("%s printed:\n%s", who, out)
	}
}

// initRefusedOnTLSCert says a `bridge init` run that exited code and printed
// out was refused by its preflight's tls-cert check.
func initRefusedOnTLSCert(code int, out string) bool {
	return code == 1 && strings.Contains(reportLine(out, "tls-cert"), "[FAIL]")
}
