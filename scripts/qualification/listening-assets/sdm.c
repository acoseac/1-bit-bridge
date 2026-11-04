/*
 * sdm.c: the listening-asset generator for a DAC's DSD qualification
 * (ops/b470-native-qualification.md, "Preparing listening assets").
 *
 * It is written for listening assets alone and shares nothing with the
 * bridge: not its decoders, not the decoder test goldens. It renders a
 * programme (tone bursts and silences defined in continuous time) as a
 * 1-bit DSD stream through a sigma-delta modulator of its own, and as
 * integer PCM at any rate, and writes DSF (LSB- or MSB-first), DSDIFF and
 * WAVE_FORMAT_EXTENSIBLE files from the containers' specifications.
 *
 * The modulator is a 1-bit error-feedback loop: v = u + (H - 1)e, y =
 * sign(v + d), e = y - v, so y = u + He with a signal transfer of exactly 1
 * and the noise transfer function H. H is designed here, at run time, by
 * Schreier's synthesizeNTF procedure for a lowpass NTF: its zeros are the
 * optimised ones (the roots of the Legendre polynomial of its order, on the
 * unit circle across the band 0..pi/OSR), its poles the maximally flat set
 * whose out-of-band gain |H(-1)| is Hinf. The defaults are order 5, OSR 64
 * and Hinf 1.4, a margin under Lee's 1.5 for a 1-bit loop, and the same
 * design is used at every DSD rate, so its band edge is the rate / 128
 * (22.05 kHz at DSD64, 44.1 kHz at DSD128, 88.2 kHz at DSD256). d is in-loop
 * TPDF dither whose peak is --dither (default 0.4, in units of the +-1
 * output); it is shaped by H like the quantisation error and breaks the
 * idle-channel limit cycle a 1-bit loop otherwise falls into: at 0.1 this
 * loop's silence carried a line at a quarter of the rate 30 dB over its
 * noise, at 0.3 one of 10 dB, and from 0.4 none. Too much dither unsettles a
 * 1-bit loop too: this one held to 0.7 and left its bounds from 0.8, so the
 * self-test runs silence at 1.5 times the dither as its margin. Silence is
 * the loop with a zero input: a dithered, noise-shaped stream of density
 * one half, never constant bytes. The loop refuses to go on past |v| > 16,
 * which a stable loop never reaches; every run reports its largest |v|.
 *
 * Levels follow the DSD convention that 0 dB SA-CD is a sine of half the
 * +-1 range (50 % modulation), the analogue level of PCM full scale. A peak
 * of P dBFS is therefore a DSD sine of amplitude 0.5 * 10^(P/20) and a PCM
 * sine of 10^(P/20) of full scale (2^(bits-1)), dithered with TPDF of +-1 LSB
 * and rounded. A DAC that maps full modulation to PCM full scale plays the
 * DSD 6.02 dB quieter than the PCM, never louder.
 *
 * Every output is a pure function of its arguments, and the random numbers
 * (xorshift64*, seeded through splitmix64) of --seed. Build it with
 * -std=c99 -ffp-contract=off (generate.py does), so no fused multiply-add
 * changes a bit between machines; libm's sin, cos and exp are the rest.
 *
 *   sdm design   [--order N] [--osr R] [--hinf H]
 *   sdm selftest [--order N] [--osr R] [--hinf H] [--dither D]
 *   sdm dsd --rate R --seed S --content NAME:FRAMES [--content NAME:FRAMES]
 *           --write KIND:SEGMENT:PATH ... [--order N] [--osr R] [--hinf H]
 *           [--dither D] [--peak-dbfs P]
 *   sdm wav --rate R --bits 24|32 --seed S --content NAME:FRAMES [--rl]
 *           --out PATH [--peak-dbfs P]
 *
 * A dsd run modulates its --content segments one after another in one loop,
 * so a later segment continues the stream of the one before it bit for bit;
 * --write puts a segment (or segments joined by +, as the loop made them) in
 * one container: dsf-lsb, dsf-msb, dsf-lsb-rl (the channels swapped), dff
 * (CHNL SLFT SRGT) or dff-chnl-rl (the same storage under CHNL SRGT SLFT,
 * which a reader honouring CHNL plays with its channels swapped). The
 * contents are main (8 s: 1 s silence, 2 s left 1 kHz, 1 s silence, 2 s
 * right 2 kHz, 2 s silence), quiet-tail (6 s: 1 s silence, left 1 kHz and
 * right 2 kHz from 1 s, steady to 1.5 s, then decaying 60 dB a second to
 * 3.5 s, then silence) and short (1 s silence, 0.5 s left 1 kHz, 0.25 s
 * silence, 0.5 s right 2 kHz, then silence for the rest of its frames).
 * Each burst rises and falls over a 10 ms raised cosine, its sine at phase
 * zero at its start, a decaying burst's at the phase that leaves it no net
 * area (burst_phase). main-rl, quiet-tail-rl and short-rl swap the
 * channels.
 *
 * A file is cut from a running loop, so its ends are where a player joins
 * it to another file or to its own 0x69 silence. Each segment boundary at an
 * even position is chosen to leave no step there (search_stream): the
 * running sum of the bits stands on its own mean either side, and the audio
 * band against 0x69 is at most BOUNDARY_CLICK, or the least of
 * BOUNDARY_TRIALS re-ditherings; a boundary at an odd position is clean only
 * as the continuation the loop made. The dsd run's JSON line reports every
 * boundary.
 */
#include <complex.h>
#include <errno.h>
#include <math.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define SDM_VERSION "1"
#define MAXORDER 8
#define VLIMIT 16.0
#define DSF_BLOCK 4096

/* order, OSR, Hinf, dither and peak dBFS, as the comment above says. */
#define DESIGN_DEFAULTS {5, 64, 1.4, 0.4, -30}

#ifndef M_PI
#define M_PI 3.14159265358979323846
#endif

static void die(const char *fmt, const char *arg) {
	fprintf(stderr, "sdm: ");
	fprintf(stderr, fmt, arg);
	fprintf(stderr, "\n");
	exit(2);
}

/* ------------------------------------------------------------ random */

typedef struct {
	uint64_t s;
} rng_t;

static uint64_t splitmix64(uint64_t *x) {
	uint64_t z = (*x += 0x9E3779B97F4A7C15ULL);
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9ULL;
	z = (z ^ (z >> 27)) * 0x94D049BB133111EBULL;
	return z ^ (z >> 31);
}

static void rng_seed(rng_t *r, uint64_t seed) {
	r->s = splitmix64(&seed);
	if (r->s == 0)
		r->s = 1;
}

static uint64_t rng_next(rng_t *r) {
	uint64_t x = r->s;
	x ^= x >> 12;
	x ^= x << 25;
	x ^= x >> 27;
	r->s = x;
	return x * 2685821657736338717ULL;
}

/* rng_unit is uniform in [0, 1), rng_tpdf triangular in (-1, 1). */
static double rng_unit(rng_t *r) { return (double)(rng_next(r) >> 11) * (1.0 / 9007199254740992.0); }
static double rng_tpdf(rng_t *r) {
	double a = rng_unit(r);
	double b = rng_unit(r);
	return a - b;
}

/* -------------------------------------------------------------- NTF */

typedef struct {
	int order;
	double osr, hinf;
	double complex zeros[MAXORDER], poles[MAXORDER];
	/* H(z) = N(z)/D(z), both monic: n[0] = d[0] = 1, n[k] the coefficient
	 * of z^-k. */
	double n[MAXORDER + 1], d[MAXORDER + 1];
} ntf_t;

/* legendre_roots puts the n roots of the Legendre polynomial P_n in x,
 * ascending: Schreier's optimised zeros (ds_optzeros) on the band edge. */
