package dlna

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
			checkDSDSilenceRate(t, h, fs)
		})
	}
}

func checkDSDSilenceRate(t *testing.T, h http.Handler, fs uint32) {
	t.Helper()
	sampleCount, payload, total := dsdSilenceSpec(fs)
	path := "/dlna/silence/dsd/" + strconv.FormatUint(uint64(fs), 10) + ".dsf"
	head := silenceDo(t, h, http.MethodHead, path, "")
	requireSilenceHead(t, head, total)
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
	checkDSDChunk(t, b, total)
	checkFmtChunk(t, b, fs, sampleCount)
	checkDataChunk(t, b, payload)
	checkSilencePadding(t, fs, payload)
}

func checkDSDChunk(t *testing.T, b []byte, total int64) {
	t.Helper()
	checkMagic(t, b[0:4], "DSD ", "DSD magic")
	checkU64(t, b[4:12], 28, "DSD chunk size")
	checkU64Want(t, b[12:20], uint64(total), "total file size")
	checkU64(t, b[20:28], 0, "metadata pointer")
}

func checkFmtChunk(t *testing.T, b []byte, fs uint32, sampleCount uint64) {
	t.Helper()
	checkMagic(t, b[28:32], "fmt ", "fmt magic")
	checkU64(t, b[32:40], 52, "fmt chunk size")
	for _, f := range []struct {
		off  int
		want uint32
		name string
	}{
		{40, 1, "version"},
		{44, 0, "format id"},
		{48, 2, "channel type"},
		{52, 2, "channels"},
		{56, fs, "fs"},
		{60, 1, "bits"},
		{72, 4096, "block size"},
		{76, 0, "reserved"},
	} {
		checkU32(t, b[f.off:f.off+4], f.want, f.name)
	}
	checkU64Want(t, b[64:72], sampleCount, "sample count")
}

func checkDataChunk(t *testing.T, b []byte, payload int64) {
	t.Helper()
	checkMagic(t, b[80:84], "data", "data magic")
	checkU64Want(t, b[84:92], uint64(12+payload), "data chunk size")
}

func checkSilencePadding(t *testing.T, fs uint32, payload int64) {
	t.Helper()
	raw := int64(fs) * 60 / 8 * 2
	if fs == 2822400 && payload <= raw {
		t.Fatalf("DSD64 44.1 payload %d is not longer than the raw sample bytes %d", payload, raw)
	}
	if fs == 3072000 && payload != raw {
		t.Fatalf("DSD64 48 kHz payload %d, raw %d: the 48 kHz family divides the block", payload, raw)
	}
}

func checkMagic(t *testing.T, got []byte, want, name string) {
	t.Helper()
	if string(got) != want {
		t.Fatalf("%s %q", name, got)
	}
}

func checkU32(t *testing.T, b []byte, want uint32, name string) {
	t.Helper()
	if got := binary.LittleEndian.Uint32(b); got != want {
		t.Fatalf("%s %d", name, got)
	}
}

func checkU64(t *testing.T, b []byte, want uint64, name string) {
	t.Helper()
	if got := binary.LittleEndian.Uint64(b); got != want {
		t.Fatalf("%s %d", name, got)
	}
}

func checkU64Want(t *testing.T, b []byte, want uint64, name string) {
	t.Helper()
	if got := binary.LittleEndian.Uint64(b); got != want {
		t.Fatalf("%s %d, want %d", name, got, want)
	}
}

