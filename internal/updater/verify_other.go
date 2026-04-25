//go:build !darwin

package updater

import "context"

// verifyBinary is a no-op on non-darwin platforms. The install path
// still verifies the SHA-256 from the release's checksums.txt over
// TLS to GitHub before reaching this function — that's the only
// authentication available for unsigned Linux/Windows artifacts
// today. A future iteration can introduce SignPath / Sigstore /
// minisign for Linux + Windows; this is the seam where that lands.
//
// Documenting "SHA-256 over TLS is the entire authentication story
// here" rather than silently noop'ing keeps the trust boundary
// honest in code review.
func verifyBinary(ctx context.Context, newBinary string) error {
	return nil
}
