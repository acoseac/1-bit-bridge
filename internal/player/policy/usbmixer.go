package policy

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ParseUSBMixer reads the kernel's table of the controls its USB audio
// driver made of each unit, a card's /proc/asound/card<N>/usbmixer (B728 B7;
// the design's section 3.4), as sound/usb/mixer.c writes it at the alsa-abi
// job's kernel tag (snd_usb_mixer_proc_read, snd_usb_mixer_dump_cval): one
// block a mixer, "USB Mixer: usb_id=…, ctrlif=<interface>, ctlerr=…", its
// "Card:" line, and for each element, "  Unit: <id>", then, when the driver
// made a control of it, "    Control: name=\"<name>\", index=<index>", and,
// when the element dumps itself, "    Info: id=<id>, control=<selector>, …".
// Each element with a Control line is one UnitControl: its block's ctrlif
// (nil where the block's header does not say one), its unit, the selector
// its Info line names (0 where it has none, or one naming another unit), and
// the control's name and index. An element with no Control line made no
// visible control and is left out. The Card line, the Volume lines and any
// line it does not know are passed over, since kernels add lines; a Control
// or Info line outside a unit, a Unit line or a Control line that does not
// read, and a table with no block at all are an error: a table that does not
// read is one the check takes for unread (Mixer.UnitsRead false), failing
// closed. It never guesses a value.
func ParseUSBMixer(b []byte) ([]UnitControl, error) {
	var out []UnitControl
	var ctrlif *int
	blocks := 0
	var cur *UnitControl // the element being read
	var curUnit int
	inUnit := false
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
		}
		cur, inUnit = nil, false
	}
	for n, line := range strings.Split(string(b), "\n") {
		ln := n + 1
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(line, "USB Mixer:"):
			flush()
			blocks++
			ctrlif = nil
			if v, ok := field(strings.TrimPrefix(line, "USB Mixer:"), "ctrlif"); ok {
				if c, err := strconv.Atoi(v); err == nil && c >= 0 {
					ctrlif = &c
				}
			}
		case strings.HasPrefix(trimmed, "Unit:"):
			flush()
			id, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(trimmed, "Unit:")))
			if err != nil || id < 0 {
				return nil, fmt.Errorf("line %d: a Unit line that does not read", ln)
			}
			curUnit, inUnit = id, true
		case strings.HasPrefix(trimmed, "Control:"):
			if !inUnit {
				return nil, fmt.Errorf("line %d: a Control line outside a unit", ln)
			}
			if cur != nil {
				// A second Control line in one element: the kernel writes one.
				return nil, fmt.Errorf("line %d: a second Control line in one unit's element", ln)
			}
			name, index, err := controlLine(strings.TrimSpace(strings.TrimPrefix(trimmed, "Control:")))
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", ln, err)
			}
			uc := UnitControl{Unit: curUnit, Name: name, Index: index}
			if ctrlif != nil {
				c := *ctrlif
				uc.ControlInterface = &c
			}
			cur = &uc
		case strings.HasPrefix(trimmed, "Info:"):
			if !inUnit {
				return nil, fmt.Errorf("line %d: an Info line outside a unit", ln)
			}
			if cur == nil {
				// An element the driver made no control of dumps itself too.
				continue
			}
			rest := strings.TrimPrefix(trimmed, "Info:")
			id, okID := field(rest, "id")
			sel, okSel := field(rest, "control")
			if okID && okSel && id == strconv.Itoa(curUnit) {
				if s, err := strconv.Atoi(sel); err == nil && s > 0 {
					cur.Selector = s
				}
			}
		}
	}
	flush()
	if blocks == 0 {
		return nil, errors.New("the table holds no USB Mixer block")
	}
	return out, nil
}

// field is the value of key in a line of comma-separated key=value pairs, as
// the kernel's mixer table writes them.
func field(s, key string) (string, bool) {
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && k == key {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// controlLine reads a Control line's rest, name="<name>", index=<index>: the
// name runs to the last `", index=` on the line, so a name holding a comma or
// a quote reads whole. The index is read as a control's is held, a uint32: one
// past it does not read, which makes the table one that does not read, where
// the check fails closed, and is never wrapped onto another control's index
// (round 3; CodeQL).
func controlLine(s string) (string, uint32, error) {
	if !strings.HasPrefix(s, `name="`) {
		return "", 0, errors.New("a Control line names no control")
	}
	rest := strings.TrimPrefix(s, `name="`)
	i := strings.LastIndex(rest, `", index=`)
	if i < 0 {
		return "", 0, errors.New("a Control line gives no index")
	}
	index, err := strconv.ParseUint(strings.TrimSpace(rest[i+len(`", index=`):]), 10, 32)
	if err != nil {
		return "", 0, errors.New("a Control line's index does not read")
	}
	return rest[:i], uint32(index), nil
}
