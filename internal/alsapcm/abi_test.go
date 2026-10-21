package alsapcm

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

// abiProbe is the output of scripts/alsa-abi/probe.c, compiled against the
// pinned kernel header for this machine's architecture and run on it.
var abiProbe = flag.String("abi-probe", "", "the file scripts/alsa-abi/probe.c printed on this architecture, for TestUAPIMatchesTheKernelHeader")

// probeSource is the probe, relative to this package's directory.
const probeSource = "../../scripts/alsa-abi/probe.c"

// abiFacts is the Go side of the comparison: for every fact the probe
// prints, keyed "<kind> <name>", what the Go definitions make of it on this
// architecture, as the probe prints it (decimal, or the meta strings).
func abiFacts() map[string]string {
	f := map[string]string{
		"meta goos":   runtime.GOOS,
		"meta goarch": runtime.GOARCH,
		"meta kernel": uapiKernel,
	}
	num := func(key string, v uint64) { f[key] = strconv.FormatUint(v, 10) }
	size := func(name string, v uintptr) { num("size "+name, uint64(v)) }
	field := func(st, name string, off, sz uintptr) {
		num("offset "+st+"."+name, uint64(off))
		num("fieldsize "+st+"."+name, uint64(sz))
	}

	size("snd_pcm_uframes_t", unsafe.Sizeof(uint64(0)))
	size("snd_pcm_sframes_t", unsafe.Sizeof(int64(0)))

	var ts timespec
	size("timespec", unsafe.Sizeof(ts))
	field("timespec", "tv_sec", unsafe.Offsetof(ts.sec), unsafe.Sizeof(ts.sec))
	field("timespec", "tv_nsec", unsafe.Offsetof(ts.nsec), unsafe.Sizeof(ts.nsec))

	var m mask
	size("snd_mask", unsafe.Sizeof(m))
	field("snd_mask", "bits", unsafe.Offsetof(m.bits), unsafe.Sizeof(m.bits))

	var iv interval
	size("snd_interval", unsafe.Sizeof(iv))
	field("snd_interval", "min", unsafe.Offsetof(iv.min), unsafe.Sizeof(iv.min))
	field("snd_interval", "max", unsafe.Offsetof(iv.max), unsafe.Sizeof(iv.max))
	for name, bit := range map[string]uint32{"openmin": intervalOpenMin, "openmax": intervalOpenMax, "integer": intervalInteger, "empty": intervalEmpty} {
		num("bit snd_interval."+name, uint64(unsafe.Offsetof(iv.flags))*8+uint64(bitIndex(bit)))
	}

	var hw hwParams
	const hp = "snd_pcm_hw_params"
	size(hp, unsafe.Sizeof(hw))
	field(hp, "flags", unsafe.Offsetof(hw.flags), unsafe.Sizeof(hw.flags))
	field(hp, "masks", unsafe.Offsetof(hw.masks), unsafe.Sizeof(hw.masks))
	field(hp, "mres", unsafe.Offsetof(hw.mres), unsafe.Sizeof(hw.mres))
	field(hp, "intervals", unsafe.Offsetof(hw.intervals), unsafe.Sizeof(hw.intervals))
	field(hp, "ires", unsafe.Offsetof(hw.ires), unsafe.Sizeof(hw.ires))
	field(hp, "rmask", unsafe.Offsetof(hw.rmask), unsafe.Sizeof(hw.rmask))
	field(hp, "cmask", unsafe.Offsetof(hw.cmask), unsafe.Sizeof(hw.cmask))
	field(hp, "info", unsafe.Offsetof(hw.info), unsafe.Sizeof(hw.info))
	field(hp, "msbits", unsafe.Offsetof(hw.msbits), unsafe.Sizeof(hw.msbits))
	field(hp, "rate_num", unsafe.Offsetof(hw.rateNum), unsafe.Sizeof(hw.rateNum))
	field(hp, "rate_den", unsafe.Offsetof(hw.rateDen), unsafe.Sizeof(hw.rateDen))
	field(hp, "fifo_size", unsafe.Offsetof(hw.fifoSize), unsafe.Sizeof(hw.fifoSize))

	var sw swParams
	const sp = "snd_pcm_sw_params"
	size(sp, unsafe.Sizeof(sw))
	field(sp, "tstamp_mode", unsafe.Offsetof(sw.tstampMode), unsafe.Sizeof(sw.tstampMode))
	field(sp, "period_step", unsafe.Offsetof(sw.periodStep), unsafe.Sizeof(sw.periodStep))
	field(sp, "sleep_min", unsafe.Offsetof(sw.sleepMin), unsafe.Sizeof(sw.sleepMin))
	field(sp, "avail_min", unsafe.Offsetof(sw.availMin), unsafe.Sizeof(sw.availMin))
	field(sp, "xfer_align", unsafe.Offsetof(sw.xferAlign), unsafe.Sizeof(sw.xferAlign))
	field(sp, "start_threshold", unsafe.Offsetof(sw.startThreshold), unsafe.Sizeof(sw.startThreshold))
	field(sp, "stop_threshold", unsafe.Offsetof(sw.stopThreshold), unsafe.Sizeof(sw.stopThreshold))
	field(sp, "silence_threshold", unsafe.Offsetof(sw.silenceThreshold), unsafe.Sizeof(sw.silenceThreshold))
	field(sp, "silence_size", unsafe.Offsetof(sw.silenceSize), unsafe.Sizeof(sw.silenceSize))
	field(sp, "boundary", unsafe.Offsetof(sw.boundary), unsafe.Sizeof(sw.boundary))
	field(sp, "proto", unsafe.Offsetof(sw.proto), unsafe.Sizeof(sw.proto))
	field(sp, "tstamp_type", unsafe.Offsetof(sw.tstampType), unsafe.Sizeof(sw.tstampType))
	field(sp, "reserved", unsafe.Offsetof(sw.reserved), unsafe.Sizeof(sw.reserved))

	var info pcmInfo
	const ip = "snd_pcm_info"
	size(ip, unsafe.Sizeof(info))
	field(ip, "device", unsafe.Offsetof(info.device), unsafe.Sizeof(info.device))
	field(ip, "subdevice", unsafe.Offsetof(info.subdevice), unsafe.Sizeof(info.subdevice))
	field(ip, "stream", unsafe.Offsetof(info.stream), unsafe.Sizeof(info.stream))
	field(ip, "card", unsafe.Offsetof(info.card), unsafe.Sizeof(info.card))
	field(ip, "id", unsafe.Offsetof(info.id), unsafe.Sizeof(info.id))
	field(ip, "name", unsafe.Offsetof(info.name), unsafe.Sizeof(info.name))
	field(ip, "subname", unsafe.Offsetof(info.subname), unsafe.Sizeof(info.subname))
	field(ip, "dev_class", unsafe.Offsetof(info.devClass), unsafe.Sizeof(info.devClass))
	field(ip, "dev_subclass", unsafe.Offsetof(info.devSubclass), unsafe.Sizeof(info.devSubclass))
	field(ip, "subdevices_count", unsafe.Offsetof(info.subdevicesCount), unsafe.Sizeof(info.subdevicesCount))
	field(ip, "subdevices_avail", unsafe.Offsetof(info.subdevicesAvail), unsafe.Sizeof(info.subdevicesAvail))
	field(ip, "reserved", unsafe.Offsetof(info.reserved), unsafe.Sizeof(info.reserved))

	var st pcmStatus
	const stp = "snd_pcm_status"
	size(stp, unsafe.Sizeof(st))
	field(stp, "state", unsafe.Offsetof(st.state), unsafe.Sizeof(st.state))
	field(stp, "pad1", unsafe.Offsetof(st.pad1), unsafe.Sizeof(st.pad1))
	field(stp, "trigger_tstamp", unsafe.Offsetof(st.triggerTstamp), unsafe.Sizeof(st.triggerTstamp))
	field(stp, "tstamp", unsafe.Offsetof(st.tstamp), unsafe.Sizeof(st.tstamp))
	field(stp, "appl_ptr", unsafe.Offsetof(st.applPtr), unsafe.Sizeof(st.applPtr))
	field(stp, "hw_ptr", unsafe.Offsetof(st.hwPtr), unsafe.Sizeof(st.hwPtr))
	field(stp, "delay", unsafe.Offsetof(st.delay), unsafe.Sizeof(st.delay))
	field(stp, "avail", unsafe.Offsetof(st.avail), unsafe.Sizeof(st.avail))
	field(stp, "avail_max", unsafe.Offsetof(st.availMax), unsafe.Sizeof(st.availMax))
	field(stp, "overrange", unsafe.Offsetof(st.overrange), unsafe.Sizeof(st.overrange))
	field(stp, "suspended_state", unsafe.Offsetof(st.suspendedState), unsafe.Sizeof(st.suspendedState))
	field(stp, "audio_tstamp_data", unsafe.Offsetof(st.audioTstampData), unsafe.Sizeof(st.audioTstampData))
	field(stp, "audio_tstamp", unsafe.Offsetof(st.audioTstamp), unsafe.Sizeof(st.audioTstamp))
	field(stp, "driver_tstamp", unsafe.Offsetof(st.driverTstamp), unsafe.Sizeof(st.driverTstamp))
	field(stp, "audio_tstamp_accuracy", unsafe.Offsetof(st.audioTstampAccuracy), unsafe.Sizeof(st.audioTstampAccuracy))
	field(stp, "reserved", unsafe.Offsetof(st.reserved), unsafe.Sizeof(st.reserved))

	var x xferi
	size("snd_xferi", unsafe.Sizeof(x))
	field("snd_xferi", "result", unsafe.Offsetof(x.result), unsafe.Sizeof(x.result))
	field("snd_xferi", "buf", unsafe.Offsetof(x.buf), unsafe.Sizeof(x.buf))
	field("snd_xferi", "frames", unsafe.Offsetof(x.frames), unsafe.Sizeof(x.frames))

	for name, v := range map[string]uint64{
		"SNDRV_PCM_VERSION":                 pcmVersion,
		"SNDRV_PROTOCOL_VERSION(2,0,15)":    uint64(protocolVersion(2, 0, 15)),
		"SNDRV_PCM_FORMAT_S16_LE":           formatS16LE,
		"SNDRV_PCM_FORMAT_S24_LE":           formatS24LE,
		"SNDRV_PCM_FORMAT_S32_LE":           formatS32LE,
		"SNDRV_PCM_FORMAT_S24_3LE":          formatS243LE,
		"SNDRV_PCM_FORMAT_LAST":             formatLast,
		"SNDRV_PCM_ACCESS_RW_INTERLEAVED":   accessRWInterleaved,
		"SNDRV_PCM_SUBFORMAT_STD":           subformatStd,
		"SNDRV_PCM_STREAM_PLAYBACK":         streamPlayback,
		"SNDRV_MASK_MAX":                    maskMax,
		"SNDRV_PCM_HW_PARAM_ACCESS":         hwParamAccess,
		"SNDRV_PCM_HW_PARAM_FORMAT":         hwParamFormat,
		"SNDRV_PCM_HW_PARAM_SUBFORMAT":      hwParamSubformat,
		"SNDRV_PCM_HW_PARAM_FIRST_MASK":     hwParamFirstMask,
		"SNDRV_PCM_HW_PARAM_LAST_MASK":      hwParamLastMask,
		"SNDRV_PCM_HW_PARAM_SAMPLE_BITS":    hwParamSampleBits,
		"SNDRV_PCM_HW_PARAM_FRAME_BITS":     hwParamFrameBits,
		"SNDRV_PCM_HW_PARAM_CHANNELS":       hwParamChannels,
		"SNDRV_PCM_HW_PARAM_RATE":           hwParamRate,
		"SNDRV_PCM_HW_PARAM_PERIOD_TIME":    hwParamPeriodTime,
		"SNDRV_PCM_HW_PARAM_PERIOD_SIZE":    hwParamPeriodSize,
		"SNDRV_PCM_HW_PARAM_PERIOD_BYTES":   hwParamPeriodBytes,
		"SNDRV_PCM_HW_PARAM_PERIODS":        hwParamPeriods,
		"SNDRV_PCM_HW_PARAM_BUFFER_TIME":    hwParamBufferTime,
		"SNDRV_PCM_HW_PARAM_BUFFER_SIZE":    hwParamBufferSize,
		"SNDRV_PCM_HW_PARAM_BUFFER_BYTES":   hwParamBufferBytes,
		"SNDRV_PCM_HW_PARAM_TICK_TIME":      hwParamTickTime,
		"SNDRV_PCM_HW_PARAM_FIRST_INTERVAL": hwParamFirstInterval,
		"SNDRV_PCM_HW_PARAM_LAST_INTERVAL":  hwParamLastInterval,
		"SNDRV_PCM_STATE_OPEN":              stateOpen,
		"SNDRV_PCM_STATE_SETUP":             stateSetup,
		"SNDRV_PCM_STATE_PREPARED":          statePrepared,
		"SNDRV_PCM_STATE_RUNNING":           stateRunning,
		"SNDRV_PCM_STATE_XRUN":              stateXrun,
		"SNDRV_PCM_STATE_DRAINING":          stateDraining,
		"SNDRV_PCM_STATE_PAUSED":            statePaused,
		"SNDRV_PCM_STATE_SUSPENDED":         stateSuspended,
		"SNDRV_PCM_STATE_DISCONNECTED":      stateDisconnected,
		"SNDRV_PCM_TSTAMP_NONE":             tstampNone,
		"SNDRV_PCM_INFO_BATCH":              infoBatch,
		"SNDRV_PCM_INFO_BLOCK_TRANSFER":     infoBlockTransfer,
		"SNDRV_PCM_INFO_RESUME":             infoResume,
		"SNDRV_PCM_INFO_PAUSE":              infoPause,
	} {
		num("const "+name, v)
	}

	for name, v := range map[string]uint32{
		"SNDRV_PCM_IOCTL_PVERSION":      ioctlPVersion,
		"SNDRV_PCM_IOCTL_INFO":          ioctlInfo,
		"SNDRV_PCM_IOCTL_HW_REFINE":     ioctlHWRefine,
		"SNDRV_PCM_IOCTL_HW_PARAMS":     ioctlHWParams,
		"SNDRV_PCM_IOCTL_HW_FREE":       ioctlHWFree,
		"SNDRV_PCM_IOCTL_SW_PARAMS":     ioctlSWParams,
		"SNDRV_PCM_IOCTL_STATUS":        ioctlStatus,
		"SNDRV_PCM_IOCTL_DELAY":         ioctlDelay,
		"SNDRV_PCM_IOCTL_PREPARE":       ioctlPrepare,
		"SNDRV_PCM_IOCTL_START":         ioctlStart,
		"SNDRV_PCM_IOCTL_DROP":          ioctlDrop,
		"SNDRV_PCM_IOCTL_DRAIN":         ioctlDrain,
		"SNDRV_PCM_IOCTL_WRITEI_FRAMES": ioctlWriteIFrames,
	} {
		num("ioctl "+name, uint64(v))
	}
	return f
}

