package dlna

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DSD silence for a renderer that rings when a DSD stream is stopped.
// Chord 2go docked to a Hugo 2 (MPD 0.21) rings continuously on pause
// or stop of a DSD256 stream. Serving this file and telling MPD to play
// it, measured 2026-10-09, stays silent; the app switches to it instead
// of sending a bare stop. Stereo only: a multichannel cast has no rate
// query here and falls back to stop.

const (
	// DSDSilencePathPrefix is the DLNA listener subtree. The one segment
	// under it is the sampling frequency in Hz plus ".dsf", exactly one
	// of the eight rates in dsdSilenceRates.
	DSDSilencePathPrefix = "/dlna/silence/dsd/"

	// DSDSilenceMaxStreams bounds concurrent GET bodies. DSD512 stereo is
	// fs/4 bytes per second: 5,644,800 B/s at 22,579,200 Hz and 6,144,000
	// B/s at 24,576,000 Hz. Four streams is about 24.6 MB/s, enough for a
	// household pause (one stream, held about ten seconds) plus a second
	// device and a retry, and a hard ceiling on an unauthenticated LAN
	// listener. HEAD does not take a slot. A stream past the cap is 503.
	DSDSilenceMaxStreams = 4

	dsdSilenceSeconds = 60
	dsdSilenceBlock   = 4096
	dsdSilenceCh      = 2
	dsdSilenceByte    = 0x69
	dsdSilenceHdrLen  = 92
)

// dsdSilenceWriteBound is how long one silence GET may block in a write.
// The listener leaves Server.WriteTimeout unset so a renderer can stream
// a track (B220). A client that sends this GET and then stops reading
// fills the socket buffer and would hold a cap slot until the connection
// died; four such clients take the silence away. 120 s covers a real-time
// play of the 60 s file plus read-ahead. The app stops the silence after
// about ten seconds. A test shortens the bound.
//
// Go 1.26.6 clears a ResponseController deadline after the handler
// returns (net/http conn.serve calls SetWriteDeadline(time.Time{})) even
// when WriteTimeout is 0, and the next request is given a write deadline
// only when WriteTimeout is positive. The silence response still sends
// Connection: close, so the deadline cannot apply to a later request on
// this connection.
var dsdSilenceWriteBound = 120 * time.Second

// dsdSilenceRates is 64·n × 44,100 and 64·n × 48,000 for n in {1, 2, 4, 8}.
var dsdSilenceRates = [...]uint32{
	2822400, 5644800, 11289600, 22579200,
	3072000, 6144000, 12288000, 24576000,
}

// dsdSilenceAsset is one precomputed header. The data bytes are not stored.
type dsdSilenceAsset struct {
	fs     uint32
	header []byte
	size   int64
}

var dsdSilenceByPath map[string]dsdSilenceAsset

func init() {
	dsdSilenceByPath = make(map[string]dsdSilenceAsset, len(dsdSilenceRates))
	for _, fs := range dsdSilenceRates {
		h, sz := buildDSFSilenceHeader(fs)
		name := strconv.FormatUint(uint64(fs), 10) + ".dsf"
		dsdSilenceByPath[DSDSilencePathPrefix+name] = dsdSilenceAsset{fs: fs, header: h, size: sz}
	}
}