static void legendre_roots(int n, double *x) {
	for (int i = 1; i <= n; i++) {
		double r = cos(M_PI * (i - 0.25) / (n + 0.5));
		for (int it = 0; it < 100; it++) {
			double p0 = 1.0, p1 = r;
			for (int k = 2; k <= n; k++) {
				double p2 = ((2.0 * k - 1.0) * r * p1 - (k - 1.0) * p0) / k;
				p0 = p1;
				p1 = p2;
			}
			double p = n == 1 ? r : p1;
			double dp = n == 1 ? 1.0 : n * (r * p1 - p0) / (r * r - 1.0);
			double dx = p / dp;
			r -= dx;
			if (fabs(dx) < 1e-15)
				break;
		}
		x[n - i] = r;
	}
	for (int i = 0; i < n; i++)
		if (fabs(x[i]) < 1e-15)
			x[i] = 0;
}

static double complex ntf_eval(const ntf_t *t, double complex z) {
	double complex h = 1;
	for (int i = 0; i < t->order; i++)
		h *= (z - t->zeros[i]) / (z - t->poles[i]);
	return h;
}

static void expand(const double complex *roots, int n, double *c) {
	double complex p[MAXORDER + 1] = {1};
	for (int i = 0; i < n; i++) {
		for (int k = i + 1; k >= 1; k--)
			p[k] = p[k] - roots[i] * p[k - 1];
	}
	for (int k = 0; k <= n; k++)
		c[k] = creal(p[k]);
}

/* ntf_design is synthesizeNTF(order, osr, opt=1, hinf, f0=0). */
static void ntf_design(ntf_t *t, int order, double osr, double hinf) {
	if (order < 1 || order > MAXORDER || osr < 2 || hinf <= 1)
		die("%s", "order 1..8, osr >= 2 and hinf > 1 are required");
	t->order = order;
	t->osr = osr;
	t->hinf = hinf;
	double x[MAXORDER];
	legendre_roots(order, x);
	for (int i = 0; i < order; i++)
		t->zeros[i] = cexp(I * (M_PI / osr) * x[i]);
	double xs = pow(0.3, order - 1), fprev = 0, delta = 0;
	int converged = 0;
	for (int itn = 0; itn < 200; itn++) {
		double me2 = -0.5 * pow(xs, 2.0 / order);
		for (int k = 1; k <= order; k++) {
			double w = (2.0 * k - 1.0) * M_PI / order;
			double complex mb2 = 1 + me2 * cexp(I * w);
			double complex p = mb2 - csqrt(mb2 * mb2 - 1);
			if (cabs(p) > 1)
				p = 1 / p;
			t->poles[k - 1] = p;
		}
		double f = creal(ntf_eval(t, -1)) - hinf;
		delta = itn == 0 ? -f / 100 : -f * delta / (f - fprev);
		double xp = xs + delta;
		xs = xp > 0 ? xp : xs * 0.1;
		fprev = f;
		if (fabs(f) < 1e-12 || fabs(delta) < 1e-15) {
			converged = 1;
			break;
		}
	}
	if (!converged)
		die("%s", "the NTF's pole iteration did not converge");
	expand(t->zeros, order, t->n);
	expand(t->poles, order, t->d);
}

/* -------------------------------------------------------- modulator */

typedef struct {
	int order;
	double b[MAXORDER + 1], a[MAXORDER + 1];
	double eh[MAXORDER], gh[MAXORDER];
	double dither;
	rng_t rng;
	double vmax, e2;
	long long steps, ones, run, longest;
	int last;
} sdm_t;

static void sdm_init(sdm_t *m, const ntf_t *t, double dither, uint64_t seed) {
	memset(m, 0, sizeof *m);
	m->order = t->order;
	for (int k = 1; k <= t->order; k++) {
		m->b[k] = t->n[k] - t->d[k];
		m->a[k] = t->d[k];
	}
	m->dither = dither;
	rng_seed(&m->rng, seed);
}

/* sdm_step takes one input sample and returns the output bit, +1 or -1. */
static int sdm_step(sdm_t *m, double u, double *err) {
	double g = 0;
	for (int k = 1; k <= m->order; k++)
		g += m->b[k] * m->eh[k - 1] - m->a[k] * m->gh[k - 1];
	double v = u + g;
	if (!(fabs(v) <= VLIMIT)) {
		fprintf(stderr, "sdm: the loop is unstable (|v| %g past %g)\n", fabs(v), VLIMIT);
		exit(3);
	}
	if (fabs(v) > m->vmax)
		m->vmax = fabs(v);
	double q = v + m->dither * rng_tpdf(&m->rng);
	int y = q >= 0 ? 1 : -1;
	double e = y - v;
	for (int k = m->order - 1; k > 0; k--) {
		m->eh[k] = m->eh[k - 1];
		m->gh[k] = m->gh[k - 1];
	}
	m->eh[0] = e;
	m->gh[0] = g;
	m->e2 += e * e;
	m->steps++;
	if (y > 0)
		m->ones++;
	if (y == m->last)
		m->run++;
	else
		m->run = 1;
	if (m->run > m->longest)
		m->longest = m->run;
	m->last = y;
	if (err)
		*err = e;
	return y;
}

/* -------------------------------------------------------- programme */

typedef struct {
	int channel;
	double freq, start, end, decay_from, decay_db;
} burst_t;

typedef struct {
	const char *name;
	double seconds; /* 0: as long as its frames */
	int nbursts;
	burst_t bursts[4];
} content_t;

static const content_t contents[] = {
    {"main", 8.0, 2, {{0, 1000, 1.0, 3.0, 0, 0}, {1, 2000, 4.0, 6.0, 0, 0}}},
    {"quiet-tail", 6.0, 2, {{0, 1000, 1.0, 3.5, 1.5, 60}, {1, 2000, 1.0, 3.5, 1.5, 60}}},
    {"short", 0, 2, {{0, 1000, 1.0, 1.5, 0, 0}, {1, 2000, 1.75, 2.25, 0, 0}}},
};

#define RAMP 0.010

static const content_t *content_named(const char *name, int *swap) {
	char base[64];
	size_t n = strlen(name);
	*swap = 0;
	if (n >= sizeof base)
		die("unknown content %s", name);
	strcpy(base, name);
	if (n > 3 && strcmp(base + n - 3, "-rl") == 0) {
		base[n - 3] = 0;
		*swap = 1;
	}
	for (size_t i = 0; i < sizeof contents / sizeof contents[0]; i++)
		if (strcmp(contents[i].name, base) == 0)
			return &contents[i];
	die("unknown content %s", name);
	return NULL;
}

/* envelope is a burst's envelope at t seconds: zero outside it, a 10 ms
 * raised cosine in and out, and its decay. */
static double envelope(const burst_t *b, double t) {
	if (t < b->start || t >= b->end)
		return 0;
	double env = 1;
	double in = t - b->start, out = b->end - t;
	if (in < RAMP)
		env = 0.5 * (1 - cos(M_PI * in / RAMP));
	else if (out < RAMP)
		env = 0.5 * (1 - cos(M_PI * out / RAMP));
	if (b->decay_db > 0 && t > b->decay_from)
		env *= pow(10.0, -b->decay_db * (t - b->decay_from) / 20.0);
	return env;
}

/* burst_phase is the phase a burst's sine starts at: the one that leaves the
 * burst no net area. A 1-bit loop delivers its input's area exactly, so an
 * input whose area is not a whole number of bits leaves the loop's running
 * sum off the bit lattice for good, which it keeps up by biasing its bit
 * pairs: the stream then cannot end on its own mean, and every boundary
 * after the burst steps (the quiet tail's decay, sine phase zero, left 7.1
 * bits at DSD64 and failed every boundary search). A burst of whole cycles
 * whose envelope is symmetric has no area at phase zero, and keeps it; a
 * decaying one starts where the area of its envelope times the sine is zero,
 * from the integral of envelope(t) e^(j w (t - start)) on a 1 MHz grid. */
static double burst_phase(const burst_t *b) {
	if (b->decay_db <= 0)
		return 0;
	double complex z = 0;
	const double dt = 1e-6;
	for (double t = b->start; t < b->end; t += dt)
		z += envelope(b, t) * cexp(I * 2 * M_PI * b->freq * (t - b->start)) * dt;
	return -carg(z);
}

