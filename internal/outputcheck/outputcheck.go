// Package outputcheck is the setup checklist of a bridge that is a player
// (backlog B473): Player, DAC, Music and Phone, four lines, each green or
// saying its fix in one sentence. It is one source for both surfaces:
// `bridge doctor` prints the lines and the console's checklist panel shows
// them, and neither words a line of its own.
//
// It judges what it is handed and reads nothing itself. cmd/bridge gathers
// the input from the GET /v1/outputs builder, the loopback track route and
// the token store, the same way for both surfaces.
//
// Protocol names are shown, not hidden (the owner, 2026-10-19): a line
// says MPD where MPD is what 1-bit talks to.
package outputcheck

import (
	"fmt"
	"strconv"
	"strings"
)

// Status is a line's verdict, in the doctor's words.
type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
)

// Line is one line of the checklist. Fix is empty on a green line and one
// sentence on any other.
type Line struct {
	Key     string `json:"key"`
	Title   string `json:"title"`
	Status  Status `json:"status"`
	Summary string `json:"summary"`
	Fix     string `json:"fix,omitempty"`
}

// Input is what the checklist judges.
type Input struct {
	// Unread is why the box could not be read (the GET /v1/outputs body),
	// empty when it was. Nothing that body carries was judged then: the
	// Player line says what failed, the DAC line that it was not checked,
	// and the Music line judges Roots, each one unchecked.
	Unread   string
	Player   Player
	DACs     []DAC
	Roots    []RootRule
	Loopback Loopback
	// Phones is the number of paired devices.
	Phones int
	// Tokens is the number of device tokens the bridge holds.
	Tokens int
}

// Player is the player software and the MPD it drives. MPDVersion is
// empty when MPD did not answer. Addresses are the LAN addresses on which
// MPD's port answered: what a phone can reach.
type Player struct {
	Software   string // volumio, moode, mpd, or ""
	Version    string
	MPDVersion string
	Port       int
	Addresses  []string
}

// DAC is one candidate card. IsMPDsOutput is true only when MPD's ALSA
// output names this card.
type DAC struct {
	ID             string
	Name           string
	USBID          string
	IsMPDsOutput   bool
	MaxPCMRate     int
	NativeDSDRates []int
}

// RootRule is one library root's path rule: proven, mismatch, unproven or
// unchecked, as GET /v1/outputs states it.
type RootRule struct {
	Root    string
	State   string
	Checked int
	Found   int
}

// Loopback is the loopback track route: its base URL while it serves; why
// it does not when it tried to start and could not; and NotServing when
// it answers on its port and refuses tracks, because output.mpd does not
// resolve on in the bridge that runs it.
type Loopback struct {
	BaseURL    string
	Err        string
	NotServing bool
}

// Lines judges in. It returns Player, DAC, Music and Phone, in that order.
func Lines(in Input) []Line {
	if in.Unread != "" {
		return []Line{unreadPlayerLine(in.Unread), uncheckedDACLine(), musicLine(in.Roots, in.Loopback), phoneLine(in.Phones, in.Tokens)}
	}
	return []Line{playerLine(in.Player), dacLine(in.DACs), musicLine(in.Roots, in.Loopback), phoneLine(in.Phones, in.Tokens)}
}

// unreadPlayerLine is the Player line of a box the bridge could not read:
// it says what failed, and never that MPD is down.
func unreadPlayerLine(why string) Line {
	return Line{Key: "player", Title: "Player", Status: Fail,
		Summary: "The bridge could not read this box's player and cards (" + why + ")",
		Fix:     "Check again; if it fails again, `bridge output player` on this box shows what the bridge reads there."}
}

// uncheckedDACLine is the DAC line of a box the bridge could not read: the
// cards were not looked at, which says nothing about whether there is one.
func uncheckedDACLine() Line {
	return Line{Key: "dac", Title: "DAC", Status: Warn,
		Summary: "Not checked, because the bridge could not read this box",
		Fix:     "Check again once the Player line reads the box."}
}

