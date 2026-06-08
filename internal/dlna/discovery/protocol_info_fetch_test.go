package discovery

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubDispatcher is a SOAPDispatcher that returns canned responses
// without standing up a net listener. The Handler field is invoked
// per Do call — tests pass an http.HandlerFunc that asserts on the
// request shape AND writes the canned response.
type stubDispatcher struct {
	handler http.HandlerFunc
}

func (s *stubDispatcher) Do(_ context.Context, req *http.Request) (*http.Response, error) {
	if s == nil || s.handler == nil {
		return nil, errors.New("stubDispatcher has no handler")
	}
	rec := httptest.NewRecorder()
	s.handler(rec, req)
	return rec.Result(), nil
}

// -----------------------------------------------------------------------------
// FetchDeviceDescription
// -----------------------------------------------------------------------------

func TestFetchDeviceDescription_HappyPath(t *testing.T) {
	disp := &stubDispatcher{
		handler: func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("method = %s, want GET", r.Method)
			}
			if !strings.HasPrefix(r.Header.Get("User-Agent"), "1-bit-bridge/") {
				t.Errorf("User-Agent = %q, want 1-bit-bridge/...", r.Header.Get("User-Agent"))
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(chordDeviceXML))
		},
	}
	desc, err := FetchDeviceDescription(
		context.Background(),
		disp,
		"http://192.168.1.42:8080/description.xml",
	)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if desc.FriendlyName != "Chord 2go" {
		t.Errorf("FriendlyName = %q", desc.FriendlyName)
	}
	if _, ok := desc.Services[ServiceAVTransport]; !ok {
		t.Error("AVTransport missing from parsed description")
	}
}

func TestFetchDeviceDescription_RejectsNon200(t *testing.T) {
	disp := &stubDispatcher{
		handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	}
	_, err := FetchDeviceDescription(context.Background(), disp, "http://x/d.xml")
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status code: %v", err)
	}
}

func TestFetchDeviceDescription_PropagatesNetworkError(t *testing.T) {
	disp := &boomDispatcher{err: errors.New("connection refused")}
	_, err := FetchDeviceDescription(context.Background(), disp, "http://x/d.xml")
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("expected wrapped network error, got %v", err)
	}
}

type boomDispatcher struct{ err error }

func (b *boomDispatcher) Do(_ context.Context, _ *http.Request) (*http.Response, error) {
	return nil, b.err
}

func TestFetchDeviceDescription_BodyExceedsCap(t *testing.T) {
	disp := &stubDispatcher{
		handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			// Stream 1 MB of pad bytes — far above
			// DefaultDescriptionMaxBytes (256 KiB).
			pad := strings.Repeat("x", 1024*1024)
			_, _ = io.WriteString(w, pad)
		},
	}
	_, err := FetchDeviceDescription(context.Background(), disp, "http://x/d.xml")
	if err == nil {
		t.Fatal("expected error for oversized body")
	}
}

// errStructuralDescription classification (bridge-12): a failure that
// re-fetching can't fix (4xx / unparseable) is marked structural so the
// discovery loop suppresses retries; transient failures (5xx / network)
// are NOT, so they retry.

func TestFetchDeviceDescription_404IsStructural(t *testing.T) {
	disp := &stubDispatcher{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}}
	_, err := FetchDeviceDescription(context.Background(), disp, "http://x/d.xml")
	if !errors.Is(err, errStructuralDescription) {
		t.Errorf("404 should be structural; got err=%v", err)
	}
}

func TestFetchDeviceDescription_500IsNotStructural(t *testing.T) {
	disp := &stubDispatcher{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}}
	_, err := FetchDeviceDescription(context.Background(), disp, "http://x/d.xml")
	if err == nil {
		t.Fatal("expected error for 500")
	}
	if errors.Is(err, errStructuralDescription) {
		t.Error("500 is transient — must NOT be marked structural")
	}
}

func TestFetchDeviceDescription_ParseFailureIsStructural(t *testing.T) {
	disp := &stubDispatcher{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<<< not valid device-description xml")
	}}
	_, err := FetchDeviceDescription(context.Background(), disp, "http://x/d.xml")
	if !errors.Is(err, errStructuralDescription) {
		t.Errorf("parse failure should be structural; got err=%v", err)
	}
}

func TestFetchDeviceDescription_NetworkErrorIsNotStructural(t *testing.T) {
	disp := &boomDispatcher{err: errors.New("connection refused")}
	_, err := FetchDeviceDescription(context.Background(), disp, "http://x/d.xml")
	if errors.Is(err, errStructuralDescription) {
		t.Error("network error is transient — must NOT be marked structural")
	}
}

// -----------------------------------------------------------------------------
// FetchGetProtocolInfo
// -----------------------------------------------------------------------------

func TestFetchGetProtocolInfo_HappyPath(t *testing.T) {
	disp := &stubDispatcher{
		handler: func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "text/xml") {
				t.Errorf("Content-Type = %q", ct)
			}
			soapAction := r.Header.Get("SOAPAction")
			wantSOAPAction := `"urn:schemas-upnp-org:service:ConnectionManager:1#GetProtocolInfo"`
			if soapAction != wantSOAPAction {
				t.Errorf("SOAPAction = %q, want %q", soapAction, wantSOAPAction)
			}
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "<u:GetProtocolInfo") {
				t.Errorf("request body missing GetProtocolInfo element: %s", body)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(chordGetProtocolInfoResponse))
		},
	}
	sinks, err := FetchGetProtocolInfo(context.Background(), disp, "http://renderer/cm/control")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(sinks) != 3 {
		t.Errorf("len(sinks) = %d, want 3 (got %v)", len(sinks), sinks)
	}
}

func TestFetchGetProtocolInfo_RejectsEmptyControlURL(t *testing.T) {
	_, err := FetchGetProtocolInfo(context.Background(), &stubDispatcher{}, "")
	if err == nil {
		t.Fatal("expected error for empty controlURL")
	}
}

func TestFetchGetProtocolInfo_RejectsNon200(t *testing.T) {
	disp := &stubDispatcher{
		handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		},
	}
	_, err := FetchGetProtocolInfo(context.Background(), disp, "http://x/cm/ctrl")
	if err == nil {
		t.Fatal("expected error for 503")
	}
}

func TestFetchGetProtocolInfo_PropagatesParseError(t *testing.T) {
	disp := &stubDispatcher{
		handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "not soap at all")
		},
	}
	_, err := FetchGetProtocolInfo(context.Background(), disp, "http://x/cm/ctrl")
	if err == nil {
		t.Fatal("expected parse error")
	}
}

// -----------------------------------------------------------------------------
// HTTPClientDispatcher
// -----------------------------------------------------------------------------

func TestHTTPClientDispatcher_NilFieldsError(t *testing.T) {
	var d *HTTPClientDispatcher
	resp, err := d.Do(context.Background(), &http.Request{})
	if resp != nil {
		_ = resp.Body.Close() // bodyclose: defensive even on err path
	}
	if err == nil {
		t.Fatal("expected error for nil receiver")
	}
	d2 := &HTTPClientDispatcher{Client: nil}
	resp, err = d2.Do(context.Background(), &http.Request{})
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected error for nil Client")
	}
}

func TestHTTPClientDispatcher_PassesThroughToClient(t *testing.T) {
	// Stand up a real httptest.Server so the path through
	// http.Client is exercised end-to-end.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	d := &HTTPClientDispatcher{Client: &http.Client{Timeout: 5 * time.Second}}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := d.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}
