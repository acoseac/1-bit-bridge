#!/usr/bin/env python3
"""Write the uevent messages TestParseUeventReadsTheCapturedMessages reads.

They are RECONSTRUCTIONS, not captures: no Linux box was at hand when they
were written (B470 PR 4a2). Each is laid out byte for byte as the kernel
lays out a uevent it broadcasts on NETLINK_KOBJECT_UEVENT group 1 (v6.12):
alloc_uevent_skb writes "ACTION@DEVPATH" and a NUL, then the environment
kobject_uevent_env built, each KEY=VALUE followed by a NUL: ACTION, DEVPATH
and SUBSYSTEM, then dev_uevent's (MAJOR, MINOR and DEVNAME for a device
with a node, DEVTYPE, DRIVER once bound), then the bus's (usb_uevent:
PRODUCT, TYPE), then the device type's (usb_dev_uevent: BUSNUM, DEVNUM;
usb_if_uevent: INTERFACE, MODALIAS, which an unbind drops), and SEQNUM
last. The places are the GO bar's on a Raspberry Pi 5, as 4a's capture
puts it (internal/alsa/testdata/sysfs/pi5-go-bar.txt); the bcdDevice and
the sequence numbers are made up.

Replace them with real ones from the Pi: build the package's test binary
(GOOS=linux GOARCH=arm64 go test -c ./internal/player/hotplug), copy it
over, and run it with -test.run TestCaptureKernelUevents
-capture-uevents DIR while the GO bar is plugged in and out; it writes
each message the kernel sends, as it came, into DIR.

Usage: reconstruct.py, with no argument. It writes into its own directory
and nowhere else, so no argument can point a write outside it; to write the
messages somewhere else, to compare them, copy the script there and run the
copy.
"""
import os
import sys

GO_BAR = "/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1/1-1"
IFACE = GO_BAR + "/1-1:1.0"
CARD = IFACE + "/sound/card5"
DEVICE_NODE = ["MAJOR=189", "MINOR=1", "DEVNAME=bus/usb/001/002"]
PRODUCT = ["PRODUCT=20b1/3009/100", "TYPE=239/2/1"]
IFACE_KEYS = ["INTERFACE=1/1/32"]
MODALIAS = ["MODALIAS=usb:v20B1p3009d0100dcEFdsc02dp01ic01isc01ip20in00"]

MESSAGES = {
    "gobar-usb-device-add.bin": ("add", GO_BAR, "usb", DEVICE_NODE + ["DEVTYPE=usb_device"] + PRODUCT + ["BUSNUM=001", "DEVNUM=002"], 5219),
    "gobar-usb-interface-add.bin": ("add", IFACE, "usb", ["DEVTYPE=usb_interface"] + PRODUCT + IFACE_KEYS + MODALIAS, 5220),
    "gobar-usb-device-bind.bin": ("bind", GO_BAR, "usb", DEVICE_NODE + ["DEVTYPE=usb_device", "DRIVER=usb"] + PRODUCT + ["BUSNUM=001", "DEVNUM=002"], 5231),
    "gobar-interface-bind.bin": ("bind", IFACE, "usb", ["DEVTYPE=usb_interface", "DRIVER=snd-usb-audio"] + PRODUCT + IFACE_KEYS + MODALIAS, 5232),
    "gobar-sound-card-add.bin": ("add", CARD, "sound", [], 5233),
    "gobar-control-add.bin": ("add", CARD + "/controlC5", "sound", ["MAJOR=116", "MINOR=160", "DEVNAME=snd/controlC5"], 5234),
    "gobar-pcm-playback-add.bin": ("add", CARD + "/pcmC5D0p", "sound", ["MAJOR=116", "MINOR=176", "DEVNAME=snd/pcmC5D0p"], 5235),
    "gobar-pcm-playback-remove.bin": ("remove", CARD + "/pcmC5D0p", "sound", ["MAJOR=116", "MINOR=176", "DEVNAME=snd/pcmC5D0p"], 5297),
    "gobar-sound-card-remove.bin": ("remove", CARD, "sound", [], 5299),
    "gobar-interface-unbind.bin": ("unbind", IFACE, "usb", ["DEVTYPE=usb_interface"] + PRODUCT + IFACE_KEYS, 5301),
    "gobar-interface-remove.bin": ("remove", IFACE, "usb", ["DEVTYPE=usb_interface"] + PRODUCT + IFACE_KEYS + MODALIAS, 5302),
    "gobar-usb-device-unbind.bin": ("unbind", GO_BAR, "usb", DEVICE_NODE + ["DEVTYPE=usb_device"] + PRODUCT + ["BUSNUM=001", "DEVNUM=002"], 5317),
    "gobar-usb-device-remove.bin": ("remove", GO_BAR, "usb", DEVICE_NODE + ["DEVTYPE=usb_device"] + PRODUCT + ["BUSNUM=001", "DEVNUM=002"], 5318),
    "hdmi-sound-card-change.bin": ("change", "/devices/platform/axi/107c701400.hdmi/sound/card0", "sound", [], 4102),
    "block-device-add.bin": ("add", "/devices/virtual/block/loop7", "block", ["MAJOR=7", "MINOR=7", "DEVNAME=loop7", "DEVTYPE=disk", "DISKSEQ=15"], 6010),
}


def message(action, devpath, subsystem, keys, seqnum):
    records = ["ACTION=" + action, "DEVPATH=" + devpath, "SUBSYSTEM=" + subsystem] + keys + ["SEQNUM=%d" % seqnum]
    return (action + "@" + devpath + "\0" + "".join(r + "\0" for r in records)).encode()


def main():
    if len(sys.argv) > 1:
        sys.exit("usage: reconstruct.py (no argument: it writes into its own directory; "
                 "to write elsewhere, copy it there and run the copy)")
    out = os.path.dirname(os.path.abspath(__file__))
    for name, fields in sorted(MESSAGES.items()):
        with open(os.path.join(out, name), "wb") as f:
            f.write(message(*fields))


if __name__ == "__main__":
    main()
