package hotplug

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// FuzzParseUevent: ParseUevent never panics, and what it accepts is a
// message the kernel could have sent: the header it returns begins the
// message, every field is free of NUL, a message over the cap or with more
// than 64 records never reads, the same bytes read the same way twice, and
// the event written back in the kernel's layout reads as itself.
func FuzzParseUevent(f *testing.F) {
	seeds, _ := filepath.Glob(filepath.Join("testdata", "uevent", "*.bin"))
	for _, s := range seeds {
		if b, err := os.ReadFile(s); err == nil {
			f.Add(b)
		}
	}
	f.Add(uevent("add", "/devices/a", "usb", "1"))
	f.Add([]byte("libudev\x00add@/devices/a\x00"))
	f.Add([]byte("remove@/x\x00ACTION=remove\x00DEVPATH=/x\x00SUBSYSTEM=sound\x00SEQNUM=0"))
	f.Fuzz(func(t *testing.T, msg []byte) {
		u, err := ParseUevent(msg)
		again, err2 := ParseUevent(msg)
		if (err == nil) != (err2 == nil) || u != again {
			t.Fatalf("two reads of one message differ: %+v %v, %+v %v", u, err, again, err2)
		}
		if err != nil {
			if u != (Uevent{}) {
				t.Fatalf("a refused message returned %+v", u)
			}
			return
		}
		if len(msg) > maxUeventBytes {
			t.Fatalf("a %d-byte message read", len(msg))
		}
		if u.Action == "" || !strings.HasPrefix(u.DevPath, "/") || u.Subsystem == "" {
			t.Fatalf("an event with a field missing: %+v", u)
		}
		for _, s := range []string{u.Action, u.DevPath, u.Subsystem, u.DevType, u.Driver} {
			if strings.IndexByte(s, 0) >= 0 {
				t.Fatalf("a field holds a NUL: %+v", u)
			}
		}
		if !bytes.HasPrefix(msg, []byte(u.Action+"@"+u.DevPath+"\x00")) {
			t.Fatalf("the header %s@%s does not begin %q", u.Action, u.DevPath, msg)
		}
		if n := bytes.Count(bytes.TrimRight(msg[len(u.Action)+len(u.DevPath)+2:], "\x00"), []byte{0}) + 1; n > maxUeventRecords {
			t.Fatalf("%d records read", n)
		}
		extra := []string{}
		if u.DevType != "" {
			extra = append(extra, "DEVTYPE="+u.DevType)
		}
		if u.Driver != "" {
			extra = append(extra, "DRIVER="+u.Driver)
		}
		back, err := ParseUevent(uevent(u.Action, u.DevPath, u.Subsystem, strconv.FormatUint(u.Seqnum, 10), extra...))
		if err != nil || back != u {
			t.Fatalf("%+v written back read as %+v, %v", u, back, err)
		}
	})
}
