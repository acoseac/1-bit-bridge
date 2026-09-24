package main

import (
	"context"
	"crypto/tls"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	servertailscale "github.com/acoseac/1-bit-bridge/internal/tailscale"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// TestWarnLECertExpiringSoon pins the 30-day threshold and the
// expired/expiring/fresh tri-state. Same test affordance shape as
// the other pure-helper tests in this repo — no autopilot
// construction, no I/O.
func TestWarnLECertExpiringSoon(t *testing.T) {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		notAfter    time.Time
		wantContain string // "" means: must return empty
	}{
		{
			name:        "zero_time_returns_empty",
			notAfter:    time.Time{},
			wantContain: "",
		},
		{
			name:        "90_days_out_returns_empty",
			notAfter:    now.Add(90 * 24 * time.Hour),
			wantContain: "",
		},
		{
			name:        "31_days_out_returns_empty",
			notAfter:    now.Add(31 * 24 * time.Hour),
			wantContain: "",
		},
		{
			name:        "30_days_exact_warns",
			notAfter:    now.Add(30 * 24 * time.Hour),
			wantContain: "expires in 30 days",
		},
		{
			name:        "15_days_warns",
			notAfter:    now.Add(15 * 24 * time.Hour),
			wantContain: "expires in 15 days",
		},
		{
			name:        "1_day_warns",
			notAfter:    now.Add(1 * 24 * time.Hour),
			wantContain: "expires in 1 days",
		},
		{
			name:        "expired_3_days_ago",
			notAfter:    now.Add(-3 * 24 * time.Hour),
			wantContain: "EXPIRED (3 days past)",
		},
		{
			name:        "expired_1_year_ago",
			notAfter:    now.Add(-365 * 24 * time.Hour),
			wantContain: "EXPIRED (365 days past)",
		},
		{
			// Sub-day expiry rounds UP: pre-fix integer truncation
			// printed "0 days past" for a cert expired < 24h ago.
			name:        "expired_12_hours_ago_rounds_up",
			notAfter:    now.Add(-12 * time.Hour),
			wantContain: "EXPIRED (1 days past)",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := warnLECertExpiringSoon("bridge.tailnet-12345.ts.net", c.notAfter, now)
			if c.wantContain == "" {
				if got != "" {
					t.Errorf("want empty, got %q", got)
				}
				return
			}
			if !strings.Contains(got, c.wantContain) {
				t.Errorf("missing %q substring; got %q", c.wantContain, got)
			}
			// magicDNS argument MUST appear so the operator can tell which
			// cert the warning refers to (relevant when running multiple
			// bridges on the same host).
			if !strings.Contains(got, "bridge.tailnet-12345.ts.net") {
				t.Errorf("warning missing magicDNS name; got %q", got)
			}
			// Warnings MUST point at the diagnostic command operators
			// can run for context — without it the warning is just noise.
			if !strings.Contains(got, "bridge tailscale status") {
				t.Errorf("warning missing diagnostic-command hint; got %q", got)
			}
		})
	}
}

// TestWarnLECertExpiringSoon_BoundaryExactly30Days documents the
// inclusive-equals semantics at the 30-day boundary. A cert at
// exactly 30 days is INSIDE the warning window (matches the
// `<= 30` style in `bridge cert info`).
func TestWarnLECertExpiringSoon_BoundaryExactly30Days(t *testing.T) {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	// Use a time exactly 30 days minus 1 hour to avoid the floor
	// boundary; documents the same intent as the table-driven case.
	cert30dExact := now.Add(30*24*time.Hour - time.Hour)
	msg := warnLECertExpiringSoon("foo.ts.net", cert30dExact, now)
	if msg == "" {
		t.Errorf("expected warning at the 30-day boundary, got empty string")
	}
	cert30d1s := now.Add(30*24*time.Hour + time.Second)
	if warnLECertExpiringSoon("foo.ts.net", cert30d1s, now) != "" {
		t.Errorf("expected NO warning at 30 days + 1 second")
	}
}

