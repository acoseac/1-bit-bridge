package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// consoleBox is a Raspberry Pi 5's first xHCI controller (pi5-go-bar's
// layout) with an HDMI card 0, which is not USB; consoleUnit adds a unit.
const consoleBox = `
platform sys/devices/platform/axi
platform sys/devices/platform/axi/107c701400.hdmi
card 0 sys/devices/platform/axi/107c701400.hdmi
proccard 0 vc4hdmi0 vc4-hdmi-0 driver=vc4-hdmi pcm=0.0
platform sys/devices/platform/axi/1000120000.pcie
platform sys/devices/platform/axi/1000120000.pcie/1f00200000.usb
platform sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0
roothub sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1 speed=480
`

// consoleHub is the root hub consoleUnit's units sit on.
const consoleHub = "sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1"

// The serials of the units these tests lay out: made up, and long enough to
// be looked for in an answer.
const (
	consoleSerialA = "GOBAR-7Q2X-91KZ-4471"
	consoleSerialB = "GOBAR-3M8D-55TT-0912"
)

// consoleUnit is a USB unit in port port of consoleHub: its model, its serial
// ("" for none), the devnum of this enumeration, the sound card it carries and
// that card's playback substreams (pcm=, "0.0" or "0.0,1.0").
func consoleUnit(port int, model, serial string, devnum, card int, pcm string) string {
	vendor, product, _ := strings.Cut(model, ":")
	dir := fmt.Sprintf("%s/1-%d", consoleHub, port)
	s := fmt.Sprintf("usbdev %s vendor=%s product=%s devnum=%d", dir, vendor, product, devnum)
	if serial != "" {
		s += " serial=" + serial
	}
	return s + fmt.Sprintf("\nusbcard %d %s/1-%d:1.0\nproccard %d Audio%d \"DAC in port %d\" usb=%s pcm=%s\n",
		card, dir, port, card, card, port, model, pcm)
}

// consoleUnplug takes the unit in port out of the box, its card with it.
func consoleUnplug(port int) string { return fmt.Sprintf("unplug %s/1-%d\n", consoleHub, port) }

