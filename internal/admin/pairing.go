package admin

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"

	qrcode "github.com/skip2/go-qrcode"
)

// buildPairURL composes the bridge://pair?... URL that the iOS app
// consumes via its onOpenURL handler. Shape is deliberately flat and
// additive so the iOS side can tolerate future fields by ignoring them.
//
//	bridge://pair
//	  ?url=<https URL to the bridge>
//	  &token=<base64url bearer token>
//	  &fingerprint=<AB:CD:...:EF>
//	  &name=<library display name>
func buildPairURL(bridgeURL, rawToken, fingerprint, libraryName string) string {
	q := url.Values{}
	q.Set("url", bridgeURL)
	q.Set("token", rawToken)
	q.Set("fingerprint", fingerprint)
	q.Set("name", libraryName)
	return "bridge://pair?" + q.Encode()
}

// defaultBridgeURL is the best-guess URL the admin UI pre-fills in the
// pairing modal — `https://<hostname>.local:<port>`. Users on networks
// where mDNS is flaky can override in the modal input to use a LAN IP
// literal. Falls back to "localhost" if os.Hostname errors, which still
// works for same-machine simulator pairing.
func defaultBridgeURL(listenAddress string) string {
	_, port, err := net.SplitHostPort(listenAddress)
	if err != nil || port == "" {
		port = "7788"
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	// macOS hostnames already carry the `.local` suffix (e.g.
	// `mac-mini.local`); Linux hostnames usually don't. Always tacking
	// on `.local` would be wrong for non-mDNS networks, so only append
	// if the hostname doesn't already contain a dot.
	return fmt.Sprintf("https://%s:%s", ensureMDNSHost(host), port)
}

func ensureMDNSHost(host string) string {
	for _, c := range host {
		if c == '.' {
			return host
		}
	}
	return host + ".local"
}

// qrPNG renders text as a 256x256 PNG QR code. Medium error correction is
// the default compromise — low would shrink the code but survive fewer
// printed-screen reads; high is overkill for a same-room workflow.
func qrPNG(text string) ([]byte, error) {
	var buf bytes.Buffer
	q, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	// DisableBorder = false (keep the quiet zone) so the default camera
	// framing works.
	if err := q.Write(256, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// qrDataURL encodes the rendered PNG as a base64 data URL so the admin
// page can <img src="..."/> it inline without a second HTTP round-trip.
func qrDataURL(text string) (string, error) {
	png, err := qrPNG(text)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}
