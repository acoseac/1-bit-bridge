package dlna

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The eight rates the endpoint accepts, spelled here so a rate dropped
// from the handler's list fails this file rather than disappearing with it.
var dsdSilenceSpecRates = []uint32{
	2822400, 5644800, 11289600, 22579200,
	3072000, 6144000, 12288000, 24576000,
}

func dsdSilenceSpec(fs uint32) (sampleCount uint64, payload, total int64) {
	sampleCount = uint64(fs) * 60
	bytesPerCh := sampleCount / 8
	nblocks := (bytesPerCh + 4095) / 4096
	payload = int64(nblocks) * 4096 * 2
	total = 92 + payload
	return sampleCount, payload, total
}

func TestDSDSilenceHeaderAndSizeForEachRate(t *testing.T) {
	h := DSDSilenceHandler()
	for _, fs := range dsdSilenceSpecRates {
		t.Run(strconv.FormatUint(uint64(fs), 10), func(t *testing.T) {
			sampleCount, payload, total := dsdSilenceSpec(fs)
			path := "/dlna/silence/dsd/" + strconv.FormatUint(uint64(fs), 10) + ".dsf"

			head := silenceDo(t, h, http.MethodHead, path, "")
			if head.Code != http.StatusOK {
				t.Fatalf("HEAD status %d", head.Code)
			}
			if head.Body.Len() != 0 {
				t.Fatalf("HEAD body %d bytes", head.Body.Len())
			}
			if got := head.Result().ContentLength; got != total {
				t.Fatalf("HEAD Content-Length %d, want %d", got, total)
			}
			if ct := head.Header().Get("Content-Type"); ct != "audio/x-dsf" {
				t.Fatalf("Content-Type %q", ct)
			}

			hdr := silenceDo(t, h, http.MethodGet, path, "bytes=0-91")
			if hdr.Code != http.StatusPartialContent {
				t.Fatalf("header range status %d", hdr.Code)
			}
			b := hdr.Body.Bytes()
			if len(b) != 92 {
				t.Fatalf("header range len %d", len(b))
			}
			if string(b[0:4]) != "DSD " {
				t.Fatalf("DSD magic %q", b[0:4])
			}
			if got := binary.LittleEndian.Uint64(b[4:12]); got != 28 {
				t.Fatalf("DSD chunk size %d", got)
			}
			if got := binary.LittleEndian.Uint64(b[12:20]); got != uint64(total) {
				t.Fatalf("total file size %d, want %d", got, total)
			}
			if got := binary.LittleEndian.Uint64(b[20:28]); got != 0 {
				t.Fatalf("metadata pointer %d", got)
			}
			if string(b[28:32]) != "fmt " {
				t.Fatalf("fmt magic %q", b[28:32])
			}
			if got := binary.LittleEndian.Uint64(b[32:40]); got != 52 {
				t.Fatalf("fmt chunk size %d", got)
			}
			if got := binary.LittleEndian.Uint32(b[40:44]); got != 1 {
				t.Fatalf("version %d", got)
			}
			if got := binary.LittleEndian.Uint32(b[44:48]); got != 0 {
				t.Fatalf("format id %d", got)
			}
			if got := binary.LittleEndian.Uint32(b[48:52]); got != 2 {
				t.Fatalf("channel type %d", got)
			}
			if got := binary.LittleEndian.Uint32(b[52:56]); got != 2 {
				t.Fatalf("channels %d", got)
			}
			if got := binary.LittleEndian.Uint32(b[56:60]); got != fs {
				t.Fatalf("fs %d", got)
			}
			if got := binary.LittleEndian.Uint32(b[60:64]); got != 1 {
				t.Fatalf("bits %d", got)
			}
			if got := binary.LittleEndian.Uint64(b[64:72]); got != sampleCount {
				t.Fatalf("sample count %d, want %d", got, sampleCount)
			}
			if got := binary.LittleEndian.Uint32(b[72:76]); got != 4096 {
				t.Fatalf("block size %d", got)
			}
			if got := binary.LittleEndian.Uint32(b[76:80]); got != 0 {
				t.Fatalf("reserved %d", got)
			}
			if string(b[80:84]) != "data" {
				t.Fatalf("data magic %q", b[80:84])
			}
			if got := binary.LittleEndian.Uint64(b[84:92]); got != uint64(12+payload) {
				t.Fatalf("data chunk size %d, want %d", got, 12+payload)
			}

			raw := int64(fs) * 60 / 8 * 2
			if fs == 2822400 && payload <= raw {
				t.Fatalf("DSD64 44.1 payload %d is not longer than the raw sample bytes %d", payload, raw)
			}
			if fs == 3072000 && payload != raw {
				t.Fatalf("DSD64 48 kHz payload %d, raw %d: the 48 kHz family divides the block", payload, raw)
			}
		})
	}
}

