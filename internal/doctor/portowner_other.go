//go:build !linux

package doctor

// portOwnedByThisUser is the non-Linux stub for checkPort's last-resort
// port attribution. It always answers "don't know" so the caller falls
// through to its Warn.
//
// The real implementation (portowner_linux.go) reads the `uid` column of
// /proc/net/tcp{,6}, which exists only on Linux. It is there to rescue one
// specific deployment shape — a bridge granted cap_net_bind_service so it
// can bind :443 unprivileged, whose resulting dumpable=0 blocks port→pid
// attribution — and that shape is Linux-only by construction: macOS has no
// file capabilities, and on Windows the native GetExtendedTcpTable probe
// answers directly with no equivalent restriction.
//
// Returning (false, nil) rather than an error is deliberate: a nil error
// means "asked and got no match", which lands on the same Warn as a real
// no-match. Reporting a mechanism error here would imply something is
// broken on hosts where there is simply nothing to ask.
func portOwnedByThisUser(int) (bool, error) { return false, nil }
