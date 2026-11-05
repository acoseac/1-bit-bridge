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
// The hint is the sentence the pairing page shows. Either summary carries
// the reason the URL is not the host's name, when there is one to give: a
// name only the bridge's own mDNS answers, with mDNS off (B739's second
// round).
func checkPairingURL(_ context.Context, d Deps) Check {
	if d.PairingPrimary == nil {
		return ok(checkNamePairingURL, "not checked")
	}
	url, why := d.PairingPrimary()
	if why != "" {
		why = ": " + why
	}
	if url == "" {
		return warn(checkNamePairingURL, "no address a phone can dial"+why, advertise.PairingNeedsEndpoint)
	}
	return ok(checkNamePairingURL, fmt.Sprintf("pairing QR dials %s%s", url, why))
}