func TestDSDSilenceDataBytesAreTheSilenceByte(t *testing.T) {
	h := DSDSilenceHandler()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	client := srv.Client()
	for _, fs := range dsdSilenceSpecRates {
		t.Run(strconv.FormatUint(uint64(fs), 10), func(t *testing.T) {
			_, payload, total := dsdSilenceSpec(fs)
			u := srv.URL + "/dlna/silence/dsd/" + strconv.FormatUint(uint64(fs), 10) + ".dsf"
			resp, err := client.Get(u)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if resp.ContentLength != total {
				t.Fatalf("Content-Length %d, want %d", resp.ContentLength, total)
			}
			buf := make([]byte, 1<<20)
			var n int64
			var dataBad int64 = -1
			for {
				nr, rerr := resp.Body.Read(buf)
				for i := 0; i < nr; i++ {
					if n >= 92 && buf[i] != 0x69 && dataBad < 0 {
						dataBad = n
					}
					n++
				}
				if rerr == io.EOF {
					break
				}
				if rerr != nil {
					t.Fatal(rerr)
				}
			}
			if n != total {
				t.Fatalf("read %d, want %d", n, total)
			}
			if dataBad >= 0 {
				t.Fatalf("byte at %d is not 0x69", dataBad)
			}
			if n-92 != payload {
				t.Fatalf("payload %d, want %d", n-92, payload)
			}
		})
	}
}

func TestDSDSilenceRangesAndHead(t *testing.T) {
	h := DSDSilenceHandler()
	const fs = 2822400
	path := "/dlna/silence/dsd/2822400.dsf"
	_, _, total := dsdSilenceSpec(fs)

	first := silenceDo(t, h, http.MethodGet, path, "bytes=0-3")
	if first.Code != http.StatusPartialContent {
		t.Fatalf("first status %d", first.Code)
	}
	if got := first.Body.String(); got != "DSD " {
		t.Fatalf("first bytes %q", got)
	}

	mid := silenceDo(t, h, http.MethodGet, path, "bytes=100000-100003")
	if mid.Code != http.StatusPartialContent {
		t.Fatalf("middle status %d", mid.Code)
	}
	if got := mid.Body.Bytes(); string(got) != "\x69\x69\x69\x69" {
		t.Fatalf("middle bytes %x", got)
	}

	lastSpec := "bytes=" + strconv.FormatInt(total-4, 10) + "-" + strconv.FormatInt(total-1, 10)
	last := silenceDo(t, h, http.MethodGet, path, lastSpec)
	if last.Code != http.StatusPartialContent {
		t.Fatalf("last status %d body %q", last.Code, last.Body.String())
	}
	if got := last.Body.Bytes(); string(got) != "\x69\x69\x69\x69" {
		t.Fatalf("last bytes %x", got)
	}

	head := silenceDo(t, h, http.MethodHead, path, "")
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD status %d", head.Code)
	}
	if head.Body.Len() != 0 {
		t.Fatalf("HEAD body %d", head.Body.Len())
	}
	if got := head.Result().ContentLength; got != total {
		t.Fatalf("HEAD length %d, want %d", got, total)
	}
}

