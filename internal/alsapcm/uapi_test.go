package alsapcm

import (
	"testing"
	"unsafe"
)

// The _IOC directions of asm-generic/ioctl.h, which linux/amd64 and
// linux/arm64 both use.
const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2
)

// ioc is the kernel's _IOC(dir, 'A', nr, size).
func ioc(dir, nr uint32, size uintptr) uint32 {
	return dir<<30 | uint32(size)<<16 | 'A'<<8 | nr
}

// TestIoctlNumbersEncodeTheirStructures pins each request number to the
// structure the backend passes with it: _IOC encodes the argument's size,
// so a field added to, or dropped from, a structure without its request
// changing fails here on any 64-bit host, before the probe in
// scripts/alsa-abi compares both with the kernel's header.
func TestIoctlNumbersEncodeTheirStructures(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the definitions are the 64-bit layout")
	}
	for _, c := range []struct {
		name string
		got  uint32
		want uint32
	}{
		{"PVERSION", ioctlPVersion, ioc(iocRead, 0x00, unsafe.Sizeof(int32(0)))},
		{"INFO", ioctlInfo, ioc(iocRead, 0x01, unsafe.Sizeof(pcmInfo{}))},
		{"HW_REFINE", ioctlHWRefine, ioc(iocRead|iocWrite, 0x10, unsafe.Sizeof(hwParams{}))},
		{"HW_PARAMS", ioctlHWParams, ioc(iocRead|iocWrite, 0x11, unsafe.Sizeof(hwParams{}))},
		{"HW_FREE", ioctlHWFree, ioc(iocNone, 0x12, 0)},
		{"SW_PARAMS", ioctlSWParams, ioc(iocRead|iocWrite, 0x13, unsafe.Sizeof(swParams{}))},
		{"STATUS", ioctlStatus, ioc(iocRead, 0x20, unsafe.Sizeof(pcmStatus{}))},
		{"DELAY", ioctlDelay, ioc(iocRead, 0x21, unsafe.Sizeof(int64(0)))},
		{"PREPARE", ioctlPrepare, ioc(iocNone, 0x40, 0)},
		{"START", ioctlStart, ioc(iocNone, 0x42, 0)},
		{"DROP", ioctlDrop, ioc(iocNone, 0x43, 0)},
		{"DRAIN", ioctlDrain, ioc(iocNone, 0x44, 0)},
		{"WRITEI_FRAMES", ioctlWriteIFrames, ioc(iocWrite, 0x50, unsafe.Sizeof(xferi{}))},
	} {
		if c.got != c.want {
			t.Errorf("SNDRV_PCM_IOCTL_%s is %#x, but its structure makes %#x", c.name, c.got, c.want)
		}
	}
}

// TestIntervalEndsFollowTheKernel pins how an interval is read: an open end
// excludes its value, a committed value is a closed interval of one value,
// and an empty one holds nothing.
func TestIntervalEndsFollowTheKernel(t *testing.T) {
	for _, c := range []struct {
		iv      interval
		lo, hi  uint32
		exact   bool
		value   uint32
		printed string
	}{
		{interval{min: 48000, max: 48000}, 48000, 48000, true, 48000, "48000"},
		{interval{min: 64, max: 4096, flags: intervalInteger}, 64, 4096, false, 0, "64..4096"},
		{interval{min: 1000, max: 2000, flags: intervalOpenMin}, 1001, 2000, false, 0, "1001..2000"},
		{interval{min: 1000, max: 2000, flags: intervalOpenMax}, 1000, 1999, false, 0, "1000..1999"},
		{interval{min: 47999, max: 48000, flags: intervalOpenMin}, 48000, 48000, false, 0, "48000"},
		{interval{min: 5, max: 5, flags: intervalEmpty}, 5, 5, false, 0, "none"},
		{interval{min: 0, max: ^uint32(0), flags: intervalOpenMin | intervalOpenMax}, 1, ^uint32(0) - 1, false, 0, "1..4294967294"},
	} {
		if c.iv.lo() != c.lo || c.iv.hi() != c.hi {
			t.Errorf("%+v: lo %d hi %d, want %d %d", c.iv, c.iv.lo(), c.iv.hi(), c.lo, c.hi)
		}
		if v, ok := c.iv.exact(); ok != c.exact || v != c.value {
			t.Errorf("%+v: exact %d %v, want %d %v", c.iv, v, ok, c.value, c.exact)
		}
		if c.iv.String() != c.printed {
			t.Errorf("%+v prints %q, want %q", c.iv, c.iv.String(), c.printed)
		}
	}
	for iv, want := range map[interval]string{
		{min: 48000, max: 48000}:                         "[48000, 48000]",
		{min: 47999, max: 48000, flags: intervalOpenMin}: "(47999, 48000]",
		{min: 1, max: 2, flags: intervalOpenMax}:         "[1, 2)",
		{min: 5, max: 5, flags: intervalEmpty}:           "[5, 5] empty",
	} {
		if iv.raw() != want {
			t.Errorf("%+v raw is %q, want %q", iv, iv.raw(), want)
		}
	}
}

// TestMaskSetAndRead pins the mask helpers: setMask leaves one bit, single
// reads it back, values lists bits across words.
func TestMaskSetAndRead(t *testing.T) {
	hw := anyParams()
	if v := hw.maskOf(hwParamFormat).values(); len(v) != maskMax {
		t.Fatalf("any() format mask holds %d values, want %d", len(v), maskMax)
	}
	hw.setMask(hwParamFormat, formatS243LE)
	if v, ok := hw.maskOf(hwParamFormat).single(); !ok || v != formatS243LE {
		t.Errorf("single = %d %v, want %d", v, ok, formatS243LE)
	}
	var m mask
	m.bits[0], m.bits[1] = 1<<2|1<<10, 1<<0
	if got := m.values(); len(got) != 3 || got[0] != 2 || got[1] != 10 || got[2] != 32 {
		t.Errorf("values %v, want [2 10 32]", got)
	}
	if _, ok := m.single(); ok {
		t.Error("three values read as one")
	}
}
