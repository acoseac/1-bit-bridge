package hotplug

import (
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
)

// countLines counts the log lines that contain s.
func countLines(logs, s string) int {
	n := 0
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, s) {
			n++
		}
	}
	return n
}

// step changes the tree, sends the kernel's events for it, and waits for
// the reading the window ends in.
func (h *harness) step(manifest string, evs []Uevent) Stats {
	h.t.Helper()
	before := h.svc.Status().Stats.Reconciles
	if manifest != "" {
		h.apply(manifest)
	}
	h.send(evs...)
	h.advance(quiet)
	return h.reconciles(before + 1)
}

// hdmiAndGoBarLinks puts back pi5-go-bar's class/sound.
const hdmiAndGoBarLinks = "link sys/class/sound/card0 sys/devices/platform/axi/107c701400.hdmi/sound/card0\n" +
	"link sys/class/sound/card1 sys/devices/platform/axi/107c706400.hdmi/sound/card1\n" +
	"link sys/class/sound/card5 " + goBar + "/1-1:1.0/sound/card5\n"

// TestEachStateIsReachedFromAvailableAndBack: from available the binding
// reaches each state 4a defines, and comes back, each change one
// generation: endpoint_missing (the card there without its playback stream,
// as mid-enumeration), identity_unsupported (no class/sound to identify a
// card by), device_missing, disabled and not_configured.
// device_ambiguous, binding_invalid and identity_transient have tests of
// their own below; a binding the config refuses never reaches the watcher,
// which config.PlayerEnabled keeps off.
func TestEachStateIsReachedFromAvailableAndBack(t *testing.T) {
	h := newHarness(t)
	gen := uint64(1)
	back := func() {
		t.Helper()
		gen++
		h.state(binding.StateAvailable, 5, gen)
	}
	reach := func(state binding.State, card int) {
		t.Helper()
		gen++
		h.state(state, card, gen)
	}

	h.step("procremove 5\nproccard 5 Audio \"iFi (by AMR) HD USB Audio\" usb=20b1:3009 pcm=1.0\n",
		[]Uevent{{Action: "change", DevPath: devpath(goBar) + "/1-1:1.0/sound/card5", Subsystem: "sound"}})
	reach(binding.StateEndpointMissing, 5)
	h.step("procremove 5\nproccard 5 Audio \"iFi (by AMR) HD USB Audio\" usb=20b1:3009 pcm=0.0\n",
		[]Uevent{{Action: "add", DevPath: devpath(goBar) + "/1-1:1.0/sound/card5/pcmC5D0p", Subsystem: "sound"}})
	back()

	h.step("remove sys/class/sound\n", []Uevent{{Action: "remove", DevPath: "/devices/virtual/sound", Subsystem: "sound"}})
	reach(binding.StateIdentityUnsupported, -1)
	h.step(hdmiAndGoBarLinks, []Uevent{{Action: "add", DevPath: "/devices/virtual/sound", Subsystem: "sound"}})
	back()

	h.step("unplug "+goBar+"\n", unplugEvents(goBar, 5))
	reach(binding.StateDeviceMissing, -1)
	h.step(plug(goBar, 5, "20b1:3009", goBarSerial, 4), plugEvents(goBar, 5))
	back()

	for _, tc := range []struct {
		state binding.State
		set   Settings
	}{
		{binding.StateDisabled, Settings{Enabled: false}},
		{binding.StateNotConfigured, Settings{Enabled: true, Device: config.PlayerDevice{}}},
	} {
		h.settings.set(tc.set)
		h.published(gen + 1)
		reach(tc.state, -1)
		h.settings.set(Settings{Enabled: true, Device: bySerial()})
		h.published(gen + 1)
		back()
	}
}

// TestASecondUnitOfAUniqueModelIsAmbiguous: under a uniqueModel binding a
// second unit of the model makes the binding device_ambiguous, neither
// chosen, and its removal makes it available again.
func TestASecondUnitOfAUniqueModelIsAmbiguous(t *testing.T) {
	h := newHarness(t, func(o *options) { o.device = byModel("20b1", "3009") })
	h.step(plug(usb1+"/1-2", 6, "20b1:3009", "", 3), plugEvents(usb1+"/1-2", 6))
	s := h.state(binding.StateDeviceAmbiguous, -1, 2)
	if m := s.Resolution().Matches; len(m) != 2 || m[0] != 5 || m[1] != 6 {
		t.Errorf("matches %v", m)
	}
	h.step("unplug "+usb1+"/1-2\n", unplugEvents(usb1+"/1-2", 6))
	h.state(binding.StateAvailable, 5, 3)
}