func TestDSDSilenceUnknownRateIs404(t *testing.T) {
	h := DSDSilenceHandler()
	for _, path := range []string{
		"/dlna/silence/dsd/44100.dsf",
		"/dlna/silence/dsd/1.dsf",
		"/dlna/silence/dsd/nope.dsf",
		"/dlna/silence/dsd/02822400.dsf",
		"/dlna/silence/dsd/2822400.DSF",
		"/dlna/silence/dsd/2822400.dsf/extra",
		"/dlna/silence/dsd/",
	} {
		t.Run(path, func(t *testing.T) {
			rec := silenceDo(t, h, http.MethodGet, path, "")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status %d", rec.Code)
			}
		})
	}
}

func TestDSDSilenceCapAnswers503AndFreesTheSlot(t *testing.T) {
	h := DSDSilenceHandler()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u := srv.URL + "/dlna/silence/dsd/2822400.dsf"
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	held := startParkedSilence(t, client, u, DSDSilenceMaxStreams)
	t.Cleanup(held.stop)

	if code := silenceStatus(t, client, http.MethodGet, u, ""); code != http.StatusServiceUnavailable {
		t.Fatalf("past the cap: status %d, want 503", code)
	}
	if code := silenceStatus(t, client, http.MethodHead, u, ""); code != http.StatusOK {
		t.Fatalf("HEAD while full: status %d, want 200", code)
	}

	held.finishOne()
	if code := silenceStatus(t, client, http.MethodGet, u, "bytes=0-15"); code != http.StatusPartialContent {
		t.Fatalf("after a stream ended: status %d, want 206", code)
	}
}

func TestDSDSilenceCapFreesTheSlotWhenTheClientDisconnects(t *testing.T) {
	h := DSDSilenceHandler()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u := srv.URL + "/dlna/silence/dsd/2822400.dsf"
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	held := startParkedSilence(t, client, u, DSDSilenceMaxStreams)
	t.Cleanup(held.stop)

	if code := silenceStatus(t, client, http.MethodGet, u, ""); code != http.StatusServiceUnavailable {
		t.Fatalf("past the cap: status %d, want 503", code)
	}
	held.cancelOne()
	deadline := time.Now().Add(5 * time.Second)
	var code int
	for {
		code = silenceStatus(t, client, http.MethodGet, u, "bytes=0-15")
		if code == http.StatusPartialContent || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code != http.StatusPartialContent {
		t.Fatalf("after a client disconnect: status %d, want 206", code)
	}
}

func TestDSDSilence404DoesNotHoldASlot(t *testing.T) {
	h := DSDSilenceHandler()
	for i := 0; i < DSDSilenceMaxStreams+2; i++ {
		rec := silenceDo(t, h, http.MethodGet, "/dlna/silence/dsd/44100.dsf", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("404 status %d", rec.Code)
		}
	}
	rec := silenceDo(t, h, http.MethodGet, "/dlna/silence/dsd/2822400.dsf", "bytes=0-15")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("a real rate after unknown rates: status %d", rec.Code)
	}
}