// bitIndex is the index of the one bit set in v.
func bitIndex(v uint32) int {
	for i := range 32 {
		if v == 1<<i {
			return i
		}
	}
	return -1
}

// parseProbe reads the probe's output: one fact a line, "<kind> <name>
// <value>", each named once.
func parseProbe(b []byte) (map[string]string, error) {
	out := map[string]string{}
	for i, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("line %d: %q is not a kind, a name and a value", i+1, line)
		}
		key := fields[0] + " " + fields[1]
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("line %d: %s is printed twice", i+1, key)
		}
		out[key] = fields[2]
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the probe printed nothing")
	}
	return out, nil
}

// compareFacts lists every fact on which the two sides differ, or that one
// side has and the other lacks.
func compareFacts(probe, goSide map[string]string) []string {
	var keys []string
	for k := range probe {
		keys = append(keys, k)
	}
	for k := range goSide {
		if _, ok := probe[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var diffs []string
	for _, k := range keys {
		p, inProbe := probe[k]
		g, inGo := goSide[k]
		switch {
		case !inProbe:
			diffs = append(diffs, k+": the Go side has "+g+", the probe prints nothing")
		case !inGo:
			diffs = append(diffs, k+": the probe prints "+p+", the Go side has nothing")
		case p != g:
			diffs = append(diffs, fmt.Sprintf("%s: the header makes %s, the Go definitions %s%s", k, p, g, hexOf(p, g)))
		}
	}
	return diffs
}

// hexOf adds the two numbers in hexadecimal, which is how ioctl numbers
// and flags read, or nothing when they are not numbers.
func hexOf(a, b string) string {
	x, errA := strconv.ParseUint(a, 10, 64)
	y, errB := strconv.ParseUint(b, 10, 64)
	if errA != nil || errB != nil {
		return ""
	}
	return fmt.Sprintf(" (%#x and %#x)", x, y)
}

// TestUAPIMatchesTheKernelHeader compares the Go definitions with what the C
// compiler made of the pinned asound.h on this architecture: every size,
// offset, field size, interval bit, constant and ioctl number the probe
// printed, and that the probe was built for this OS and architecture from
// the kernel tag uapiKernel names. scripts/alsa-abi/check.sh builds and runs
// the probe and then this test, on linux/amd64 and linux/arm64 in CI; with
// no -abi-probe it is skipped.
func TestUAPIMatchesTheKernelHeader(t *testing.T) {
	if *abiProbe == "" {
		t.Skip("no -abi-probe: scripts/alsa-abi/check.sh builds the probe against the pinned header and runs this with it")
	}
	b, err := os.ReadFile(*abiProbe)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := parseProbe(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range compareFacts(probe, abiFacts()) {
		t.Error(d)
	}
	if !t.Failed() {
		t.Logf("%d facts of asound.h at %s agree with the Go definitions on %s/%s", len(probe), uapiKernel, runtime.GOOS, runtime.GOARCH)
	}
}

// probeCalls are the probe's fact-printing macros and the keys each makes.
var probeCalls = []struct {
	re   *regexp.Regexp
	keys func(m []string) []string
}{
	{regexp.MustCompile(`\bT?SIZE\((\w+)\)`), func(m []string) []string { return []string{"size " + m[1]} }},
	{regexp.MustCompile(`\bFIELD\((\w+),\s*(\w+)\)`), func(m []string) []string {
		return []string{"offset " + m[1] + "." + m[2], "fieldsize " + m[1] + "." + m[2]}
	}},
	{regexp.MustCompile(`\bCONST\((\w+)\)`), func(m []string) []string { return []string{"const " + m[1]} }},
	{regexp.MustCompile(`\bIOCTL\((\w+)\)`), func(m []string) []string { return []string{"ioctl " + m[1]} }},
	{regexp.MustCompile(`\bBIT\((\w+)\)`), func(m []string) []string { return []string{"bit snd_interval." + m[1]} }},
	{regexp.MustCompile(`\bMETA\("(\w+)"`), func(m []string) []string { return []string{"meta " + m[1]} }},
	{regexp.MustCompile(`\bNUM\("(\w+)", "([^"]+)"`), func(m []string) []string { return []string{m[1] + " " + m[2]} }},
}

// TestProbeAndGoNameTheSameFacts pins, on every OS and without a C
// compiler, that probe.c prints a fact for every one the Go side compares
// and no other, so a definition added on one side and not the other fails
// here rather than first in the alsa-abi job. It reads the probe's macro
// calls; the preprocessor lines, the macros' own definitions with their
// continuation lines among them, are skipped.
func TestProbeAndGoNameTheSameFacts(t *testing.T) {
	src, err := os.ReadFile(probeSource)
	if err != nil {
		t.Fatal(err)
	}
	inProbe := map[string]string{}
	continued := false
	for _, line := range strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n") {
		directive := continued || strings.HasPrefix(strings.TrimSpace(line), "#")
		continued = directive && strings.HasSuffix(line, "\\")
		if directive {
			continue
		}
		for _, call := range probeCalls {
			for _, m := range call.re.FindAllStringSubmatch(line, -1) {
				for _, k := range call.keys(m) {
					if _, dup := inProbe[k]; dup {
						t.Errorf("probe.c prints %s twice", k)
					}
					inProbe[k] = "?"
				}
			}
		}
	}
	goSide := abiFacts()
	for k := range goSide {
		if _, ok := inProbe[k]; !ok {
			t.Errorf("the Go side compares %s, which probe.c does not print", k)
		}
	}
	for k := range inProbe {
		if _, ok := goSide[k]; !ok {
			t.Errorf("probe.c prints %s, which the Go side does not compare", k)
		}
	}
}

// TestCompareFactsReportsEachKindOfDifference pins the comparison itself:
// a value that differs, a fact only one side has, and a probe line that is
// not three fields are each reported, so a probe that printed less, or
// something else, cannot pass.
func TestCompareFactsReportsEachKindOfDifference(t *testing.T) {
	probe := map[string]string{"size a": "8", "size b": "16", "ioctl c": "1"}
	goSide := map[string]string{"size a": "8", "size b": "24", "const d": "2"}
	diffs := compareFacts(probe, goSide)
	want := []string{
		"const d: the Go side has 2, the probe prints nothing",
		"ioctl c: the probe prints 1, the Go side has nothing",
		"size b: the header makes 16, the Go definitions 24 (0x10 and 0x18)",
	}
	if !slices.Equal(diffs, want) {
		t.Errorf("diffs:\n%s\nwant:\n%s", strings.Join(diffs, "\n"), strings.Join(want, "\n"))
	}
	for _, bad := range []string{"", "size a", "size a 8 9", "size a 8\nsize a 8"} {
		if _, err := parseProbe([]byte(bad)); err == nil {
			t.Errorf("parseProbe accepted %q", bad)
		}
	}
}
