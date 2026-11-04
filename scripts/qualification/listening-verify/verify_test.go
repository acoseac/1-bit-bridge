package listeningverify

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/cmplx"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

var assetDir = flag.String("listening-assets", "", "a generated asset directory: manifest*.json, runs.json and assets/")

// limits are what a decoded fixture is held to. production's are the
// generator's design with a margin: its self-test measures -108 dB SA-CD in
// band and -21 dB SA-CD from 20 to 100 kHz at DSD64.
type limits struct {
	// silence is the most a silent stretch may hold from 20 Hz to 20 kHz,
	// in dB SA-CD (0 dB SA-CD: a sine of half the +-1 range).
	silence float64
	// hf is the most 20 to 100 kHz may hold, in dB SA-CD, by DSD rate.
	hf map[int]float64
	// hfSpread is how far that band may differ between silence and tones:
	// a loop whose noise follows its input is not stable.
	hfSpread float64
	// line is how far one bin from 20 to 150 kHz may stand over the median
	// of its neighbours, in dB.
	line float64
	// level is how far a block's tone may differ from its reference's,
	// 6.02 dB down, in dB; frequency how far a burst's may, in Hz; absent
	// the most a block may read, in dBFS, of a tone its reference has not.
	level, frequency, absent float64
	// run is the longest run of equal bits.
	run int
	// boundary is the most the audio band may hold in the 10 ms around a
	// boundary the sink plays, in dB SA-CD: 60 dB under the tones.
	boundary float64
}

var production = limits{silence: -100, hf: map[int]float64{2822400: -18, 5644800: -45, 11289600: -80}, hfSpread: 1.5, line: 10, level: 0.05, frequency: 0.01, absent: -100, run: 16, boundary: -90}

// saCD converts a power of the +-1 stream to dB SA-CD.
func saCD(power float64) float64 { return 10 * math.Log10(power/0.125) }

const analysisRate = 352800

// ---------------------------------------------------------------- DSD

// dsd is a fixture's bits: canonical bytes per channel, bit 7 earliest.
type dsd struct {
	rate int
	bits int64
	ch   [2][]byte
}

func (d dsd) bit(c int, k int64) int {
	return int(d.ch[c][k>>3]>>(7-uint(k&7))) & 1
}