/* sample is the content's value on a channel at t seconds, peak 1. */
static double sample(const content_t *c, int channel, double t) {
	static double phase[sizeof contents / sizeof contents[0]][4];
	static int ready;
	if (!ready) {
		for (size_t k = 0; k < sizeof contents / sizeof contents[0]; k++)
			for (int i = 0; i < contents[k].nbursts; i++)
				phase[k][i] = burst_phase(&contents[k].bursts[i]);
		ready = 1;
	}
	size_t k = (size_t)(c - contents);
	double s = 0;
	for (int i = 0; i < c->nbursts; i++) {
		const burst_t *b = &c->bursts[i];
		if (b->channel != channel || t < b->start || t >= b->end)
			continue;
		s += envelope(b, t) * sin(2.0 * M_PI * b->freq * (t - b->start) + phase[k][i]);
	}
	return s;
}

/* ------------------------------------------------------------ output */

static void put_le(unsigned char *p, uint64_t v, int n) {
	for (int i = 0; i < n; i++)
		p[i] = (unsigned char)(v >> (8 * i));
}
static void put_be(unsigned char *p, uint64_t v, int n) {
	for (int i = 0; i < n; i++)
		p[i] = (unsigned char)(v >> (8 * (n - 1 - i)));
}

static FILE *create(const char *path) {
	FILE *f = fopen(path, "wb");
	if (!f) {
		fprintf(stderr, "sdm: %s: %s\n", path, strerror(errno));
		exit(2);
	}
	return f;
}
static void put(FILE *f, const void *p, size_t n, const char *path) {
	if (n && fwrite(p, 1, n, f) != n) {
		fprintf(stderr, "sdm: %s: write failed\n", path);
		exit(2);
	}
}
static void finish(FILE *f, const char *path) {
	if (fflush(f) != 0 || ferror(f) || fclose(f) != 0) {
		fprintf(stderr, "sdm: %s: close failed\n", path);
		exit(2);
	}
}

static unsigned char bitrev(unsigned char b) {
	unsigned char r = 0;
	for (int i = 0; i < 8; i++)
		if (b & (1u << i))
			r |= (unsigned char)(0x80u >> i);
	return r;
}

/* A segment's bits per channel, canonical: bit 7 of byte 0 is the
 * earliest, and the bits after the last are zero. */
typedef struct {
	long long bits;
	unsigned char *ch[2];
} stream_t;

static void write_dsf(const char *path, const stream_t *s, int rate, int lsb, int swap) {
	long long bytes = (s->bits + 7) / 8;
	long long blocks = (bytes + DSF_BLOCK - 1) / DSF_BLOCK;
	long long data = blocks * DSF_BLOCK * 2;
	unsigned char h[28 + 52 + 12];
	memset(h, 0, sizeof h);
	memcpy(h, "DSD ", 4);
	put_le(h + 4, 28, 8);
	put_le(h + 12, (uint64_t)(28 + 52 + 12 + data), 8);
	put_le(h + 20, 0, 8); /* no metadata */
	memcpy(h + 28, "fmt ", 4);
	put_le(h + 32, 52, 8);
	put_le(h + 40, 1, 4); /* format version */
	put_le(h + 44, 0, 4); /* DSD raw */
	put_le(h + 48, 2, 4); /* channel type: stereo (FL, FR) */
	put_le(h + 52, 2, 4);
	put_le(h + 56, (uint64_t)rate, 4);
	put_le(h + 60, lsb ? 1 : 8, 4);
	put_le(h + 64, (uint64_t)s->bits, 8);
	put_le(h + 72, DSF_BLOCK, 4);
	put_le(h + 76, 0, 4);
	memcpy(h + 80, "data", 4);
	put_le(h + 84, (uint64_t)(12 + data), 8);
	FILE *f = create(path);
	put(f, h, sizeof h, path);
	unsigned char block[DSF_BLOCK];
	for (long long b = 0; b < blocks; b++) {
		for (int c = 0; c < 2; c++) {
			const unsigned char *src = s->ch[swap ? 1 - c : c];
			memset(block, 0, sizeof block);
			for (long long i = 0; i < DSF_BLOCK; i++) {
				long long at = b * DSF_BLOCK + i;
				if (at >= bytes)
					break;
				block[i] = lsb ? bitrev(src[at]) : src[at];
			}
			put(f, block, sizeof block, path);
		}
	}
	finish(f, path);
}

static void dff_chunk(unsigned char *p, const char *id, uint64_t size) {
	memcpy(p, id, 4);
	put_be(p + 4, size, 8);
}

static void write_dff(const char *path, const stream_t *s, int rate, int chnl_rl) {
	if (s->bits % 8)
		die("%s: a DSDIFF file holds whole bytes per channel", path);
	long long bytes = s->bits / 8;
	/* PROP: "SND " + FS (12+4) + CHNL (12+10) + CMPR (12+19, padded). */
	static const char cname[] = "not compressed";
	uint64_t cmpr = 4 + 1 + (sizeof cname - 1);
	uint64_t prop = 4 + (12 + 4) + (12 + 10) + (12 + cmpr + (cmpr & 1));
	uint64_t sound = (uint64_t)bytes * 2;
	uint64_t form = 4 + (12 + 4) + (12 + prop) + (12 + sound + (sound & 1));
	unsigned char h[16 + 16 + 12 + 4 + 16 + 22 + 32 + 12];
	unsigned char *p = h;
	memset(h, 0, sizeof h);
	dff_chunk(p, "FRM8", form);
	memcpy(p + 12, "DSD ", 4);
	p += 16;
	dff_chunk(p, "FVER", 4);
	put_be(p + 12, 0x01050000, 4);
	p += 16;
	dff_chunk(p, "PROP", prop);
	memcpy(p + 12, "SND ", 4);
	p += 16;
	dff_chunk(p, "FS  ", 4);
	put_be(p + 12, (uint64_t)rate, 4);
	p += 16;
	dff_chunk(p, "CHNL", 10);
	put_be(p + 12, 2, 2);
	memcpy(p + 14, chnl_rl ? "SRGTSLFT" : "SLFTSRGT", 8);
	p += 22;
	dff_chunk(p, "CMPR", cmpr);
	memcpy(p + 12, "DSD ", 4);
	p[16] = (unsigned char)(sizeof cname - 1);
	memcpy(p + 17, cname, sizeof cname - 1);
	p += 12 + cmpr + (cmpr & 1);
	dff_chunk(p, "DSD ", sound);
	p += 12;
	if ((size_t)(p - h) != sizeof h)
		die("%s", "DSDIFF header layout");
	FILE *f = create(path);
	put(f, h, sizeof h, path);
	unsigned char frame[2 * 4096];
	for (long long at = 0; at < bytes; at += 4096) {
		long long n = bytes - at < 4096 ? bytes - at : 4096;
		for (long long i = 0; i < n; i++) {
			frame[2 * i] = s->ch[0][at + i];
			frame[2 * i + 1] = s->ch[1][at + i];
		}
		put(f, frame, (size_t)(2 * n), path);
	}
	finish(f, path);
}

/* ------------------------------------------------------------ args */

typedef struct {
	int order;
	double osr, hinf, dither, peak;
} design_args;

static double number(const char *s, const char *what) {
	char *end;
	errno = 0;
	double v = strtod(s, &end);
	if (errno || end == s || *end)
		die("bad %s", what);
	return v;
}

static long long integer(const char *s, const char *what) {
	char *end;
	errno = 0;
	long long v = strtoll(s, &end, 10);
	if (errno || end == s || *end || v < 0)
		die("bad %s", what);
	return v;
}

static int design_flag(design_args *d, int argc, char **argv, int *i) {
	const char *f = argv[*i];
	if (*i + 1 >= argc)
		return 0;
	if (strcmp(f, "--order") == 0)
		d->order = (int)integer(argv[++*i], "--order");
	else if (strcmp(f, "--osr") == 0)
		d->osr = number(argv[++*i], "--osr");
	else if (strcmp(f, "--hinf") == 0)
		d->hinf = number(argv[++*i], "--hinf");
	else if (strcmp(f, "--dither") == 0)
		d->dither = number(argv[++*i], "--dither");
	else if (strcmp(f, "--peak-dbfs") == 0)
		d->peak = number(argv[++*i], "--peak-dbfs");
	else
		return 0;
	return 1;
}

