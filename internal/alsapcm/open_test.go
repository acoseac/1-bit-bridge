package alsapcm

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// cfg48k is a request every newFake node takes: 48 kHz, 24 bits in three
// bytes.
var cfg48k = Config{RateHz: 48000, Format: format.S243LE}

// wantReason fails the test unless err carries reason.
func wantReason(t *testing.T, err error, reason Reason) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("got no error, want %s", reason)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("got %T %v, want an *Error with reason %s", err, err, reason)
	}
	if e.Reason != reason {
		t.Fatalf("got reason %s (%v), want %s", e.Reason, err, reason)
	}
	return e
}

// TestOpenRefusesAnEndpointNoCardCanHave pins that a card or device number
// outside 0..255 is refused before anything is opened.
func TestOpenRefusesAnEndpointNoCardCanHave(t *testing.T) {
	for _, ep := range []Endpoint{{-1, 0}, {0, -1}, {256, 0}, {0, 256}} {
		k := newFake(t)
		_, err := open(k, k.now, ep, cfg48k)
		wantReason(t, err, ReasonBadEndpoint)
		if len(k.opened) > 0 {
			t.Errorf("%v: opened %q", ep, k.opened)
		}
	}
}

// TestOpenOpensOnlyTheEndpointsPlaybackNode pins the one path an Endpoint
// names, /dev/snd/pcmC<card>D<device>p.
func TestOpenOpensOnlyTheEndpointsPlaybackNode(t *testing.T) {
	for _, c := range []struct {
		ep   Endpoint
		path string
	}{
		{Endpoint{Card: 5, Device: 0}, "/dev/snd/pcmC5D0p"},
		{Endpoint{Card: 12, Device: 3}, "/dev/snd/pcmC12D3p"},
		{Endpoint{Card: 0, Device: 255}, "/dev/snd/pcmC0D255p"},
	} {
		k := newFake(t)
		k.info.card, k.info.device = int32(c.ep.Card), uint32(c.ep.Device)
		d, err := open(k, k.now, c.ep, cfg48k)
		if err != nil {
			t.Fatalf("%v: %v", c.ep, err)
		}
		if len(k.opened) != 1 || k.opened[0] != c.path {
			t.Errorf("%v opened %q, want only %s", c.ep, k.opened, c.path)
		}
		if got := d.Params().Endpoint; got != c.ep {
			t.Errorf("params endpoint %v, want %v", got, c.ep)
		}
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	}
}

// TestOpenRefusesAConfigItCannotPack pins that a format without a packer,
// a rate out of range and a buffer time out of range are refused before
// anything is opened.
func TestOpenRefusesAConfigItCannotPack(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no format":       {RateHz: 48000},
		"S24_BE":          {RateHz: 48000, Format: "S24_BE"},
		"FLOAT_LE":        {RateHz: 48000, Format: "FLOAT_LE"},
		"DSD_U32_BE":      {RateHz: 352800, Format: "DSD_U32_BE"},
		"rate 0":          {RateHz: 0, Format: format.S16LE},
		"rate 7999":       {RateHz: 7999, Format: format.S16LE},
		"rate 1536001":    {RateHz: 1536001, Format: format.S16LE},
		"negative buffer": {RateHz: 48000, Format: format.S16LE, BufferTime: -time.Millisecond},
		"3 s buffer":      {RateHz: 48000, Format: format.S16LE, BufferTime: 3 * time.Second},
	} {
		k := newFake(t)
		_, err := openFake(k, cfg)
		if ReasonOf(err) != ReasonBadConfig {
			t.Errorf("%s: got %v, want %s", name, err, ReasonBadConfig)
		}
		if len(k.opened) > 0 {
			t.Errorf("%s: opened %q", name, k.opened)
		}
	}
}

// TestOpenNamesABusyEndpoint pins that a node another process holds (MPD
// playing) is a busy refusal that names the endpoint, from the
// non-blocking open's EBUSY, with nothing left open.
func TestOpenNamesABusyEndpoint(t *testing.T) {
	k := newFake(t)
	k.openErr = errnoEBUSY
	_, err := openFake(k, cfg48k)
	e := wantReason(t, err, ReasonBusy)
	if !errors.Is(err, errnoEBUSY) {
		t.Errorf("%v does not wrap EBUSY", err)
	}
	for _, want := range []string{"card 5 device 0", "/dev/snd/pcmC5D0p", "busy"} {
		if !strings.Contains(e.Error(), want) {
			t.Errorf("%q does not say %q", e.Error(), want)
		}
	}
	if k.closes != 0 || len(k.calls) != 0 {
		t.Errorf("a refused open closed %d times and issued %#x", k.closes, k.calls)
	}
}

