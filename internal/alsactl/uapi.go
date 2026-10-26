package alsactl

import (
	"unsafe"
)

// Definitions follow Linux v6.12 include/uapi/sound/asound.h, checked by
// scripts/alsa-abi/probe.c. The unions keep eight-byte alignment on both hosts.
type elemID struct {
	NumID, Interface, Device, Subdevice uint32
	Name                                [44]byte
	Index                               uint32
}
type elemList struct {
	Offset, Space, Used, Count uint32
	IDs                        unsafe.Pointer
	Reserved                   [50]byte
}
type elemInfo struct {
	ID                  elemID
	Type, Access, Count uint32
	Owner               int32
	Value               [16]uint64
	Reserved            [64]byte
}
type elemValue struct {
	ID       elemID
	Indirect uint32
	Value    [128]uint64
	Reserved [128]byte
}

const (
	ctlList    = uint32(3<<30 | 80<<16 | 'U'<<8 | 0x10)
	ctlInfo    = uint32(3<<30 | 272<<16 | 'U'<<8 | 0x11)
	ctlRead    = uint32(3<<30 | 1224<<16 | 'U'<<8 | 0x12)
	ctlTLVRead = uint32(3<<30 | 8<<16 | 'U'<<8 | 0x1a)
)

// ABIFacts is the control-reader half of the shared pinned-header probe.
func ABIFacts() map[string]uint64 {
	var id elemID
	var list elemList
	var info elemInfo
	var value elemValue
	f := map[string]uint64{"size snd_ctl_elem_id": uint64(unsafe.Sizeof(id)), "size snd_ctl_elem_list": uint64(unsafe.Sizeof(list)), "size snd_ctl_elem_info": uint64(unsafe.Sizeof(info)), "size snd_ctl_elem_value": uint64(unsafe.Sizeof(value)), "ioctl SNDRV_CTL_IOCTL_ELEM_LIST": uint64(ctlList), "ioctl SNDRV_CTL_IOCTL_ELEM_INFO": uint64(ctlInfo), "ioctl SNDRV_CTL_IOCTL_ELEM_READ": uint64(ctlRead), "ioctl SNDRV_CTL_IOCTL_TLV_READ": uint64(ctlTLVRead)}
	field := func(st, n string, off, size uintptr) {
		f["offset "+st+"."+n] = uint64(off)
		f["fieldsize "+st+"."+n] = uint64(size)
	}
	field("snd_ctl_elem_id", "numid", unsafe.Offsetof(id.NumID), unsafe.Sizeof(id.NumID))
	field("snd_ctl_elem_id", "iface", unsafe.Offsetof(id.Interface), unsafe.Sizeof(id.Interface))
	field("snd_ctl_elem_id", "device", unsafe.Offsetof(id.Device), unsafe.Sizeof(id.Device))
	field("snd_ctl_elem_id", "subdevice", unsafe.Offsetof(id.Subdevice), unsafe.Sizeof(id.Subdevice))
	field("snd_ctl_elem_id", "name", unsafe.Offsetof(id.Name), unsafe.Sizeof(id.Name))
	field("snd_ctl_elem_id", "index", unsafe.Offsetof(id.Index), unsafe.Sizeof(id.Index))
	field("snd_ctl_elem_list", "offset", unsafe.Offsetof(list.Offset), unsafe.Sizeof(list.Offset))
	field("snd_ctl_elem_list", "space", unsafe.Offsetof(list.Space), unsafe.Sizeof(list.Space))
	field("snd_ctl_elem_list", "used", unsafe.Offsetof(list.Used), unsafe.Sizeof(list.Used))
	field("snd_ctl_elem_list", "count", unsafe.Offsetof(list.Count), unsafe.Sizeof(list.Count))
	field("snd_ctl_elem_list", "pids", unsafe.Offsetof(list.IDs), unsafe.Sizeof(list.IDs))
	field("snd_ctl_elem_list", "reserved", unsafe.Offsetof(list.Reserved), unsafe.Sizeof(list.Reserved))
	field("snd_ctl_elem_info", "id", unsafe.Offsetof(info.ID), unsafe.Sizeof(info.ID))
	field("snd_ctl_elem_info", "type", unsafe.Offsetof(info.Type), unsafe.Sizeof(info.Type))
	field("snd_ctl_elem_info", "access", unsafe.Offsetof(info.Access), unsafe.Sizeof(info.Access))
	field("snd_ctl_elem_info", "count", unsafe.Offsetof(info.Count), unsafe.Sizeof(info.Count))
	field("snd_ctl_elem_info", "owner", unsafe.Offsetof(info.Owner), unsafe.Sizeof(info.Owner))
	field("snd_ctl_elem_info", "value", unsafe.Offsetof(info.Value), unsafe.Sizeof(info.Value))
	field("snd_ctl_elem_info", "reserved", unsafe.Offsetof(info.Reserved), unsafe.Sizeof(info.Reserved))
	field("snd_ctl_elem_value", "id", unsafe.Offsetof(value.ID), unsafe.Sizeof(value.ID))
	field("snd_ctl_elem_value", "value", unsafe.Offsetof(value.Value), unsafe.Sizeof(value.Value))
	field("snd_ctl_elem_value", "reserved", unsafe.Offsetof(value.Reserved), unsafe.Sizeof(value.Reserved))
	return f
}