// TestAHubOrAGoneControllerInvalidatesAPortBinding: under a port binding,
// a hub put in the bound port, or the controller the port hangs from gone,
// is binding_invalid, and the DAC back in the port is available again.
func TestAHubOrAGoneControllerInvalidatesAPortBinding(t *testing.T) {
	h := newHarness(t, func(o *options) { o.device = byPort(goBarPort) })
	h.state(binding.StateAvailable, 5, 1)
	h.step("unplug "+goBar+"\nusbdev "+goBar+" vendor=05e3 product=0610 class=09 devnum=3\n"+
		plug(goBar+"/1-1.3", 5, "20b1:3009", goBarSerial, 4),
		append(unplugEvents(goBar, 5), plugEvents(goBar+"/1-1.3", 5)...))
	s := h.state(binding.StateBindingInvalid, -1, 2)
	if !strings.Contains(s.Resolution().Reason, "hub") {
		t.Errorf("reason %s", s.Resolution().Reason)
	}
	h.step("unplug "+goBar+"\n"+plug(goBar, 5, "20b1:3009", goBarSerial, 5), plugEvents(goBar, 5))
	h.state(binding.StateAvailable, 5, 3)

	const controller = "sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0"
	h.step("unplug "+controller+"\n", []Uevent{{Action: "remove", DevPath: devpath(controller) + "/usb1", Subsystem: "usb", DevType: "usb_device"}})
	s = h.state(binding.StateBindingInvalid, -1, 4)
	if !strings.Contains(s.Resolution().Reason, "platform-xhci-hcd.0") {
		t.Errorf("reason %s", s.Resolution().Reason)
	}
}

// TestAnotherUnitAtTheBoundCardIsASwap: under a port binding another model
// plugged into the bound port, at the bound card number, in one window, is
// still available, and the snapshot says the unit changed at the same card.
func TestAnotherUnitAtTheBoundCardIsASwap(t *testing.T) {
	h := newHarness(t, func(o *options) { o.device = byPort(goBarPort) })
	h.step("unplug "+goBar+"\n"+plug(goBar, 5, "1234:0f03", "", 3), append(unplugEvents(goBar, 5), plugEvents(goBar, 5)...))
	s := h.state(binding.StateAvailable, 5, 2)
	if c := s.Change(); !c.Unit || !c.Swap || c.State || c.ReEnumerated {
		t.Errorf("change %+v; want another unit at the same card", c)
	}
	if id := s.Resolution().Identity.ID.String(); id != "1234:0f03" {
		t.Errorf("the bound unit is %s", id)
	}
}

// TestUnplugIsMissingAndReplugIsAvailableAndNothingElse: unplugged, the
// bound DAC is device_missing in the next generation, and plugged back it
// is available in the one after, with nothing else published and nothing
// in the snapshot that could start playback: a state, its reason and what
// changed.
func TestUnplugIsMissingAndReplugIsAvailableAndNothingElse(t *testing.T) {
	h := newHarness(t)
	h.step("unplug "+goBar+"\n", unplugEvents(goBar, 5))
	s := h.state(binding.StateDeviceMissing, -1, 2)
	if c := s.Change(); !c.State || c.Previous != binding.StateAvailable || c.ReEnumerated || c.Unit {
		t.Errorf("the unplug's change %+v", c)
	}
	h.step(plug(goBar, 5, "20b1:3009", goBarSerial, 3), plugEvents(goBar, 5))
	s = h.state(binding.StateAvailable, 5, 3)
	if c := s.Change(); !c.State || c.Previous != binding.StateDeviceMissing || c.ReEnumerated || c.Unit || c.Swap {
		t.Errorf("the replug's change %+v", c)
	}
	h.advance(periodic)
	if st := h.reconciles(4); st.Publishes != 3 {
		t.Errorf("published %d times in all", st.Publishes)
	}
	if n := countLines(h.logs.String(), "level=INFO msg=\"player DAC: "); n != 3 {
		t.Errorf("%d state lines, want 3 (available, device_missing, available):\n%s", n, h.logs.String())
	}
}

// procOnly is a card 7 in /proc/asound alone: a unit the kernel is adding,
// whose sysfs half is not there yet. usb is its usbid, "" for none.
func procOnly(usb string) string {
	if usb == "" {
		return "proccard 7 New \"Arriving DAC\" driver=snd-usb-hiface pcm=0.0\n"
	}
	return "proccard 7 New \"Arriving DAC\" usb=" + usb + " pcm=0.0\n"
}