// buildDSFSilenceHeader is the Sony DSF v1.01 layout WriteDSF uses, with
// every data byte 0x69 including the block padding. The returned header is
// the 92 bytes before the payload: DSD chunk (28), fmt chunk (52), and the
// data chunk's 12-byte header. total is 92 plus the payload.
func buildDSFSilenceHeader(fs uint32) (header []byte, total int64) {
	sampleCount := uint64(fs) * dsdSilenceSeconds
	if sampleCount%8 != 0 {
		panic("dlna: dsd silence rate is not a whole number of bytes")
	}
	bytesPerCh := sampleCount / 8
	nblocks := (bytesPerCh + dsdSilenceBlock - 1) / dsdSilenceBlock
	payload := nblocks * dsdSilenceBlock * dsdSilenceCh
	h := make([]byte, dsdSilenceHdrLen)
	copy(h[0:4], "DSD ")
	binary.LittleEndian.PutUint64(h[4:12], 28)
	binary.LittleEndian.PutUint64(h[12:20], uint64(dsdSilenceHdrLen)+payload)
	copy(h[28:32], "fmt ")
	binary.LittleEndian.PutUint64(h[32:40], 52)
	binary.LittleEndian.PutUint32(h[40:44], 1) // version
	// format id 0
	binary.LittleEndian.PutUint32(h[48:52], 2) // channel type: stereo
	binary.LittleEndian.PutUint32(h[52:56], dsdSilenceCh)
	binary.LittleEndian.PutUint32(h[56:60], fs)
	binary.LittleEndian.PutUint32(h[60:64], 1) // bits per sample
	binary.LittleEndian.PutUint64(h[64:72], sampleCount)
	binary.LittleEndian.PutUint32(h[72:76], dsdSilenceBlock)
	// reserved 0
	copy(h[80:84], "data")
	binary.LittleEndian.PutUint64(h[84:92], 12+payload)
	return h, int64(dsdSilenceHdrLen) + int64(payload)
}

// dsdSilenceFile is the virtual DSF. Read fills payload bytes with 0x69
// and never allocates the body. done is the request context's cancel
// signal; the handler does not store the context.
type dsdSilenceFile struct {
	header []byte
	size   int64
	off    int64
	done   <-chan struct{}
}

func (f *dsdSilenceFile) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	select {
	case <-f.done:
		return 0, context.Canceled
	default:
	}
	if f.off >= f.size {
		return 0, io.EOF
	}
	n := 0
	if f.off < int64(len(f.header)) {
		n = copy(p, f.header[f.off:])
		f.off += int64(n)
		if n == len(p) || f.off >= f.size {
			return n, nil
		}
	}
	remain := f.size - f.off
	space := int64(len(p) - n)
	if space > remain {
		space = remain
	}
	dst := p[n : n+int(space)]
	for i := range dst {
		dst[i] = dsdSilenceByte
	}
	n += int(space)
	f.off += space
	return n, nil
}

func (f *dsdSilenceFile) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = f.off + offset
	case io.SeekEnd:
		next = f.size + offset
	default:
		return 0, errDSDSilenceWhence
	}
	if next < 0 {
		return 0, errDSDSilenceSeek
	}
	f.off = next
	return next, nil
}

var (
	errDSDSilenceWhence = errString("dlna: dsd silence: invalid whence")
	errDSDSilenceSeek   = errString("dlna: dsd silence: negative seek")
)

type errString string

func (e errString) Error() string { return string(e) }

// dsdSilenceHandler serves the virtual files. slots is a non-blocking
// semaphore: a GET past DSDSilenceMaxStreams answers 503 without starting
// a body, and the slot is released when ServeHTTP returns (the copy
// finished, or the client went away and the write failed).
type dsdSilenceHandler struct {
	slots chan struct{}
}

// DSDSilenceHandler is the GET/HEAD handler mounted at DSDSilencePathPrefix.
func DSDSilenceHandler() http.Handler {
	return &dsdSilenceHandler{slots: make(chan struct{}, DSDSilenceMaxStreams)}
}

func (h *dsdSilenceHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	asset, ok := dsdSilenceByPath[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodGet {
		select {
		case h.slots <- struct{}{}:
			defer func() { <-h.slots }()
		default:
			http.Error(w, "dsd silence stream limit reached", http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", mimeDSF)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("transferMode.dlna.org", "Streaming")
	// close so the write deadline, set on this connection, cannot apply
	// to a later request. See dsdSilenceWriteBound.
	w.Header().Set("Connection", "close")
	if r.Method == http.MethodGet {
		boundDSDSilenceWrite(w)
	}
	name := strconv.FormatUint(uint64(asset.fs), 10) + ".dsf"
	http.ServeContent(w, r, name, time.Time{}, &dsdSilenceFile{
		header: asset.header,
		size:   asset.size,
		done:   r.Context().Done(),
	})
}

// boundDSDSilenceWrite stops a GET that has stopped reading. A writer
// that cannot set a deadline (the recorder the unit tests use) still
// serves the body.
func boundDSDSilenceWrite(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(dsdSilenceWriteBound))
}
