// Package tsnet wraps the upstream tailscale.com/tsnet library so the
// bridge can run as its own embedded tailnet node — no external
// `tailscaled` daemon, no CLI shell-out, no on-disk Let's Encrypt
// cert files.
//
// The bridge's tailscale integration has two paths:
//
//   - mode=cli (default, internal/tailscale): shells out to the
//     `tailscale` CLI for endpoint detection (`tailscale status
//     --json`) and LE cert minting (`tailscale cert <magicDNS>`).
//     Requires a host-level tailscaled and `tailscale` in $PATH;
//     LE cert/key files are written to disk for the SNI cert
//     switcher to load. This is the historical flow.
//
//   - mode=tsnet (this package): the bridge process IS the tailnet
//     node. tsnet.Server.ListenTLS terminates LE in-process, so
//     there's no on-disk cert material at all and no SNI switcher
//     is needed. State (machine identity) persists under
//     <dataDir>/tailscale/.
//
// The two paths converge on the same internal/api mux. Operators
// opt into tsnet via `tailscale.mode: tsnet` in bridge.yaml; the
// CLI path stays default until the tsnet path soaks for at least
// one release.
//
// Lifecycle is owned by cmd/bridge/main.go: construct a *Server,
// call Start (blocks until tailnet-reachable; surfaces an
// interactive AuthURL on first run when no AuthKey is supplied),
// call ListenTLS to get the second http listener for *.ts.net
// connections, and call Close on shutdown to drain magicsock /
// netcheck / control-plane goroutines (without Close, integration
// tests would leak goroutines per spawn).
package tsnet