// TestACardThatNeverSettlesIsPublishedTransientAfterTheFollowUps: a card in
// /proc alone that may be the bound unit holds the binding's answer back
// for three follow-ups, 500 ms apart, and is then published as it stands:
// the card transient, the binding identity_transient. No more follow-ups
// come until the next event.
func TestACardThatNeverSettlesIsPublishedTransientAfterTheFollowUps(t *testing.T) {
	h := newHarness(t)
	h.step(procOnly(""), []Uevent{{Action: "add", DevPath: devpath(usb1) + "/1-2/1-2:1.0/sound/card7", Subsystem: "sound"}})
	r2 := h.clock.Now()
	for i := 1; i <= DefaultFollowUps; i++ {
		st := h.waitNext(r2.Add(time.Duration(i) * followUp))
		if st.Stats.Publishes != 1 {
			t.Fatalf("published before follow-up %d: %+v", i, st.Stats)
		}
		h.advance(followUp)
		h.reconciles(uint64(2 + i))
	}
	s := h.state(binding.StateIdentityTransient, -1, 2)
	var seven *Card
	for _, c := range s.Cards() {
		if c.Index == 7 {
			c := c
			seven = &c
		}
	}
	if seven == nil || seven.State != binding.CardTransient || !strings.Contains(s.Resolution().Reason, "card 7") {
		t.Errorf("card 7 %+v, reason %s", seven, s.Resolution().Reason)
	}
	last := r2.Add(time.Duration(DefaultFollowUps) * followUp)
	h.waitNext(last.Add(periodic))
	h.advance(periodic)
	if st := h.waitNext(last.Add(2 * periodic)); st.Stats.Reconciles != uint64(3+DefaultFollowUps) {
		t.Errorf("a periodic reading of the same transient card took follow-ups: %+v", st.Stats)
	}
}

// TestAnUnplugBesideAnUnrelatedTransientCardIsMissingAtOnce: the bound DAC
// unplugged while another model's card is still in /proc alone is
// device_missing in the first reading, not after the follow-ups: that card
// cannot be the bound unit. The follow-ups go on for it, and once it
// settles it is published as the card it is.
func TestAnUnplugBesideAnUnrelatedTransientCardIsMissingAtOnce(t *testing.T) {
	h := newHarness(t)
	h.step("unplug "+goBar+"\n"+procOnly("1234:0f05"), append(unplugEvents(goBar, 5),
		Uevent{Action: "add", DevPath: devpath(usb1) + "/1-2/1-2:1.0/sound/card7", Subsystem: "sound"}))
	r2 := h.clock.Now()
	s := h.state(binding.StateDeviceMissing, -1, 2)
	if len(s.Cards()) != 3 || s.Cards()[2].State != binding.CardTransient {
		t.Errorf("cards %+v", s.Cards())
	}
	h.waitNext(r2.Add(followUp))
	h.apply("usbdev " + usb1 + "/1-2 vendor=1234 product=0f05 devnum=3\nusbcard 7 " + usb1 + "/1-2/1-2:1.0\n")
	h.advance(followUp)
	h.reconciles(3)
	s = h.state(binding.StateDeviceMissing, -1, 3)
	if c := s.Cards()[2]; c.Index != 7 || c.State != binding.CardUSB {
		t.Errorf("card 7 once settled: %+v", c)
	}
}

// TestATransientCardIsNotLatched: a card in /proc alone that may be the
// bound unit (its model) holds the answer back, and the next follow-up,
// once its sysfs half is there, settles it: identity_transient is never
// published.
func TestATransientCardIsNotLatched(t *testing.T) {
	h := newHarness(t)
	h.step(procOnly("20b1:3009"), []Uevent{{Action: "add", DevPath: devpath(usb1) + "/1-2/1-2:1.0/sound/card7", Subsystem: "sound"}})
	h.waitNext(h.clock.Now().Add(followUp))
	if st := h.svc.Status(); st.Stats.Publishes != 1 {
		t.Fatalf("a transient answer was published: %+v", st.Stats)
	}
	h.apply("usbdev " + usb1 + "/1-2 vendor=20b1 product=3009 serial=OTHER-GOBAR-0000000002 devnum=3\nusbcard 7 " + usb1 + "/1-2/1-2:1.0\n")
	h.advance(followUp)
	h.reconciles(3)
	s := h.state(binding.StateAvailable, 5, 2)
	if len(s.Cards()) != 4 {
		t.Errorf("cards %+v", s.Cards())
	}
	if strings.Contains(h.logs.String(), string(binding.StateIdentityTransient)) {
		t.Errorf("identity_transient reached the log:\n%s", h.logs.String())
	}
}