// readBridgeDSD decodes a DSD file with the bridge's own decoder.
func readBridgeDSD(path string) (dsd, format.NativeDSD, error) {
	f, err := os.Open(path)
	if err != nil {
		return dsd{}, format.NativeDSD{}, err
	}
	defer f.Close()
	src, err := decode.Open(f)
	if err != nil {
		return dsd{}, format.NativeDSD{}, err
	}
	defer src.Close()
	d, ok := src.(decode.DSDDecoder)
	if !ok {
		return dsd{}, format.NativeDSD{}, errors.New("the bridge's decoder opens it as PCM")
	}
	p := d.Format()
	out := dsd{rate: p.BitRateHz, bits: p.Frames}
	want := (p.Frames + 7) / 8
	buf := [][]byte{make([]byte, 1<<16), make([]byte, 1<<16)}
	for {
		n, err := d.ReadDSD(context.Background(), buf)
		out.ch[0] = append(out.ch[0], buf[0][:n]...)
		out.ch[1] = append(out.ch[1], buf[1][:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			return dsd{}, p, err
		}
		if n == 0 {
			return dsd{}, p, io.ErrNoProgress
		}
	}
	if int64(len(out.ch[0])) != want || int64(len(out.ch[1])) != want {
		return dsd{}, p, fmt.Errorf("the bridge's decoder gave %d bytes a channel for %d bits", len(out.ch[0]), p.Frames)
	}
	return out, p, nil
}

func bitrev(b byte) byte {
	var r byte
	for i := 0; i < 8; i++ {
		if b&(1<<uint(i)) != 0 {
			r |= 0x80 >> uint(i)
		}
	}
	return r
}

// readOwnDSF reads a DSF file from Sony's specification (1.01), apart from
// the bridge's decoder: DSD and fmt chunks, block-interleaved data, bit 0
// earliest when bits per sample is 1.
func readOwnDSF(b []byte) (dsd, error) {
	le32 := func(at int) uint32 { return binary.LittleEndian.Uint32(b[at:]) }
	le64 := func(at int) uint64 { return binary.LittleEndian.Uint64(b[at:]) }
	if len(b) < 92 || string(b[0:4]) != "DSD " || le64(4) != 28 || le64(12) != uint64(len(b)) {
		return dsd{}, errors.New("DSF: DSD chunk")
	}
	if string(b[28:32]) != "fmt " || le64(32) != 52 || le32(40) != 1 || le32(44) != 0 || le32(48) != 2 || le32(52) != 2 {
		return dsd{}, errors.New("DSF: fmt chunk: version 1, DSD raw, stereo")
	}
	bps, count, block := le32(60), le64(64), le32(72)
	if (bps != 1 && bps != 8) || block != 4096 || string(b[80:84]) != "data" || le64(84) != uint64(len(b)-80) {
		return dsd{}, errors.New("DSF: bits per sample, block size or data chunk")
	}
	data := b[92:]
	nbytes := int((count + 7) / 8)
	blocks := (nbytes + 4095) / 4096
	if len(data) != blocks*8192 {
		return dsd{}, errors.New("DSF: data is not whole blocks")
	}
	out := dsd{rate: int(le32(56)), bits: int64(count)}
	for c := 0; c < 2; c++ {
		ch := make([]byte, 0, blocks*4096)
		for k := 0; k < blocks; k++ {
			ch = append(ch, data[k*8192+c*4096:k*8192+(c+1)*4096]...)
		}
		for _, pad := range ch[nbytes:] {
			if pad != 0 {
				return dsd{}, errors.New("DSF: padding after the last sample is not zero")
			}
		}
		ch = ch[:nbytes]
		if bps == 1 {
			for i := range ch {
				ch[i] = bitrev(ch[i])
			}
		}
		out.ch[c] = ch
	}
	return out, nil
}

// readOwnDFF reads a DSDIFF file from Philips' specification (1.5): FRM8,
// FVER, PROP with FS, CHNL and CMPR, and the DSD sound chunk, its channels
// interleaved byte by byte in CHNL order.
func readOwnDFF(b []byte) (dsd, error) {
	be64 := func(at int) uint64 { return binary.BigEndian.Uint64(b[at:]) }
	if len(b) < 16 || string(b[0:4]) != "FRM8" || be64(4) != uint64(len(b)-12) || string(b[12:16]) != "DSD " {
		return dsd{}, errors.New("DFF: FRM8 form")
	}
	chunks := map[string][]byte{}
	for at := 16; at < len(b); {
		if at+12 > len(b) {
			return dsd{}, errors.New("DFF: a chunk header is cut short")
		}
		id, size := string(b[at:at+4]), be64(at+4)
		if size > uint64(len(b)-at-12) {
			return dsd{}, errors.New("DFF: a chunk runs past the form")
		}
		chunks[id] = b[at+12 : at+12+int(size)]
		at += 12 + int(size) + int(size&1)
	}
	prop := chunks["PROP"]
	if !bytes.Equal(chunks["FVER"], []byte{1, 5, 0, 0}) || len(prop) < 4 || string(prop[0:4]) != "SND " {
		return dsd{}, errors.New("DFF: FVER 1.5 and PROP SND")
	}
	sub := map[string][]byte{}
	for at := 4; at < len(prop); {
		if at+12 > len(prop) {
			return dsd{}, errors.New("DFF: a PROP chunk header is cut short")
		}
		id, size := string(prop[at:at+4]), binary.BigEndian.Uint64(prop[at+4:])
		if size > uint64(len(prop)-at-12) {
			return dsd{}, errors.New("DFF: a PROP chunk runs past PROP")
		}
		sub[id] = prop[at+12 : at+12+int(size)]
		at += 12 + int(size) + int(size&1)
	}
	chnl, cmpr := sub["CHNL"], sub["CMPR"]
	if len(sub["FS  "]) != 4 || len(chnl) != 10 || binary.BigEndian.Uint16(chnl) != 2 || len(cmpr) < 4 || string(cmpr[0:4]) != "DSD " {
		return dsd{}, errors.New("DFF: FS, a stereo CHNL and uncompressed CMPR")
	}
	order := [2]string{string(chnl[2:6]), string(chnl[6:10])}
	var left int
	switch order {
	case [2]string{"SLFT", "SRGT"}:
		left = 0
	case [2]string{"SRGT", "SLFT"}:
		left = 1
	default:
		return dsd{}, fmt.Errorf("DFF: CHNL %q", order)
	}
	sound := chunks["DSD "]
	if len(sound)%2 != 0 {
		return dsd{}, errors.New("DFF: sound data is not whole frames")
	}
	n := len(sound) / 2
	out := dsd{rate: int(binary.BigEndian.Uint32(sub["FS  "])), bits: int64(8 * n)}
	stored := [2][]byte{make([]byte, n), make([]byte, n)}
	for i := 0; i < n; i++ {
		stored[0][i], stored[1][i] = sound[2*i], sound[2*i+1]
	}
	out.ch[0], out.ch[1] = stored[left], stored[1-left]
	return out, nil
}

// ---------------------------------------------------------------- PCM

// pcm is a reference's samples, scaled so full scale is 1.
type pcm struct {
	rate, bits int
	frames     int64
	ch         [2][]float64
}

// readBridgeWAV decodes a WAV file with the bridge's decoder, and reads its
// samples again from its own RIFF chunks; the two must agree.
func readBridgeWAV(path string) (pcm, format.PCM, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return pcm{}, format.PCM{}, err
	}
	src, err := decode.Open(bytes.NewReader(b))
	if err != nil {
		return pcm{}, format.PCM{}, err
	}
	defer src.Close()
	d, ok := src.(decode.Decoder)
	if !ok {
		return pcm{}, format.PCM{}, errors.New("the bridge's decoder does not open it as PCM")
	}
	p := d.Format()
	scale := math.Ldexp(1, -(p.ValidBits - 1))
	out := pcm{rate: p.RateHz, bits: p.ValidBits, frames: p.Frames}
	samples := make([]int32, 8192)
	var raw []int32
	for {
		n, err := d.ReadFrames(context.Background(), samples)
		raw = append(raw, samples[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			return pcm{}, p, err
		}
		if n == 0 {
			return pcm{}, p, io.ErrNoProgress
		}
	}
	own, err := readOwnWAV(b)
	if err != nil {
		return pcm{}, p, err
	}
	if len(own) != len(raw) {
		return pcm{}, p, fmt.Errorf("the bridge's decoder gave %d samples, the file holds %d", len(raw), len(own))
	}
	for i := range raw {
		if raw[i] != own[i] {
			return pcm{}, p, fmt.Errorf("sample %d: the bridge's decoder gave %d, the file holds %d", i, raw[i], own[i])
		}
	}
	for c := 0; c < 2; c++ {
		out.ch[c] = make([]float64, len(raw)/2)
		for i := range out.ch[c] {
			out.ch[c][i] = float64(raw[2*i+c]) * scale
		}
	}
	return out, p, nil
}

// readOwnWAV reads a stereo WAVE_FORMAT_EXTENSIBLE or PCM file's integer
// samples from its fmt and data chunks.
func readOwnWAV(b []byte) ([]int32, error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, errors.New("WAV: RIFF WAVE")
	}
	var bits, channels int
	for at := 12; at+8 <= len(b); {
		id, size := string(b[at:at+4]), int(binary.LittleEndian.Uint32(b[at+4:]))
		if size > len(b)-at-8 {
			return nil, errors.New("WAV: a chunk runs past the file")
		}
		body := b[at+8 : at+8+size]
		switch id {
		case "fmt ":
			if len(body) < 16 {
				return nil, errors.New("WAV: fmt chunk too small")
			}
			channels, bits = int(binary.LittleEndian.Uint16(body[2:])), int(binary.LittleEndian.Uint16(body[14:]))
		case "data":
			if channels != 2 || (bits != 24 && bits != 32) {
				return nil, errors.New("WAV: a stereo 24- or 32-bit fmt before data")
			}
			w := bits / 8
			out := make([]int32, len(body)/w)
			for i := range out {
				v := uint32(0)
				for k := 0; k < w; k++ {
					v |= uint32(body[i*w+k]) << (8 * uint(k))
				}
				out[i] = int32(v<<uint(32-bits)) >> uint(32-bits)
			}
			return out, nil
		}
		at += 8 + size + size&1
	}
	return nil, errors.New("WAV: no data chunk")
}

// ---------------------------------------------------------- decimation

// decimator low-passes a 1-bit stream and keeps every d-th sample, through
// byte tables: a Kaiser-windowed sinc of 64d+1 taps, cut off at half the
// output rate, beta 14.5 (some 140 dB down from 202 kHz, so nothing folds
// into the 100 kHz the analysis reads), unity gain at DC on the +-1 stream.
type decimator struct {
	d      int
	tables [][256]float64
}

func besselI0(x float64) float64 {
	s, t := 1.0, 1.0
	for k := 1; k < 300; k++ {
		t *= (x / float64(2*k)) * (x / float64(2*k))
		s += t
		if t < 1e-17*s {
			break
		}
	}
	return s
}