static void print_design(const ntf_t *t, const design_args *d) {
	printf("{\"generator\":\"sdm.c\",\"version\":\"%s\",\"order\":%d,\"osr\":%g,\"hinf\":%g,\"dither\":%g,", SDM_VERSION, t->order, t->osr, t->hinf, d->dither);
	printf("\"zeros\":[");
	for (int i = 0; i < t->order; i++)
		printf("%s[%.17g,%.17g]", i ? "," : "", creal(t->zeros[i]), cimag(t->zeros[i]));
	printf("],\"poles\":[");
	for (int i = 0; i < t->order; i++)
		printf("%s[%.17g,%.17g]", i ? "," : "", creal(t->poles[i]), cimag(t->poles[i]));
	printf("],\"n\":[");
	for (int i = 0; i <= t->order; i++)
		printf("%s%.17g", i ? "," : "", t->n[i]);
	printf("],\"d\":[");
	for (int i = 0; i <= t->order; i++)
		printf("%s%.17g", i ? "," : "", t->d[i]);
	printf("]}\n");
}

/* ------------------------------------------------------------- dsd */

static double bessel_i0(double x);

/* finite_or makes a level JSON can hold: no click measured is -999. */
static double finite_or(double v) { return isfinite(v) ? v : -999; }

#define BOUNDARY_EPS 0.02
#define BOUNDARY_TRIALS 2000
#define BOUNDARY_CLICK -100.0

typedef struct {
	const content_t *c;
	int swap;
	long long frames;
} segment_t;

/* boundary_t is what the search at one boundary chose: whether it searched
 * (a segment too short for its windows is not), its trials, and how far the
 * running sum there stood off its mean before (end) and after (start). */
typedef struct {
	int searched, trials;
	double end, start, endClick, startClick;
} boundary_t;

static uint64_t trial_seed(uint64_t base, int boundary, int trial) {
	uint64_t x = base ^ ((uint64_t)boundary << 40) ^ ((uint64_t)trial << 8);
	return splitmix64(&x);
}

static double input_at(const segment_t *seg, const long long *P, int nseg, long long n, int c, double amp, long long rate) {
	int s = 0;
	while (s + 1 < nseg && n >= P[s + 1])
		s++;
	int src = seg[s].swap ? 1 - c : c;
	return amp * sample(seg[s].c, src, (double)(n - P[s]) / (double)rate);
}

static void put_bit(stream_t *out, const long long *P, int nseg, int c, long long n, int y) {
	int s = 0;
	while (s + 1 < nseg && n >= P[s + 1])
		s++;
	long long k = n - P[s];
	unsigned char mask = (unsigned char)(0x80u >> (k & 7));
	if (y > 0)
		out[s].ch[c][k >> 3] |= mask;
	else
		out[s].ch[c][k >> 3] &= (unsigned char)~mask;
}

/* probe_t measures what a boundary leaves in the audio band: the bits
 * either side of it, the far side 0x69 silence (a player's, or a file that
 * starts cleanly, which leaves the same), low-passed at 20 kHz by a
 * Kaiser-windowed sinc (beta 5.65, some 60 dB down from 25 kHz at DSD64).
 * Its level is the energy of that low-passed signal in dB SA-CD over 10 ms,
 * the window the verifier measures a boundary in. */
typedef struct {
	int half, step;
	double *h;
} probe_t;

static void probe_init(probe_t *p, long long rate) {
	p->half = (int)(rate / 2756); /* 1024 taps either side at DSD64 */
	p->step = p->half / 64;
	p->h = malloc(sizeof(double) * (size_t)(2 * p->half + 1));
	if (!p->h)
		die("%s", "out of memory");
	double fc = 20000.0 / (double)rate, sum = 0, beta = 5.65, i0b = bessel_i0(beta);
	for (int i = 0; i <= 2 * p->half; i++) {
		double x = i - p->half, r = x / p->half;
		double v = x == 0 ? 2 * fc : sin(2 * M_PI * fc * x) / (M_PI * x);
		p->h[i] = v * bessel_i0(beta * sqrt(fmax(0, 1 - r * r))) / i0b;
		sum += p->h[i];
	}
	for (int i = 0; i <= 2 * p->half; i++)
		p->h[i] /= sum;
}

/* probe_level: x holds 4 * half bits as +-1, the boundary at 2 * half. */
static double probe_level(const probe_t *p, const signed char *x, long long rate) {
	int H = p->half;
	double e = 0;
	for (int t = H; t < 3 * H; t += p->step) {
		double v = 0;
		for (int j = -H; j <= H; j++)
			v += p->h[j + H] * x[t + j];
		e += v * v * p->step;
	}
	return 10 * log10(e / (0.01 * (double)rate) / 0.125 + 1e-300);
}

/* silence_bit is 0x69's bit at stream position n, as +-1: the pattern's bit
 * for n mod 8, positions before the stream's start included. */
static int silence_bit(long long n) { return ((0x69 >> (7 - (int)(((n % 8) + 8) % 8))) & 1) ? 1 : -1; }

/* window runs the loop over [from, to), writing the bits, and measures
 * boundary b: end and start are how far the running sum there stands off
 * its mean over the w positions before and after; endClick what the bits
 * before b leave against 0x69 after it, startClick what 0x69 before b
 * leaves against the bits after (each only when the window holds them). */
typedef struct {
	double end, start, endClick, startClick;
} measure_t;

static measure_t window(sdm_t *m, const segment_t *seg, const long long *P, int nseg, int c, double amp, long long rate, stream_t *out,
                        long long from, long long to, long long b, long long w, const probe_t *pr, signed char *buf) {
	double r = 0, before = 0, after = 0, atb = 0;
	long long H2 = 2LL * pr->half;
	for (long long n = from; n < to; n++) {
		if (n == b)
			atb = r;
		if (n >= b - w && n < b)
			before += r;
		if (n > b && n <= b + w)
			after += r;
		int y = sdm_step(m, input_at(seg, P, nseg, n, c, amp, rate), NULL);
		put_bit(out, P, nseg, c, n, y);
		if (n >= b - H2 && n < b + H2)
			buf[n - (b - H2)] = (signed char)y;
		r += y;
	}
	if (to == b)
		atb = r;
	if (to > b && to <= b + w)
		after += r;
	measure_t me = {atb - before / (double)w, after / (double)w - atb, -INFINITY, -INFINITY};
	signed char *x = buf + 4 * H2;
	if (from <= b - H2) {
		for (long long i = 0; i < H2; i++) {
			x[i] = buf[i];
			x[H2 + i] = (signed char)silence_bit(b + i);
		}
		me.endClick = probe_level(pr, x, rate);
	}
	if (to >= b + H2) {
		for (long long i = 0; i < H2; i++) {
			x[i] = (signed char)silence_bit(b - H2 + i);
			x[H2 + i] = buf[H2 + i];
		}
		me.startClick = probe_level(pr, x, rate);
	}
	return me;
}

/* search_stream modulates channel c's whole stream, segment after segment
 * in one loop, choosing the dither at each boundary: at the stream's start
 * the first seed, and at each later boundary at an even position the
 * re-dithering of the 10 ms before it, whose running sum stands within
 * BOUNDARY_EPS of its mean either side and whose clicks are the least of
 * BOUNDARY_TRIALS, or the first under BOUNDARY_CLICK. Only an even position
 * is searched: the loop's silence is bit pairs of one 1 and one 0, so at an
 * odd position the sum always stands one off its mean, whatever the dither;
 * a join there is clean only as the continuation the loop made. */
