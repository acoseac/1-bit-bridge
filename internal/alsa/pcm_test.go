package alsa

import (
	"reflect"
	"testing"
)

// TestParseHWParamsOnEveryCapture reads what MPD opened the iFi with in
// each capture: DSD_U32_BE at 352800 frames/s for native DSD256, S16_LE at
// 44.1 kHz, S24_3LE at 96, 192 and 384 kHz, and "closed" when stopped.
func TestParseHWParamsOnEveryCapture(t *testing.T) {
	for _, c := range captures {
		t.Run(c.name, func(t *testing.T) {
			h, probs := ParseHWParams(readFixture(t, c.name, "asound/card5/pcm0p/sub0/hw_params"))
			if len(probs) != 0 {
				t.Errorf("problems on a captured file: %q", probs)
			}
			if !c.running {
				if !reflect.DeepEqual(h, HWParams{Closed: true}) {
					t.Errorf("idle hw_params = %+v, want closed and nothing else", h)
				}
				return
			}
			want := HWParams{
				Access: c.access, Format: c.format, Subformat: "STD", Channels: intPtr(2),
				Rate: intPtr(c.rate), RateNum: intPtr(c.rate), RateDen: intPtr(1),
				PeriodSize: int64Ptr(int64(c.period)), BufferSize: int64Ptr(int64(c.buffer)),
			}
			if !reflect.DeepEqual(h, want) {
				t.Errorf("hw_params = %+v\nwant        %+v", h, want)
			}
		})
	}
}

// TestParseHWParamsShapes covers "no setup", a file cut short, values that
// do not parse, and lines the parser does not know (OSS emulation's).
func TestParseHWParamsShapes(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  HWParams
		probs []string
	}{
		{name: "no setup", in: "no setup\n", want: HWParams{NoSetup: true}},
		{name: "closed, then something", in: "closed\nformat: S16_LE\n", want: HWParams{Closed: true},
			probs: []string{`line 2: 1 line(s) after "closed": "format: S16_LE"`}},
		{name: "empty", in: "", probs: []string{"empty file"}},
		{
			name: "a repeated key keeps its first value",
			in: "access: RW_INTERLEAVED\nformat: S16_LE\nformat: DSD_U32_BE\nsubformat: STD\nchannels: 2\n" +
				"rate: 44100 (44100/1)\nperiod_size: 5513\nbuffer_size: 22050\n",
			want: HWParams{Access: "RW_INTERLEAVED", Format: "S16_LE", Subformat: "STD", Channels: intPtr(2),
				Rate: intPtr(44100), RateNum: intPtr(44100), RateDen: intPtr(1), PeriodSize: int64Ptr(5513), BufferSize: int64Ptr(22050)},
			probs: []string{"line 3: a second format line; the first is kept"},
		},
		{
			name: "cut short after the rate",
			in:   "access: RW_INTERLEAVED\nformat: S32_LE\nsubformat: STD\nchannels: 2\nrate: 48000 (48000/1)\n",
			want: HWParams{Access: "RW_INTERLEAVED", Format: "S32_LE", Subformat: "STD", Channels: intPtr(2),
				Rate: intPtr(48000), RateNum: intPtr(48000), RateDen: intPtr(1)},
			probs: []string{"no period_size line (cut short?)", "no buffer_size line (cut short?)"},
		},
		{
			name: "values that do not parse, and OSS lines",
			in: "access: RW_INTERLEAVED\nformat: S16 LE\nsubformat: STD\nchannels: -2\nrate: 44100\n" +
				"period_size: 5513\nbuffer_size: lots\nOSS format: S16_LE\ngarbage\n",
			want: HWParams{Access: "RW_INTERLEAVED", Subformat: "STD", PeriodSize: int64Ptr(5513)},
			probs: []string{
				`line 2: format "S16 LE" is not one word`,
				`line 4: channels "-2" is not a number`,
				`line 5: rate "44100" is not "RATE (NUM/DEN)"`,
				`line 7: buffer_size "lots" is not a number`,
				`line 9: not a hw_params line: "garbage"`,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, probs := ParseHWParams([]byte(tc.in))
			if !reflect.DeepEqual(h, tc.want) {
				t.Errorf("hw_params = %+v\nwant        %+v", h, tc.want)
			}
			if tc.probs == nil {
				tc.probs = []string{}
			}
			if !reflect.DeepEqual(probs, tc.probs) {
				t.Errorf("problems = %q\nwant       %q", probs, tc.probs)
			}
		})
	}
}