func newDecimator(d int) *decimator {
	taps := 64*d + 1
	center := float64(32 * d)
	fc := 0.5 / float64(d)
	beta := 14.5
	h := make([]float64, 8*(8*d+1))
	sum := 0.0
	for i := 0; i < taps; i++ {
		x := float64(i) - center
		s := 2 * fc
		if x != 0 {
			s = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		r := x / center
		h[i] = s * besselI0(beta*math.Sqrt(math.Max(0, 1-r*r))) / besselI0(beta)
		sum += h[i]
	}
	for i := range h {
		h[i] /= sum
	}
	dm := &decimator{d: d, tables: make([][256]float64, 8*d+1)}
	for j := range dm.tables {
		for v := 0; v < 256; v++ {
			s := 0.0
			for i := 0; i < 8; i++ {
				if v&(0x80>>uint(i)) != 0 {
					s += h[8*j+i]
				} else {
					s -= h[8*j+i]
				}
			}
			dm.tables[j][v] = s
		}
	}
	return dm
}

// run decimates bits; an output whose window reaches past the stream's
// whole bytes is zero.
func (dm *decimator) run(ch []byte, bits int64) []float64 {
	whole := int(bits / 8)
	out := make([]float64, int(bits/int64(dm.d)))
	span := len(dm.tables)
	for m := range out {
		start := m*dm.d/8 - 4*dm.d
		if start < 0 || start+span > whole {
			continue
		}
		s := 0.0
		for j := 0; j < span; j++ {
			s += dm.tables[j][ch[start+j]]
		}
		out[m] = s
	}
	return out
}

// ------------------------------------------------------------- analysis

// blocks are a signal's 10 ms blocks: the 1 kHz and 2 kHz components of
// each, Hann windowed, as complex amplitudes (a steady sine of amplitude a
// gives |X| = a), and each block's RMS. The window keeps a DSD stream's
// ultrasonic noise out of the bins: a 10 ms rectangle's sidelobes let it
// read some -90 dBFS at 1 kHz in silence, which the reference has not.
type blocks struct {
	rate int
	x    [2][2][]complex128 // [channel][0: 1 kHz, 1: 2 kHz][block]
	rms  [2][]float64
}

var toneHz = [2]float64{1000, 2000}

func analyse(ch [2][]float64, rate int) blocks {
	n := rate / 100
	out := blocks{rate: rate}
	var tw [2][]complex128
	wsum := 0.0
	for i := 0; i < n; i++ {
		wsum += 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n))
	}
	for f := range toneHz {
		tw[f] = make([]complex128, n)
		for i := range tw[f] {
			w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n))
			tw[f][i] = complex(w, 0) * cmplx.Exp(complex(0, -2*math.Pi*toneHz[f]*float64(i)/float64(rate)))
		}
	}
	nb := len(ch[0]) / n
	for c := 0; c < 2; c++ {
		out.rms[c] = make([]float64, nb)
		for f := range toneHz {
			out.x[c][f] = make([]complex128, nb)
		}
		for b := 0; b < nb; b++ {
			seg := ch[c][b*n : (b+1)*n]
			var acc [2]complex128
			s2 := 0.0
			for i, v := range seg {
				acc[0] += complex(v, 0) * tw[0][i]
				acc[1] += complex(v, 0) * tw[1][i]
				s2 += v * v
			}
			for f := range toneHz {
				out.x[c][f][b] = acc[f] * complex(2/wsum, 0)
			}
			out.rms[c][b] = math.Sqrt(s2 / float64(n))
		}
	}
	return out
}

// welch is a Welch estimate of x's one-sided power per bin, Kaiser windowed
// (beta 20), summing to its mean square, over nfft-point segments; nil when
// x is shorter than one.
func welch(x []float64, nfft int) []float64 {
	p, segs := welchSum(x, nfft)
	if segs == 0 {
		return nil
	}
	for k := range p {
		p[k] /= float64(segs)
	}
	return p
}

// welchSum is welch's segments' periodograms summed, and their count.
func welchSum(x []float64, nfft int) ([]float64, int) {
	w := make([]float64, nfft)
	wsum := 0.0
	for i := range w {
		r := 2*float64(i)/float64(nfft-1) - 1
		w[i] = besselI0(20*math.Sqrt(math.Max(0, 1-r*r))) / besselI0(20)
		wsum += w[i] * w[i]
	}
	p := make([]float64, nfft/2+1)
	buf := make([]complex128, nfft)
	segs := 0
	for at := 0; at+nfft <= len(x); at += nfft / 2 {
		for i := range buf {
			buf[i] = complex(x[at+i]*w[i], 0)
		}
		fft(buf)
		for k := range p {
			m := real(buf[k])*real(buf[k]) + imag(buf[k])*imag(buf[k])
			if k != 0 && k != nfft/2 {
				m *= 2
			}
			p[k] += m / (wsum * float64(nfft))
		}
		segs++
	}
	return p, segs
}

func fft(x []complex128) {
	n := len(x)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		wl := cmplx.Exp(complex(0, -2*math.Pi/float64(size)))
		for i := 0; i < n; i += size {
			w := complex(1, 0)
			for k := 0; k < size/2; k++ {
				u, v := x[i+k], x[i+k+size/2]*w
				x[i+k], x[i+k+size/2] = u+v, u-v
				w *= wl
			}
		}
	}
}

func bandPower(p []float64, rate int, f1, f2 float64) float64 {
	nfft := 2 * (len(p) - 1)
	s := 0.0
	for k := 1; k < len(p)-1; k++ {
		f := float64(k) * float64(rate) / float64(nfft)
		if f >= f1 && f < f2 {
			s += p[k]
		}
	}
	return s
}

// line is the most one bin from f1 to f2 stands over the median of its 129
// neighbours, in dB, and where.
func line(p []float64, rate int, f1, f2 float64) (float64, float64) {
	nfft := 2 * (len(p) - 1)
	worst, at := math.Inf(-1), 0.0
	buf := make([]float64, 129)
	for k := 64; k < len(p)-64; k++ {
		f := float64(k) * float64(rate) / float64(nfft)
		if f < f1 || f >= f2 {
			continue
		}
		copy(buf, p[k-64:k+65])
		sort.Float64s(buf)
		if r := 10 * math.Log10(p[k]/buf[64]); r > worst {
			worst, at = r, f
		}
	}
	return worst, at
}

// stretches are runs of at least min consecutive true values in mask, each
// shrunk by edge on both sides: [start, end) block indexes.
func stretches(mask []bool, min, edge int) [][2]int {
	var out [][2]int
	for i := 0; i < len(mask); {
		if !mask[i] {
			i++
			continue
		}
		j := i
		for j < len(mask) && mask[j] {
			j++
		}
		if a, b := i+edge, j-edge; b-a >= min {
			out = append(out, [2]int{a, b})
		}
		i = j
	}
	return out
}

// order is the channel order a signal's tones show: LR with the 1 kHz on
// the left and the 2 kHz on the right, RL the reverse, "" otherwise.
func order(b blocks) string {
	var e [2][2]float64
	for c := 0; c < 2; c++ {
		for f := 0; f < 2; f++ {
			for _, x := range b.x[c][f] {
				e[c][f] += real(x)*real(x) + imag(x)*imag(x)
			}
		}
	}
	// A 10 ms block that holds a burst's 10 ms ramp leaks some 40 dB of it
	// into the other tone's bin; a swap reverses the energies by 80 dB.
	dominant := func(a, b float64) bool { return a > 1e3*b }
	switch {
	case dominant(e[0][0], e[1][0]) && dominant(e[1][1], e[0][1]):
		return "LR"
	case dominant(e[1][0], e[0][0]) && dominant(e[0][1], e[1][1]):
		return "RL"
	}
	return ""
}

// ------------------------------------------------------------- fixtures

// decoded is one DSD fixture decoded and analysed.
type decoded struct {
	bits    dsd
	signal  [2][]float64 // at analysisRate, +-1 scale
	blocks  blocks
	longest int
	density [2]float64
	order   string
}

