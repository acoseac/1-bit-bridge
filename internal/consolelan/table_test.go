package consolelan

import (
	"encoding/binary"
	"errors"
	"testing"
)

// TestTheInterfaceTableIsReadAtItsNativeRowSize builds a three-row
// MIB_IPINTERFACE_TABLE at the native layout on every platform, its offsets
// written here as numbers of their own (NumEntries at 0, rows from 8 of 168
// bytes each, a row's index at 16, ForwardingEnabled at 41, WeakHostSend at
// 42, WeakHostReceive at 43), every other byte a non-zero filler, and each row
// one flag alone, one of them a non-zero byte other than 1. Each row reads its
// own index and flags, so a reader stepping by another row size (x/sys's 176)
// or reading a flag at another offset is found; a buffer one byte short of its
// rows is refused, as a table that does not read.
func TestTheInterfaceTableIsReadAtItsNativeRowSize(t *testing.T) {
	const size = 8 + 168*3
	b := make([]byte, size)
	for i := range b {
		b[i] = 0xA5
	}
	binary.LittleEndian.PutUint32(b[0:], 3)
	type flags struct{ forwarding, weakSend, weakReceive byte }
	rows := []struct {
		index uint32
		flags flags
		want  Row
	}{
		{1, flags{0, 1, 0}, Row{Index: 1, WeakHostSend: true}},
		{14, flags{0, 0, 1}, Row{Index: 14, WeakHostReceive: true}},
		{7, flags{0x80, 0, 0}, Row{Index: 7, Forwarding: true}},
	}
	for k, r := range rows {
		at := 8 + 168*k
		binary.LittleEndian.PutUint32(b[at+16:], r.index)
		b[at+41], b[at+42], b[at+43] = r.flags.forwarding, r.flags.weakSend, r.flags.weakReceive
	}
	got, err := parseInterfaceTable(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(rows) {
		t.Fatalf("read %d rows, want %d", len(got), len(rows))
	}
	for k, r := range rows {
		if got[k].Index != r.want.Index {
			t.Fatalf("row %d reads the index %d, want %d: its bytes were read at another row's place", k, got[k].Index, r.want.Index)
		}
		if got[k] != r.want {
			t.Fatalf("row %d (index %d) reads the flags %+v, want %+v", k, r.want.Index, got[k], r.want)
		}
	}
	_, err = parseInterfaceTable(b[:size-1])
	var re *RefusalError
	if !errors.As(err, &re) || re.Reason != ReasonHostModelUnreadable {
		t.Fatalf("a table one byte short of its rows read with %v, want %s", err, ReasonHostModelUnreadable)
	}
}
