package alsa

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// goBarUnitFiles writes the GO bar's USB strings, firmware and descriptors
// into the pi5-go-bar tree at root, each of files that is not "" (a key
// mapped to "" is left out).
func goBarUnitFiles(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	for k, v := range files {
		if v == nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(root, goBarDevice, k), v, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTheDSDPolicyReadsAUnitsModel (B728 B7): UnitModel reads what the
// effective DSD policy keys a DAC by and parses its gain from: the card's
// unit as Card reads it (its IDs, its interface, its enumeration), its USB
// strings and firmware, its descriptors whole past the 4 KiB an attribute
// is otherwise read to, and the configuration its interface's name carries;
// a string the unit does not have is "", one that does not read is an
// error, and so is a card that is no USB unit.
func TestTheDSDPolicyReadsAUnitsModel(t *testing.T) {
	desc := bytes.Repeat([]byte{9, 4, 0, 0, 0, 1, 1, 0, 0}, 1000) // 9,000 bytes
	_, root := readSysfs(t, "pi5-go-bar")
	goBarUnitFiles(t, root, map[string][]byte{"manufacturer": []byte("iFi (by AMR)\n"), "product": []byte("iFi GO bar\n"), "bcdDevice": []byte("0123\n"), "descriptors": desc})
	r := SysfsReader{Root: filepath.Join(root, "sys")}
	m, err := r.UnitModel(5)
	if err != nil {
		t.Fatal(err)
	}
	if m.USB.ID.Vendor != 0x20b1 || m.USB.ID.Product != 0x3009 || m.USB.Interface != 0 || m.USB.DevNum != 2 {
		t.Errorf("the unit %+v, want the GO bar's IDs, interface 0 and devnum 2", m.USB)
	}
	if m.Manufacturer != "iFi (by AMR)" || m.Product != "iFi GO bar" || m.BCDDevice != "0123" || m.Config != 1 {
		t.Errorf("the model %q %q %q config %d, want the strings as sysfs gives them, trimmed, and configuration 1", m.Manufacturer, m.Product, m.BCDDevice, m.Config)
	}
	if !bytes.Equal(m.Descriptors, desc) {
		t.Errorf("the descriptors read %d bytes, want all %d", len(m.Descriptors), len(desc))
	}

	t.Run("absent strings are empty", func(t *testing.T) {
		_, root := readSysfs(t, "pi5-go-bar")
		m, err := SysfsReader{Root: filepath.Join(root, "sys")}.UnitModel(5)
		if err != nil || m.Manufacturer != "" || m.Product != "" || m.BCDDevice != "" || m.Descriptors != nil {
			t.Errorf("a unit with no strings, firmware or descriptors: %+v, %v; want them empty and no error", m, err)
		}
	})
	t.Run("a firmware that is no hexadecimal is an error", func(t *testing.T) {
		_, root := readSysfs(t, "pi5-go-bar")
		goBarUnitFiles(t, root, map[string][]byte{"bcdDevice": []byte("oops\n")})
		if _, err := (SysfsReader{Root: filepath.Join(root, "sys")}).UnitModel(5); err == nil {
			t.Error("a bcdDevice of oops read")
		}
	})
	t.Run("a string with a control character is an error", func(t *testing.T) {
		_, root := readSysfs(t, "pi5-go-bar")
		goBarUnitFiles(t, root, map[string][]byte{"product": []byte("GO\x00bar")})
		if _, err := (SysfsReader{Root: filepath.Join(root, "sys")}).UnitModel(5); err == nil {
			t.Error("a product string holding a NUL read")
		}
	})
	t.Run("a card that is no USB unit is an error", func(t *testing.T) {
		if _, err := r.UnitModel(0); err == nil {
			t.Error("the HDMI card read as a USB unit")
		}
	})
	t.Run("descriptors past their bound are not read", func(t *testing.T) {
		_, root := readSysfs(t, "pi5-go-bar")
		goBarUnitFiles(t, root, map[string][]byte{"descriptors": make([]byte, maxDescriptorBytes+1)})
		m, err := SysfsReader{Root: filepath.Join(root, "sys")}.UnitModel(5)
		if err != nil || m.Descriptors != nil {
			t.Errorf("descriptors past %d bytes: %d read, %v; want none read, and the unit read", maxDescriptorBytes, len(m.Descriptors), err)
		}
	})
}

// TestTheUSBMixerTableIsReadAsItIs (B728 B7): USBMixer is the card's
// usbmixer file, byte for byte, confined to the ALSA tree; a card with none
// answers fs.ErrNotExist.
func TestTheUSBMixerTableIsReadAsItIs(t *testing.T) {
	proc := t.TempDir()
	dir := filepath.Join(proc, "asound", "card5")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	table := []byte("USB Mixer: usb_id=0x25ce0025, ctrlif=0, ctlerr=0\nCard: Mytek Brooklyn DAC+ at usb-0000:01:00.0-1.3, high speed\n  Unit: 10\n    Control: name=\"PCM Playback Volume\", index=0\n    Info: id=10, control=2, cmask=0x3, channels=2, type=\"S16\"\n    Volume: min=-32768, max=0, dBmin=-12800, dBmax=0\n")
	if err := os.WriteFile(filepath.Join(dir, "usbmixer"), table, 0o600); err != nil {
		t.Fatal(err)
	}
	r := Reader{ProcRoot: proc}
	got, err := r.USBMixer(5)
	if err != nil || !bytes.Equal(got, table) {
		t.Fatalf("USBMixer(5): %q, %v; want the file as it is", got, err)
	}
	if _, err := r.USBMixer(6); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("USBMixer of a card with none: %v; want fs.ErrNotExist", err)
	}
}

// TestAPCMDevicesPlaybackAltsetsAreItsStreamFileReadWhole (B728 B7):
// PlaybackAltsets are the playback altsets of the captured DSD256 stream,
// and a stream file that does not parse whole, or is not there, is an
// error, never a shorter list.
func TestAPCMDevicesPlaybackAltsetsAreItsStreamFileReadWhole(t *testing.T) {
	r := Reader{ProcRoot: filepath.Join("testdata", "dsd256-native", "proc")}
	alts, err := r.PlaybackAltsets(5, 0)
	if err != nil || len(alts) == 0 {
		t.Fatalf("PlaybackAltsets(5, 0): %v, %v", alts, err)
	}
	native := false
	for _, a := range alts {
		if a.DSDFormat() != "" {
			native = true
		}
	}
	if !native {
		t.Errorf("the captured DSD256 stream's altsets %+v list no native DSD format", alts)
	}
	if _, err := r.PlaybackAltsets(5, 1); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a device with no stream file: %v; want fs.ErrNotExist", err)
	}
	proc := t.TempDir()
	dir := filepath.Join(proc, "asound", "card5")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stream, err := os.ReadFile(filepath.Join("testdata", "dsd256-native", "proc", "asound", "card5", "stream0"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stream0"), append(stream, []byte("    Format: S32_LE\n    Format: S24_3LE\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Reader{ProcRoot: proc}).PlaybackAltsets(5, 0); err == nil {
		t.Error("a stream file that does not parse whole gave altsets")
	}
}