func decodeDSD(path string, limits limits) (*decoded, []string) {
	var problems []string
	bridge, _, err := readBridgeDSD(path)
	if err != nil {
		return nil, []string{"the bridge's decoder: " + err.Error()}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, []string{err.Error()}
	}
	var own dsd
	if strings.HasSuffix(path, ".dff") {
		own, err = readOwnDFF(raw)
	} else {
		own, err = readOwnDSF(raw)
	}
	if err != nil {
		return nil, []string{"the independent reader: " + err.Error()}
	}
	if own.rate != bridge.rate || own.bits != bridge.bits || !bytes.Equal(own.ch[0], bridge.ch[0]) || !bytes.Equal(own.ch[1], bridge.ch[1]) {
		problems = append(problems, "the bridge's decoder and the independent reader disagree on the bits")
	}
	d := &decoded{bits: bridge}
	if bridge.rate%analysisRate != 0 || (bridge.rate/analysisRate)%8 != 0 {
		return nil, append(problems, fmt.Sprintf("rate %d is not a DSD rate this check decimates", bridge.rate))
	}
	dm := newDecimator(bridge.rate / analysisRate)
	for c := 0; c < 2; c++ {
		d.signal[c] = dm.run(bridge.ch[c], bridge.bits)
		ones, run, last := int64(0), 0, -1
		for k := int64(0); k < bridge.bits; k++ {
			v := bridge.bit(c, k)
			ones += int64(v)
			if v == last {
				run++
			} else {
				run = 1
			}
			last = v
			if run > d.longest {
				d.longest = run
			}
		}
		d.density[c] = float64(ones) / float64(bridge.bits)
	}
	if d.longest > limits.run {
		problems = append(problems, fmt.Sprintf("a run of %d equal bits (at most %d)", d.longest, limits.run))
	}
	for c := 0; c < 2; c++ {
		if math.Abs(d.density[c]-0.5) > 0.01 {
			problems = append(problems, fmt.Sprintf("channel %d's density is %.4f", c, d.density[c]))
		}
	}
	d.blocks = analyse(d.signal, analysisRate)
	d.order = order(d.blocks)
	return d, problems
}

// named is a fixture and the manifest that declares it.
type named struct {
	manifest string
	f        qualification.Fixture
}

// orderProblems are the fixtures naming the decoded DSD file at path whose
// manifest declares another channel order than its tones show.
func orderProblems(d *decoded, path string, fixtures []named) []string {
	var out []string
	for _, n := range fixtures {
		if d.order != n.f.Order {
			out = append(out, fmt.Sprintf("%s %s (%s): decoded channel order %q, the manifest says %q", n.manifest, n.f.ID, path, d.order, n.f.Order))
		}
	}
	return out
}

// fixtureReport is what compareToReference measured, for the report.
type fixtureReport struct {
	level, freqOff, silence, hfSilence, hfTones, line, lineAt float64
	steady                                                    int
}

// compareToReference holds a decoded DSD fixture to its WAV reference: the
// DSD's tones are the reference's 6.02 dB down (0 dB SA-CD is half the +-1
// range), block by block and channel by channel, at the same frequencies;
// where the reference is silent the DSD has no tone and no more in-band
// noise than limits.silence; the 20-100 kHz noise is under its bound in
// silence and in tones, alike in both, with no line standing out of it.
func compareToReference(d *decoded, ref pcm, limits limits) (fixtureReport, []string) {
	var problems []string
	var rep fixtureReport
	blockProblems := 0
	note := func(format string, args ...any) {
		if blockProblems++; blockProblems <= 8 {
			problems = append(problems, fmt.Sprintf(format, args...))
		}
	}
	rb := analyse(ref.ch, ref.rate)
	nb := len(rb.rms[0])
	if len(d.blocks.rms[0]) < nb {
		nb = len(d.blocks.rms[0])
	}
	rep.silence, rep.hfSilence, rep.hfTones, rep.line = math.Inf(-1), math.Inf(-1), math.Inf(-1), math.Inf(-1)
	worst := 0.0
	var silentMask, toneMask [2][]bool
	for c := 0; c < 2; c++ {
		silentMask[c] = make([]bool, nb)
		toneMask[c] = make([]bool, nb)
		for b := 0; b < nb; b++ {
			silentMask[c][b] = rb.rms[c][b] < 1e-6
			toneMask[c][b] = rb.rms[c][b] > 1e-3
			for f := 0; f < 2; f++ {
				r := cmplx.Abs(rb.x[c][f][b])
				g := 2 * cmplx.Abs(d.blocks.x[c][f][b])
				switch {
				case r >= math.Pow(10, -90.0/20):
					if math.Abs(g-r) > (math.Pow(10, limits.level/20)-1)*r+1e-6 {
						note("block %d channel %d %g Hz: %.2f dBFS against the reference's %.2f", b, c, toneHz[f], 20*math.Log10(g), 20*math.Log10(r))
					}
					if r >= math.Pow(10, -60.0/20) {
						worst = math.Max(worst, math.Abs(20*math.Log10(g/r)))
						rep.steady++
					}
				case r < 1e-6:
					if g > math.Pow(10, limits.absent/20) {
						note("block %d channel %d: a %g Hz tone at %.1f dBFS where the reference has none", b, c, toneHz[f], 20*math.Log10(g))
					}
				}
			}
		}
	}
	rep.level = worst
	if blockProblems > 8 {
		problems = append(problems, fmt.Sprintf("and %d more blocks", blockProblems-8))
	}
	if rep.steady == 0 {
		problems = append(problems, "no tone to compare")
	}
	// Frequencies: the phase of a steady burst's blocks advances by
	// 2 pi (f - f0) 10 ms a block.
	for c := 0; c < 2; c++ {
		for f := 0; f < 2; f++ {
			peak := 0.0
			for b := 0; b < nb; b++ {
				peak = math.Max(peak, cmplx.Abs(rb.x[c][f][b]))
			}
			if peak < 1e-3 {
				continue
			}
			for _, sig := range []struct {
				name string
				x    []complex128
			}{{"DSD", d.blocks.x[c][f]}, {"reference", rb.x[c][f]}} {
				sum, n := 0.0, 0
				for b := 1; b < nb; b++ {
					if cmplx.Abs(rb.x[c][f][b]) > 0.99*peak && cmplx.Abs(rb.x[c][f][b-1]) > 0.99*peak {
						sum += cmplx.Phase(sig.x[b] / sig.x[b-1])
						n++
					}
				}
				if n < 10 {
					continue
				}
				off := sum / float64(n) / (2 * math.Pi * 0.01)
				rep.freqOff = math.Max(rep.freqOff, math.Abs(off))
				if math.Abs(off) > limits.frequency {
					problems = append(problems, fmt.Sprintf("channel %d's %g Hz burst in the %s is %.4f Hz off", c, toneHz[f], sig.name, off))
				}
			}
		}
	}
	if d.order == "" || d.order != order(rb) {
		problems = append(problems, fmt.Sprintf("the DSD's channel order is %q, its reference's %q", d.order, order(rb)))
	}
	// Silences and the 20-100 kHz band, stretch by stretch; lines in the
	// periodograms of all of a channel's silence, and of all its tones.
	n := analysisRate / 100
	const nfft = 8192
	hfBound, ok := limits.hf[d.bits.rate]
	if !ok {
		problems = append(problems, fmt.Sprintf("no 20-100 kHz bound for rate %d", d.bits.rate))
	}
	for c := 0; c < 2; c++ {
		for kind, mask := range [2][]bool{silentMask[c], toneMask[c]} {
			pooled := make([]float64, nfft/2+1)
			pooledSegs := 0
			for _, s := range stretches(mask, 6, 2) {
				p, segs := welchSum(d.signal[c][s[0]*n:s[1]*n], nfft)
				if segs == 0 {
					continue
				}
				for k := range p {
					pooled[k] += p[k]
					p[k] /= float64(segs)
				}
				pooledSegs += segs
				in, hf := saCD(bandPower(p, analysisRate, 20, 20000)), saCD(bandPower(p, analysisRate, 20000, 100000))
				if kind == 0 {
					rep.silence, rep.hfSilence = math.Max(rep.silence, in), math.Max(rep.hfSilence, hf)
					if in > limits.silence {
						problems = append(problems, fmt.Sprintf("channel %d silence at %.2f s holds %.1f dB SA-CD in band", c, float64(s[0])/100, in))
					}
				} else {
					rep.hfTones = math.Max(rep.hfTones, hf)
				}
			}
			if pooledSegs < 8 {
				continue
			}
			for k := range pooled {
				pooled[k] /= float64(pooledSegs)
			}
			if l, at := line(pooled, analysisRate, 20000, 150000); l > rep.line {
				rep.line, rep.lineAt = l, at
			}
		}
	}
	if math.Max(rep.hfSilence, rep.hfTones) > hfBound {
		problems = append(problems, fmt.Sprintf("20-100 kHz holds %.1f dB SA-CD (bound %g)", math.Max(rep.hfSilence, rep.hfTones), hfBound))
	}
	if !math.IsInf(rep.hfTones, -1) && math.Abs(rep.hfSilence-rep.hfTones) > limits.hfSpread {
		problems = append(problems, fmt.Sprintf("20-100 kHz differs between silence (%.1f) and tones (%.1f)", rep.hfSilence, rep.hfTones))
	}
	if rep.line > limits.line {
		problems = append(problems, fmt.Sprintf("a line %.1f dB over the noise at %.0f Hz", rep.line, rep.lineAt))
	}
	return rep, problems
}