func playerLine(p Player) Line {
	l := Line{Key: "player", Title: "Player"}
	port := p.Port
	if port <= 0 {
		port = 6600
	}
	name := softwareName(p.Software)
	if p.MPDVersion == "" {
		l.Status = Fail
		if name != "" && name != "MPD" {
			l.Summary = fmt.Sprintf("%s is installed, but MPD is not answering on port %d", name, port)
		} else {
			l.Summary = fmt.Sprintf("MPD is not answering on port %d", port)
		}
		l.Fix = "Start MPD on this box (sudo systemctl start mpd), or set output.endpoint to the address it listens on."
		return l
	}
	head := "MPD " + p.MPDVersion
	if name != "" && name != "MPD" {
		head = strings.TrimSpace(name+" "+p.Version) + " · " + head
	}
	if len(p.Addresses) == 0 {
		l.Status = Warn
		l.Summary = fmt.Sprintf("%s answers on port %d on this box only", head, port)
		l.Fix = `Set bind_to_address "any" in mpd.conf and restart MPD, so 1-bit on your phone can reach it.`
		return l
	}
	l.Status = OK
	l.Summary = fmt.Sprintf("%s on port %d, reachable at %s", head, port, strings.Join(p.Addresses, ", "))
	return l
}

func dacLine(dacs []DAC) Line {
	l := Line{Key: "dac", Title: "DAC"}
	if len(dacs) == 0 {
		// No card MPD's output resolves to, and no USB card to offer. The
		// box may hold an I2S HAT behind a device name this reader does not
		// resolve, so the line does not say USB alone.
		l.Status = Fail
		l.Summary = "No DAC found on this box"
		l.Fix = "Plug the DAC in by USB and switch it on, or point MPD's audio_output at its card; `bridge output dac` lists the cards."
		return l
	}
	for _, d := range dacs {
		if d.IsMPDsOutput {
			l.Status = OK
			l.Summary = dacName(d) + " is MPD's output · " + capabilities(d)
			return l
		}
	}
	names := make([]string, 0, len(dacs))
	for _, d := range dacs {
		names = append(names, dacName(d))
	}
	l.Status = Warn
	l.Summary = strings.Join(names, ", ") + " found, but MPD's output does not name it"
	l.Fix = fmt.Sprintf(`Point MPD's audio_output at the DAC (device "hw:CARD=%s"), or choose it in the player's output settings.`, dacs[0].ID)
	return l
}

func dacName(d DAC) string {
	name := d.Name
	if name == "" {
		name = d.ID
	}
	if d.USBID != "" {
		name += " (USB " + d.USBID + ")"
	}
	return name
}

// capabilities is what the card can receive: the highest PCM rate and the
// highest native DSD rate, from every playback altset.
func capabilities(d DAC) string {
	parts := []string{}
	if d.MaxPCMRate > 0 {
		parts = append(parts, "PCM up to "+khz(d.MaxPCMRate))
	}
	top := 0
	for _, r := range d.NativeDSDRates {
		if r > top {
			top = r
		}
	}
	if top > 0 {
		parts = append(parts, "native DSD up to "+dsdName(top))
	} else {
		parts = append(parts, "no native DSD")
	}
	return strings.Join(parts, " · ")
}

// khz is a sample rate in kHz, one decimal when it has one (352.8 kHz).
func khz(rate int) string {
	if rate%1000 == 0 {
		return strconv.Itoa(rate/1000) + " kHz"
	}
	return strconv.FormatFloat(float64(rate)/1000, 'f', 1, 64) + " kHz"
}

// dsdName names a one-bit rate by its multiple of 64 × 44.1 kHz (DSD256 is
// 11.2896 MHz), or of 64 × 48 kHz for that family.
func dsdName(rate int) string {
	switch {
	case rate%2822400 == 0:
		return "DSD" + strconv.Itoa(64*(rate/2822400))
	case rate%3072000 == 0:
		return "DSD" + strconv.Itoa(64*(rate/3072000)) + " (48 kHz family)"
	default:
		return strconv.FormatFloat(float64(rate)/1e6, 'f', 4, 64) + " MHz"
	}
}

