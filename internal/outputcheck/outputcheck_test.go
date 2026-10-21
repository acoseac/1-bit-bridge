package outputcheck

import (
	"strings"
	"testing"
)

// ifi is the iFi GO bar as /v1/outputs reads it on the Volumio Pi.
var ifi = DAC{
	ID: "Audio", Name: "iFi (by AMR) HD USB Audio", USBID: "20b1:3009", IsMPDsOutput: true,
	MaxPCMRate: 384000, NativeDSDRates: []int{2822400, 5644800, 11289600},
}

// healthy is a Volumio box where everything works.
func healthy() Input {
	return Input{
		Player:   Player{Software: "volumio", Version: "4.119", MPDVersion: "0.24.0", Port: 6600, Addresses: []string{"192.168.1.42"}},
		DACs:     []DAC{ifi},
		Roots:    []RootRule{{Root: "INTERNAL", State: "proven", Checked: 20, Found: 20}},
		Loopback: Loopback{BaseURL: "http://127.0.0.1:7791"},
		Phones:   1,
		Tokens:   1,
	}
}

// line returns the line with key k, and fails when there is none.
func line(t *testing.T, lines []Line, k string) Line {
	t.Helper()
	for _, l := range lines {
		if l.Key == k {
			return l
		}
	}
	t.Fatalf("no %s line in %+v", k, lines)
	return Line{}
}

// TestTheChecklistIsFourLinesInOrder: Player, DAC, Music, Phone, always.
func TestTheChecklistIsFourLinesInOrder(t *testing.T) {
	for _, in := range []Input{healthy(), {}} {
		lines := Lines(in)
		var keys, titles []string
		for _, l := range lines {
			keys = append(keys, l.Key)
			titles = append(titles, l.Title)
		}
		if got := strings.Join(keys, ","); got != "player,dac,music,phone" {
			t.Fatalf("keys %s", got)
		}
		if got := strings.Join(titles, ","); got != "Player,DAC,Music,Phone" {
			t.Fatalf("titles %s", got)
		}
	}
}

// TestEveryLineIsGreenOrSaysItsFixInOneSentence walks every state each
// line can be in: green carries no fix, anything else carries exactly one
// sentence.
func TestEveryLineIsGreenOrSaysItsFixInOneSentence(t *testing.T) {
	cases := map[string]func(*Input){
		"healthy":              func(*Input) {},
		"mpd down":             func(in *Input) { in.Player.MPDVersion, in.Player.Addresses = "", nil },
		"mpd on this box only": func(in *Input) { in.Player.Addresses = nil },
		"no dac":               func(in *Input) { in.DACs = nil },
		"dac not mpd's":        func(in *Input) { in.DACs[0].IsMPDsOutput = false },
		"no roots":             func(in *Input) { in.Roots = nil },
		"unproven":             func(in *Input) { in.Roots[0] = RootRule{Root: "INTERNAL", State: "unproven"} },
		"mismatch":             func(in *Input) { in.Roots[0] = RootRule{Root: "INTERNAL", State: "mismatch", Checked: 20, Found: 3} },
		"unchecked":            func(in *Input) { in.Roots[0] = RootRule{Root: "INTERNAL", State: "unchecked"} },
		"unproven, no route": func(in *Input) {
			in.Roots[0] = RootRule{Root: "INTERNAL", State: "unproven"}
			in.Loopback = Loopback{Err: "listen tcp4 127.0.0.1:7791: bind: address already in use"}
		},
		"unproven, route not running": func(in *Input) {
			in.Roots[0] = RootRule{Root: "INTERNAL", State: "unproven"}
			in.Loopback = Loopback{}
		},
		"unproven, route not serving": func(in *Input) {
			in.Roots[0] = RootRule{Root: "INTERNAL", State: "unproven"}
			in.Loopback = Loopback{NotServing: true}
		},
		"no phone":            func(in *Input) { in.Phones, in.Tokens = 0, 0 },
		"tokens, none in use": func(in *Input) { in.Phones, in.Tokens = 0, 2 },
		"unread": func(in *Input) {
			*in = Input{Unread: "context canceled", Roots: []RootRule{{Root: "INTERNAL", State: "unchecked"}}, Phones: 1}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := healthy()
			mutate(&in)
			for _, l := range Lines(in) {
				if l.Summary == "" {
					t.Errorf("%s: no summary", l.Key)
				}
				switch l.Status {
				case OK:
					if l.Fix != "" {
						t.Errorf("%s is green and carries a fix: %q", l.Key, l.Fix)
					}
				case Warn, Fail:
					if !oneSentence(l.Fix) {
						t.Errorf("%s (%s): fix %q is not one sentence", l.Key, l.Status, l.Fix)
					}
				default:
					t.Errorf("%s: status %q", l.Key, l.Status)
				}
			}
		})
	}
}

// oneSentence: one terminal period, at the end, starting with a capital.
func oneSentence(s string) bool {
	if s == "" || !strings.HasSuffix(s, ".") || strings.ToUpper(s[:1]) != s[:1] {
		return false
	}
	body := strings.TrimSuffix(s, ".")
	for _, end := range []string{". ", "! ", "? "} {
		if strings.Contains(body, end) {
			return false
		}
	}
	return true
}

