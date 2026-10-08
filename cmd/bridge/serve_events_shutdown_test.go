package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// eventStreamShutdownBound is how long a held event stream may stay open
// after shutdown starts. The grace is 5s; an open stream that ignores the
// serve context holds Shutdown until that grace expires.
const eventStreamShutdownBound = time.Second

// TestEventStreamsEndWhenServeShutsDown holds the phone's event stream, a
// pairing event stream and an HTTP/3 event stream on a real serve, then
// stops it. Each stream has to close on its own, well inside the grace,
// and the stop must not report that it ran the grace out.
func TestEventStreamsEndWhenServeShutsDown(t *testing.T) {
	b := startConsoleBridge(t, "mdns:\n  enabled: false\n", nil)
	token := mintedToken(t, b)

	// A Transport with its own TLS config does not speak HTTP/2 unless
	// asked (net/http issue 14275). The phone's stream is HTTP/2.
	api := &http.Client{Transport: &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
	}}
	events := openEventStream(t, "GET /v1/events", api, b.apiBase+"/v1/events", "Bearer "+token)
	pairing := openPairingEventStream(t, api, b.apiBase)
	h3 := openHTTP3EventStream(t, b.apiBase+"/v1/events", "Bearer "+token)

	started := time.Now()
	b.stop()
	streams := []heldStream{events, pairing, h3}
	// Wait long enough to see the grace run out, then judge against the
	// bound. A stream that only ends when the grace expires fails here,
	// and the log says how long it actually took.
	observeUntil := started.Add(8 * time.Second)
	for _, stream := range streams {
		wait := time.Until(observeUntil)
		if wait < 0 {
			wait = 0
		}
		select {
		case err := <-stream.done:
			elapsed := time.Since(started)
			t.Logf("%s ended %s after shutdown (err=%v)", stream.name, elapsed.Round(time.Millisecond), err)
			if elapsed > eventStreamShutdownBound {
				t.Errorf("%s stayed open %s after shutdown; the bound is %s",
					stream.name, elapsed.Round(time.Millisecond), eventStreamShutdownBound)
			}
			if err != nil {
				t.Errorf("%s closed with %v; a shutdown is a finished response the client reconnects from",
					stream.name, err)
			}
		case <-time.After(wait):
			t.Errorf("%s still open %s after shutdown started",
				stream.name, time.Since(started).Round(time.Millisecond))
		}
	}
	select {
	case code := <-b.done:
		if code != 0 {
			t.Errorf("serve exited %d", code)
		}
	case <-serveGiveUp(t):
		t.Fatal("serve did not return")
	}
	t.Logf("serve returned %s after shutdown", time.Since(started).Round(time.Millisecond))
	out := b.stderr.String()
	if strings.Contains(out, "http shutdown: context deadline exceeded") {
		t.Errorf("the API server ran out the shutdown grace:\n%s", out)
	}
	if strings.Contains(out, "did not drain within grace") || strings.Contains(out, "lan h3 shutdown:") {
		t.Errorf("an HTTP/3 stream held the drain past the grace:\n%s", out)
	}
}

type heldStream struct {
	name string
	body io.ReadCloser
	done <-chan error
}

func openEventStream(t *testing.T, name string, client *http.Client, url, auth string) heldStream {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		resp.Body.Close()
		t.Fatalf("GET %s content type %q", url, ct)
	}
	if resp.ProtoMajor != 2 {
		resp.Body.Close()
		t.Fatalf("%s spoke %s; the phone holds this stream over HTTP/2", name, resp.Proto)
	}
	return holdStream(t, name, resp.Body)
}

func openPairingEventStream(t *testing.T, client *http.Client, apiBase string) heldStream {
	t.Helper()
	const secret = "shutdown-stream-secret"
	sum := sha256.Sum256([]byte(secret))
	body := `{"deviceName":"Shutdown Phone","pollSecretHash":"` + hex.EncodeToString(sum[:]) + `"}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		apiBase+"/v1/pairing/requests", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/pairing/requests: %v", err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/pairing/requests = %d: %s", resp.StatusCode, raw)
	}
	var created struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode pairing create: %v: %s", err, raw)
	}
	if created.RequestID == "" {
		t.Fatalf("pairing create returned no request id: %s", raw)
	}
	return openEventStream(t, "GET /v1/pairing/{id}/events", client,
		apiBase+"/v1/pairing/"+created.RequestID+"/events", "Bearer "+secret)
}

func openHTTP3EventStream(t *testing.T, url, auth string) heldStream {
	t.Helper()
	tr := &http3.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{http3.NextProtoH3},
	}}
	t.Cleanup(func() { _ = tr.Close() })
	var last error
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", auth)
		resp, err := tr.RoundTrip(req)
		if err == nil {
			if resp.StatusCode != http.StatusOK {
				raw, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				t.Fatalf("HTTP/3 GET %s = %d: %s", url, resp.StatusCode, raw)
			}
			if resp.ProtoMajor != 3 {
				resp.Body.Close()
				t.Fatalf("HTTP/3 GET %s spoke %s", url, resp.Proto)
			}
			return holdStream(t, "HTTP/3 GET /v1/events", resp.Body)
		}
		last = err
		if !eventStreamNotUpYet(err) || time.Now().After(deadline) {
			t.Fatalf("HTTP/3 GET %s: %v", url, last)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// eventStreamNotUpYet is a UDP listener that is not bound yet. A
// response the client cannot read is a different failure, and retrying
// it opens a new stream every 50ms.
func eventStreamNotUpYet(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no recent network activity")
}

func holdStream(t *testing.T, name string, body io.ReadCloser) heldStream {
	t.Helper()
	t.Cleanup(func() { _ = body.Close() })
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		done <- err
	}()
	return heldStream{name: name, body: body, done: done}
}

func mintedToken(t *testing.T, b *consoleBridge) string {
	t.Helper()
	mint := pairViaAdmin(t, t.Context(), b.console, b.adminBase+"/api/tokens",
		`{"name":"event shutdown"}`, http.StatusCreated, b.stderr)
	token := linkQueryItems(t, mint.PairURL)["token"]
	if token == "" {
		t.Fatalf("the pairing link carries no token: %s", mint.PairURL)
	}
	return token
}