static void search_stream(sdm_t *m, const ntf_t *t, double dither, uint64_t base, const segment_t *seg, const long long *P, int nseg, int c,
                          double amp, long long rate, stream_t *out, boundary_t *bounds) {
	probe_t pr;
	probe_init(&pr, rate);
	const long long w = rate / 2000, tail = rate / 100, H2 = 2LL * pr.half;
	signed char *buf = malloc((size_t)(6 * H2));
	if (!buf)
		die("%s", "out of memory");
	memset(bounds, 0, sizeof(boundary_t) * (size_t)(nseg + 1));
	for (int b = 0; b <= nseg; b++)
		bounds[b].endClick = bounds[b].startClick = -INFINITY;
	double best = INFINITY;
	int pick = 0;
	long long first = w > H2 ? w : H2;
	if (P[1] >= first) {
		for (int tr = 0; tr < BOUNDARY_TRIALS; tr++) {
			sdm_init(m, t, dither, trial_seed(base, 0, tr));
			measure_t me = window(m, seg, P, nseg, c, amp, rate, out, 0, first, 0, w, &pr, buf);
			double score = (fabs(me.start) <= BOUNDARY_EPS ? 0 : 1000) + me.startClick;
			if (score < best) {
				best = score;
				pick = tr;
				bounds[0].start = me.start;
				bounds[0].startClick = me.startClick;
			}
			if (fabs(me.start) <= BOUNDARY_EPS && me.startClick <= BOUNDARY_CLICK)
				break;
		}
		bounds[0].searched = 1;
		bounds[0].trials = pick + 1;
	}
	sdm_init(m, t, dither, trial_seed(base, 0, pick));
	long long pos = 0;
	for (int b = 1; b <= nseg; b++) {
		long long at = P[b], from = at - tail, to = b < nseg ? at + first : at;
		if (from < pos || (b < nseg && to > P[b + 1]))
			continue; /* too short to search: the next window takes it */
		for (; pos < from; pos++)
			put_bit(out, P, nseg, c, pos, sdm_step(m, input_at(seg, P, nseg, pos, c, amp, rate), NULL));
		sdm_t saved = *m;
		best = INFINITY;
		pick = 0;
		int trials = at % 2 == 0 ? BOUNDARY_TRIALS : 1;
		for (int tr = 0; tr < trials; tr++) {
			*m = saved;
			rng_seed(&m->rng, trial_seed(base, b, tr));
			measure_t me = window(m, seg, P, nseg, c, amp, rate, out, from, to, at, w, &pr, buf);
			int sumok = fabs(me.end) <= BOUNDARY_EPS && (b == nseg || fabs(me.start) <= BOUNDARY_EPS);
			double click = b < nseg ? fmax(me.endClick, me.startClick) : me.endClick;
			double score = (sumok ? 0 : 1000) + click;
			if (score < best) {
				best = score;
				pick = tr;
			}
			if (sumok && click <= BOUNDARY_CLICK)
				break;
		}
		*m = saved;
		rng_seed(&m->rng, trial_seed(base, b, pick));
		measure_t me = window(m, seg, P, nseg, c, amp, rate, out, from, to, at, w, &pr, buf);
		bounds[b].searched = at % 2 == 0;
		bounds[b].trials = pick + 1;
		bounds[b].end = me.end;
		bounds[b].endClick = me.endClick;
		bounds[b].start = b < nseg ? me.start : 0;
		bounds[b].startClick = b < nseg ? me.startClick : -INFINITY;
		pos = to;
	}
	for (; pos < P[nseg]; pos++)
		put_bit(out, P, nseg, c, pos, sdm_step(m, input_at(seg, P, nseg, pos, c, amp, rate), NULL));
	free(buf);
	free(pr.h);
}

static int dsd_main(int argc, char **argv) {
	design_args da = DESIGN_DEFAULTS;
	long long rate = 0;
	uint64_t seed = 0;
	int seeded = 0, nseg = 0, nw = 0;
	segment_t seg[8];
	const char *writes[32];
	for (int i = 2; i < argc; i++) {
		if (design_flag(&da, argc, argv, &i))
			continue;
		if (i + 1 >= argc)
			die("%s needs a value", argv[i]);
		if (strcmp(argv[i], "--rate") == 0)
			rate = integer(argv[++i], "--rate");
		else if (strcmp(argv[i], "--seed") == 0) {
			seed = (uint64_t)integer(argv[++i], "--seed");
			seeded = 1;
		} else if (strcmp(argv[i], "--content") == 0) {
			char name[64];
			const char *arg = argv[++i], *colon = strchr(arg, ':');
			if (!colon || (size_t)(colon - arg) >= sizeof name || nseg == 8)
				die("bad --content %s", arg);
			memcpy(name, arg, (size_t)(colon - arg));
			name[colon - arg] = 0;
			seg[nseg].c = content_named(name, &seg[nseg].swap);
			seg[nseg].frames = integer(colon + 1, "--content frames");
			if (seg[nseg].frames <= 0)
				die("bad --content %s", arg);
			nseg++;
		} else if (strcmp(argv[i], "--write") == 0) {
			if (nw == 32)
				die("%s", "too many --write");
			writes[nw++] = argv[++i];
		} else
			die("unknown flag %s", argv[i]);
	}
	if (rate != 2822400 && rate != 5644800 && rate != 11289600 && rate != 22579200)
		die("%s", "--rate must be a DSD rate (2822400, 5644800, 11289600 or 22579200)");
	if (!seeded || nseg == 0 || nw == 0)
		die("%s", "--seed, --content and --write are required");
	ntf_t t;
	ntf_design(&t, da.order, da.osr, da.hinf);
	double amp = 0.5 * pow(10.0, da.peak / 20.0);
	if (amp > 0.25)
		die("%s", "--peak-dbfs above -6 is refused: these are listening assets");
	/* P[s] is segment s's first bit in the loop's stream, P[nseg] its end. */
	long long P[9];
	P[0] = 0;
	for (int s = 0; s < nseg; s++)
		P[s + 1] = P[s] + seg[s].frames;
	stream_t out[8];
	for (int s = 0; s < nseg; s++) {
		out[s].bits = seg[s].frames;
		for (int c = 0; c < 2; c++) {
			out[s].ch[c] = calloc((size_t)((seg[s].frames + 7) / 8), 1);
			if (!out[s].ch[c])
				die("%s", "out of memory");
		}
		if (seg[s].c->seconds > 0 && seg[s].frames != (long long)llround(seg[s].c->seconds * (double)rate))
			fprintf(stderr, "sdm: note: %s holds %lld frames, its programme %g s\n", seg[s].c->name, seg[s].frames, seg[s].c->seconds);
	}
	/* A file boundary leaves no step. Joined at a boundary, two streams
	 * click by as much as the running sum of their bits stands off its own
	 * local mean there, on either side: a 1-bit loop keeps that sum bounded,
	 * not still, so a file cut where it happens to be leaves a step, a click
	 * of up to -54 dB SA-CD when followed by another file or by a player's
	 * 0x69 silence. So at each boundary at an even position the loop is
	 * re-dithered, from 10 ms before it, until the sum there is within
	 * BOUNDARY_EPS of its mean over the 0.5 ms before (a file's end) and the
	 * 0.5 ms after (a file's start) and the bits either side leave at most
	 * BOUNDARY_CLICK in the audio band against 0x69, or else the least of
	 * BOUNDARY_TRIALS re-ditherings is kept; at the stream's start its first
	 * dither seed is chosen the same way. An odd position cannot be
	 * (search_stream says why). Each boundary is reported: searched or not,
	 * its trials, both offsets and both clicks. */
	boundary_t bounds[2][9];
	sdm_t m[2];
	for (int c = 0; c < 2; c++) {
		search_stream(&m[c], &t, da.dither, seed * 2 + (uint64_t)c, seg, P, nseg, c, amp, rate, out, bounds[c]);
	}
	for (int w = 0; w < nw; w++) {
		char kind[32];
		const char *arg = writes[w], *c1 = strchr(arg, ':');
		const char *c2 = c1 ? strchr(c1 + 1, ':') : NULL;
		if (!c1 || !c2 || (size_t)(c1 - arg) >= sizeof kind)
			die("bad --write %s", arg);
		memcpy(kind, arg, (size_t)(c1 - arg));
		kind[c1 - arg] = 0;
		char segbuf[16];
		if ((size_t)(c2 - c1 - 1) >= sizeof segbuf)
			die("bad --write %s", arg);
		memcpy(segbuf, c1 + 1, (size_t)(c2 - c1 - 1));
		segbuf[c2 - c1 - 1] = 0;
		/* SEGMENT is an index, or indexes joined by + for their
		 * concatenation: the stream the loop made, as one file. */
		stream_t joined = {0, {NULL, NULL}};
		const stream_t *src = NULL;
		int parts = 0;
		for (char *tok = strtok(segbuf, "+"); tok; tok = strtok(NULL, "+")) {
			long long si = integer(tok, "--write segment");
			if (si >= nseg)
				die("bad --write segment %s", arg);
			src = &out[si];
			parts++;
			long long at = joined.bits;
			joined.bits += src->bits;
			for (int c = 0; c < 2; c++) {
				joined.ch[c] = realloc(joined.ch[c], (size_t)((joined.bits + 7) / 8));
				if (!joined.ch[c])
					die("%s", "out of memory");
				for (long long k = (at + 7) / 8; k < (joined.bits + 7) / 8; k++)
					joined.ch[c][k] = 0;
				if (at % 8 && at / 8 < (joined.bits + 7) / 8)
					joined.ch[c][at / 8] &= (unsigned char)(0xFF00u >> (at % 8));
				for (long long k = 0; k < src->bits; k++)
					if (src->ch[c][k >> 3] & (0x80u >> (k & 7)))
						joined.ch[c][(at + k) >> 3] |= (unsigned char)(0x80u >> ((at + k) & 7));
			}
		}
		if (parts == 0)
			die("bad --write segment %s", arg);
		const stream_t *s = parts == 1 ? src : &joined;
		const char *path = c2 + 1;
		if (strcmp(kind, "dsf-lsb") == 0)
			write_dsf(path, s, (int)rate, 1, 0);
		else if (strcmp(kind, "dsf-msb") == 0)
			write_dsf(path, s, (int)rate, 0, 0);
		else if (strcmp(kind, "dsf-lsb-rl") == 0)
			write_dsf(path, s, (int)rate, 1, 1);
		else if (strcmp(kind, "dff") == 0)
			write_dff(path, s, (int)rate, 0);
		else if (strcmp(kind, "dff-chnl-rl") == 0)
			write_dff(path, s, (int)rate, 1);
		else
			die("unknown --write kind %s", kind);
		free(joined.ch[0]);
		free(joined.ch[1]);
	}
	printf("{\"rate\":%lld,\"seed\":%llu,\"amplitude\":%.17g,\"segments\":[", rate, (unsigned long long)seed, amp);
	for (int s = 0; s < nseg; s++)
		printf("%s{\"content\":\"%s%s\",\"bits\":%lld}", s ? "," : "", seg[s].c->name, seg[s].swap ? "-rl" : "", out[s].bits);
	printf("],\"vmax\":[%.6g,%.6g],\"longestRun\":[%lld,%lld],\"density\":[%.9f,%.9f],\"errorPower\":[%.6g,%.6g],\"boundaries\":[",
	       m[0].vmax, m[1].vmax, m[0].longest, m[1].longest,
	       (double)m[0].ones / (double)m[0].steps, (double)m[1].ones / (double)m[1].steps,
	       m[0].e2 / (double)m[0].steps, m[1].e2 / (double)m[1].steps);
	for (int b = 0; b <= nseg; b++)
		printf("%s{\"at\":%lld,\"searched\":[%d,%d],\"trials\":[%d,%d],\"end\":[%.4f,%.4f],\"start\":[%.4f,%.4f],\"endClick\":[%.1f,%.1f],\"startClick\":[%.1f,%.1f]}",
		       b ? "," : "", P[b], bounds[0][b].searched, bounds[1][b].searched, bounds[0][b].trials, bounds[1][b].trials,
		       bounds[0][b].end, bounds[1][b].end, bounds[0][b].start, bounds[1][b].start,
		       finite_or(bounds[0][b].endClick), finite_or(bounds[1][b].endClick), finite_or(bounds[0][b].startClick), finite_or(bounds[1][b].startClick));
	printf("]}\n");
	return 0;
}