// --------------------------------------------------------------- joins

// boundaryLevels builds the stream a sink plays for a run of compatible DSD
// fixtures: 0x69 silence before them (its prefill), their bits joined as the
// sink joins them, inside a byte where a fixture ends inside one, then 0x69
// silence phased to its position (the pattern's bit for each position mod
// 8, as the sink pads a last partial word, then its tail). It answers the
// audio band's level, in dB SA-CD, in the 10 ms around each boundary: the
// prefill's end, each join, and the tail's start. The silences are half a
// second, for the measurement; how long the sink's own last does not move
// the step at their edge.
func boundaryLevels(parts []*decoded) ([]float64, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	rate := parts[0].bits.rate
	half := int64(rate / 2)
	total := 2 * half
	for _, p := range parts {
		if p.bits.rate != rate {
			return nil, errors.New("one run of different rates")
		}
		total += p.bits.bits
	}
	var stream [2][]byte
	for c := 0; c < 2; c++ {
		stream[c] = make([]byte, (total+7)/8)
	}
	set := func(c int, n int64) { stream[c][n>>3] |= 0x80 >> uint(n&7) }
	silence := func(from, to int64) {
		for n := from; n < to; n++ {
			if (0x69>>uint(7-n%8))&1 == 1 {
				set(0, n)
				set(1, n)
			}
		}
	}
	silence(0, half)
	at := half
	bounds := []int64{at}
	for _, p := range parts {
		for c := 0; c < 2; c++ {
			for k := int64(0); k < p.bits.bits; k++ {
				if p.bits.bit(c, k) == 1 {
					set(c, at+k)
				}
			}
		}
		at += p.bits.bits
		bounds = append(bounds, at)
	}
	silence(at, total)
	dm := newDecimator(rate / analysisRate)
	lp := lowpass20k()
	var out []float64
	win := analysisRate / 100
	reach := int64(win/2 + len(lp)/2 + 256)
	for _, j := range bounds {
		worst := math.Inf(-1)
		for c := 0; c < 2; c++ {
			// The decimated stream from reach outputs before the boundary
			// to reach after, low-passed only in the 10 ms around it.
			lo := (j/int64(dm.d) - reach) * int64(dm.d) / 8
			hi := (j/int64(dm.d) + reach) * int64(dm.d) / 8
			if lo < 0 || hi > int64(len(stream[c])) {
				return nil, errors.New("a boundary too near the stream's edge")
			}
			x := dm.run(stream[c][lo:hi], (hi-lo)*8)
			mid := int(j/int64(dm.d) - lo*8/int64(dm.d))
			s := 0.0
			for i := mid - win/2; i < mid+win/2; i++ {
				v := 0.0
				for k, h := range lp {
					v += h * x[i-len(lp)/2+k]
				}
				s += v * v
			}
			worst = math.Max(worst, saCD(s/float64(win)))
		}
		out = append(out, worst)
	}
	return out, nil
}

// lowpass20k is a 20 kHz low-pass at analysisRate: a Kaiser-windowed sinc
// of 1025 taps cut off at 21 kHz, beta 12.
func lowpass20k() []float64 {
	const taps = 1025
	fc := 21000.0 / analysisRate
	h := make([]float64, taps)
	sum := 0.0
	for i := range h {
		x := float64(i - taps/2)
		s := 2 * fc
		if x != 0 {
			s = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		r := x / (taps / 2)
		h[i] = s * besselI0(12*math.Sqrt(math.Max(0, 1-r*r))) / besselI0(12)
		sum += h[i]
	}
	for i := range h {
		h[i] /= sum
	}
	return h
}

// --------------------------------------------------------------- ffmpeg

// ffmpegOrder decodes a DSD file with ffmpeg's dsd_* decoders, when ffmpeg
// is on PATH, and answers the channel order its tones show.
func ffmpegOrder(path string) (string, bool, error) {
	exe, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", false, nil
	}
	out, err := exec.Command(exe, "-v", "error", "-i", path, "-f", "f32le", "-acodec", "pcm_f32le", "-ar", "352800", "-").Output()
	if err != nil {
		return "", true, err
	}
	n := len(out) / 8
	var ch [2][]float64
	for c := 0; c < 2; c++ {
		ch[c] = make([]float64, n)
		for i := 0; i < n; i++ {
			ch[c][i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(out[8*i+4*c:])))
		}
	}
	return order(analyse(ch, 352800)), true, nil
}

// --------------------------------------------------------------- schema

