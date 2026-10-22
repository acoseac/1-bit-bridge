package hotplug

import (
	"bytes"
	"errors"
	"strconv"
)

// The bounds a uevent message is read within. The kernel builds a uevent's
// environment in a 2048-byte buffer of at most 64 records
// (UEVENT_BUFFER_SIZE, UEVENT_NUM_ENVP) and puts "ACTION@DEVPATH" before it,
// so a message past maxUeventBytes is not one the kernel sends whole: it is
// read with MSG_TRUNC and dropped (kernelSource).
const (
	maxUeventBytes   = 8192
	maxUeventRecords = 64
)

// errMalformedUevent is every message ParseUevent refuses.
var errMalformedUevent = errors.New("hotplug: not a kernel uevent")

// Uevent is one kernel uevent, as far as the watcher reads one. Its fields
// decide whether to reconcile (Wakes), and DevPath is compared, as a
// string, with the directories of the cards a reading found, so a removal
// is matched to the card it took. Nothing here is ever made into a path to
// open: the reconciliation reads the tree through the alsa readers alone.
type Uevent struct {
	Action    string
	DevPath   string
	Subsystem string
	// DevType and Driver are the optional DEVTYPE and DRIVER records, ""
	// when absent.
	DevType, Driver string
	Seqnum          uint64
}

// ParseUevent reads one message the kernel broadcasts on
// NETLINK_KOBJECT_UEVENT: "ACTION@DEVPATH" and a NUL, then KEY=VALUE
// records, each followed by a NUL (the last NUL may be missing). ACTION,
// DEVPATH, SUBSYSTEM and SEQNUM must each be there once, ACTION and DEVPATH
// as the header says, SEQNUM a decimal number; at most 64 records and 8 KiB.
// Anything else, udev's own "libudev" messages included, is
// errMalformedUevent, and nothing of it is returned. It reads msg and
// allocates only the strings it returns.
func ParseUevent(msg []byte) (Uevent, error) {
	if len(msg) == 0 || len(msg) > maxUeventBytes {
		return Uevent{}, errMalformedUevent
	}
	end := bytes.IndexByte(msg, 0)
	if end < 0 {
		return Uevent{}, errMalformedUevent
	}
	action, devpath, ok := bytes.Cut(msg[:end], []byte("@"))
	if !ok || len(action) == 0 || len(devpath) < 2 || devpath[0] != '/' {
		return Uevent{}, errMalformedUevent
	}
	var (
		u                                Uevent
		gotAction, gotPath, gotSubsystem bool
		gotSeqnum                        bool
		records                          int
	)
	rest := msg[end+1:]
	for len(rest) > 0 {
		rec := rest
		if i := bytes.IndexByte(rest, 0); i >= 0 {
			rec, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		if len(rec) == 0 {
			// Only the kernel's own trailing NUL may end in nothing.
			if len(rest) > 0 {
				return Uevent{}, errMalformedUevent
			}
			break
		}
		if records++; records > maxUeventRecords {
			return Uevent{}, errMalformedUevent
		}
		key, value, ok := bytes.Cut(rec, []byte("="))
		if !ok || len(key) == 0 {
			return Uevent{}, errMalformedUevent
		}
		var seen *bool
		switch string(key) {
		case "ACTION":
			if !bytes.Equal(value, action) {
				return Uevent{}, errMalformedUevent
			}
			seen = &gotAction
		case "DEVPATH":
			if !bytes.Equal(value, devpath) {
				return Uevent{}, errMalformedUevent
			}
			seen = &gotPath
		case "SUBSYSTEM":
			if len(value) == 0 {
				return Uevent{}, errMalformedUevent
			}
			u.Subsystem = string(value)
			seen = &gotSubsystem
		case "SEQNUM":
			n, err := strconv.ParseUint(string(value), 10, 64)
			if err != nil {
				return Uevent{}, errMalformedUevent
			}
			u.Seqnum = n
			seen = &gotSeqnum
		case "DEVTYPE":
			u.DevType = string(value)
		case "DRIVER":
			u.Driver = string(value)
		}
		if seen != nil {
			if *seen {
				return Uevent{}, errMalformedUevent
			}
			*seen = true
		}
	}
	if !gotAction || !gotPath || !gotSubsystem || !gotSeqnum {
		return Uevent{}, errMalformedUevent
	}
	u.Action, u.DevPath = string(action), string(devpath)
	return u, nil
}

// Wakes reports whether the event is one the watcher reconciles for: a
// sound or usb device added, removed, changed, bound or unbound. Every other
// subsystem and action is ignored.
func (u Uevent) Wakes() bool {
	switch u.Subsystem {
	case "sound", "usb":
	default:
		return false
	}
	switch u.Action {
	case "add", "remove", "change", "bind", "unbind":
		return true
	}
	return false
}

// removes reports whether the event takes a device away: a remove, or an
// unbind, after which the driver no longer holds the device, so an open
// node of it is dead.
func (u Uevent) removes() bool { return u.Action == "remove" || u.Action == "unbind" }
