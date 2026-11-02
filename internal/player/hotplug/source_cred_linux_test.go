//go:build linux

package hotplug

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

// controlMsg lays one control message out as Linux writes it beside a
// message: a cmsghdr (cmsg_len, a size_t, then cmsg_level and cmsg_type),
// the payload at CMSG_LEN(0), and the message padded with zeros to
// CMSG_SPACE. msgLen overrides cmsg_len when it is not negative.
func controlMsg(level, typ int, payload []byte, msgLen int) []byte {
	b := make([]byte, unix.CmsgSpace(len(payload)))
	if msgLen < 0 {
		msgLen = unix.CmsgLen(len(payload))
	}
	lenSize := unix.SizeofCmsghdr - 8
	if lenSize == 8 {
		binary.NativeEndian.PutUint64(b, uint64(msgLen))
	} else {
		binary.NativeEndian.PutUint32(b, uint32(msgLen))
	}
	binary.NativeEndian.PutUint32(b[lenSize:], uint32(level))
	binary.NativeEndian.PutUint32(b[lenSize+4:], uint32(typ))
	copy(b[unix.CmsgLen(0):], payload)
	return b
}

// scmCredentials is an SCM_CREDENTIALS message whose payload is payload.
func scmCredentials(payload []byte) []byte {
	return controlMsg(unix.SOL_SOCKET, unix.SCM_CREDENTIALS, payload, -1)
}

// ucredBytes is a struct ucred as the kernel lays it out: pid, uid, gid.
func ucredBytes(pid int32, uid, gid uint32) []byte {
	b := make([]byte, unix.SizeofUcred)
	binary.NativeEndian.PutUint32(b, uint32(pid))
	binary.NativeEndian.PutUint32(b[4:], uid)
	binary.NativeEndian.PutUint32(b[8:], gid)
	return b
}

// inBuffer is oob at the head of a larger buffer that holds n bytes of fill
// after it, as the source's buffer holds what an earlier message left there:
// a read past oob's end finds the fill, not a fault, and says so.
func inBuffer(oob []byte, fill byte, n int) []byte {
	b := bytes.Repeat([]byte{fill}, len(oob)+n)
	copy(b, oob)
	return b[:len(oob)]
}

// credString names credentials in a failure: "none" for nil.
func credString(c *credentials) string {
	if c == nil {
		return "none"
	}
	return fmt.Sprintf("pid=%d uid=%d gid=%#x", c.pid, c.uid, c.gid)
}

// credentialCase is control data the source could be handed, and the
// credentials it must read from it.
type credentialCase struct {
	name string
	oob  []byte
	want *credentials
}

// credentialCases are the shapes of control data backlog B616 names, laid
// out as Linux writes them, with the credentials each carries: the ucred of
// the last SCM_CREDENTIALS message whose payload holds a whole one, and none
// from a payload that is empty or shorter than a ucred, alone or beside a
// whole one. A short payload sits at the head of a buffer of fill
// (inBuffer), so a read past its end finds bytes that are not the
// payload's: its message's padding, which reads as uid 0, and the fill.
func credentialCases() []credentialCase {
	user := &credentials{pid: 4242, uid: 1000, gid: 1001}
	kernel := &credentials{}
	whole := unix.UnixCredentials(&unix.Ucred{Pid: 4242, Uid: 1000, Gid: 1001})
	root := unix.UnixCredentials(&unix.Ucred{})
	empty := scmCredentials(nil)
	short := scmCredentials(ucredBytes(666, 0, 0)[:4])
	return []credentialCase{
		{"none", nil, nil},
		{"a_whole_ucred", whole, user},
		{"the_kernels", root, kernel},
		{"another_message_alone", unix.UnixRights(7), nil},
		{"an_empty_payload", empty, nil},
		{"a_short_payload", inBuffer(short, 0xA5, 16), nil},
		{"a_payload_one_byte_short", scmCredentials(ucredBytes(666, 7, 7)[:unix.SizeofUcred-1]), nil},
		{"a_payload_longer_than_a_ucred", scmCredentials(append(ucredBytes(4242, 1000, 1001), 0xEE, 0xEE, 0xEE, 0xEE)), user},
		{"an_empty_payload_after_a_whole_ucred", slices.Concat(whole, empty), user},
		{"a_whole_ucred_after_an_empty_payload", slices.Concat(empty, whole), user},
		{"a_short_payload_after_a_whole_ucred", inBuffer(slices.Concat(whole, short), 0xA5, 16), user},
		{"a_whole_ucred_after_a_short_payload", slices.Concat(short, whole), user},
		{"two_whole_ucreds", slices.Concat(whole, root), kernel},
		{"a_length_past_the_buffer", controlMsg(unix.SOL_SOCKET, unix.SCM_CREDENTIALS, ucredBytes(0, 0, 0), 1<<20), nil},
		{"a_length_below_the_header", controlMsg(unix.SOL_SOCKET, unix.SCM_CREDENTIALS, ucredBytes(0, 0, 0), 4), nil},
		{"a_tail_shorter_than_a_header_after_a_whole_ucred", slices.Concat(whole, []byte{1, 2, 3, 4}), user},
	}
}