// validate holds v to the JSON Schema keywords the qualification schemas
// use: type, const, enum, required, properties, additionalProperties false,
// items, minItems, maxItems, minLength, pattern and minimum.
func validate(schema map[string]any, v any, at string) []string {
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, at+": "+fmt.Sprintf(format, args...)) }
	if t, ok := schema["type"].(string); ok {
		switch t {
		case "object":
			if _, ok := v.(map[string]any); !ok {
				fail("not an object")
				return problems
			}
		case "array":
			if _, ok := v.([]any); !ok {
				fail("not an array")
				return problems
			}
		case "string":
			if _, ok := v.(string); !ok {
				fail("not a string")
				return problems
			}
		case "integer":
			n, ok := v.(json.Number)
			if _, err := n.Int64(); !ok || err != nil {
				fail("not an integer")
				return problems
			}
		default:
			fail("schema type %q is not one this check reads", t)
		}
	}
	if c, ok := schema["const"]; ok && !jsonEqual(c, v) {
		fail("is not %v", c)
	}
	if e, ok := schema["enum"].([]any); ok {
		found := false
		for _, x := range e {
			found = found || jsonEqual(x, v)
		}
		if !found {
			fail("is not one of %v", e)
		}
	}
	if s, ok := v.(string); ok {
		if m, ok := schema["minLength"].(json.Number); ok {
			if n, _ := m.Int64(); int64(len([]rune(s))) < n {
				fail("shorter than %d", n)
			}
		}
		if p, ok := schema["pattern"].(string); ok && !matchHex64(p, s) {
			fail("does not match %s", p)
		}
	}
	if n, ok := v.(json.Number); ok {
		if m, ok := schema["minimum"].(json.Number); ok {
			x, _ := n.Float64()
			y, _ := m.Float64()
			if x < y {
				fail("under %v", m)
			}
		}
	}
	if obj, ok := v.(map[string]any); ok {
		props, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, r := range required {
			if _, ok := obj[r.(string)]; !ok {
				fail("lacks %s", r)
			}
		}
		for k, x := range obj {
			sub, ok := props[k].(map[string]any)
			if !ok {
				if schema["additionalProperties"] == false {
					fail("has %s, which the schema does not allow", k)
				}
				continue
			}
			problems = append(problems, validate(sub, x, at+"."+k)...)
		}
	}
	if arr, ok := v.([]any); ok {
		if m, ok := schema["minItems"].(json.Number); ok {
			if n, _ := m.Int64(); int64(len(arr)) < n {
				fail("fewer than %d items", n)
			}
		}
		if m, ok := schema["maxItems"].(json.Number); ok {
			if n, _ := m.Int64(); int64(len(arr)) > n {
				fail("more than %d items", n)
			}
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, x := range arr {
				problems = append(problems, validate(items, x, fmt.Sprintf("%s[%d]", at, i))...)
			}
		}
	}
	return problems
}

func jsonEqual(a, b any) bool {
	x, okx := a.(json.Number)
	y, oky := b.(json.Number)
	if okx && oky {
		fx, ex := x.Float64()
		fy, ey := y.Float64()
		return ex == nil && ey == nil && fx == fy
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

// matchHex64 is the schema's one pattern, ^[0-9a-f]{64}$; any other pattern
// fails, so a schema that grows one is noticed here.
func matchHex64(pattern, s string) bool {
	if pattern != "^[0-9a-f]{64}$" || len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

func loadSchema(name string) (map[string]any, error) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "player", "qualification", name))
	if err != nil {
		return nil, err
	}
	v, err := decodeJSON(b)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("the schema is not an object")
	}
	return m, nil
}

// ----------------------------------------------------------------- runs

// run is one entry of runs.json: a dsd-test invocation the session makes.
type run struct {
	Manifest, Transport, Format, Scenario, Purpose string
	Rate                                           int
	Fixtures                                       []string
	Native                                         *struct {
		Format string
		Rate   int
	}
}

// checkRun holds a run to what bridge output dsd-test checks of its fixture
// list before it opens anything (cmd/bridge/output_dsdtest.go): every ID in
// the manifest; outside the transition, each a DSD fixture at the run's
// rate, a backend-lifecycle's WAV fixtures aside, which must play a DSD one;
// a DoP fixture's reference at its carrier (rate / 16); a DoP contact one
// fixture whose carrier frames and the plan's 10 ms prefill and tail fit
// ten seconds; the transition three DSD fixtures at its native, DoP and
// native rates. Every DSD fixture it names, and its reference, are what the
// harness verifies before playing (verifyNativeFixture), which the manifest
// check covers.
func checkRun(r run, m qualification.Manifest) []string {
	var problems []string
	byID := map[string]qualification.Fixture{}
	for _, f := range m.Fixtures {
		byID[f.ID] = f
	}
	dsd := func(f qualification.Fixture) bool { return f.Container == "DSF" || f.Container == "DFF" }
	transition := r.Scenario == qualification.TransitionScenario
	switch r.Scenario {
	case "level-check", "matrix", "interactive", "backend-lifecycle", qualification.TransitionScenario:
	default:
		problems = append(problems, "scenario "+r.Scenario)
	}
	if r.Rate != 2822400 && r.Rate != 5644800 && r.Rate != 11289600 {
		problems = append(problems, fmt.Sprintf("--bit-rate %d", r.Rate))
	}
	if !(qualification.Tuple{Transport: r.Transport, Format: r.Format}).ValidTransport() {
		problems = append(problems, "transport "+r.Transport+" with format "+r.Format)
	}
	anyDSD := false
	for i, id := range r.Fixtures {
		f, ok := byID[id]
		if !ok {
			problems = append(problems, id+" is not in "+r.Manifest)
			continue
		}
		anyDSD = anyDSD || dsd(f)
		if transition {
			if r.Native == nil {
				continue
			}
			rate := r.Native.Rate
			if i == 1 {
				rate = r.Rate
			}
			if !dsd(f) || f.Rate != rate {
				problems = append(problems, "the transition's fixture "+id)
			}
			continue
		}
		if (f.Rate != r.Rate || !dsd(f)) && !(r.Scenario == "backend-lifecycle" && f.Container == "WAV") {
			problems = append(problems, "fixture-list tuple mismatch: "+id)
		}
		if r.Transport == "dop" && dsd(f) {
			carrier := r.Rate / 16
			if byID[f.PCMReference].Rate != carrier {
				problems = append(problems, "DoP requires same-carrier PCM reference: "+id)
			}
			if r.Scenario == "level-check" && (f.Frames+15)/16+2*int64((carrier*10+999)/1000) > int64(carrier)*10 {
				problems = append(problems, "DoP contact exceeds ten seconds: "+id)
			}
		}
		if (r.Scenario == "level-check" || r.Scenario == "matrix") && dsd(f) {
			if _, ok := byID[f.PCMReference]; !ok {
				problems = append(problems, "no reference for "+id)
			}
		}
	}
	if r.Scenario == "backend-lifecycle" && !anyDSD {
		problems = append(problems, "backend lifecycle requires native execution")
	}
	if r.Transport == "dop" && r.Scenario == "level-check" && len(r.Fixtures) != 1 {
		problems = append(problems, "DoP contact requires exactly one fixture")
	}
	if transition && (len(r.Fixtures) != 3 || r.Native == nil) {
		problems = append(problems, "the transition plays three DSD fixtures: native, DoP, native")
	}
	if len(r.Fixtures) == 0 {
		problems = append(problems, "no fixtures")
	}
	return problems
}

// ------------------------------------------------------------ the check