func musicLine(roots []RootRule, lb Loopback) Line {
	l := Line{Key: "music", Title: "Music"}
	if len(roots) == 0 {
		l.Status = OK
		l.Summary = "Ready: can play music from another bridge or NAS"
		return l
	}
	var first *RootRule
	names := make([]string, 0, len(roots))
	for i := range roots {
		names = append(names, roots[i].Root)
		if roots[i].State != "proven" && first == nil {
			first = &roots[i]
		}
	}
	if first == nil {
		l.Status = OK
		l.Summary = "MPD plays " + strings.Join(names, ", ") + " from its own database"
		return l
	}
	switch first.State {
	case "mismatch":
		l.Summary = fmt.Sprintf("MPD's database does not list %s's tracks yet (%d of %d found)", first.Root, first.Found, first.Checked)
		l.Fix = "Update MPD's database (mpc update, or the player's library rescan)."
	case "unchecked":
		l.Summary = "MPD was not asked about " + first.Root
		l.Fix = "Make sure MPD answers (the Player line), then check again."
	default:
		l.Summary = first.Root + " is not in MPD's music directory"
		l.Fix = "Link " + first.Root + " into MPD's music directory (or move it there), then update MPD's database."
	}
	if lb.BaseURL != "" {
		// It plays: MPD fetches those tracks from the bridge, unaltered.
		l.Status = Warn
		l.Summary += "; it plays through the bridge's loopback HTTP route"
		return l
	}
	l.Status = Fail
	switch {
	case lb.NotServing:
		// The route is up and refuses tracks: output.mpd does not resolve
		// on in the bridge that runs it, whatever this reading found.
		l.Summary += ", and the running bridge's loopback HTTP route answers but is not serving tracks"
		l.Fix = "Restart the bridge, so it reads output.mpd and looks for MPD again."
	case lb.Err != "":
		l.Summary += ", and the bridge's loopback HTTP route is not serving"
		l.Fix = "Free the route's port or set output.loopbackPort, then restart the bridge."
	default:
		l.Summary += ", and the bridge's loopback HTTP route is not serving"
		l.Fix = "Start the bridge, so MPD can fetch those tracks over its loopback HTTP route."
	}
	return l
}

// phoneLine judges the phones that can use their token (phones) among the
// device tokens the bridge holds (tokens).
func phoneLine(phones, tokens int) Line {
	l := Line{Key: "phone", Title: "Phone"}
	switch {
	case phones > 0:
		l.Status = OK
		devices := "device"
		if phones != 1 {
			devices = "devices"
		}
		l.Summary = fmt.Sprintf("%d %s paired; 1-bit casts to this box over MPD", phones, devices)
	case tokens > 0:
		// Tokens no phone can use: expired, never used, or not used since
		// a rotation replaced the secret the phone held.
		l.Status = Warn
		if tokens == 1 {
			l.Summary = "1 device token is issued, but no phone uses it: it has expired, or has not been used since it was issued or rotated"
		} else {
			l.Summary = fmt.Sprintf("%d device tokens are issued, but no phone uses one: each has expired, or has not been used since it was issued or rotated", tokens)
		}
		l.Fix = "Open 1-bit on your phone so it connects, or scan this bridge's pairing QR again (console, Devices)."
	default:
		l.Status = Warn
		l.Summary = "No phone paired yet"
		l.Fix = "Open 1-bit on your phone and scan this bridge's pairing QR (console, Devices)."
	}
	return l
}

func softwareName(s string) string {
	switch s {
	case "volumio":
		return "Volumio"
	case "moode":
		return "moOde"
	case "mpd":
		return "MPD"
	default:
		return ""
	}
}