// TestOpenTypesWhatTheOpenRefused pins the reason each refusal of the open
// call itself becomes.
func TestOpenTypesWhatTheOpenRefused(t *testing.T) {
	for _, c := range []struct {
		err  error
		want Reason
	}{
		{errnoENOENT, ReasonNoDevice},
		{errnoENODEV, ReasonNoDevice},
		{errnoENXIO, ReasonNoDevice},
		{errnoEACCES, ReasonPermission},
		{errnoEPERM, ReasonPermission},
		{errnoEBUSY, ReasonBusy},
		{errNotALSANode, ReasonNotPCM},
		{errnoELOOP, ReasonNotPCM},
		{errno(5), ReasonKernel},
	} {
		k := newFake(t)
		k.openErr = c.err
		_, err := openFake(k, cfg48k)
		if ReasonOf(err) != c.want {
			t.Errorf("open failing with %v: got %v, want %s", c.err, err, c.want)
		}
	}
}

// TestOpenRefusesAProtocolBelowTheFloor pins the protocol check: another
// major or minor is refused whatever its micro, and so is a micro below
// pcmVersionFloor, with the node closed and nothing asked of it but its
// version.
func TestOpenRefusesAProtocolBelowTheFloor(t *testing.T) {
	for _, c := range []struct {
		version uint32
		ok      bool
	}{
		{protocolVersion(2, 0, 14), false},
		{protocolVersion(2, 0, 0), false},
		{protocolVersion(2, 1, 0), false},
		{protocolVersion(2, 1, 18), false},
		{protocolVersion(3, 0, 18), false},
		{protocolVersion(1, 0, 18), false},
		{protocolVersion(2, 0, 15), true},
		{protocolVersion(2, 0, 18), true},
		{protocolVersion(2, 0, 30), true},
	} {
		k := newFake(t)
		k.version = c.version
		d, err := openFake(k, cfg48k)
		if c.ok {
			if err != nil {
				t.Errorf("protocol %s: %v", ProtocolString(c.version), err)
				continue
			}
			if d.Params().Protocol != c.version {
				t.Errorf("params protocol %s, want %s", ProtocolString(d.Params().Protocol), ProtocolString(c.version))
			}
			_ = d.Close()
			continue
		}
		e := wantReason(t, err, ReasonProtocol)
		if !strings.Contains(e.Detail, ProtocolString(c.version)) {
			t.Errorf("%q does not name the kernel's protocol %s", e.Detail, ProtocolString(c.version))
		}
		if len(k.calls) != 1 || k.calls[0] != ioctlPVersion {
			t.Errorf("protocol %s: issued %#x, want PVERSION alone", ProtocolString(c.version), k.calls)
		}
		k.checkClosedOnce(t)
	}
}

// TestOpenChecksTheNodeIsTheEndpointAskedFor pins the INFO check: a node
// the kernel says is another card, another device or a capture stream is
// refused, and closed.
func TestOpenChecksTheNodeIsTheEndpointAskedFor(t *testing.T) {
	for name, set := range map[string]func(*pcmInfo){
		"another card":   func(i *pcmInfo) { i.card = 4 },
		"another device": func(i *pcmInfo) { i.device = 1 },
		"capture":        func(i *pcmInfo) { i.stream = 1 },
	} {
		k := newFake(t)
		set(&k.info)
		_, err := openFake(k, cfg48k)
		e := wantReason(t, err, ReasonNotPCM)
		if !strings.Contains(e.Detail, "/dev/snd/pcmC5D0p") {
			t.Errorf("%s: %q does not name the node", name, e.Detail)
		}
		if k.callsOf(ioctlHWRefine) != 0 {
			t.Errorf("%s: negotiated with the wrong node", name)
		}
		k.checkClosedOnce(t)
	}
}

