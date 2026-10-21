/*
 * probe.c prints what this architecture's C compiler makes of the kernel's
 * ALSA PCM interface, include/uapi/sound/asound.h, for the parts the
 * bridge's pure-Go backend (internal/alsapcm) uses: every size, offset and
 * field size of the structures it passes, the bit each of snd_interval's
 * one-bit fields occupies, the constants and the ioctl numbers. One fact a
 * line, "<kind> <name> <value>", values in decimal.
 *
 * check.sh builds it against asound.h fetched at the kernel tag
 * internal/alsapcm pins, runs it, and hands the output to the Go test
 * TestUAPIMatchesTheKernelHeader, which compares each fact with the Go
 * definitions compiled for the same architecture.
 * TestProbeAndGoNameTheSameFacts holds the macro calls below and the Go
 * side to the same list of facts on every OS.
 *
 * It is never part of the bridge: no Go package builds it, and the bridge
 * needs no C toolchain at run time.
 */
#include <stddef.h>
#include <stdio.h>
#include <string.h>
#include <sys/ioctl.h>
#include <time.h>
#include <sound/asound.h>

#ifndef UAPI_KERNEL
#error "build with -DUAPI_KERNEL='\"<tag>\"', the kernel tag asound.h was fetched at"
#endif

#if defined(__x86_64__)
#define GOARCH "amd64"
#elif defined(__aarch64__)
#define GOARCH "arm64"
#else
#error "the bridge's ALSA backend runs on linux/amd64 and linux/arm64 only"
#endif

#if !defined(__BYTE_ORDER__) || __BYTE_ORDER__ != __ORDER_LITTLE_ENDIAN__
#error "the bridge's ALSA backend's definitions are little-endian"
#endif