// TestAHealthyBoxIsGreenAndNamesItsProtocol: the owner asked that the
// protocol be shown, not hidden. The Player line names MPD and its port,
// and the Phone line says 1-bit casts over MPD.
func TestAHealthyBoxIsGreenAndNamesItsProtocol(t *testing.T) {
	lines := Lines(healthy())
	for _, l := range lines {
		if l.Status != OK {
			t.Fatalf("%s is %s: %s / %s", l.Key, l.Status, l.Summary, l.Fix)
		}
	}
	p := line(t, lines, "player")
	for _, want := range []string{"Volumio 4.119", "MPD 0.24.0", "port 6600", "192.168.1.42"} {
		if !strings.Contains(p.Summary, want) {
			t.Errorf("player summary %q lacks %q", p.Summary, want)
		}
	}
	d := line(t, lines, "dac")
	for _, want := range []string{"iFi (by AMR) HD USB Audio", "20b1:3009", "384 kHz", "DSD256"} {
		if !strings.Contains(d.Summary, want) {
			t.Errorf("dac summary %q lacks %q", d.Summary, want)
		}
	}
	if m := line(t, lines, "music"); !strings.Contains(m.Summary, "INTERNAL") {
		t.Errorf("music summary %q does not name the root", m.Summary)
	}
	if ph := line(t, lines, "phone"); !strings.Contains(ph.Summary, "MPD") {
		t.Errorf("phone summary %q does not name the protocol", ph.Summary)
	}
}

// TestABoxWithNoLibraryIsReady: a player box whose music lives on another
// bridge or a NAS is not a failure (the two-bridges pitfall).
func TestABoxWithNoLibraryIsReady(t *testing.T) {
	in := healthy()
	in.Roots = nil
	m := line(t, Lines(in), "music")
	if m.Status != OK || m.Summary != "Ready: can play music from another bridge or NAS" || m.Fix != "" {
		t.Fatalf("music line %+v", m)
	}
}

// TestEachStateSaysWhatToDo: the fix names the thing to change.
func TestEachStateSaysWhatToDo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Input)
		key    string
		status Status
		fix    []string
	}{
		{"mpd down", func(in *Input) { in.Player.MPDVersion, in.Player.Addresses = "", nil }, "player", Fail, []string{"Start MPD"}},
		{"mpd on this box only", func(in *Input) { in.Player.Addresses = nil }, "player", Warn, []string{"bind_to_address", "mpd.conf"}},
		{"no dac", func(in *Input) { in.DACs = nil }, "dac", Fail, []string{"USB", "audio_output", "bridge output dac"}},
		{"dac not mpd's", func(in *Input) { in.DACs[0].IsMPDsOutput = false }, "dac", Warn, []string{"audio_output", "hw:CARD=Audio"}},
		{"unproven", func(in *Input) { in.Roots[0] = RootRule{Root: "INTERNAL", State: "unproven"} }, "music", Warn, []string{"INTERNAL", "music directory"}},
		{"mismatch", func(in *Input) { in.Roots[0] = RootRule{Root: "INTERNAL", State: "mismatch", Checked: 20, Found: 3} }, "music", Warn, []string{"Update MPD's database"}},
		{"unchecked", func(in *Input) { in.Roots[0] = RootRule{Root: "INTERNAL", State: "unchecked"} }, "music", Warn, []string{"Player line"}},
		{"unproven, port taken", func(in *Input) {
			in.Roots[0] = RootRule{Root: "INTERNAL", State: "unproven"}
			in.Loopback = Loopback{Err: "bind: address already in use"}
		}, "music", Fail, []string{"output.loopbackPort"}},
		{"unproven, route not running", func(in *Input) {
			in.Roots[0] = RootRule{Root: "INTERNAL", State: "unproven"}
			in.Loopback = Loopback{}
		}, "music", Fail, []string{"Start the bridge"}},
		{"unproven, route not serving", func(in *Input) {
			in.Roots[0] = RootRule{Root: "INTERNAL", State: "unproven"}
			in.Loopback = Loopback{NotServing: true}
		}, "music", Fail, []string{"Restart the bridge", "output.mpd"}},
		{"no phone", func(in *Input) { in.Phones, in.Tokens = 0, 0 }, "phone", Warn, []string{"pairing QR"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := healthy()
			tc.mutate(&in)
			l := line(t, Lines(in), tc.key)
			if l.Status != tc.status {
				t.Fatalf("%s status %s, want %s (%s / %s)", tc.key, l.Status, tc.status, l.Summary, l.Fix)
			}
			for _, want := range tc.fix {
				if !strings.Contains(l.Fix, want) {
					t.Errorf("fix %q lacks %q", l.Fix, want)
				}
			}
		})
	}
}

