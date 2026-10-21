package alsapcm

import "unsafe"

// The definitions in this file are the parts of the kernel's ALSA PCM
// interface the backend uses, from include/uapi/sound/asound.h of the
// kernel release uapiKernel names (GPL-2.0+ WITH Linux-syscall-note, the
// note that lets user programs use the kernel's interface). They are the
// layout a 64-bit little-endian process sees, linux/amd64 and linux/arm64
// alike. The header is not vendored: scripts/alsa-abi/check.sh fetches it
// at that tag, compiles a probe against it on each architecture and runs
// TestUAPIMatchesTheKernelHeader, which compares every size, offset, ioctl
// number, bit and constant below with what the C compiler made of the
// header.

// uapiKernel is the kernel release whose asound.h the definitions follow,
// as a tag of torvalds/linux. check.sh reads it from this line.
const uapiKernel = "v6.12"

// protocolVersion is SNDRV_PROTOCOL_VERSION(major, minor, subminor).
func protocolVersion(major, minor, subminor uint32) uint32 {
	return major<<16 | minor<<8 | subminor
}

const (
	// pcmVersion is the header's SNDRV_PCM_VERSION: the protocol these
	// definitions speak, sent to the kernel in swParams.proto.
	pcmVersion = 2<<16 | 0<<8 | 18

	// pcmVersionFloor is the oldest kernel PCM protocol Open accepts:
	// 2.0.15, which Linux 5.6 (2020) brought with the y2038 rework of the
	// PCM interface. ALSA promises compatibility within a major.minor
	// (SNDRV_PROTOCOL_INCOMPATIBLE), so a different major or minor is
	// refused whatever its micro, and on a 64-bit process every structure
	// here has had the size its request number encodes since well before
	// 2.0.15. The floor is a qualification bound, not an ABI one: every
	// kernel the owner's hardware runs is newer (Raspberry Pi 5 kernels are
	// 6.x, Debian 12's is 6.1, Ubuntu 24.04's 6.8), the oldest kernel line
	// still maintained, 5.10, carries it, and an open on anything older
	// is a typed refusal naming both versions rather than an exact open
	// nobody has checked.
	pcmVersionFloor = 2<<16 | 0<<8 | 15
)

// The sample formats (snd_pcm_format_t) the packers emit, the one access
// and subformat this backend asks for, and the playback stream.
const (
	formatS16LE  = 2  // SNDRV_PCM_FORMAT_S16_LE
	formatS24LE  = 6  // SNDRV_PCM_FORMAT_S24_LE: four bytes, low three significant
	formatS32LE  = 10 // SNDRV_PCM_FORMAT_S32_LE
	formatS243LE = 32 // SNDRV_PCM_FORMAT_S24_3LE: three bytes
	formatLast   = 52 // SNDRV_PCM_FORMAT_LAST in v6.12: SNDRV_PCM_FORMAT_DSD_U32_BE

	accessRWInterleaved = 3 // SNDRV_PCM_ACCESS_RW_INTERLEAVED: WRITEI_FRAMES
	subformatStd        = 0 // SNDRV_PCM_SUBFORMAT_STD
	streamPlayback      = 0 // SNDRV_PCM_STREAM_PLAYBACK
	maskMax             = 256
)

// The hw_params parameters (snd_pcm_hw_param_t): three masks, then twelve
// intervals from hwParamFirstInterval.
const (
	hwParamAccess        = 0
	hwParamFormat        = 1
	hwParamSubformat     = 2
	hwParamFirstMask     = hwParamAccess
	hwParamLastMask      = hwParamSubformat
	hwParamSampleBits    = 8
	hwParamFrameBits     = 9
	hwParamChannels      = 10
	hwParamRate          = 11
	hwParamPeriodTime    = 12
	hwParamPeriodSize    = 13
	hwParamPeriodBytes   = 14
	hwParamPeriods       = 15
	hwParamBufferTime    = 16
	hwParamBufferSize    = 17
	hwParamBufferBytes   = 18
	hwParamTickTime      = 19
	hwParamFirstInterval = hwParamSampleBits
	hwParamLastInterval  = hwParamTickTime
)

// The interval flags: snd_interval's four one-bit fields, which a 64-bit
// little-endian compiler allocates from the lowest bit of their word.
const (
	intervalOpenMin = 1 << 0
	intervalOpenMax = 1 << 1
	intervalInteger = 1 << 2
	intervalEmpty   = 1 << 3
)

// The stream states (snd_pcm_state_t).
const (
	stateOpen         = 0
	stateSetup        = 1
	statePrepared     = 2
	stateRunning      = 3
	stateXrun         = 4
	stateDraining     = 5
	statePaused       = 6
	stateSuspended    = 7
	stateDisconnected = 8
)

// The setup's info flags this backend reports (SNDRV_PCM_INFO_*).
const (
	infoBatch         = 0x00000010
	infoBlockTransfer = 0x00010000
	infoResume        = 0x00040000
	infoPause         = 0x00080000
)

// tstampNone is SNDRV_PCM_TSTAMP_NONE: no timestamps in the status.
const tstampNone = 0