// fakeNodeInfo is what a healthy `tailscale status --json` reports for
// a node with MagicDNS on: everything detectAndMint needs to go on to
// the mint.
func fakeNodeInfo() servertailscale.NodeInfo {
	return servertailscale.NodeInfo{
		CLIAvailable:  true,
		BinaryPath:    "/fake/tailscale",
		NodeName:      "bridge",
		MagicDNSName:  "bridge.example.ts.net",
		TailnetSuffix: "example.ts.net",
	}
}

// scriptedCLI is a tailscaleCLI whose two calls are closures, so a test
// can make either one behave like the real CLI under a kill.
type scriptedCLI struct {
	detect func(ctx context.Context) (servertailscale.NodeInfo, error)
	mint   func(ctx context.Context, certPath, keyPath string) error
}

func (c scriptedCLI) Detect(ctx context.Context) (servertailscale.NodeInfo, error) {
	return c.detect(ctx)
}

func (c scriptedCLI) MintCert(ctx context.Context, _, _, certPath, keyPath string) error {
	return c.mint(ctx, certPath, keyPath)
}

// loadTestCert mints a self-signed pair for host under dir and loads it
// the way the auto-pilot loads an LE cert, Leaf included.
func loadTestCert(t *testing.T, dir, name, host string) *tls.Certificate {
	t.Helper()
	certPath, keyPath := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	if err := servertls.Generate(certPath, keyPath, host); err != nil {
		t.Fatal(err)
	}
	c, err := servertls.LoadTailscaleCertFromDisk(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// servingAutoPilot is an auto-pilot in the state a successful mint
// leaves: the LE cert installed, the MagicDNS suffix routed to it, and
// a snapshot saying so. Returns the LE cert, so a test can ask whether a
// *.ts.net handshake still gets it.
func servingAutoPilot(t *testing.T, cli tailscaleCLI) (a *tailscaleAutoPilot, le *tls.Certificate, stdout, stderr *safeBuffer) {
	t.Helper()
	dir := t.TempDir()
	mgr := servertls.NewManager(loadTestCert(t, dir, "self", "localhost"))
	le = loadTestCert(t, dir, "le", "bridge.example.ts.net")
	mgr.SetTailscaleCert(le)
	mgr.SetMagicDNSSuffix("example.ts.net")
	stdout, stderr = &safeBuffer{}, &safeBuffer{}
	a = newTailscaleAutoPilot(filepath.Join(dir, "data"), "127.0.0.1:7788", mgr, cli, stdout, stderr)
	a.publish(tailscaleStatus{
		CLIAvailable:      true,
		MagicDNSName:      "bridge.example.ts.net",
		HTTPSCertsEnabled: true,
		CertPresent:       true,
	})
	return a, le, stdout, stderr
}

// servedFor reports the certificate a handshake for sni is given.
func servedFor(t *testing.T, a *tailscaleAutoPilot, sni string) *tls.Certificate {
	t.Helper()
	c, err := a.certManager.Get(&tls.ClientHelloInfo{ServerName: sni})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestACancelledTailscalePassChangesNothing pins what detectAndMint does
// when its context is CANCELLED while a CLI call runs: nothing. It logs
// no failure, publishes no snapshot, and leaves the LE cert serving.
//
// Three things cancel that context, and none is a failure: shutdown,
// Disable(), and the admin client that pressed "Re-mint now" going away
// (RefreshNow runs on the request's context). The pass used to treat
// all three as a failed CLI call. A cancelled `tailscale status` read
// as "detect failed", and that branch unloads the LE cert and the
// MagicDNS suffix, so closing the console tab mid-refresh left every
// *.ts.net client on the self-signed cert until the next successful
// pass, a day away on the renewer. Shutdown only made it noise ("mint
// failed: context canceled"), but serve now waits for this pass, so
// that noise would land in the journal on every shutdown that
// interrupted one.
//
// A DEADLINE is different: a CLI that ran out of time failed, and the
// last case pins that it is still reported as one.
func TestACancelledTailscalePassChangesNothing(t *testing.T) {
	killedStatus := errors.New("tailscale status: signal: killed")
	cases := []struct {
		name string
		cli  scriptedCLI
	}{
		{
			name: "cancelled during tailscale status",
			cli: scriptedCLI{
				detect: func(ctx context.Context) (servertailscale.NodeInfo, error) {
					<-ctx.Done()
					return servertailscale.NodeInfo{CLIAvailable: true, BinaryPath: "/fake/tailscale"}, killedStatus
				},
				mint: func(context.Context, string, string) error {
					t.Error("a pass cancelled during detect went on to mint")
					return nil
				},
			},
		},
		{
			name: "cancelled during tailscale cert",
			cli: scriptedCLI{
				detect: func(context.Context) (servertailscale.NodeInfo, error) { return fakeNodeInfo(), nil },
				mint: func(ctx context.Context, _, _ string) error {
					<-ctx.Done()
					return ctx.Err() // what MintCert returns for a cancel
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, le, _, stderr := servingAutoPilot(t, tc.cli)
			before := a.Snapshot()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			got := a.RefreshNow(ctx)

			if c := servedFor(t, a, "bridge.example.ts.net"); c != le {
				t.Error("a cancelled pass unloaded the LE cert: a *.ts.net handshake now gets the " +
					"self-signed one, until a later pass succeeds")
			}
			if s := stderr.String(); s != "" {
				t.Errorf("a cancelled pass reported a failure: %q", s)
			}
			if after := a.Snapshot(); !reflect.DeepEqual(after, before) {
				t.Errorf("a cancelled pass published a snapshot:\n before %+v\n after  %+v", before, after)
			}
			if !reflect.DeepEqual(got, before) {
				t.Errorf("RefreshNow answered a cancelled pass with %+v, want the unchanged %+v", got, before)
			}
		})
	}

	t.Run("a deadline is still a failure", func(t *testing.T) {
		a, _, _, stderr := servingAutoPilot(t, scriptedCLI{
			detect: func(ctx context.Context) (servertailscale.NodeInfo, error) {
				<-ctx.Done()
				return servertailscale.NodeInfo{CLIAvailable: true, BinaryPath: "/fake/tailscale"}, killedStatus
			},
			mint: func(context.Context, string, string) error { return nil },
		})
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()

		a.RefreshNow(ctx)

		if s := stderr.String(); !strings.Contains(s, "detect failed") {
			t.Errorf("a `tailscale status` that ran out of time was not reported: stderr=%q", s)
		}
		if a.Snapshot().CertPresent {
			t.Error("a failed detect left the snapshot claiming a cert is present")
		}
	})
}

// TestAMintedTailscaleCertIsReportedOnServesStdout pins the happy path
// through the tailscaleCLI seam, and where its one line goes. The
// "minted LE cert" line went to os.Stdout while every other line the
// auto-pilot prints went to serve's own streams. In production the two
// are the same file, but a boot test's is a buffer, so on a host
// running tailscaled the line escaped into `go test`'s output, printed
// by whichever test happened to be running. The flake that
// TestServeWaitsForAnInFlightTailscaleMint pins was first read beside
// one such line, from a run that had passed.
func TestAMintedTailscaleCertIsReportedOnServesStdout(t *testing.T) {
	a, le, stdout, stderr := servingAutoPilot(t, scriptedCLI{
		detect: func(context.Context) (servertailscale.NodeInfo, error) { return fakeNodeInfo(), nil },
		mint: func(_ context.Context, certPath, keyPath string) error {
			return servertls.Generate(certPath, keyPath, "bridge.example.ts.net")
		},
	})

	got := a.RefreshNow(context.Background())

	if !got.CertPresent || got.LastError != "" {
		t.Fatalf("a mint that succeeded was reported as %+v; stderr=%q", got, stderr.String())
	}
	if c := servedFor(t, a, "bridge.example.ts.net"); c == le || c == nil {
		t.Error("the freshly minted cert was not installed")
	}
	if s := stdout.String(); !strings.Contains(s, "tailscale (admin): minted LE cert for bridge.example.ts.net") {
		t.Errorf("the mint was not reported on serve's stdout: %q", s)
	}
}