/* ------------------------------------------------------------- wav */

static int wav_main(int argc, char **argv) {
	design_args da = DESIGN_DEFAULTS;
	long long rate = 0, bits = 0, frames = 0;
	uint64_t seed = 0;
	int seeded = 0, rl = 0;
	const char *path = NULL;
	const content_t *c = NULL;
	int swap = 0;
	for (int i = 2; i < argc; i++) {
		if (strcmp(argv[i], "--rl") == 0) {
			rl = 1;
			continue;
		}
		if (design_flag(&da, argc, argv, &i))
			continue;
		if (i + 1 >= argc)
			die("%s needs a value", argv[i]);
		if (strcmp(argv[i], "--rate") == 0)
			rate = integer(argv[++i], "--rate");
		else if (strcmp(argv[i], "--bits") == 0)
			bits = integer(argv[++i], "--bits");
		else if (strcmp(argv[i], "--seed") == 0) {
			seed = (uint64_t)integer(argv[++i], "--seed");
			seeded = 1;
		} else if (strcmp(argv[i], "--out") == 0)
			path = argv[++i];
		else if (strcmp(argv[i], "--content") == 0) {
			char name[64];
			const char *arg = argv[++i], *colon = strchr(arg, ':');
			if (!colon || (size_t)(colon - arg) >= sizeof name)
				die("bad --content %s", arg);
			memcpy(name, arg, (size_t)(colon - arg));
			name[colon - arg] = 0;
			c = content_named(name, &swap);
			frames = integer(colon + 1, "--content frames");
		} else
			die("unknown flag %s", argv[i]);
	}
	if (rl)
		swap = !swap;
	if (rate < 8000 || rate > 768000 || (bits != 24 && bits != 32) || !seeded || !path || !c || frames <= 0)
		die("%s", "wav needs --rate 8000..768000, --bits 24|32, --seed, --content NAME:FRAMES and --out");
	double amp = pow(10.0, da.peak / 20.0);
	if (amp > 0.5)
		die("%s", "--peak-dbfs above -6 is refused: these are listening assets");
	double full = ldexp(1.0, (int)bits - 1);
	long long maxv = (1LL << (bits - 1)) - 1, minv = -(1LL << (bits - 1));
	int width = (int)bits / 8;
	uint64_t data = (uint64_t)frames * 2 * (uint64_t)width;
	if (data > 0xFFFFFFFFULL - 68)
		die("%s", "a RIFF file holds less than 4 GiB");
	unsigned char h[68];
	memset(h, 0, sizeof h);
	memcpy(h, "RIFF", 4);
	put_le(h + 4, 60 + data, 4);
	memcpy(h + 8, "WAVE", 4);
	memcpy(h + 12, "fmt ", 4);
	put_le(h + 16, 40, 4);
	put_le(h + 20, 0xFFFE, 2); /* WAVE_FORMAT_EXTENSIBLE */
	put_le(h + 22, 2, 2);
	put_le(h + 24, (uint64_t)rate, 4);
	put_le(h + 28, (uint64_t)rate * 2 * (uint64_t)width, 4);
	put_le(h + 32, (uint64_t)(2 * width), 2);
	put_le(h + 34, (uint64_t)bits, 2);
	put_le(h + 36, 22, 2);
	put_le(h + 38, (uint64_t)bits, 2); /* valid bits */
	put_le(h + 40, 3, 4);              /* FL | FR */
	static const unsigned char pcm[16] = {1, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xAA, 0, 0x38, 0x9B, 0x71};
	memcpy(h + 44, pcm, 16);
	memcpy(h + 60, "data", 4);
	put_le(h + 64, data, 4);
	FILE *f = create(path);
	put(f, h, sizeof h, path);
	rng_t r;
	rng_seed(&r, seed);
	unsigned char buf[8 * 4096];
	long long peak = 0;
	for (long long at = 0; at < frames; at += 4096) {
		long long n = frames - at < 4096 ? frames - at : 4096;
		unsigned char *p = buf;
		for (long long k = 0; k < n; k++) {
			double tsec = (double)(at + k) / (double)rate;
			for (int ch = 0; ch < 2; ch++) {
				double x = amp * full * sample(c, swap ? 1 - ch : ch, tsec) + rng_tpdf(&r);
				long long v = llround(x);
				if (v > maxv)
					v = maxv;
				if (v < minv)
					v = minv;
				if (llabs(v) > peak)
					peak = llabs(v);
				put_le(p, (uint64_t)v, width);
				p += width;
			}
		}
		put(f, buf, (size_t)(p - buf), path);
	}
	finish(f, path);
	printf("{\"rate\":%lld,\"bits\":%lld,\"frames\":%lld,\"content\":\"%s%s\",\"peak\":%lld,\"peakDBFS\":%.4f}\n", rate, bits, frames, c->name, swap ? "-rl" : "", peak,
	       20 * log10((double)peak / full));
	return 0;
}