func requireSilenceHead(t *testing.T, rec *httptest.ResponseRecorder, total int64) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD body %d bytes", rec.Body.Len())
	}
	if got := rec.Result().ContentLength; got != total {
		t.Fatalf("HEAD Content-Length %d, want %d", got, total)
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
			requireSilenceBytes(t, client, u, payload, total)
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

	requireSilenceHead(t, silenceDo(t, h, http.MethodHead, path, ""), total)
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

// silenceSlotWatch counts slot acquires or releases. Install it before
// the handlers run: a release can happen during the copy, before
// finishOne or cancelOne returns. The count is exact. The channel only
// wakes the waiter; a full buffer drops the wake, so the handler is not
// held after the slot is free.
type silenceSlotWatch struct {
	mu   sync.Mutex
	n    int
	ch   chan struct{}
	what string
}

func watchSilenceSlotReleases(t *testing.T) *silenceSlotWatch {
	t.Helper()
	return watchSilenceSlotHook(t, &dsdSilenceSlotReleasedHookForTests, "released")
}

func watchSilenceSlotAcquires(t *testing.T) *silenceSlotWatch {
	t.Helper()
	return watchSilenceSlotHook(t, &dsdSilenceSlotAcquiredHookForTests, "taken")
}

func watchSilenceSlotHook(t *testing.T, dest *atomic.Pointer[func()], what string) *silenceSlotWatch {
	t.Helper()
	w := &silenceSlotWatch{ch: make(chan struct{}, 1), what: what}
	hook := func() {
		w.mu.Lock()
		w.n++
		w.mu.Unlock()
		select {
		case w.ch <- struct{}{}:
		default:
		}
	}
	dest.Store(&hook)
	t.Cleanup(func() { dest.Store(nil) })
	return w
}

func (w *silenceSlotWatch) await(t *testing.T, n int, budget time.Duration) {
	t.Helper()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	for {
		w.mu.Lock()
		got := w.n
		w.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-w.ch:
		case <-timer.C:
			w.mu.Lock()
			got = w.n
			w.mu.Unlock()
			t.Fatalf("silence slot %s %d of %d", w.what, got, n)
		}
	}
}

func TestDSDSilenceCapAnswers503AndFreesTheSlot(t *testing.T) {
	client, u, held := fillSilenceCap(t)
	if code := silenceStatus(t, client, http.MethodHead, u, ""); code != http.StatusOK {
		t.Fatalf("HEAD while full: status %d, want 200", code)
	}
	released := watchSilenceSlotReleases(t)
	held.finishOne()
	released.await(t, 1, 5*time.Second)
	if code := silenceStatus(t, client, http.MethodGet, u, "bytes=0-15"); code != http.StatusPartialContent {
		t.Fatalf("after a stream ended: status %d, want 206", code)
	}
}