// checkDir checks a generated asset directory and answers its report's
// lines and every problem it found.
func checkDir(dir string, limits limits) (report, problems []string) {
	say := func(format string, args ...any) { report = append(report, fmt.Sprintf(format, args...)) }
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	schema, err := loadSchema("manifest-v1.schema.json")
	if err != nil {
		bad("schema: %v", err)
		return
	}
	names, _ := filepath.Glob(filepath.Join(dir, "manifest*.json"))
	sort.Strings(names)
	if len(names) == 0 {
		bad("no manifest*.json in %s", dir)
		return
	}
	assets := filepath.Join(dir, "assets")
	manifests := map[string]qualification.Manifest{}
	type pair struct{ dsd, ref string }
	pairs := map[pair][]string{}
	// Every fixture that names a file is held to it: two fixtures of one
	// file, or of one manifest and another, may declare it differently.
	var fixtures []named
	dsdFiles := map[string][]named{}
	for _, name := range names {
		base := filepath.Base(name)
		b, err := os.ReadFile(name)
		if err != nil {
			bad("%s: %v", base, err)
			continue
		}
		v, err := decodeJSON(b)
		if err != nil {
			bad("%s: %v", base, err)
			continue
		}
		if p := validate(schema, v, base); len(p) > 0 {
			problems = append(problems, p...)
		}
		m, err := qualification.ReadManifest(bytes.NewReader(b))
		if err != nil {
			bad("%s: the qualification loader refuses it: %v", base, err)
			continue
		}
		if err := m.CheckAssets(assets); err != nil {
			bad("%s: CheckAssets: %v", base, err)
			continue
		}
		manifests[base] = m
		byID := map[string]qualification.Fixture{}
		for _, f := range m.Fixtures {
			byID[f.ID] = f
		}
		for _, f := range m.Fixtures {
			fixtures = append(fixtures, named{base, f})
			if f.Container == "WAV" {
				continue
			}
			dsdFiles[f.Path] = append(dsdFiles[f.Path], named{base, f})
			ref := byID[f.PCMReference]
			pairs[pair{f.Path, ref.Path}] = append(pairs[pair{f.Path, ref.Path}], base+" "+f.ID)
		}
		say("%s: %d fixtures; schema, ReadManifest and CheckAssets pass; sha256 %s", base, len(m.Fixtures), fileDigest(b))
	}
	// What the generation wrote beside the assets names no directory of the
	// machine that made it, nor its user or host: a copied directory says
	// nothing of where it was made.
	meta, read := metadataProblems(dir, identifyingNames())
	problems = append(problems, meta...)
	say("metadata: %d files beside the assets hold no absolute path, user or host name (%d found)", read, len(meta))
	// The headers, as dsd-test checks each fixture it plays and its
	// reference (verifyNativeFixture): each file decoded once.
	pcmHeaders, dsdHeaders := map[string]format.PCM{}, map[string]format.NativeDSD{}
	headerErr := map[string]error{}
	for _, n := range fixtures {
		f, path := n.f, filepath.Join(assets, n.f.Path)
		if _, ok := headerErr[f.Path]; !ok {
			var err error
			if f.Container == "WAV" {
				_, pcmHeaders[f.Path], err = readBridgeWAV(path)
			} else {
				_, dsdHeaders[f.Path], err = readBridgeDSD(path)
			}
			headerErr[f.Path] = err
		}
		err := headerErr[f.Path]
		if err == nil && f.Container == "WAV" {
			if p := pcmHeaders[f.Path]; p.RateHz != f.Rate || p.Channels != f.Channels || p.Frames != f.Frames {
				err = errors.New("PCM reference header disagrees with manifest")
			}
		} else if err == nil {
			if p := dsdHeaders[f.Path]; p.BitRateHz != f.Rate || p.Channels != f.Channels || p.Frames != f.Frames || !strings.EqualFold(string(p.Container), f.Container) {
				err = errors.New("DSD fixture header disagrees with manifest")
			}
		}
		if err != nil {
			bad("%s %s (%s): %v", n.manifest, f.ID, f.Path, err)
		}
	}
	say("headers: %d fixtures of %d files decode with the bridge's decoders as their manifests declare", len(fixtures), len(headerErr))
	decodedDSD := map[string]*decoded{}
	paths := make([]string, 0, len(dsdFiles))
	for p := range dsdFiles {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	type result struct {
		d     *decoded
		probs []string
	}
	results := make([]result, len(paths))
	var wg sync.WaitGroup
	work := make(chan int)
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				d, probs := decodeDSD(filepath.Join(assets, paths[i]), limits)
				results[i] = result{d, probs}
			}
		}()
	}
	for i := range paths {
		work <- i
	}
	close(work)
	wg.Wait()
	for i, p := range paths {
		d, probs := results[i].d, results[i].probs
		for _, x := range probs {
			bad("%s: %s", p, x)
		}
		if d == nil {
			continue
		}
		ffOrder, ran, ffErr := ffmpegOrder(filepath.Join(assets, p))
		problems = append(problems, orderProblems(d, p, dsdFiles[p])...)
		for _, n := range dsdFiles[p] {
			if ran && (ffErr != nil || ffOrder != n.f.Order) {
				bad("%s %s (%s): ffmpeg's decode: order %q, %v", n.manifest, n.f.ID, p, ffOrder, ffErr)
			}
		}
		decodedDSD[p] = d
		say("%s: %d bits a channel at %d, bridge decoder and independent reader agree; order %s; longest run %d; density %.6f/%.6f",
			p, d.bits.bits, d.bits.rate, d.order, d.longest, d.density[0], d.density[1])
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		say("ffmpeg is not on PATH: its dsd_* decoders were not run")
	}
	type worstAt struct{ level, freqOff, silence, hfSilence, hfTones, line, boundary float64 }
	worst := map[int]*worstAt{}
	at := func(rate int) *worstAt {
		if worst[rate] == nil {
			worst[rate] = &worstAt{silence: math.Inf(-1), hfSilence: math.Inf(-1), hfTones: math.Inf(-1), line: math.Inf(-1), boundary: math.Inf(-1)}
		}
		return worst[rate]
	}
	refs := map[string]pcm{}
	keys := make([]pair, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].dsd+keys[i].ref < keys[j].dsd+keys[j].ref })
	for _, k := range keys {
		d := decodedDSD[k.dsd]
		if d == nil {
			continue
		}
		ref, ok := refs[k.ref]
		if !ok {
			var err error
			ref, _, err = readBridgeWAV(filepath.Join(assets, k.ref))
			if err != nil {
				bad("%s: %v", k.ref, err)
				continue
			}
			refs[k.ref] = ref
		}
		if d.bits.bits*int64(ref.rate) != ref.frames*int64(d.bits.rate) {
			bad("%s against %s: the durations differ", k.dsd, k.ref)
		}
		rep, probs := compareToReference(d, ref, limits)
		for _, x := range probs {
			bad("%s against %s: %s", k.dsd, k.ref, x)
		}
		w := at(d.bits.rate)
		w.level, w.freqOff, w.silence = math.Max(w.level, rep.level), math.Max(w.freqOff, rep.freqOff), math.Max(w.silence, rep.silence)
		w.hfSilence, w.hfTones, w.line = math.Max(w.hfSilence, rep.hfSilence), math.Max(w.hfTones, rep.hfTones), math.Max(w.line, rep.line)
		say("%s against %s (%s): %d tone blocks within %.4f dB, bursts within %.2g Hz; silence %.1f dB SA-CD in band; 20-100 kHz %.1f (silence) %.1f (tones) dB SA-CD; largest line %.1f dB at %.0f Hz",
			k.dsd, k.ref, strings.Join(pairs[k], ", "), rep.steady, rep.level, rep.freqOff, rep.silence, rep.hfSilence, rep.hfTones, rep.line, rep.lineAt)
	}
	// The session's runs, and the joins its compatible runs make.
	rb, err := os.ReadFile(filepath.Join(dir, "runs.json"))
	if err != nil {
		bad("runs.json: %v", err)
		return
	}
	var runs struct{ Runs []run }
	if err := json.Unmarshal(rb, &runs); err != nil {
		bad("runs.json: %v", err)
		return
	}
	measured := map[string]bool{}
	for i, r := range runs.Runs {
		m, ok := manifests[r.Manifest]
		if !ok {
			bad("run %d: no manifest %s", i, r.Manifest)
			continue
		}
		for _, p := range checkRun(r, m) {
			bad("run %d (%s %s %s %d): %s", i, r.Manifest, r.Transport, r.Scenario, r.Rate, p)
		}
		byID := map[string]qualification.Fixture{}
		for _, f := range m.Fixtures {
			byID[f.ID] = f
		}
		// Each stretch of consecutive DSD fixtures is one sink's run: the
		// matrix's and the interactive runs' whole lists, a lifecycle's
		// fixtures between its PCM edges, a transition's each alone.
		var stretch []string
		flush := func() {
			defer func() { stretch = nil }()
			key := strings.Join(stretch, " ")
			if len(stretch) == 0 || measured[key] {
				return
			}
			measured[key] = true
			var parts []*decoded
			var bits int64
			for _, id := range stretch {
				d := decodedDSD[byID[id].Path]
				if d == nil {
					return
				}
				parts = append(parts, d)
				bits += d.bits.bits
			}
			levels, err := boundaryLevels(parts)
			if err != nil {
				bad("the boundaries of %s: %v", key, err)
				return
			}
			words := make([]string, len(levels))
			w := at(parts[0].bits.rate)
			for j, l := range levels {
				w.boundary = math.Max(w.boundary, l)
				words[j] = fmt.Sprintf("%.1f", l)
				if l > limits.boundary {
					bad("the boundaries of %s: boundary %d holds %.1f dB SA-CD in band (bound %g)", key, j, l, limits.boundary)
				}
			}
			whole := "on a whole 32-bit word, so the sink pads nothing"
			if bits%32 != 0 {
				whole = fmt.Sprintf("%d bits into a 32-bit word, which the sink pads with 0x69", bits%32)
			}
			say("boundaries of %s (0x69, the joins, 0x69): %s dB SA-CD; it ends %s", key, strings.Join(words, ", "), whole)
		}
		for _, id := range r.Fixtures {
			f, ok := byID[id]
			if ok && (f.Container == "DSF" || f.Container == "DFF") && r.Scenario != qualification.TransitionScenario {
				stretch = append(stretch, id)
				continue
			}
			flush()
			if ok && r.Scenario == qualification.TransitionScenario {
				stretch = []string{id}
				flush()
			}
		}
		flush()
	}
	say("runs.json: %d runs hold to dsd-test's fixture-list rules", len(runs.Runs))
	for _, rate := range []int{2822400, 5644800, 11289600} {
		w, ok := worst[rate]
		if !ok {
			continue
		}
		say("DSD%d: tone blocks within %.4f dB and bursts within %.2g Hz of their references; silence at most %.1f dB SA-CD in band; 20-100 kHz at most %.1f dB SA-CD; no line over %.1f dB; boundaries at most %.1f dB SA-CD",
			rate/44100, w.level, w.freqOff, w.silence, math.Max(w.hfSilence, w.hfTones), w.line, w.boundary)
	}
	return report, problems
}