// TestABoxThatCouldNotBeReadSaysWhatFailed: when the outputs body could not
// be read, nothing it carries was judged. The Player line fails and says
// what failed, never that MPD is down; the DAC line says it was not
// checked, never that there is no DAC; the Music line judges the
// configured roots as unchecked, so a taken port still fails with its fix,
// and a box with no root is still ready; the Phone line is as usual.
func TestABoxThatCouldNotBeReadSaysWhatFailed(t *testing.T) {
	in := Input{
		Unread:   "context deadline exceeded",
		Roots:    []RootRule{{Root: "Music", State: "unchecked"}},
		Loopback: Loopback{Err: "listen tcp4 127.0.0.1:7791: bind: address already in use"},
		Phones:   1,
	}
	lines := Lines(in)
	p := line(t, lines, "player")
	if p.Status != Fail || !strings.Contains(p.Summary, "context deadline exceeded") || strings.Contains(p.Summary, "not answering") {
		t.Errorf("player %+v", p)
	}
	d := line(t, lines, "dac")
	if d.Status != Warn || !strings.Contains(d.Summary, "Not checked") || strings.Contains(d.Summary, "No DAC") {
		t.Errorf("dac %+v", d)
	}
	m := line(t, lines, "music")
	if m.Status != Fail || !strings.Contains(m.Summary, "MPD was not asked about Music") || !strings.Contains(m.Fix, "output.loopbackPort") {
		t.Errorf("music %+v", m)
	}
	if ph := line(t, lines, "phone"); ph.Status != OK {
		t.Errorf("phone %+v", ph)
	}
	in.Roots = nil
	if m := line(t, Lines(in), "music"); m.Status != OK || m.Summary != "Ready: can play music from another bridge or NAS" {
		t.Errorf("music with no roots %+v", m)
	}
}

// TestThePhoneLineSaysWhenNoTokenIsInUse: a device token no phone can use
// (expired, never used, or not used since it was rotated) is no paired
// phone. With tokens issued and none in use, the line says that and what
// to do, rather than that no phone was ever paired, and never goes green.
func TestThePhoneLineSaysWhenNoTokenIsInUse(t *testing.T) {
	for _, tc := range []struct {
		phones, tokens int
		status         Status
		summary, fix   string
	}{
		{0, 0, Warn, "No phone paired yet", "pairing QR"},
		{0, 1, Warn, "1 device token is issued, but no phone uses it", "pairing QR"},
		{0, 3, Warn, "3 device tokens are issued, but no phone uses one", "pairing QR"},
		{1, 3, OK, "1 device paired", ""},
	} {
		in := healthy()
		in.Phones, in.Tokens = tc.phones, tc.tokens
		p := line(t, Lines(in), "phone")
		if p.Status != tc.status || !strings.Contains(p.Summary, tc.summary) || !strings.Contains(p.Fix, tc.fix) {
			t.Errorf("%d of %d tokens in use: %+v", tc.phones, tc.tokens, p)
		}
	}
}

// TestTheMusicLineSaysTheRouteAnswersButIsNotServing: a route that is up
// and refuses tracks is not a route that did not start: the line says the
// route answers, and does not send the reader to free a port.
func TestTheMusicLineSaysTheRouteAnswersButIsNotServing(t *testing.T) {
	in := healthy()
	in.Roots[0] = RootRule{Root: "INTERNAL", State: "unproven"}
	in.Loopback = Loopback{NotServing: true}
	m := line(t, Lines(in), "music")
	if !strings.Contains(m.Summary, "answers but is not serving") || strings.Contains(m.Fix, "port") {
		t.Fatalf("music line %+v", m)
	}
}

// TestNoDACIsNotOnlyAUSBQuestion: a box whose card MPD's output does not
// resolve to may hold an I2S HAT, not a USB DAC, so the line says no DAC,
// and its fix covers pointing MPD at a card as well as plugging one in.
func TestNoDACIsNotOnlyAUSBQuestion(t *testing.T) {
	in := healthy()
	in.DACs = nil
	d := line(t, Lines(in), "dac")
	if d.Summary != "No DAC found on this box" {
		t.Fatalf("summary %q", d.Summary)
	}
}

// TestTheDACLineReadsTheCardsCapabilities: a DSD rate is named by its
// multiple of 64·44.1 kHz, a 48 kHz-family rate by 64·48 kHz, and a DAC
// with no native DSD says so.
func TestTheDACLineReadsTheCardsCapabilities(t *testing.T) {
	in := healthy()
	in.DACs[0].MaxPCMRate = 768000
	in.DACs[0].NativeDSDRates = []int{2822400, 3072000, 22579200, 24576000}
	d := line(t, Lines(in), "dac")
	for _, want := range []string{"768 kHz", "DSD512"} {
		if !strings.Contains(d.Summary, want) {
			t.Errorf("summary %q lacks %q", d.Summary, want)
		}
	}
	in.DACs[0].MaxPCMRate = 352800
	in.DACs[0].NativeDSDRates = nil
	d = line(t, Lines(in), "dac")
	for _, want := range []string{"352.8 kHz", "no native DSD"} {
		if !strings.Contains(d.Summary, want) {
			t.Errorf("summary %q lacks %q", d.Summary, want)
		}
	}
}