// The ioctl requests, _IOC(dir, 'A', nr, size) with the 64-bit sizes of
// their arguments. TestIoctlNumbersEncodeTheirStructures checks each
// against the structure below on every 64-bit host.
const (
	ioctlPVersion     uint32 = 0x80044100 // _IOR('A', 0x00, int)
	ioctlInfo         uint32 = 0x81204101 // _IOR('A', 0x01, struct snd_pcm_info)
	ioctlHWRefine     uint32 = 0xc2604110 // _IOWR('A', 0x10, struct snd_pcm_hw_params)
	ioctlHWParams     uint32 = 0xc2604111 // _IOWR('A', 0x11, struct snd_pcm_hw_params)
	ioctlHWFree       uint32 = 0x00004112 // _IO('A', 0x12)
	ioctlSWParams     uint32 = 0xc0884113 // _IOWR('A', 0x13, struct snd_pcm_sw_params)
	ioctlStatus       uint32 = 0x80984120 // _IOR('A', 0x20, struct snd_pcm_status)
	ioctlDelay        uint32 = 0x80084121 // _IOR('A', 0x21, snd_pcm_sframes_t)
	ioctlPrepare      uint32 = 0x00004140 // _IO('A', 0x40)
	ioctlStart        uint32 = 0x00004142 // _IO('A', 0x42)
	ioctlDrop         uint32 = 0x00004143 // _IO('A', 0x43)
	ioctlDrain        uint32 = 0x00004144 // _IO('A', 0x44)
	ioctlWriteIFrames uint32 = 0x40184150 // _IOW('A', 0x50, struct snd_xferi)
	// ioctlPause encodes an int, but the kernel takes that int as the
	// argument itself, (unsigned long)arg, and never reads it through a
	// pointer (snd_pcm_common_ioctl; alsa-lib passes it the same way), so it
	// goes through sys.ioctlValue: 1 pauses a running stream, 0 releases a
	// paused one.
	ioctlPause uint32 = 0x40044145 // _IOW('A', 0x45, int)
)

// mask is struct snd_mask: one bit per value, SNDRV_MASK_MAX bits.
type mask struct {
	bits [maskMax / 32]uint32
}

// interval is struct snd_interval. flags holds its one-bit fields,
// openmin, openmax, integer and empty (intervalOpenMin…intervalEmpty).
type interval struct {
	min, max uint32
	flags    uint32
}

// hwParams is struct snd_pcm_hw_params, the argument of HW_REFINE and
// HW_PARAMS.
type hwParams struct {
	flags     uint32
	masks     [hwParamLastMask - hwParamFirstMask + 1]mask
	mres      [5]mask
	intervals [hwParamLastInterval - hwParamFirstInterval + 1]interval
	ires      [9]interval
	rmask     uint32 // the parameters to refine
	cmask     uint32 // the parameters the kernel changed
	info      uint32 // SNDRV_PCM_INFO_* of the setup
	msbits    uint32 // the significant bits of a sample
	rateNum   uint32
	rateDen   uint32
	fifoSize  uint64 // snd_pcm_uframes_t
	// tail is sync[16] and reserved[48] since Linux 6.11, and reserved[64]
	// before it. Nothing here reads it; it is sent as zeros.
	tail [64]byte
}

// swParams is struct snd_pcm_sw_params, the argument of SW_PARAMS.
type swParams struct {
	tstampMode       int32
	periodStep       uint32
	sleepMin         uint32
	availMin         uint64
	xferAlign        uint64
	startThreshold   uint64
	stopThreshold    uint64
	silenceThreshold uint64
	silenceSize      uint64
	boundary         uint64
	proto            uint32
	tstampType       uint32
	reserved         [56]byte
}

// pcmInfo is struct snd_pcm_info, the argument of INFO.
type pcmInfo struct {
	device          uint32
	subdevice       uint32
	stream          int32
	card            int32
	id              [64]byte
	name            [80]byte
	subname         [32]byte
	devClass        int32
	devSubclass     int32
	subdevicesCount uint32
	subdevicesAvail uint32
	// sync is pad1[16] since Linux 6.11, and union snd_pcm_sync_id
	// before it. Nothing here reads it.
	sync     [16]byte
	reserved [64]byte
}

// timespec is struct timespec on a 64-bit Linux: eight bytes of seconds,
// eight of nanoseconds.
type timespec struct {
	sec  int64
	nsec int64
}

// pcmStatus is struct snd_pcm_status as a 64-bit process sees it, the
// argument of STATUS. pad1 is the header's __time_pad, four bytes where
// time_t is eight.
type pcmStatus struct {
	state               int32
	pad1                [4]byte
	triggerTstamp       timespec
	tstamp              timespec
	applPtr             uint64
	hwPtr               uint64
	delay               int64
	avail               uint64
	availMax            uint64
	overrange           uint64
	suspendedState      int32
	audioTstampData     uint32
	audioTstamp         timespec
	driverTstamp        timespec
	audioTstampAccuracy uint32
	reserved            [20]byte
}

// xferi is struct snd_xferi, the argument of WRITEI_FRAMES. buf is a real
// pointer, never a uintptr: the collector then keeps the samples it points
// at alive, and a stack move updates it. The kernel reads the samples
// during the call and keeps nothing after it.
type xferi struct {
	result int64
	buf    unsafe.Pointer
	frames uint64
}