/* -------------------------------------------------------- self-test */

static void fft(double complex *x, int n) {
	for (int i = 1, j = 0; i < n; i++) {
		int bit = n >> 1;
		for (; j & bit; bit >>= 1)
			j ^= bit;
		j ^= bit;
		if (i < j) {
			double complex t = x[i];
			x[i] = x[j];
			x[j] = t;
		}
	}
	for (int len = 2; len <= n; len <<= 1) {
		double complex wl = cexp(-2 * M_PI * I / len);
		for (int i = 0; i < n; i += len) {
			double complex w = 1;
			for (int k = 0; k < len / 2; k++) {
				double complex u = x[i + k], v = x[i + k + len / 2] * w;
				x[i + k] = u + v;
				x[i + k + len / 2] = u - v;
				w *= wl;
			}
		}
	}
}

static double bessel_i0(double x) {
	double s = 1, t = 1;
	for (int k = 1; k < 200; k++) {
		t *= (x / (2 * k)) * (x / (2 * k));
		s += t;
		if (t < 1e-17 * s)
			break;
	}
	return s;
}

/* psd is a Welch estimate of d's one-sided power per bin, Kaiser windowed
 * (beta 20: sidelobes far below the in-band noise), summing to its mean
 * square. */
static void psd(const double *d, long long len, int nfft, double *p) {
	double *w = malloc(sizeof(double) * (size_t)nfft);
	double complex *x = malloc(sizeof(double complex) * (size_t)nfft);
	if (!w || !x)
		die("%s", "out of memory");
	double wsum = 0, beta = 20, i0b = bessel_i0(beta);
	for (int i = 0; i < nfft; i++) {
		double r = 2.0 * i / (nfft - 1) - 1;
		w[i] = bessel_i0(beta * sqrt(1 - r * r)) / i0b;
		wsum += w[i] * w[i];
	}
	for (int k = 0; k <= nfft / 2; k++)
		p[k] = 0;
	int segs = 0;
	for (long long at = 0; at + nfft <= len; at += nfft / 2) {
		for (int i = 0; i < nfft; i++)
			x[i] = d[at + i] * w[i];
		fft(x, nfft);
		for (int k = 0; k <= nfft / 2; k++) {
			double m = creal(x[k]) * creal(x[k]) + cimag(x[k]) * cimag(x[k]);
			p[k] += (k == 0 || k == nfft / 2 ? 1 : 2) * m / (wsum * nfft);
		}
		segs++;
	}
	for (int k = 0; k <= nfft / 2; k++)
		p[k] /= segs;
	free(w);
	free(x);
}

static double band(const double *p, int nfft, double rate, double f1, double f2) {
	double s = 0;
	for (int k = 1; k < nfft / 2; k++) {
		double f = (double)k * rate / nfft;
		if (f >= f1 && f < f2)
			s += p[k];
	}
	return s;
}

static double predicted(const ntf_t *t, double e2, int nfft, double rate, double f1, double f2) {
	double s = 0;
	for (int k = 1; k < nfft / 2; k++) {
		double f = (double)k * rate / nfft;
		if (f >= f1 && f < f2) {
			double h = cabs(ntf_eval(t, cexp(I * 2 * M_PI * f / rate)));
			s += 2 * e2 * h * h / nfft;
		}
	}
	return s;
}

static int cmp_double(const void *a, const void *b) {
	double x = *(const double *)a, y = *(const double *)b;
	return (x > y) - (x < y);
}

/* tonality is the largest ratio of a bin to the median of its 129-bin
 * neighbourhood between f1 and f2, in dB: a limit cycle stands out of the
 * shaped noise, which is smooth there. */
static double tonality(const double *p, int nfft, double rate, double f1, double f2, double *at) {
	double worst = -1e9, buf[129];
	for (int k = 64; k < nfft / 2 - 64; k++) {
		double f = (double)k * rate / nfft;
		if (f < f1 || f >= f2)
			continue;
		for (int j = -64; j <= 64; j++)
			buf[j + 64] = p[k + j];
		qsort(buf, 129, sizeof buf[0], cmp_double);
		double r = 10 * log10(p[k] / buf[64]);
		if (r > worst) {
			worst = r;
			*at = f;
		}
	}
	return worst;
}

static int failures;

static void check(int ok, const char *what) {
	printf("  %s %s\n", ok ? "ok  " : "FAIL", what);
	if (!ok)
		failures++;
}

static double db(double x) { return 10 * log10(x); }

/* selftest_run modulates len samples of a 1 kHz sine of amplitude amp (0:
 * silence) at DSD64 and checks the loop against its design. */
static void selftest_run(const ntf_t *t, double dither, double amp, long long len, int strict) {
	const double rate = 2822400;
	sdm_t m;
	sdm_init(&m, t, dither, 12345);
	double *d = malloc(sizeof(double) * (size_t)len);
	signed char *y = malloc((size_t)len);
	if (!d || !y)
		die("%s", "out of memory");
	for (long long k = 0; k < len; k++) {
		double u = amp * sin(2 * M_PI * 1000.0 * (double)k / rate);
		y[k] = (signed char)sdm_step(&m, u, NULL);
		d[k] = y[k] - u;
	}
	double e2 = m.e2 / (double)m.steps;
	printf("amplitude %g (%.2f dB SA-CD): |v| max %.4f, error power %.4f, density %.6f, longest run %lld bits\n", amp,
	       amp > 0 ? 20 * log10(amp / 0.5) : -INFINITY, m.vmax, e2, (double)m.ones / (double)m.steps, m.longest);
	if (!strict) {
		check(m.vmax < VLIMIT, "the loop stays bounded at a level far above the assets'");
		free(d);
		free(y);
		return;
	}
	check(m.vmax < 3.0, "|v| stays under 3");
	check(m.longest <= 16, "no run of more than 16 equal bits (no constant bytes)");
	int nfft = 1 << 18;
	double *p = malloc(sizeof(double) * (size_t)(nfft / 2 + 1));
	if (!p)
		die("%s", "out of memory");
	psd(d, len, nfft, p);
	/* y - u is H applied to e exactly, so a band's power is |H|^2 over e's
	 * own spectrum there. The prediction takes e white at its measured
	 * power; a 1-bit loop's e has less power in band than that, and a limit
	 * cycle or an overload puts more there. Each band must stay within its
	 * bound and at most 1 dB over the white prediction. */
	struct {
		const char *name;
		double f1, f2, bound;
	} bands[] = {{"in-band 20 Hz-20 kHz", 20, 20000, -100}, {"20-100 kHz", 20000, 100000, -18}, {"100 kHz-1.4 MHz", 100000, 1400000, 9.5}};
	char what[240];
	for (size_t b = 0; b < sizeof bands / sizeof bands[0]; b++) {
		double meas = db(band(p, nfft, rate, bands[b].f1, bands[b].f2)) + 9.0309;
		double pred = db(predicted(t, e2, nfft, rate, bands[b].f1, bands[b].f2)) + 9.0309;
		snprintf(what, sizeof what, "%s: %.2f dB SA-CD measured (bound %g), %.2f for white error", bands[b].name, meas, bands[b].bound, pred);
		check(meas <= bands[b].bound && meas <= pred + 1, what);
	}
	/* Third-octave bands across the audio band: an in-band idle tone stands
	 * over the white prediction of its band, whose notches leakage cannot
	 * fill at this window's sidelobes. */
	double worst = -1e9, worstf = 0;
	for (double fc = 63.0; fc < 17000; fc *= pow(2.0, 1.0 / 3.0)) {
		double lo = fc * pow(2.0, -1.0 / 6.0), hi = fc * pow(2.0, 1.0 / 6.0);
		double r = db(band(p, nfft, rate, lo, hi)) - db(predicted(t, e2, nfft, rate, lo, hi));
		if (r > worst) {
			worst = r;
			worstf = fc;
		}
	}
	snprintf(what, sizeof what, "no idle tone in band: every third-octave band is at most %.2f dB over white error (worst at %.0f Hz)", worst, worstf);
	check(worst <= 3, what);
	double at = 0;
	double tone = tonality(p, nfft, rate, 25000, 1300000, &at);
	snprintf(what, sizeof what, "no line out of band: the largest bin is %.1f dB over its neighbours' median (at %.0f Hz)", tone, at);
	check(tone < 10, what);
	if (amp == 0) {
		double mean = 2 * (double)m.ones / (double)m.steps - 1;
		snprintf(what, sizeof what, "silence has no DC: mean %.2e over %lld bits", mean, m.steps);
		check(fabs(mean) < 1e-3, what);
	} else {
		/* 14112 samples are five cycles of 1 kHz at DSD64. */
		long long n = 14112LL * (len / 14112);
		double complex acc = 0;
		for (long long k = 0; k < n; k++)
			acc += y[k] * cexp(-I * 2 * M_PI * 1000.0 * (double)k / rate);
		double got = 2 * cabs(acc) / (double)n;
		snprintf(what, sizeof what, "the tone's amplitude passes unchanged: %.4f dB off", 20 * log10(got / amp));
		check(fabs(20 * log10(got / amp)) < 0.01, what);
	}
	free(p);
	free(d);
	free(y);
}