func fileDigest(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

// ------------------------------------------------------------- metadata

// absolutePath finds an absolute path starting a word of a generated file's
// text: POSIX, a drive's, a share's, or one under ~. A slash inside a word
// ("rate/128", "pi/64", "assets/x.dsf") starts none.
var absolutePath = regexp.MustCompile(`(?:^|[\s"'=:(\[,])(/[^\s"',)\]]+|[A-Za-z]:[\\/][^\s"',)\]]*|\\\\[^\s"',)\]]+|~[A-Za-z0-9_.-]*/[^\s"',)\]]*)`)

// exposure is what of text could say where, or by whom, a file was made:
// the absolute paths it holds, and each of names it holds as a word. A name
// shorter than four characters is too likely a word of the text's own.
func exposure(text string, names []string) []string {
	var found []string
	for _, m := range absolutePath.FindAllStringSubmatch(text, -1) {
		found = append(found, m[1])
	}
	for _, name := range names {
		if len(name) < 4 {
			continue
		}
		if regexp.MustCompile(`(?:^|[^A-Za-z0-9_])` + regexp.QuoteMeta(name) + `(?:$|[^A-Za-z0-9_])`).MatchString(text) {
			found = append(found, name)
		}
	}
	return found
}

// identifyingNames are the names a generated file must not carry: the
// user's, as the environment and the home directory give it, and the host's.
func identifyingNames() []string {
	set := map[string]bool{}
	for _, v := range []string{"USER", "LOGNAME", "USERNAME"} {
		set[os.Getenv(v)] = true
	}
	if home, err := os.UserHomeDir(); err == nil {
		set[filepath.Base(home)] = true
	}
	if host, err := os.Hostname(); err == nil {
		set[host] = true
		set[strings.SplitN(host, ".", 2)[0]] = true
	}
	delete(set, "")
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// metadataProblems reads the files a generation writes beside its assets,
// its JSON and text files whole and its tar's headers, for what exposure
// finds in them, and says how many it read.
func metadataProblems(dir string, names []string) (problems []string, read int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{err.Error()}, 0
	}
	for _, e := range entries {
		name, path := e.Name(), filepath.Join(dir, e.Name())
		switch {
		case !e.Type().IsRegular():
			continue
		case name == "assets.tar":
			p, err := tarProblems(path, names)
			if err != nil {
				p = append(p, fmt.Sprintf("%s: %v", name, err))
			}
			problems = append(problems, p...)
		case strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".txt") || name == "SHA256SUMS":
			b, err := os.ReadFile(path)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", name, err))
				continue
			}
			for _, hit := range exposure(string(b), names) {
				problems = append(problems, fmt.Sprintf("%s names %q, which says where or by whom it was made", name, hit))
			}
		default:
			continue
		}
		read++
	}
	return problems, read
}

// tarProblems reads a tar's headers, and none of its contents: each
// member's name, link, owner and PAX records, for what exposure finds, and
// any owner a member keeps.
func tarProblems(path string, names []string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var problems []string
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return problems, nil
		}
		if err != nil {
			return problems, err
		}
		fields := []string{h.Name, h.Linkname, h.Uname, h.Gname}
		for _, v := range h.PAXRecords {
			fields = append(fields, v)
		}
		for _, field := range fields {
			for _, hit := range exposure(field, names) {
				problems = append(problems, fmt.Sprintf("assets.tar member %s names %q", h.Name, hit))
			}
		}
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			problems = append(problems, fmt.Sprintf("assets.tar member %s has an owner", h.Name))
		}
	}
}

// TestListeningAssets checks the generated directory -listening-assets
// names (see the package doc), and logs what it measured.
func TestListeningAssets(t *testing.T) {
	if *assetDir == "" {
		t.Skip("no -listening-assets directory: nothing to check")
	}
	report, problems := checkDir(*assetDir, production)
	for _, line := range report {
		t.Log(line)
	}
	for _, p := range problems {
		t.Error(p)
	}
}