func TestTheDLNAListenerServesDSDSilence(t *testing.T) {
	s, err := NewServer(ServerConfig{
		Library:       newTestLib(testTrack("t1", "Test Track")),
		UDN:           "uuid:test-dsd-silence",
		FriendlyName:  "Test",
		ListenAddress: ":7790",
		ServerURL:     "http://127.0.0.1:7790",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/dlna/silence/dsd/2822400.dsf", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-3")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	if string(body) != "DSD " {
		t.Fatalf("body %q", body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/x-dsf" {
		t.Fatalf("Content-Type %q", ct)
	}
}

func TestDSDSilenceFFProbe(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not on PATH")
	}
	h := DSDSilenceHandler()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	resp, err := srv.Client().Get(srv.URL + "/dlna/silence/dsd/2822400.dsf")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	f, err := os.CreateTemp(t.TempDir(), "silence-*.dsf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "stream=codec_name,codec_type,sample_rate,channels:format=duration",
		"-of", "json", f.Name()).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var probe struct {
		Streams []struct {
			CodecName  string `json:"codec_name"`
			CodecType  string `json:"codec_type"`
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("ffprobe json: %v\n%s", err, out)
	}
	if len(probe.Streams) != 1 {
		t.Fatalf("streams %d: %s", len(probe.Streams), out)
	}
	st := probe.Streams[0]
	if st.CodecType != "audio" || st.Channels != 2 || st.CodecName == "" {
		t.Fatalf("stream %+v\n%s", st, out)
	}
	// ffmpeg reports a DSF's DSD rate as bytes per second (the 1-bit
	// rate divided by 8): 2,822,400 Hz is 352,800.
	if st.SampleRate != "352800" && st.SampleRate != "2822400" {
		t.Fatalf("sample_rate %q\n%s", st.SampleRate, out)
	}
	if !strings.Contains(st.CodecName, "dsd") {
		t.Fatalf("codec %q\n%s", st.CodecName, out)
	}
	if !strings.HasPrefix(probe.Format.Duration, "60.00") {
		t.Fatalf("duration %q", probe.Format.Duration)
	}
}

func silenceDo(t *testing.T, h http.Handler, method, path, rng string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func silenceStatus(t *testing.T, client *http.Client, method, u, rng string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// parkedSilence holds n full GETs that have received headers and 32 KiB,
// so each occupies a cap slot inside ServeContent.
type parkedSilence struct {
	t      *testing.T
	cancel []context.CancelFunc
	body   []io.ReadCloser
	stop   func()
}

func startParkedSilence(t *testing.T, client *http.Client, u string, n int) *parkedSilence {
	t.Helper()
	p := &parkedSilence{
		t:      t,
		cancel: make([]context.CancelFunc, n),
		body:   make([]io.ReadCloser, n),
	}
	var once sync.Once
	p.stop = func() {
		once.Do(func() {
			for i := range p.cancel {
				if p.cancel[i] != nil {
					p.cancel[i]()
				}
				if p.body[i] != nil {
					p.body[i].Close()
				}
			}
		})
	}
	ctxs := make([]context.Context, n)
	for i := 0; i < n; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		p.cancel[i] = cancel
		ctxs[i] = ctx
	}
	var wg sync.WaitGroup
	type parked struct {
		i    int
		body io.ReadCloser
	}
	ready := make(chan parked, n)
	errc := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctxs[i], http.MethodGet, u, nil)
			if err != nil {
				errc <- err
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				errc <- err
				return
			}
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				errc <- errStatus(resp.StatusCode)
				return
			}
			if _, err := io.ReadFull(resp.Body, make([]byte, 32<<10)); err != nil {
				resp.Body.Close()
				errc <- err
				return
			}
			ready <- parked{i, resp.Body}
		}(i)
	}
	got := 0
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for got < n {
		select {
		case pk := <-ready:
			p.body[pk.i] = pk.body
			got++
		case err := <-errc:
			p.stop()
			wg.Wait()
			t.Fatal(err)
		case <-deadline.C:
			p.stop()
			wg.Wait()
			t.Fatalf("parked %d of %d silence streams", got, n)
		}
	}
	return p
}

func (p *parkedSilence) finishOne() {
	p.t.Helper()
	if _, err := io.Copy(io.Discard, p.body[0]); err != nil {
		p.t.Fatal(err)
	}
	p.body[0].Close()
	p.body[0] = nil
	p.cancel[0]()
	p.cancel[0] = nil
}

func (p *parkedSilence) cancelOne() {
	p.t.Helper()
	p.cancel[0]()
	p.body[0].Close()
	p.body[0] = nil
	p.cancel[0] = nil
}

type errStatus int

func (e errStatus) Error() string {
	return "parked stream status " + strconv.Itoa(int(e))
}