#define META(name, value) printf("meta %s %s\n", name, value)
#define NUM(kind, name, value) printf("%s %s %llu\n", kind, name, (unsigned long long)(value))
#define SIZE(type) NUM("size", #type, sizeof(struct type))
#define TSIZE(type) NUM("size", #type, sizeof(type))
#define FIELD(type, field) do { \
	printf("offset %s.%s %llu\n", #type, #field, (unsigned long long)offsetof(struct type, field)); \
	printf("fieldsize %s.%s %llu\n", #type, #field, (unsigned long long)sizeof(((struct type *)0)->field)); \
} while (0)
#define CONST(name) NUM("const", #name, name)
#define IOCTL(name) NUM("ioctl", #name, name)
#define BIT(field) do { \
	struct snd_interval iv; \
	memset(&iv, 0, sizeof iv); \
	iv.field = 1; \
	printf("bit snd_interval.%s %ld\n", #field, bit_of(&iv)); \
} while (0)

/*
 * bit_of returns the index of the one bit set in an interval, counting
 * from the lowest bit of its first byte, or -1 when not exactly one is.
 */
static long bit_of(const struct snd_interval *iv)
{
	unsigned char b[sizeof *iv];
	long found = -1;
	size_t i;
	int j;

	memcpy(b, iv, sizeof b);
	for (i = 0; i < sizeof b; i++) {
		for (j = 0; j < 8; j++) {
			if (!((b[i] >> j) & 1))
				continue;
			if (found >= 0)
				return -1;
			found = (long)(i * 8 + (size_t)j);
		}
	}
	return found;
}

int main(void)
{
	META("goos", "linux");
	META("goarch", GOARCH);
	META("kernel", UAPI_KERNEL);

	TSIZE(snd_pcm_uframes_t);
	TSIZE(snd_pcm_sframes_t);

	SIZE(timespec);
	FIELD(timespec, tv_sec);
	FIELD(timespec, tv_nsec);

	SIZE(snd_mask);
	FIELD(snd_mask, bits);

	SIZE(snd_interval);
	FIELD(snd_interval, min);
	FIELD(snd_interval, max);
	BIT(openmin);
	BIT(openmax);
	BIT(integer);
	BIT(empty);

	SIZE(snd_pcm_hw_params);
	FIELD(snd_pcm_hw_params, flags);
	FIELD(snd_pcm_hw_params, masks);
	FIELD(snd_pcm_hw_params, mres);
	FIELD(snd_pcm_hw_params, intervals);
	FIELD(snd_pcm_hw_params, ires);
	FIELD(snd_pcm_hw_params, rmask);
	FIELD(snd_pcm_hw_params, cmask);
	FIELD(snd_pcm_hw_params, info);
	FIELD(snd_pcm_hw_params, msbits);
	FIELD(snd_pcm_hw_params, rate_num);
	FIELD(snd_pcm_hw_params, rate_den);
	FIELD(snd_pcm_hw_params, fifo_size);

	SIZE(snd_pcm_sw_params);
	FIELD(snd_pcm_sw_params, tstamp_mode);
	FIELD(snd_pcm_sw_params, period_step);
	FIELD(snd_pcm_sw_params, sleep_min);
	FIELD(snd_pcm_sw_params, avail_min);
	FIELD(snd_pcm_sw_params, xfer_align);
	FIELD(snd_pcm_sw_params, start_threshold);
	FIELD(snd_pcm_sw_params, stop_threshold);
	FIELD(snd_pcm_sw_params, silence_threshold);
	FIELD(snd_pcm_sw_params, silence_size);
	FIELD(snd_pcm_sw_params, boundary);
	FIELD(snd_pcm_sw_params, proto);
	FIELD(snd_pcm_sw_params, tstamp_type);
	FIELD(snd_pcm_sw_params, reserved);

	SIZE(snd_pcm_info);
	FIELD(snd_pcm_info, device);
	FIELD(snd_pcm_info, subdevice);
	FIELD(snd_pcm_info, stream);
	FIELD(snd_pcm_info, card);
	FIELD(snd_pcm_info, id);
	FIELD(snd_pcm_info, name);
	FIELD(snd_pcm_info, subname);
	FIELD(snd_pcm_info, dev_class);
	FIELD(snd_pcm_info, dev_subclass);
	FIELD(snd_pcm_info, subdevices_count);
	FIELD(snd_pcm_info, subdevices_avail);
	FIELD(snd_pcm_info, reserved);

	SIZE(snd_pcm_status);
	FIELD(snd_pcm_status, state);
	FIELD(snd_pcm_status, pad1);
	FIELD(snd_pcm_status, trigger_tstamp);
	FIELD(snd_pcm_status, tstamp);
	FIELD(snd_pcm_status, appl_ptr);
	FIELD(snd_pcm_status, hw_ptr);
	FIELD(snd_pcm_status, delay);
	FIELD(snd_pcm_status, avail);
	FIELD(snd_pcm_status, avail_max);
	FIELD(snd_pcm_status, overrange);
	FIELD(snd_pcm_status, suspended_state);
	FIELD(snd_pcm_status, audio_tstamp_data);
	FIELD(snd_pcm_status, audio_tstamp);
	FIELD(snd_pcm_status, driver_tstamp);
	FIELD(snd_pcm_status, audio_tstamp_accuracy);
	FIELD(snd_pcm_status, reserved);

	SIZE(snd_xferi);
	FIELD(snd_xferi, result);
	FIELD(snd_xferi, buf);
	FIELD(snd_xferi, frames);

	CONST(SNDRV_PCM_VERSION);
	NUM("const", "SNDRV_PROTOCOL_VERSION(2,0,15)", SNDRV_PROTOCOL_VERSION(2, 0, 15));
	CONST(SNDRV_PCM_FORMAT_S16_LE);
	CONST(SNDRV_PCM_FORMAT_S24_LE);
	CONST(SNDRV_PCM_FORMAT_S32_LE);
	CONST(SNDRV_PCM_FORMAT_S24_3LE);
	CONST(SNDRV_PCM_ACCESS_RW_INTERLEAVED);
	CONST(SNDRV_PCM_SUBFORMAT_STD);
	CONST(SNDRV_PCM_STREAM_PLAYBACK);
	CONST(SNDRV_MASK_MAX);
	CONST(SNDRV_PCM_HW_PARAM_ACCESS);
	CONST(SNDRV_PCM_HW_PARAM_FORMAT);
	CONST(SNDRV_PCM_HW_PARAM_SUBFORMAT);
	CONST(SNDRV_PCM_HW_PARAM_FIRST_MASK);
	CONST(SNDRV_PCM_HW_PARAM_LAST_MASK);
	CONST(SNDRV_PCM_HW_PARAM_SAMPLE_BITS);
	CONST(SNDRV_PCM_HW_PARAM_FRAME_BITS);
	CONST(SNDRV_PCM_HW_PARAM_CHANNELS);
	CONST(SNDRV_PCM_HW_PARAM_RATE);
	CONST(SNDRV_PCM_HW_PARAM_PERIOD_TIME);
	CONST(SNDRV_PCM_HW_PARAM_PERIOD_SIZE);
	CONST(SNDRV_PCM_HW_PARAM_PERIOD_BYTES);
	CONST(SNDRV_PCM_HW_PARAM_PERIODS);
	CONST(SNDRV_PCM_HW_PARAM_BUFFER_TIME);
	CONST(SNDRV_PCM_HW_PARAM_BUFFER_SIZE);
	CONST(SNDRV_PCM_HW_PARAM_BUFFER_BYTES);
	CONST(SNDRV_PCM_HW_PARAM_TICK_TIME);
	CONST(SNDRV_PCM_HW_PARAM_FIRST_INTERVAL);
	CONST(SNDRV_PCM_HW_PARAM_LAST_INTERVAL);
	CONST(SNDRV_PCM_STATE_OPEN);
	CONST(SNDRV_PCM_STATE_SETUP);
	CONST(SNDRV_PCM_STATE_PREPARED);
	CONST(SNDRV_PCM_STATE_RUNNING);
	CONST(SNDRV_PCM_STATE_XRUN);
	CONST(SNDRV_PCM_STATE_DRAINING);
	CONST(SNDRV_PCM_STATE_PAUSED);
	CONST(SNDRV_PCM_STATE_SUSPENDED);
	CONST(SNDRV_PCM_STATE_DISCONNECTED);
	CONST(SNDRV_PCM_TSTAMP_NONE);
	CONST(SNDRV_PCM_INFO_BATCH);
	CONST(SNDRV_PCM_INFO_BLOCK_TRANSFER);
	CONST(SNDRV_PCM_INFO_RESUME);
	CONST(SNDRV_PCM_INFO_PAUSE);

	IOCTL(SNDRV_PCM_IOCTL_PVERSION);
	IOCTL(SNDRV_PCM_IOCTL_INFO);
	IOCTL(SNDRV_PCM_IOCTL_HW_REFINE);
	IOCTL(SNDRV_PCM_IOCTL_HW_PARAMS);
	IOCTL(SNDRV_PCM_IOCTL_HW_FREE);
	IOCTL(SNDRV_PCM_IOCTL_SW_PARAMS);
	IOCTL(SNDRV_PCM_IOCTL_STATUS);
	IOCTL(SNDRV_PCM_IOCTL_DELAY);
	IOCTL(SNDRV_PCM_IOCTL_PREPARE);
	IOCTL(SNDRV_PCM_IOCTL_START);
	IOCTL(SNDRV_PCM_IOCTL_DROP);
	IOCTL(SNDRV_PCM_IOCTL_DRAIN);
	IOCTL(SNDRV_PCM_IOCTL_WRITEI_FRAMES);
	return 0;
}