// parseCredentialsHolding is parseCredentials under a recover of the test's
// own, so a shape that panics is reported as one and the others still run.
func parseCredentialsHolding(oob []byte) (c *credentials, panicked any) {
	defer func() { panicked = recover() }()
	return parseCredentials(oob), nil
}

// TestParseCredentialsReadsOnlyAWholeUcred pins the credentials the source
// takes for a message's sender (backlog B616): the ucred of the last
// SCM_CREDENTIALS message whose payload holds a whole one, and none from a
// payload that is empty or shorter than a ucred, alone or beside a whole
// one, with no panic and no read past the payload's end. The messages are
// laid out as x/sys's own encoder lays them out.
func TestParseCredentialsReadsOnlyAWholeUcred(t *testing.T) {
	ours := scmCredentials(ucredBytes(4242, 1000, 1001))
	if theirs := unix.UnixCredentials(&unix.Ucred{Pid: 4242, Uid: 1000, Gid: 1001}); !bytes.Equal(ours, theirs) {
		t.Fatalf("the test lays SCM_CREDENTIALS out as % x, x/sys as % x", ours, theirs)
	}
	for _, tc := range credentialCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, panicked := parseCredentialsHolding(tc.oob)
			if panicked != nil {
				t.Fatalf("panicked: %v", panicked)
			}
			if credString(got) != credString(tc.want) {
				t.Errorf("read %s, want %s", credString(got), credString(tc.want))
			}
		})
	}
}

// wholeUcredOf is the credentials control data carries, decoded by hand:
// the ucred of the last SCM_CREDENTIALS message whose payload holds a whole
// one, or nil.
func wholeUcredOf(oob []byte) *credentials {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	var c *credentials
	for _, m := range msgs {
		if m.Header.Level == unix.SOL_SOCKET && m.Header.Type == unix.SCM_CREDENTIALS && len(m.Data) >= unix.SizeofUcred {
			c = &credentials{
				pid: int32(binary.NativeEndian.Uint32(m.Data)),
				uid: binary.NativeEndian.Uint32(m.Data[4:]),
				gid: binary.NativeEndian.Uint32(m.Data[8:]),
			}
		}
	}
	return c
}

// FuzzParseCredentials holds the source's credentials parse to backlog
// B616 on any bytes: it does not panic, and what it reads is wholeUcredOf's,
// the ucred of the last SCM_CREDENTIALS message whose payload holds a whole
// one, decoded by hand from that payload. Its seeds are credentialCases',
// the empty payload that panicked before the fix among them, and the one
// committed in testdata/fuzz/FuzzParseCredentials
// (TestTheCommittedCredentialsSeedIsAnEmptyPayload).
func FuzzParseCredentials(f *testing.F) {
	for _, tc := range credentialCases() {
		f.Add(tc.oob)
	}
	f.Fuzz(func(t *testing.T, oob []byte) {
		if got, want := parseCredentials(oob), wholeUcredOf(oob); credString(got) != credString(want) {
			t.Fatalf("read %s, want %s", credString(got), credString(want))
		}
	})
}

// TestTheCommittedCredentialsSeedIsAnEmptyPayload pins the seed committed
// in testdata/fuzz/FuzzParseCredentials: an SCM_CREDENTIALS message with no
// payload, as amd64 and arm64 Linux lay it out, the shape on which x/sys's
// ParseUnixCredentials panicked in the source before backlog B616. The file
// holds it in the fuzz engine's format, under the name the engine gives such
// a file, and it reads as no credentials.
func TestTheCommittedCredentialsSeedIsAnEmptyPayload(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skipf("the seed is the control data amd64 and arm64 lay out, not %s's", runtime.GOARCH)
	}
	oob := scmCredentials(nil)
	content := fmt.Sprintf("go test fuzz v1\n[]byte(%q)\n", oob)
	name := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))[:16]
	b, err := os.ReadFile(filepath.Join("testdata", "fuzz", "FuzzParseCredentials", name))
	if err != nil {
		t.Errorf("no seed %s: %v", name, err)
	} else if string(b) != content {
		t.Errorf("seed %s holds %q, want %q", name, b, content)
	}
	if got, panicked := parseCredentialsHolding(oob); panicked != nil || got != nil {
		t.Errorf("the seed read %s (panicked %v), want none", credString(got), panicked)
	}
}