// consoleClock is a clock a test moves.
type consoleClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *consoleClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *consoleClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// consoleView is the console's view of a serve that started no player, over
// the box laid out from units, at a clock the test moves; root is the box's
// tree, which sysfstest.Apply changes.
func consoleView(t *testing.T, units ...string) (*consolePlayer, string, *consoleClock) {
	t.Helper()
	root := sysfstest.BuildText(t, consoleBox+strings.Join(units, ""))
	clock := &consoleClock{now: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	c := newConsolePlayer(consolePlayerWiring{GOOS: "linux", Now: clock.Now,
		Trees: outputTrees{proc: filepath.Join(root, "proc"), sysfs: filepath.Join(root, "sys")}})
	return c, root, clock
}

// listOf is a listing of the box with no binding configured, failing the
// test on one that could not be read.
func listOf(t *testing.T, c *consolePlayer) admin.PlayerDACList {
	t.Helper()
	list := c.DACs(context.Background(), &config.Config{})
	if !list.Read {
		t.Fatalf("the box could not be read: %s", list.Problem)
	}
	return list
}

// rowOf is the listing's row for card at endpoint, failing the test on none.
func rowOf(t *testing.T, list admin.PlayerDACList, card int, endpoint string) admin.PlayerDACCard {
	t.Helper()
	for _, r := range list.Cards {
		if r.Card == card && (r.Endpoint == endpoint || endpoint == "") {
			return r
		}
	}
	t.Fatalf("no row for card %d %s in %+v", card, endpoint, list.Cards)
	return admin.PlayerDACCard{}
}

// kindOf is the row's answer for kind.
func kindOf(t *testing.T, r admin.PlayerDACCard, kind string) admin.PlayerDACKind {
	t.Helper()
	for _, k := range r.Kinds {
		if k.Kind == kind {
			return k
		}
	}
	t.Fatalf("card %d lists no %s kind: %+v", r.Card, kind, r.Kinds)
	return admin.PlayerDACKind{}
}

// noSerialIn fails the test when v, as JSON and as Go prints it, holds a
// serial or six characters of one.
func noSerialIn(t *testing.T, what string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{string(b), fmt.Sprintf("%+v", v)} {
		for _, serial := range []string{consoleSerialA, consoleSerialB} {
			for i := 0; i+6 <= len(serial); i++ {
				if strings.Contains(out, serial[i:i+6]) {
					t.Fatalf("%s holds the serial: %s", what, out)
				}
			}
		}
	}
}

// TestTheConsoleListsEachPlaybackEndpointOfTheBox pins the DAC list: one row
// for each USB card's playback endpoint, its model, its serial's label and
// its port, the three kinds of binding with whether each names it now, the
// one the page proposes, and a selection token; a card that is not USB is
// listed with why and no token; no serial reaches the list.
func TestTheConsoleListsEachPlaybackEndpointOfTheBox(t *testing.T) {
	c, _, _ := consoleView(t, consoleUnit(1, "20b1:3009", consoleSerialA, 2, 5, "0.0"))
	list := listOf(t, c)
	hdmi := rowOf(t, list, 0, "")
	if hdmi.Selection != "" || hdmi.Why == "" {
		t.Errorf("the HDMI card: selection %q, why %q; want no token and why", hdmi.Selection, hdmi.Why)
	}
	r := rowOf(t, list, 5, "device 0 subdevice 0")
	if len(r.Selection) != 32 || strings.Trim(r.Selection, "0123456789abcdef") != "" {
		t.Errorf("the GO bar's token is %q, want 32 hexadecimal digits", r.Selection)
	}
	if r.USBID != "20b1:3009" || r.SerialLabel != usbid.New(consoleSerialA).Label() || r.Port == "" || r.Name != "DAC in port 1" {
		t.Errorf("the GO bar's row: %+v", r)
	}
	for _, kind := range []string{config.BindingSerial, config.BindingUniqueModel, config.BindingPort} {
		if k := kindOf(t, r, kind); !k.Available || k.Why != "" {
			t.Errorf("a %s binding of the one GO bar: %+v, want available", kind, k)
		}
	}
	if r.Recommended != config.BindingSerial {
		t.Errorf("the proposed binding is %q, want serial for a unit that has one", r.Recommended)
	}
	noSerialIn(t, "the list", list)
}

// TestTheConsoleListsTwoUnitsOfOneModel: beside a second GO bar no
// uniqueModel binding names either, and each unit's serial and port bindings
// do; a choice by serial or by port binds the unit chosen.
func TestTheConsoleListsTwoUnitsOfOneModel(t *testing.T) {
	c, _, _ := consoleView(t, consoleUnit(1, "20b1:3009", consoleSerialA, 2, 5, "0.0"), consoleUnit(2, "20b1:3009", consoleSerialB, 3, 6, "0.0"))
	list := listOf(t, c)
	a, b := rowOf(t, list, 5, ""), rowOf(t, list, 6, "")
	for _, r := range []admin.PlayerDACCard{a, b} {
		if k := kindOf(t, r, config.BindingUniqueModel); k.Available || !strings.Contains(k.Why, string(binding.StateDeviceAmbiguous)) {
			t.Errorf("card %d by uniqueModel beside another unit of its model: %+v, want refused as ambiguous", r.Card, k)
		}
		if !kindOf(t, r, config.BindingSerial).Available || !kindOf(t, r, config.BindingPort).Available {
			t.Errorf("card %d: its serial and port bindings should name it: %+v", r.Card, r.Kinds)
		}
	}
	dev, err := c.Chosen(context.Background(), a.Selection, config.BindingSerial)
	if err != nil || dev.Binding != config.BindingSerial || !dev.Serial.Equal(usbid.New(consoleSerialA)) {
		t.Fatalf("card 5 by serial: %+v, %v; want the first unit's serial", dev, err)
	}
	dev, err = c.Chosen(context.Background(), b.Selection, config.BindingPort)
	if err != nil || dev.Binding != config.BindingPort || dev.Port == "" || dev.Serial.Present() || dev.Port == mustPortOf(t, a) {
		t.Fatalf("card 6 by port: %+v, %v; want the second unit's port", dev, err)
	}
	if _, err := c.Chosen(context.Background(), a.Selection, config.BindingUniqueModel); !errors.Is(err, admin.ErrPlayerBindingRefused) {
		t.Errorf("a uniqueModel choice the list refused: %v, want ErrPlayerBindingRefused", err)
	}
}

// mustPortOf is the port a row lists, its place alone.
func mustPortOf(t *testing.T, r admin.PlayerDACCard) string {
	t.Helper()
	p, err := usbid.ParsePort(r.Port)
	if err != nil {
		t.Fatalf("card %d's port %q: %v", r.Card, r.Port, err)
	}
	return p.Place().String()
}

// TestTheConsoleListsAUnitWithNoSerial: a unit with no serial cannot be bound
// by serial, and the page proposes uniqueModel for it.
func TestTheConsoleListsAUnitWithNoSerial(t *testing.T) {
	c, _, _ := consoleView(t, consoleUnit(1, "1234:0c02", "", 2, 3, "0.0"))
	r := rowOf(t, listOf(t, c), 3, "")
	if k := kindOf(t, r, config.BindingSerial); k.Available || !strings.Contains(k.Why, "no serial") {
		t.Errorf("a serial binding of a unit with none: %+v, want refused, saying it has no serial", k)
	}
	if none := (usbid.Serial{}).Label(); r.Recommended != config.BindingUniqueModel || r.SerialLabel != none {
		t.Errorf("a unit with no serial: proposed %q, serial label %q", r.Recommended, r.SerialLabel)
	}
}

// TestTheConsoleListsEachEndpointOfACard: a card with two playback endpoints
// is two rows, each with a token of its own, and a choice of the second binds
// its endpoint.
func TestTheConsoleListsEachEndpointOfACard(t *testing.T) {
	c, _, _ := consoleView(t, consoleUnit(1, "20b1:3009", consoleSerialA, 2, 5, "0.0,1.0"))
	list := listOf(t, c)
	first, second := rowOf(t, list, 5, "device 0 subdevice 0"), rowOf(t, list, 5, "device 1 subdevice 0")
	if first.Selection == "" || second.Selection == "" || first.Selection == second.Selection {
		t.Fatalf("two endpoints' tokens: %q and %q, want two of their own", first.Selection, second.Selection)
	}
	dev, err := c.Chosen(context.Background(), second.Selection, config.BindingSerial)
	if err != nil || dev.PCMDevice != 1 || dev.Subdevice != 0 {
		t.Fatalf("the second endpoint: %+v, %v; want device 1", dev, err)
	}
}

// TestAChoiceOfACardThatChangedSinceTheListIsStale pins the identity check
// between the list and the choice: another unit plugged in at the card's
// number, the same unit unplugged and plugged in again (a new enumeration), a
// token past its time and one the server never handed out are each stale,
// and nothing the box holds then is bound; a card that did not change is
// bound as the list showed it.
func TestAChoiceOfACardThatChangedSinceTheListIsStale(t *testing.T) {
	stale := func(t *testing.T, c *consolePlayer, token, kind, what string) {
		t.Helper()
		dev, err := c.Chosen(context.Background(), token, kind)
		if !errors.Is(err, admin.ErrPlayerSelectionStale) {
			t.Fatalf("%s: %+v, %v; want ErrPlayerSelectionStale", what, dev, err)
		}
		noSerialIn(t, what+"'s refusal", err.Error())
	}
	t.Run("another unit at the card's number", func(t *testing.T) {
		c, root, _ := consoleView(t, consoleUnit(1, "20b1:3009", consoleSerialA, 2, 5, "0.0"))
		token := rowOf(t, listOf(t, c), 5, "").Selection
		sysfstest.Apply(t, root, consoleUnplug(1)+consoleUnit(1, "20b1:3009", consoleSerialB, 3, 5, "0.0"))
		for _, kind := range []string{config.BindingSerial, config.BindingUniqueModel, config.BindingPort} {
			stale(t, c, token, kind, "a "+kind+" choice of a card another unit took")
		}
	})
	t.Run("the same unit plugged in again", func(t *testing.T) {
		c, root, _ := consoleView(t, consoleUnit(1, "20b1:3009", consoleSerialA, 2, 5, "0.0"))
		token := rowOf(t, listOf(t, c), 5, "").Selection
		sysfstest.Apply(t, root, consoleUnplug(1)+consoleUnit(1, "20b1:3009", consoleSerialA, 4, 5, "0.0"))
		stale(t, c, token, config.BindingSerial, "a choice of a unit enumerated again")
	})
	t.Run("the unit gone", func(t *testing.T) {
		c, root, _ := consoleView(t, consoleUnit(1, "20b1:3009", consoleSerialA, 2, 5, "0.0"))
		token := rowOf(t, listOf(t, c), 5, "").Selection
		sysfstest.Apply(t, root, consoleUnplug(1))
		stale(t, c, token, config.BindingSerial, "a choice of a unit unplugged")
	})
	t.Run("a token past its time, and one never handed out", func(t *testing.T) {
		c, _, clock := consoleView(t, consoleUnit(1, "20b1:3009", consoleSerialA, 2, 5, "0.0"))
		token := rowOf(t, listOf(t, c), 5, "").Selection
		stale(t, c, strings.Repeat("0", 32), config.BindingSerial, "a token never handed out")
		clock.advance(consoleSelectionTTL + time.Second)
		stale(t, c, token, config.BindingSerial, "a token past its time")
	})
	t.Run("nothing changed", func(t *testing.T) {
		c, _, clock := consoleView(t, consoleUnit(1, "20b1:3009", consoleSerialA, 2, 5, "0.0"))
		token := rowOf(t, listOf(t, c), 5, "").Selection
		clock.advance(consoleSelectionTTL - time.Second)
		dev, err := c.Chosen(context.Background(), token, config.BindingUniqueModel)
		if err != nil || dev.Binding != config.BindingUniqueModel || dev.VendorID != "20b1" || dev.ProductID != "3009" || dev.Serial.Present() {
			t.Fatalf("a choice of a card that did not change: %+v, %v", dev, err)
		}
	})
}

// TestAUnitSwappedWhileTheBoxIsReadIsNotOffered pins the reading's bracket:
// another unit of the same model swapped in at a card's number between the
// first reading of sysfs and the last, which /proc's usbid cannot tell, is
// set aside, offered no token, rather than listed with /proc's endpoints of
// the unit before it.
func TestAUnitSwappedWhileTheBoxIsReadIsNotOffered(t *testing.T) {
	c, root, _ := consoleView(t, consoleUnit(1, "20b1:3009", consoleSerialA, 2, 5, "0.0"))
	trees := c.trees
	var once sync.Once
	c.observer = func() binding.Observer {
		obs := trees.observer()
		obs.AfterProc = func() {
			once.Do(func() {
				sysfstest.Apply(t, root, consoleUnplug(1)+consoleUnit(1, "20b1:3009", consoleSerialB, 3, 5, "0.0"))
			})
		}
		return obs
	}
	r := rowOf(t, listOf(t, c), 5, "")
	if r.Selection != "" || !strings.Contains(r.Why, "changed while the box was read") {
		t.Fatalf("a card swapped while the box was read: token %q, why %q; want none, and why", r.Selection, r.Why)
	}
}

// TestABoxThatCannotBeReadOffersNothing: with no sysfs to read, the list says
// why and offers nothing, and a choice is refused as a box that could not be
// read, whatever token it names.
func TestABoxThatCannotBeReadOffersNothing(t *testing.T) {
	c, root, _ := consoleView(t, consoleUnit(1, "20b1:3009", consoleSerialA, 2, 5, "0.0"))
	token := rowOf(t, listOf(t, c), 5, "").Selection
	if err := os.RemoveAll(filepath.Join(root, "sys")); err != nil {
		t.Fatal(err)
	}
	list := c.DACs(context.Background(), &config.Config{})
	if list.Read || list.Problem == "" || len(list.Cards) != 0 {
		t.Fatalf("a box with no sysfs: %+v, want no reading, its problem, and no card", list)
	}
	if _, err := c.Chosen(context.Background(), token, config.BindingSerial); !errors.Is(err, admin.ErrPlayerBoxUnread) {
		t.Fatalf("a choice on a box that cannot be read: %v, want ErrPlayerBoxUnread", err)
	}
}

// TestTheTokensAreBounded: the view keeps the newest consoleSelectionCap
// tokens, so listing again and again grows nothing.
func TestTheTokensAreBounded(t *testing.T) {
	c, _, clock := consoleView(t, consoleUnit(1, "20b1:3009", consoleSerialA, 2, 5, "0.0,1.0"))
	var last string
	for i := 0; i < consoleSelectionCap; i++ {
		clock.advance(time.Second)
		last = rowOf(t, listOf(t, c), 5, "device 1 subdevice 0").Selection
	}
	c.mu.Lock()
	n := len(c.selections)
	c.mu.Unlock()
	if n > consoleSelectionCap {
		t.Fatalf("the view keeps %d tokens, want at most %d", n, consoleSelectionCap)
	}
	if _, err := c.Chosen(context.Background(), last, config.BindingSerial); err != nil {
		t.Fatalf("the newest token: %v", err)
	}
}