static int selftest_main(int argc, char **argv) {
	design_args da = DESIGN_DEFAULTS;
	for (int i = 2; i < argc; i++)
		if (!design_flag(&da, argc, argv, &i))
			die("unknown flag %s", argv[i]);
	ntf_t t;
	ntf_design(&t, da.order, da.osr, da.hinf);
	printf("sdm.c version %s self-test: order %d, OSR %g, Hinf %g, dither %g\n", SDM_VERSION, t.order, t.osr, t.hinf, da.dither);
	double hmax = 0;
	for (int i = 0; i <= 20000; i++) {
		double h = cabs(ntf_eval(&t, cexp(I * M_PI * i / 20000.0)));
		if (h > hmax)
			hmax = h;
	}
	char what[200];
	snprintf(what, sizeof what, "|H(-1)| is Hinf (%.9f) and no gain exceeds it (%.9f)", cabs(ntf_eval(&t, -1)), hmax);
	check(fabs(cabs(ntf_eval(&t, -1)) - t.hinf) < 1e-9 && hmax <= t.hinf + 1e-9, what);
	int inside = 1, onc = 1;
	for (int i = 0; i < t.order; i++) {
		inside = inside && cabs(t.poles[i]) < 1;
		onc = onc && fabs(cabs(t.zeros[i]) - 1) < 1e-12 && fabs(carg(t.zeros[i])) <= M_PI / t.osr;
	}
	check(inside, "every pole lies inside the unit circle");
	check(onc, "every zero lies on the unit circle inside the band");
	double sum = 0;
	for (int i = 0; i <= t.order; i++)
		sum += t.n[i];
	check(fabs(sum) < 1e-12 || t.order % 2 == 0, "H has a zero at DC");
	long long len = 1LL << 22; /* 1.49 s at DSD64 */
	selftest_run(&t, da.dither, 0, len, 1);
	selftest_run(&t, da.dither, 0.5 * pow(10, -30 / 20.0), len, 1);
	/* The margins: a tone 24 dB over the assets', and silence at 1.5 times
	 * the dither, each stays bounded. */
	selftest_run(&t, da.dither, 0.25, len / 4, 0);
	printf("silence at dither %g: ", 1.5 * da.dither);
	selftest_run(&t, 1.5 * da.dither, 0, len, 0);
	/* Every content's bursts have no net area at the bit rate, so the
	 * loop's running sum stays on the bit lattice after them. */
	for (size_t k = 0; k < sizeof contents / sizeof contents[0]; k++) {
		const double rate = 2822400;
		long long n = (long long)((contents[k].seconds > 0 ? contents[k].seconds : 3.5) * rate);
		for (int ch = 0; ch < 2; ch++) {
			double area = 0;
			for (long long i = 0; i < n; i++)
				area += 0.5 * pow(10, -30 / 20.0) * sample(&contents[k], ch, (double)i / rate);
			snprintf(what, sizeof what, "%s channel %d's bursts leave no net area: %+.5f bits at DSD64", contents[k].name, ch, area);
			check(fabs(area) < 0.01, what);
		}
	}
	/* The boundaries: a quiet tail's stream, whose decaying bursts need
	 * their zero-area phase as much as their ends need the search, starts
	 * and ends on its own mean and leaves at most BOUNDARY_CLICK against
	 * 0x69 on either side. */
	{
		const long long rate = 2822400;
		segment_t seg = {&contents[1], 0, 6 * rate};
		long long P[2] = {0, seg.frames};
		stream_t out;
		out.bits = seg.frames;
		for (int c = 0; c < 2; c++) {
			out.ch[c] = calloc((size_t)((seg.frames + 7) / 8), 1);
			if (!out.ch[c])
				die("%s", "out of memory");
		}
		double amp = 0.5 * pow(10, -30 / 20.0);
		for (int c = 0; c < 2; c++) {
			sdm_t m;
			boundary_t b[2];
			search_stream(&m, &t, da.dither, 77 + (uint64_t)c, &seg, P, 1, c, amp, rate, &out, b);
			snprintf(what, sizeof what, "quiet-tail channel %d starts within %.1f dB SA-CD of 0x69 and ends within %.1f (sums %+.4f, %+.4f)", c,
			         b[0].startClick, b[1].endClick, b[0].start, b[1].end);
			check(b[0].startClick <= BOUNDARY_CLICK && b[1].endClick <= BOUNDARY_CLICK && fabs(b[0].start) <= BOUNDARY_EPS && fabs(b[1].end) <= BOUNDARY_EPS, what);
		}
		free(out.ch[0]);
		free(out.ch[1]);
	}
	/* Determinism: the same seed gives the same bits. */
	sdm_t a, b;
	sdm_init(&a, &t, da.dither, 99);
	sdm_init(&b, &t, da.dither, 99);
	int same = 1;
	for (int k = 0; k < 100000; k++) {
		double u = 0.01 * sin(k * 0.001);
		same = same && sdm_step(&a, u, NULL) == sdm_step(&b, u, NULL);
	}
	check(same, "the same seed gives the same stream");
	printf("%s\n", failures ? "self-test FAILED" : "self-test passed");
	return failures ? 1 : 0;
}

static int design_main(int argc, char **argv) {
	design_args da = DESIGN_DEFAULTS;
	for (int i = 2; i < argc; i++)
		if (!design_flag(&da, argc, argv, &i))
			die("unknown flag %s", argv[i]);
	ntf_t t;
	ntf_design(&t, da.order, da.osr, da.hinf);
	print_design(&t, &da);
	return 0;
}

int main(int argc, char **argv) {
	if (argc >= 2 && strcmp(argv[1], "dsd") == 0)
		return dsd_main(argc, argv);
	if (argc >= 2 && strcmp(argv[1], "wav") == 0)
		return wav_main(argc, argv);
	if (argc >= 2 && strcmp(argv[1], "selftest") == 0)
		return selftest_main(argc, argv);
	if (argc >= 2 && strcmp(argv[1], "design") == 0)
		return design_main(argc, argv);
	fprintf(stderr, "usage: sdm design|selftest|dsd|wav ... (see the comment at the top of sdm.c)\n");
	return 2;
}
