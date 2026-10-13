package doctor

import (
	"context"
	"fmt"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
)

const checkNamePairingURL = "pairing-url"

// checkPairingURL warns when the pairing QR has no address a phone can
// dial. That is a detected container whose hostname is the generated id
// and whose customEndpoints list names nothing the phone can reach.
// The hint is the sentence the pairing page shows.
func checkPairingURL(_ context.Context, d Deps) Check {
	if d.PairingPrimary == nil {
		return ok(checkNamePairingURL, "not checked")
	}
	url, reachable := d.PairingPrimary()
	if !reachable || url == "" {
		return warn(checkNamePairingURL, "no address a phone can dial", advertise.PairingNeedsEndpoint)
	}
	return ok(checkNamePairingURL, fmt.Sprintf("pairing QR dials %s", url))
}