// TestOpenCommitsExactlyWhatWasAsked pins the negotiation for each format
// the packers write: the request goes to the kernel exactly (one access,
// format and subformat, two channels, the rate), the committed values are
// read back into Params, the software parameters make an underrun an xrun
// and fill no silence, and the stream is prepared last.
func TestOpenCommitsExactlyWhatWasAsked(t *testing.T) {
	for _, f := range []struct {
		format     format.ALSAFormat
		code       uint32
		frameBytes int
	}{
		{format.S16LE, formatS16LE, 4},
		{format.S243LE, formatS243LE, 6},
		{format.S24LE, formatS24LE, 8},
		{format.S32LE, formatS32LE, 8},
	} {
		for _, rate := range []int{44100, 96000, 384000} {
			k := newFake(t)
			k.hw.formats = append(k.hw.formats, formatS24LE)
			d, err := openFake(k, Config{RateHz: rate, Format: f.format})
			if err != nil {
				t.Fatalf("%s at %d: %v", f.format, rate, err)
			}
			p := d.Params()
			if p.Format != f.format || p.RateHz != rate || p.Channels != Channels {
				t.Errorf("params %s, %d ch, %d Hz; want %s, 2 ch, %d Hz", p.Format, p.Channels, p.RateHz, f.format, rate)
			}
			if p.RateNum != uint32(rate) || p.RateDen != 1 {
				t.Errorf("rate %d/%d, want %d/1", p.RateNum, p.RateDen, rate)
			}
			if p.FrameBytes() != f.frameBytes {
				t.Errorf("%s frame is %d bytes, want %d", f.format, p.FrameBytes(), f.frameBytes)
			}
			c := k.committed
			if v, _ := c.maskOf(hwParamAccess).single(); v != accessRWInterleaved {
				t.Errorf("committed access %d", v)
			}
			if v, _ := c.maskOf(hwParamFormat).single(); v != f.code {
				t.Errorf("committed format %d, want %d", v, f.code)
			}
			if v, _ := c.maskOf(hwParamSubformat).single(); v != subformatStd {
				t.Errorf("committed subformat %d", v)
			}
			sw := k.sw
			if sw == nil {
				t.Fatal("no SW_PARAMS")
			}
			if sw.startThreshold != uint64(p.BufferFrames) || sw.stopThreshold != uint64(p.BufferFrames) ||
				sw.availMin != uint64(p.PeriodFrames) || sw.silenceThreshold != 0 || sw.silenceSize != 0 ||
				sw.proto != pcmVersion || sw.tstampMode != tstampNone || sw.periodStep != 1 {
				t.Errorf("sw params %+v for buffer %d, period %d", *sw, p.BufferFrames, p.PeriodFrames)
			}
			if p.StartThreshold != p.BufferFrames || p.StopThreshold != p.BufferFrames || p.AvailMin != p.PeriodFrames {
				t.Errorf("params thresholds %d/%d/%d", p.StartThreshold, p.StopThreshold, p.AvailMin)
			}
			if last := k.calls[len(k.calls)-1]; last != ioctlPrepare || k.state != statePrepared {
				t.Errorf("last call %#x, state %d: want PREPARE last and the stream prepared", last, k.state)
			}
			if err := d.Close(); err != nil {
				t.Error(err)
			}
			k.checkClosedOnce(t)
		}
	}
}

// TestOpenAimsTheBufferNearTwoHundredMilliseconds pins the geometry Open
// asks for on hardware with room for it: 200 ms of buffer in four periods.
func TestOpenAimsTheBufferNearTwoHundredMilliseconds(t *testing.T) {
	k := newFake(t)
	d := mustOpen(t, k, cfg48k)
	defer func() { _ = d.Close() }()
	p := d.Params()
	if p.BufferFrames != 9600 || p.PeriodFrames != 2400 || p.Periods != 4 {
		t.Errorf("buffer %d, period %d, periods %d; want 9600, 2400, 4", p.BufferFrames, p.PeriodFrames, p.Periods)
	}
	if p.BufferTime() != 200*time.Millisecond || p.PeriodTime() != 50*time.Millisecond {
		t.Errorf("buffer %v, period %v", p.BufferTime(), p.PeriodTime())
	}
	if p.Subdevice != 0 || p.ID != "USB Audio" || p.Name != "USB Audio" {
		t.Errorf("info %d %q %q", p.Subdevice, p.ID, p.Name)
	}
}

// TestOpenTakesTheGeometryRefineNarrowsTo pins that buffer and period
// sizes may vary: a hardware buffer smaller than the aim gives the largest
// it has, and sizes the hardware only takes in steps are found by a range
// when the exact size is refused, and reported as granted.
func TestOpenTakesTheGeometryRefineNarrowsTo(t *testing.T) {
	t.Run("a smaller buffer", func(t *testing.T) {
		k := newFake(t)
		k.hw.bufMax = 4096
		d := mustOpen(t, k, cfg48k)
		defer func() { _ = d.Close() }()
		p := d.Params()
		if p.BufferFrames != 4096 || p.PeriodFrames != 1024 {
			t.Errorf("buffer %d, period %d; want 4096, 1024", p.BufferFrames, p.PeriodFrames)
		}
	})
	t.Run("sizes in steps of 1000 frames", func(t *testing.T) {
		k := newFake(t)
		k.hw.granule = 1000
		d := mustOpen(t, k, cfg48k)
		defer func() { _ = d.Close() }()
		p := d.Params()
		if p.BufferFrames != 9000 || p.PeriodFrames != 3000 || p.Periods != 3 {
			t.Errorf("buffer %d, period %d, periods %d; want 9000, 3000, 3", p.BufferFrames, p.PeriodFrames, p.Periods)
		}
	})
	t.Run("a longer aim", func(t *testing.T) {
		k := newFake(t)
		d := mustOpen(t, k, Config{RateHz: 96000, Format: format.S32LE, BufferTime: 500 * time.Millisecond})
		defer func() { _ = d.Close() }()
		if p := d.Params(); p.BufferFrames != 48000 || p.PeriodFrames != 12000 {
			t.Errorf("buffer %d, period %d; want 48000, 12000", p.BufferFrames, p.PeriodFrames)
		}
	})
}