func TestDSDSilenceCapFreesTheSlotWhenTheClientDisconnects(t *testing.T) {
	client, u, held := fillSilenceCap(t)
	released := watchSilenceSlotReleases(t)
	held.cancelOne()
	released.await(t, 1, 5*time.Second)
	if code := silenceStatus(t, client, http.MethodGet, u, "bytes=0-15"); code != http.StatusPartialContent {
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

func TestAStalledSilenceReaderFreesItsSlot(t *testing.T) {
	prev := dsdSilenceWriteBound
	dsdSilenceWriteBound = 3 * time.Second
	t.Cleanup(func() { dsdSilenceWriteBound = prev })

	s, err := NewServer(ServerConfig{
		Library:        newTestLib(testTrack("t1", "Test Track")),
		UDN:            "uuid:test-dsd-silence-stall",
		FriendlyName:   "Test",
		ListenAddress:  ":7790",
		ServerURL:      "http://127.0.0.1:7790",
		TelemetryStore: NewTelemetryStore(8),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	acquired := watchSilenceSlotAcquires(t)
	released := watchSilenceSlotReleases(t)
	conns := make([]net.Conn, DSDSilenceMaxStreams)
	for i := range conns {
		conns[i] = stallSilenceGET(t, srv.Listener.Addr().String())
	}
	t.Cleanup(func() {
		for _, c := range conns {
			if c != nil {
				c.Close()
			}
		}
	})
	// Four acquires means the four stalled handlers hold the slots. A
	// GET sent before that can take one of them.
	acquired.await(t, DSDSilenceMaxStreams, 5*time.Second)

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	u := srv.URL + "/dlna/silence/dsd/2822400.dsf"
	// A probe that returns 200 or 206 took a slot a stalled handler had
	// already given back. That status can come back before releaseSlot,
	// so those releases are part of the wait along with the four stalls.
	var probeOK int
	if code := waitSilenceStatus(t, client, u, 2*time.Second, func(code int) bool {
		if code == http.StatusOK || code == http.StatusPartialContent {
			probeOK++
			return false
		}
		return code == http.StatusServiceUnavailable
	}); code != http.StatusServiceUnavailable {
		t.Fatalf("cap never filled: status %d, want 503", code)
	}
	released.await(t, DSDSilenceMaxStreams+probeOK, 8*time.Second)
	code := silenceStatus(t, client, http.MethodGet, u, "bytes=0-15")
	if code != http.StatusOK && code != http.StatusPartialContent {
		t.Fatalf("after the write deadline: status %d, want 200 or 206", code)
	}
}

func stallSilenceGET(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	req := "GET /dlna/silence/dsd/2822400.dsf HTTP/1.1\r\nHost: " + addr + "\r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		c.Close()
		t.Fatal(err)
	}
	return c
}

func waitSilenceStatus(t *testing.T, client *http.Client, u string, budget time.Duration, done func(int) bool) int {
	t.Helper()
	deadline := time.Now().Add(budget)
	code := 0
	for {
		code = silenceStatus(t, client, http.MethodGet, u, "bytes=0-15")
		if done(code) || time.Now().After(deadline) {
			return code
		}
		time.Sleep(50 * time.Millisecond)
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

type parked struct {
	i    int
	body io.ReadCloser
}

func startParkedSilence(t *testing.T, client *http.Client, u string, n int) *parkedSilence {
	t.Helper()
	p := &parkedSilence{
		t:      t,
		cancel: make([]context.CancelFunc, n),
		body:   make([]io.ReadCloser, n),
	}
	p.stop = stopParkedSilence(p)
	ctxs := make([]context.Context, n)
	for i := 0; i < n; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		p.cancel[i] = cancel
		ctxs[i] = ctx
	}
	var wg sync.WaitGroup
	ready := make(chan parked, n)
	errc := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go fetchParkedSilence(&wg, client, u, ctxs[i], i, ready, errc)
	}
	awaitParkedSilence(p, &wg, ready, errc, n)
	return p
}

func stopParkedSilence(p *parkedSilence) func() {
	var once sync.Once
	return func() {
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
}

func fetchParkedSilence(wg *sync.WaitGroup, client *http.Client, u string, ctx context.Context, i int, ready chan<- parked, errc chan<- error) {
	defer wg.Done()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
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
}

func awaitParkedSilence(p *parkedSilence, wg *sync.WaitGroup, ready <-chan parked, errc <-chan error, n int) {
	p.t.Helper()
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
			p.t.Fatal(err)
		case <-deadline.C:
			p.stop()
			wg.Wait()
			p.t.Fatalf("parked %d of %d silence streams", got, n)
		}
	}
}

func fillSilenceCap(t *testing.T) (*http.Client, string, *parkedSilence) {
	t.Helper()
	srv := httptest.NewServer(DSDSilenceHandler())
	t.Cleanup(srv.Close)
	u := srv.URL + "/dlna/silence/dsd/2822400.dsf"
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	held := startParkedSilence(t, client, u, DSDSilenceMaxStreams)
	t.Cleanup(held.stop)
	if code := silenceStatus(t, client, http.MethodGet, u, ""); code != http.StatusServiceUnavailable {
		t.Fatalf("past the cap: status %d, want 503", code)
	}
	return client, u, held
}

func requireSilenceBytes(t *testing.T, client *http.Client, u string, payload, total int64) {
	t.Helper()
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
	n, dataBad := readSilenceBody(t, resp.Body)
	if n != total {
		t.Fatalf("read %d, want %d", n, total)
	}
	if dataBad >= 0 {
		t.Fatalf("byte at %d is not 0x69", dataBad)
	}
	if n-92 != payload {
		t.Fatalf("payload %d, want %d", n-92, payload)
	}
}

func readSilenceBody(t *testing.T, r io.Reader) (n, dataBad int64) {
	t.Helper()
	dataBad = -1
	buf := make([]byte, 1<<20)
	for {
		nr, rerr := r.Read(buf)
		for i := 0; i < nr; i++ {
			if n >= 92 && buf[i] != 0x69 && dataBad < 0 {
				dataBad = n
			}
			n++
		}
		if rerr == io.EOF {
			return n, dataBad
		}
		if rerr != nil {
			t.Fatal(rerr)
		}
	}
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