// TestParseStatusOnEveryCapture reads the substream's status in each
// capture: RUNNING, owned by thread 6584, with the buffer's counters, and
// "closed" when stopped.
func TestParseStatusOnEveryCapture(t *testing.T) {
	for _, c := range captures {
		t.Run(c.name, func(t *testing.T) {
			st, probs := ParseStatus(readFixture(t, c.name, "asound/card5/pcm0p/sub0/status"))
			if len(probs) != 0 {
				t.Errorf("problems on a captured file: %q", probs)
			}
			if !c.running {
				if !reflect.DeepEqual(st, Status{Closed: true}) {
					t.Errorf("idle status = %+v, want closed and nothing else", st)
				}
				return
			}
			want := Status{
				State: "RUNNING", OwnerPID: intPtr(6584), TriggerTime: c.triggerTime, Timestamp: "0.000000000",
				Delay: int64Ptr(c.delay), Avail: int64Ptr(c.avail), AvailMax: int64Ptr(c.availMax),
				HWPtr: int64Ptr(c.hwPtr), ApplPtr: int64Ptr(c.applPtr),
			}
			if !reflect.DeepEqual(st, want) {
				t.Errorf("status = %+v\nwant     %+v", st, want)
			}
		})
	}
}

// TestParseStatusShapes covers the error verdict, a file cut short, a
// negative delay (a signed count the kernel can print), and values that do
// not parse.
func TestParseStatusShapes(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  Status
		probs []string
	}{
		{name: "error", in: "error -19\n", want: Status{Error: intPtr(-19)}},
		{
			name: "a repeated key keeps its first value",
			in: "state: RUNNING\nstate: XRUN\nowner_pid   : 7\ntrigger_time: 1.000000000\ntstamp      : 0.000000000\n" +
				"delay       : 1\navail       : 2\navail_max   : 3\n-----\nhw_ptr      : 4\nappl_ptr    : 5\n",
			want: Status{State: "RUNNING", OwnerPID: intPtr(7), TriggerTime: "1.000000000", Timestamp: "0.000000000",
				Delay: int64Ptr(1), Avail: int64Ptr(2), AvailMax: int64Ptr(3), HWPtr: int64Ptr(4), ApplPtr: int64Ptr(5)},
			probs: []string{"line 2: a second state line; the first is kept"},
		},
		{name: "empty", in: "\n", probs: []string{"empty file"}},
		{
			name: "cut short after avail_max",
			in:   "state: PREPARED\nowner_pid   : 1\ntrigger_time: 0.000000000\ntstamp      : 0.000000000\ndelay       : -3\navail       : 0\navail_max   : 0\n-----\n",
			want: Status{State: "PREPARED", OwnerPID: intPtr(1), TriggerTime: "0.000000000", Timestamp: "0.000000000",
				Delay: int64Ptr(-3), Avail: int64Ptr(0), AvailMax: int64Ptr(0)},
			probs: []string{"no hw_ptr line (cut short?)", "no appl_ptr line (cut short?)"},
		},
		{
			name: "values that do not parse",
			in: "state: RUN NING\nowner_pid   : -5\ntrigger_time: 12.5\ntstamp      : 0.000000000\ndelay       : 1\n" +
				"avail       : x\navail_max   : 2\n-----\nhw_ptr      : 3\nappl_ptr    : 4\n",
			want: Status{Timestamp: "0.000000000", Delay: int64Ptr(1), AvailMax: int64Ptr(2), HWPtr: int64Ptr(3), ApplPtr: int64Ptr(4)},
			probs: []string{
				`line 1: state "RUN NING" is not one word`,
				`line 2: owner_pid "-5" is not a number`,
				`line 3: trigger_time "12.5" is not "SECONDS.NANOSECONDS"`,
				`line 6: avail "x" is not a number`,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, probs := ParseStatus([]byte(tc.in))
			if !reflect.DeepEqual(st, tc.want) {
				t.Errorf("status = %+v\nwant     %+v", st, tc.want)
			}
			if tc.probs == nil {
				tc.probs = []string{}
			}
			if !reflect.DeepEqual(probs, tc.probs) {
				t.Errorf("problems = %q\nwant       %q", probs, tc.probs)
			}
		})
	}
}
