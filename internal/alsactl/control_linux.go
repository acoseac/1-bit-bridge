//go:build linux && (amd64 || arm64)

package alsactl

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"runtime"
	"strings"
	"unsafe"
)

type linuxDevice struct{ fd int }

func platformOpen(card int) (Device, error) {
	fd, e := unix.Open(fmt.Sprintf("/dev/snd/controlC%d", card), unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	var st unix.Stat_t
	if e = unix.Fstat(fd, &st); e != nil {
		unix.Close(fd)
		return nil, e
	}
	if st.Mode&unix.S_IFMT != unix.S_IFCHR || unix.Major(uint64(st.Rdev)) != 116 {
		unix.Close(fd)
		return nil, errors.New("not an ALSA control node")
	}
	return &linuxDevice{fd: fd}, nil
}
func (d *linuxDevice) Close() error { return unix.Close(d.fd) }
func ctlIO[T any](d *linuxDevice, req uint32, v *T) error {
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(d.fd), uintptr(req), uintptr(unsafe.Pointer(v)))
	runtime.KeepAlive(v)
	if e != 0 {
		return e
	}
	return nil
}
func (d *linuxDevice) Controls() ([]Control, error) {
	ids := make([]elemID, 1024)
	list := elemList{Space: 1024, IDs: unsafe.Pointer(&ids[0])}
	e := ctlIO(d, ctlList, &list)
	runtime.KeepAlive(ids)
	if e != nil {
		return nil, e
	}
	if list.Count > 1024 || list.Used != list.Count {
		return nil, errors.New("control listing incomplete")
	}
	out := make([]Control, 0, list.Used)
	for _, id := range ids[:list.Used] {

		c, e := d.readControl(id)
		if e != nil {
			c = Control{Interface: id.Interface, Device: id.Device, Subdevice: id.Subdevice, Index: id.Index, Name: strings.TrimRight(string(id.Name[:]), "\x00"), ReadError: e.Error()}
		}
		out = append(out, c)

	}
	return out, nil
}

func (d *linuxDevice) readControl(id elemID) (Control, error) {
	info := elemInfo{ID: id}
	if e := ctlIO(d, ctlInfo, &info); e != nil {
		return Control{}, e
	}
	if info.Access&1 == 0 {
		return Control{}, errors.New("control unreadable")
	}
	width, limit := 0, 0
	switch info.Type {
	case 1, 2:
		width, limit = 8, 128
	case 3:
		width, limit = 4, 128
	case 4:
		width, limit = 1, 512
	case 5:
		width, limit = 176, 1
	case 6:
		width, limit = 8, 64
	default:
		return Control{}, errors.New("unknown control type")
	}
	if info.Count > uint32(limit) {
		return Control{}, errors.New("control value exceeds bound")
	}
	v := elemValue{ID: id}
	if e := ctlIO(d, ctlRead, &v); e != nil {
		return Control{}, e
	}
	c := Control{Interface: id.Interface, Device: id.Device, Subdevice: id.Subdevice, Index: id.Index, Name: strings.TrimRight(string(id.Name[:]), "\x00"), Type: info.Type, Access: info.Access, Count: info.Count, Values: append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(&v.Value)), int(info.Count)*width)...), Info: append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(&info.Value)), 128)...)}
	if info.Access&(1<<4) != 0 {
		tlv := make([]uint32, 1026)
		tlv[0] = id.NumID
		tlv[1] = 4096
		if e := ctlIO(d, ctlTLVRead, &tlv[0]); e != nil {
			return Control{}, e
		}
		if tlv[3] > 4088 {
			return Control{}, errors.New("control TLV exceeds bound")
		}
		n := 8 + int(tlv[3])
		c.TLV = append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(&tlv[2])), n)...)
	}
	return c, nil
}