// TestOpenRefusesWhatTheHardwareDoesNotTake pins that a rate, format or
// channel count the hardware lacks is refused at refine, naming what it
// offers, before HW_PARAMS, and that the node is closed.
func TestOpenRefusesWhatTheHardwareDoesNotTake(t *testing.T) {
	for _, c := range []struct {
		name  string
		cfg   Config
		set   func(*fakeHW)
		names []string
	}{
		{"a rate it lacks", Config{RateHz: 22050, Format: format.S16LE}, nil,
			[]string{"S16_LE, 2 channels, 22050 Hz", "44100..384000 Hz", "S16_LE S32_LE S24_3LE"}},
		{"a format it lacks", Config{RateHz: 48000, Format: format.S24LE}, nil,
			[]string{"S24_LE, 2 channels, 48000 Hz"}},
		{"more channels only", cfg48k, func(h *fakeHW) { h.chMin, h.chMax = 4, 8 },
			[]string{"4..8 channels"}},
		{"formats it does not pack", Config{RateHz: 48000, Format: format.S16LE}, func(h *fakeHW) { h.formats = []uint32{31, 52} },
			[]string{"formats 31, 52"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newFake(t)
			if c.set != nil {
				c.set(&k.hw)
			}
			_, err := openFake(k, c.cfg)
			e := wantReason(t, err, ReasonNotSupported)
			for _, want := range c.names {
				if !strings.Contains(e.Detail, want) {
					t.Errorf("%q does not say %q", e.Detail, want)
				}
			}
			if k.callsOf(ioctlHWParams) != 0 || k.callsOf(ioctlHWFree) != 0 {
				t.Errorf("issued %#x after the refine refused", k.calls)
			}
			k.checkClosedOnce(t)
		})
	}
}

// TestOpenRefusesAReadBackThatIsNotWhatWasAsked pins the exact rule: a
// committed rate, format or channel count other than the one asked for, a
// rate that is not exactly that rate over 1, or a geometry left open is
// refused, and the parameters are freed before the node is closed.
func TestOpenRefusesAReadBackThatIsNotWhatWasAsked(t *testing.T) {
	for _, c := range []struct {
		name   string
		commit func(*hwParams)
		says   string
	}{
		{"rate", func(p *hwParams) { p.setInteger(hwParamRate, 44100); p.rateNum = 44100 }, "rate: asked 48000 Hz"},
		{"rate fraction", func(p *hwParams) { p.rateNum, p.rateDen = 48001, 1 }, "48001/1"},
		{"no rate fraction", func(p *hwParams) { p.rateNum, p.rateDen = 0, 0 }, "0/0"},
		{"format", func(p *hwParams) { p.setMask(hwParamFormat, formatS32LE) }, "format: asked S24_3LE"},
		{"channels", func(p *hwParams) { p.setInteger(hwParamChannels, 1) }, "channels: asked 2"},
		{"access", func(p *hwParams) { p.setMask(hwParamAccess, 0) }, "access"},
		{"period open", func(p *hwParams) { p.setRange(hwParamPeriodSize, 1024, 2048) }, "period"},
		{"buffer open", func(p *hwParams) { p.intervalOf(hwParamBufferSize).flags |= intervalOpenMax }, "buffer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newFake(t)
			k.commit = c.commit
			_, err := openFake(k, cfg48k)
			e := wantReason(t, err, ReasonMismatch)
			if !strings.Contains(e.Detail, c.says) {
				t.Errorf("%q does not say %q", e.Detail, c.says)
			}
			if k.callsOf(ioctlHWFree) != 1 || k.callsOf(ioctlSWParams) != 0 || k.callsOf(ioctlPrepare) != 0 {
				t.Errorf("issued %#x: want HW_FREE once, and no SW_PARAMS or PREPARE", k.calls)
			}
			k.checkClosedOnce(t)
		})
	}
}
