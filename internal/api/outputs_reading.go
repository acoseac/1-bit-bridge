package api

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/mpd"
	"github.com/acoseac/1-bit-bridge/internal/mpd/conf"
	"github.com/acoseac/1-bit-bridge/internal/signalpath"
)

// OutputReading is GET /v1/outputs/{id}/reading: what the DAC of one
// output receives now, read by the bridge from the kernel and from the MPD
// beside it, and the bridge's verdict on its format. Every key is always
// present; a list is [] and a value the reading could not establish is
// null.
type OutputReading struct {
	OutputID string    `json:"outputId"`
	TakenAt  time.Time `json:"takenAt"`
	// AgeMs is how old the reading was when this answer was written: the
	// route serves one reading for up to 500 ms.
	AgeMs int64 `json:"ageMs"`
	// State is whole, settling or unknown, and StateReasons say why it is
	// not whole.
	State        string         `json:"state"`
	StateReasons []string       `json:"stateReasons"`
	MPD          *ReadingMPD    `json:"mpd"`
	Song         *ReadingSong   `json:"song"`
	DAC          *ReadingDAC    `json:"dac"`
	Verdict      ReadingVerdict `json:"verdict"`
}

// ReadingMPD is the MPD the bridge dialled, read around the /proc read:
// its status after it, and the commands between.
type ReadingMPD struct {
	State          string             `json:"state"`
	SongID         *int               `json:"songId"`
	Elapsed        *float64           `json:"elapsed"`
	Audio          string             `json:"audio"`
	ReplayGainMode string             `json:"replayGainMode"`
	Volume         *int               `json:"volume"`
	Xfade          *float64           `json:"xfade"`
	MixRampDB      *float64           `json:"mixrampDb"`
	MixRampDelay   *float64           `json:"mixrampDelay"`
	Outputs        []ReadingMPDOutput `json:"outputs"`
	Config         *ReadingMPDConfig  `json:"config"`
}

// ReadingMPDOutput is one element of MPD's live outputs reply.
type ReadingMPDOutput struct {
	ID         int               `json:"id"`
	Name       string            `json:"name"`
	Plugin     string            `json:"plugin"`
	Enabled    bool              `json:"enabled"`
	Attributes map[string]string `json:"attributes"`
}

// ReadingMPDConfig is what mpd.conf, read at this reading, says of the
// output and the settings the warnings name: information, which MPD may
// not run with.
type ReadingMPDConfig struct {
	VolumeNormalization string                `json:"volumeNormalization"`
	AudioOutputFormat   string                `json:"audioOutputFormat"`
	Output              *ReadingMPDConfOutput `json:"output"`
}

// ReadingMPDConfOutput is the output whose thread holds this output's
// card, or with none holding it the one enabled alsa output whose device
// reaches it, as mpd.conf configures it.
type ReadingMPDConfOutput struct {
	Name           string   `json:"name"`
	Device         string   `json:"device"`
	Chain          []string `json:"chain"`
	MixerType      string   `json:"mixerType"`
	MixerControl   string   `json:"mixerControl"`
	DoP            bool     `json:"dop"`
	Format         string   `json:"format"`
	AllowedFormats string   `json:"allowedFormats"`
	Filters        string   `json:"filters"`
}

// ReadingSong is the song MPD is on, as MPD names it, and this bridge's
// row for it when it is one.
type ReadingSong struct {
	URI    string            `json:"uri"`
	Format string            `json:"format"`
	Row    *ReadingBridgeRow `json:"row"`
}

// ReadingBridgeRow is this bridge's own row for the file MPD is on.
type ReadingBridgeRow struct {
	Path          string   `json:"path"`
	Via           string   `json:"via"`
	Current       bool     `json:"current"`
	Codec         string   `json:"codec"`
	SampleRate    *float64 `json:"sampleRate"`
	BitsPerSample *int     `json:"bitsPerSample"`
	Channels      *int     `json:"channels"`
	IsDSD         *bool    `json:"isDSD"`
}

// ReadingDAC is the output's card and the playback substream read on it.
type ReadingDAC struct {
	Card      int           `json:"card"`
	ID        string        `json:"id"`
	Device    int           `json:"device"`
	Subdevice *int          `json:"subdevice"`
	State     string        `json:"state"`
	Format    string        `json:"format"`
	Rate      *int          `json:"rate"`
	Channels  *int          `json:"channels"`
	Owner     *ReadingOwner `json:"owner"`
	Mixer     *ReadingMixer `json:"mixer"`
}

// ReadingOwner is the thread that holds the substream and its process.
type ReadingOwner struct {
	TID     int     `json:"tid"`
	Thread  *string `json:"thread"`
	PID     *int    `json:"pid"`
	Process *string `json:"process"`
}

// ReadingMixer is the DAC's own mixer as MPD's hardware mixer drives it:
// information, which never changes the verdict.
type ReadingMixer struct {
	Control string `json:"control"`
	Volume  *int   `json:"volume"`
}

// ReadingVerdict is the bridge's verdict on the format the DAC receives:
// format_preserved, format_preserved_dop or withheld, a code for each
// reason it is withheld, bitExactness (not_verifiable: no reading of an
// MPD output shows its samples), and the warnings beside it.
type ReadingVerdict struct {
	Value        string           `json:"value"`
	Reasons      []string         `json:"reasons"`
	BitExactness string           `json:"bitExactness"`
	Warnings     []ReadingWarning `json:"warnings"`
}

// ReadingWarning is one setting the reading read that would change the
// samples, and where it read it: mpd (MPD reported it), mpd_conf (mpd.conf
// says it) or alsa_conf (the ALSA files say it).
type ReadingWarning struct {
	Code   string `json:"code"`
	Source string `json:"source"`
}

// OutputTrack is what the reading needs of one of this bridge's rows: its
// format, and the size and modification time the scanner read it at.
type OutputTrack struct {
	Codec         string
	SampleRate    *float64
	BitsPerSample *int
	Channels      *int
	IsDSD         *bool
	Size          int64
	MTimeNS       int64
}

// readingState is the reading's cache and its flight, under
// localOutputs.mu. proc, conf, alsa and row mark a step that reads files
// (the /proc read, mpd.conf, the ALSA configuration, the look for the
// song's row) still running past an earlier reading's deadline: a reading
// does not start a second of one while it runs, so a /proc file or a mount
// that hangs holds one goroutine, not one per call (boundedStep).
type readingState struct {
	snap                  *readingSnap
	flight                *readingFlight
	proc, conf, alsa, row bool
}

type readingFlight struct {
	done chan struct{}
	snap *readingSnap
}

// readingLimits are the reading's deadlines: the whole reading, the dial,
// each MPD command, the /proc read, the mpd.conf read, the ALSA
// configuration's, and the look for the song's row.
type readingLimits struct {
	build, dial, command, proc, conf, alsa, file time.Duration
}

var defaultReadingLimits = readingLimits{
	build:   3 * time.Second,
	dial:    750 * time.Millisecond,
	command: 750 * time.Millisecond,
	proc:    500 * time.Millisecond,
	conf:    500 * time.Millisecond,
	alsa:    500 * time.Millisecond,
	file:    500 * time.Millisecond,
}

// readingSnap is one reading of the box, which every output's view is cut
// from: MPD's status before the /proc read and after it, the read commands
// between and the settings read again after it, every card, mpd.conf, what
// the ALSA configuration says of each output's device, and the song's row.
type readingSnap struct {
	at       time.Time
	rep      mpd.Report
	endpoint string
	password string
	conf     conf.File
	confRead bool

	// mpdCode is why MPD could not be read, "" when every command
	// answered.
	mpdCode string
	before  mpd.Reply
	after   mpd.Reply
	between time.Duration
	current mpd.Reply
	gain    mpd.Reply
	outputs mpd.Reply
	// gainAfter and outputsAfter are replay_gain_status and outputs read
	// again after the /proc read: what the verdict reads of them must be
	// the same at both ends of it.
	gainAfter    mpd.Reply
	outputsAfter mpd.Reply

	cards    alsa.Snapshot
	procCode string
	// alsaCode is alsa_config_unread when the ALSA configuration was not
	// had: the cards a PCM name resolves to are not known, so neither are
	// the output ids, and every well-formed id is answered unknown.
	alsaCode string
	// picks are the cards GET /v1/outputs lists from these cards, and
	// chains every alsa output of mpd.conf's with the chain its device
	// takes and the first stage on the way that changes samples: both are
	// resolved once a reading, from the ALSA configuration files, so a call
	// the cache answers reads nothing.
	picks  []dacPick
	chains []outputChain

	row *songRow

	// failed is a reading a step of the bridge's own failed in (a panic):
	// an honest unknown, reading_failed.
	failed bool
	// unidentified is a reading whose probe ran past its deadline and left
	// no MPD endpoint: the output ids cannot be told apart, as with /proc
	// unreadable, and every well-formed id is answered, unknown.
	unidentified bool
}

// outputChain is one alsa audio_output of mpd.conf: the live outputs
// element of the same name, whether MPD reports it enabled, the chain its
// device takes to a card when there is one, and the first stage of a
// plugin that changes samples on the way (alsa.PCMConfig.SampleStage).
type outputChain struct {
	conf    conf.AudioOutput
	live    *mpd.ServerOutput
	enabled bool
	chain   alsa.Chain
	ok      bool
	stage   string
}

// songRow is one of this bridge's rows for the song MPD is on.
type songRow struct {
	path    string
	via     string
	track   OutputTrack
	current bool
}

// Reading is GET /v1/outputs/{id}/reading's body for id. found is false
// for an id that names no output. err is the caller's context ending
// while it waited for a reading another call is taking.
func (o *localOutputs) Reading(ctx context.Context, id string) (OutputReading, bool, error) {
	if !validOutputID(id) {
		return OutputReading{}, false, nil
	}
	snap, err := o.readingSnapshot(ctx)
	if err != nil {
		return OutputReading{}, false, err
	}
	r, ok := o.readingView(snap, id)
	return r, ok, nil
}

// validOutputID is the shape outputID writes: 64 lower-case hex digits.
func validOutputID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// readingSnapshot is the read-through cache: a reading younger than
// outputCacheTTL is served as it is, one reading is taken at a time, and
// every caller that arrives while it is taken waits for it. Nothing reads
// between calls: there is no sampler. The reading runs on a context
// detached from its first caller, under its own deadline, so a caller that
// leaves takes nothing from the ones waiting.
func (o *localOutputs) readingSnapshot(ctx context.Context) (*readingSnap, error) {
	o.armIdle()
	o.mu.Lock()
	if s := o.reading.snap; s != nil && o.now().Sub(s.at) < outputCacheTTL {
		o.mu.Unlock()
		return s, nil
	}
	if f := o.reading.flight; f != nil {
		o.mu.Unlock()
		select {
		case <-f.done:
			return f.snap, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &readingFlight{done: make(chan struct{})}
	o.reading.flight = f
	o.readings++
	o.mu.Unlock()

	// The flight completes however the reading ends, a panic included: a
	// flight left open would hold every later reading, and the idle close
	// never runs while a reading counts.
	var snap *readingSnap
	defer func() {
		if snap == nil {
			snap = &readingSnap{failed: true, at: o.now()}
		}
		o.mu.Lock()
		o.reading.snap = snap
		o.reading.flight = nil
		o.readings--
		f.snap = snap
		close(f.done)
		o.mu.Unlock()
		o.armIdle()
	}()
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.limits.build)
	defer cancel()
	snap = o.takeReading(bctx)
	return snap, nil
}

// takeReading reads the box once. MPD's status is read before the /proc
// read and again after it, with currentsong, replay_gain_status and
// outputs between and the last two read again after it, so a song change,
// a seek, a pause or a change of the settings across the read shows.
// mpd.conf and the ALSA configuration are read after that bracket, so their
// deadlines never widen it: what they say is information beside the
// verdict, read again at every reading (the player rewrites mpd.conf and
// restarts MPD when its settings change, and the probe's copy can be half a
// minute old). A step that cannot be had leaves a code and the rest of the
// reading stands; a step of the bridge's own that panics leaves
// reading_failed.
func (o *localOutputs) takeReading(ctx context.Context) (snap *readingSnap) {
	defer func() {
		if r := recover(); r != nil {
			logOutputPanic("the reading", r)
			snap = &readingSnap{failed: true, at: o.now()}
		}
	}()
	cfg := o.outputConfig()
	_, rep, opt, _, probeLate := o.probeVerdict(ctx)
	s := &readingSnap{rep: rep, password: cfg.Password}
	s.endpoint = rep.MPD.Endpoint
	if s.endpoint == "" {
		s.endpoint = opt.Endpoint
	}

	var sess outputSession
	var code string
	if probeLate && s.endpoint == "" {
		// The probe that finds MPD ran past its deadline and left no
		// endpoint, which the output ids are made from: which output an id
		// names is not known, and there is no MPD to dial.
		s.unidentified, code = true, signalpath.MPDTimeout
	} else {
		sess, code = o.readingSession(ctx, s.endpoint, cfg.Password)
	}
	ask := func(cmd func(outputSession, context.Context) (mpd.Reply, error)) (mpd.Reply, bool) {
		if code != "" {
			return mpd.Reply{}, false
		}
		reply, failed, broken := o.readingCommand(ctx, sess, cmd)
		code = failed
		return reply, broken
	}
	before, broken := ask(statusCommand)
	if broken {
		// A session MPD closed (a restart, a timeout of its own) is dialled
		// once more in this reading.
		if sess, code = o.readingSession(ctx, s.endpoint, cfg.Password); code == "" {
			before, _ = ask(statusCommand)
		}
	}
	first := o.now()
	gainCommand := func(c outputSession, ctx context.Context) (mpd.Reply, error) { return c.ReplayGainStatus(ctx) }
	outputsCommand := func(c outputSession, ctx context.Context) (mpd.Reply, error) { return c.Outputs(ctx) }
	s.current, _ = ask(func(c outputSession, ctx context.Context) (mpd.Reply, error) { return c.CurrentSong(ctx) })
	s.gain, _ = ask(gainCommand)
	s.outputs, _ = ask(outputsCommand)
	s.cards, s.procCode = o.readCards(ctx)
	s.after, _ = ask(statusCommand)
	s.between = o.now().Sub(first)
	s.gainAfter, _ = ask(gainCommand)
	s.outputsAfter, _ = ask(outputsCommand)
	s.before, s.mpdCode = before, code
	if rep.ConfPath != "" {
		s.conf, s.confRead = o.readConf(ctx, confRoot(opt.Root, rep.ConfPath), rep.ConfPath)
	}
	// With no card there is nothing to resolve a device to, and the ALSA
	// configuration is not read.
	if s.procCode == "" && len(s.cards.Cards) > 0 {
		// A configuration not had is not resolved from: the cards a PCM
		// name reaches would fall back to every USB card, and an output
		// GET /v1/outputs lists through that name would read as none.
		if pcm, had := o.readALSAConfig(ctx, alsa.ConfigRoot(s.cards.ProcRoot)); had {
			s.picks = dacsFrom(s.cards, s.rep, pcm.ResolveOutputDevice)
			s.chains = outputChains(s, pcm)
		} else {
			s.alsaCode = signalpath.ALSAConfigUnread
		}
	}
	if s.mpdCode == "" {
		s.row = o.lookUpSong(ctx, s)
	}
	s.at = o.now()
	return s
}

// outputChains resolves the chain of every alsa output mpd.conf names, and
// the first stage on its way that changes samples, from the ALSA
// configuration this reading read, enabled only as the live outputs reply
// says: a file's enabled setting is not what MPD runs with (its state file
// restores another). An output with no device plays to ALSA's default PCM.
func outputChains(s *readingSnap, pcm alsa.PCMConfig) []outputChain {
	if !s.confRead {
		return nil
	}
	cards := make([]alsa.Card, len(s.cards.Cards))
	for i, c := range s.cards.Cards {
		cards[i] = c.Card
	}
	live := mpd.ServerOutputs(s.outputs)
	var out []outputChain
	for _, c := range s.conf.Outputs {
		if c.Type != "alsa" {
			continue
		}
		oc := outputChain{conf: c}
		for i := range live {
			if live[i].Name == c.Name {
				oc.live = &live[i]
				break
			}
		}
		oc.enabled = oc.live != nil && oc.live.Enabled && oc.live.Plugin == "alsa"
		device := strings.TrimSpace(c.Device)
		if device == "" {
			device = "default"
		}
		oc.chain, oc.ok = pcm.ResolveOutputChain(device, cards)
		oc.stage = pcm.SampleStage(device)
		out = append(out, oc)
	}
	return out
}

// boundedStep runs one step of the reading that reads files (the /proc
// read, mpd.conf, the ALSA configuration, the look for the song's row) on
// a goroutine of its own, and waits for it at most limit and no longer
// than ctx. A file can hold a read for good (a dead network mount; nothing
// cancels a system call that waits on one), so a step still running from
// an earlier reading is neither joined nor doubled: busy marks it, under
// mu, and this reading's step is late at once. Joining it would also hand
// this reading what was read before its first status, outside its bracket.
// A panic in fn fails the step and is logged; it never ends the process.
func boundedStep[T any](o *localOutputs, busy *bool, ctx context.Context, limit time.Duration, name string, fn func() T) (val T, late, failed bool) {
	o.mu.Lock()
	if *busy {
		o.mu.Unlock()
		return val, true, false
	}
	*busy = true
	o.mu.Unlock()
	done := make(chan struct{})
	var out T
	var panicked bool
	go func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				logOutputPanic(name, r)
			}
			o.mu.Lock()
			*busy = false
			o.mu.Unlock()
			close(done)
		}()
		out = fn()
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-done:
		if panicked {
			return val, false, true
		}
		return out, false, false
	case <-timer.C:
	case <-ctx.Done():
	}
	return val, true, false
}

// logOutputPanic logs a panic in a step of the reading or the probe: the
// step and the stack, and the panic's value only when it is the runtime's
// own error, which carries none of what the step read (a value code
// panics with could quote a file, and mpd.conf holds a password).
func logOutputPanic(step string, r any) {
	attrs := []any{"step", step, "stack", string(debug.Stack())}
	if rerr, ok := r.(runtime.Error); ok {
		attrs = append(attrs, "error", rerr.Error())
	}
	logger.Error("output reading: a step panicked", attrs...)
}

// readConf reads mpd.conf whole, its includes too, under the conf
// deadline (boundedStep). read is false when it could not be read, did
// not answer in time, or the step failed: mpd.config is then null, and no
// warning comes from it.
func (o *localOutputs) readConf(ctx context.Context, root, path string) (conf.File, bool) {
	type result struct {
		f   conf.File
		err error
	}
	r, late, failed := boundedStep(o, &o.reading.conf, ctx, o.limits.conf, "the mpd.conf read", func() result {
		f, err := o.confFile(root, path, path)
		return result{f, err}
	})
	if late || failed || r.err != nil {
		return conf.File{}, false
	}
	return r.f, true
}

// readALSAConfig reads the ALSA configuration once, under its deadline
// (boundedStep). had is false for one not had in time, or whose read
// failed: nothing is resolved from it.
func (o *localOutputs) readALSAConfig(ctx context.Context, root string) (pcm alsa.PCMConfig, had bool) {
	c, late, failed := boundedStep(o, &o.reading.alsa, ctx, o.limits.alsa, "the ALSA configuration read", func() alsa.PCMConfig {
		return o.alsaConfig(root)
	})
	if late || failed {
		return alsa.PCMConfig{}, false
	}
	return c, true
}

// confRoot is the root mpd.conf is read under: the probe's, except that a
// path naming a volume is read under that volume's root, which is what /
// is there. A Linux path names none; a Windows test's temp directory does,
// on another drive than the working directory that / would mean.
func confRoot(root, path string) string {
	if v := filepath.VolumeName(path); v != "" {
		return v + string(filepath.Separator)
	}
	return root
}

func statusCommand(c outputSession, ctx context.Context) (mpd.Reply, error) { return c.Status(ctx) }

// readingSession is the shared session, dialled under the reading's own
// dial deadline when there is none, and the code of a dial that failed.
func (o *localOutputs) readingSession(ctx context.Context, endpoint, password string) (outputSession, string) {
	if endpoint == "" {
		return nil, signalpath.MPDUnreachable
	}
	dctx, cancel := context.WithTimeout(ctx, o.limits.dial)
	defer cancel()
	sess, err := o.session(dctx, endpoint, password)
	if err == nil && sess != nil {
		return sess, ""
	}
	var ack *mpd.ACKError
	switch {
	case errors.As(err, &ack):
		return nil, signalpath.MPDRefused
	case dctx.Err() != nil:
		return nil, signalpath.MPDTimeout
	}
	return nil, signalpath.MPDUnreachable
}

// readingCommand runs one MPD command under its own deadline and names a
// failure. An ACK is MPD refusing the command, and the connection is still
// in step. A context error is a command that never got its turn on the
// session, so nothing was written. Anything else failed with the command
// written: MPD may still answer it, and that late reply would answer the
// next command, so the session is dropped. broken reports that drop. A
// failure once the command's deadline has passed is a timeout: the
// client's socket deadline is the context's, and its timeout can come back
// before the context's own timer has marked the context done.
func (o *localOutputs) readingCommand(ctx context.Context, sess outputSession, cmd func(outputSession, context.Context) (mpd.Reply, error)) (reply mpd.Reply, code string, broken bool) {
	cctx, cancel := context.WithTimeout(ctx, o.limits.command)
	defer cancel()
	reply, err := cmd(sess, cctx)
	if err == nil {
		return reply, "", false
	}
	var ack *mpd.ACKError
	if errors.As(err, &ack) {
		return reply, signalpath.MPDRefused, false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return reply, signalpath.MPDTimeout, false
	}
	o.dropSession(sess)
	if deadline, ok := cctx.Deadline(); cctx.Err() != nil || (ok && !time.Now().Before(deadline)) {
		return reply, signalpath.MPDTimeout, true
	}
	return reply, signalpath.MPDUnreachable, true
}

// readCards reads every card once, under the /proc deadline (boundedStep):
// a read still running past an earlier reading's deadline makes this one a
// timeout at once, and a read that panicked is a /proc that could not be
// read.
func (o *localOutputs) readCards(ctx context.Context) (alsa.Snapshot, string) {
	type result struct {
		snap alsa.Snapshot
		err  error
	}
	r, late, failed := boundedStep(o, &o.reading.proc, ctx, o.limits.proc, "the /proc read", func() result {
		snap, err := o.cards()
		return result{snap, err}
	})
	switch {
	case late:
		return alsa.Snapshot{}, signalpath.ProcTimeout
	case failed:
		return alsa.Snapshot{}, signalpath.ProcUnreadable
	case r.err == nil:
		return r.snap, ""
	case errors.Is(r.err, fs.ErrNotExist):
		// No /proc/asound: a box with no sound card, which is a card
		// missing, not a /proc that could not be read.
		return alsa.Snapshot{Cards: []alsa.CardReading{}}, ""
	}
	return alsa.Snapshot{}, signalpath.ProcUnreadable
}

// lookUpSong names the song MPD is on as one of this bridge's rows, under
// the look's deadline (boundedStep): os.Stat on a dead network mount does
// not return. A look still running from an earlier reading is not doubled,
// and the song is then not named.
func (o *localOutputs) lookUpSong(ctx context.Context, s *readingSnap) *songRow {
	uri, _ := s.current.Get("file")
	if uri == "" || o.track == nil {
		return nil
	}
	musicDir := s.rep.MusicDirectory
	if s.confRead && s.conf.MusicDirectory != "" {
		musicDir = s.conf.MusicDirectory
	}
	roots := o.roots()
	base := ""
	if o.loopback != nil {
		base = o.loopback()
	}
	lctx, cancel := context.WithTimeout(ctx, o.limits.file)
	defer cancel()
	row, late, failed := boundedStep(o, &o.reading.row, lctx, o.limits.file, "the look for the song's row", func() *songRow {
		return o.findRow(lctx, uri, musicDir, roots, base)
	})
	if late || failed {
		return nil
	}
	return row
}

// findRow is this bridge's row for the file MPD names, or nil. A URL of
// this bridge's loopback track route names its row by the track id. A
// database path is a row when a library root's prefix rule takes it, the
// rest of the path stays under that root, and os.SameFile agrees that
// MPD's file and the root's are one file: a path outside every root is
// never looked up.
func (o *localOutputs) findRow(ctx context.Context, uri, musicDir string, roots []string, base string) *songRow {
	if id, ok := loopbackTrackID(uri, base); ok {
		if o.loopTrack == nil {
			return nil
		}
		path, abs, ok := o.loopTrack(id)
		if !ok {
			return nil
		}
		return o.rowAt(ctx, path, abs, "loopback")
	}
	if strings.Contains(uri, "://") || strings.HasPrefix(uri, "/") || musicDir == "" {
		return nil
	}
	multi := len(roots) > 1
	libs := make([]mpd.LibraryRoot, len(roots))
	for i, root := range roots {
		libs[i] = mpd.LibraryRoot{ID: filepath.Base(root), Path: root, Multi: multi}
	}
	for i, rule := range mpd.ProveRoots(ctx, musicDir, libs, nil) {
		if rule.State == mpd.PathUnproven {
			continue
		}
		rel, ok := strings.CutPrefix(uri, rule.PrependMpdPrefix)
		if !ok || rel == "" || !filepath.IsLocal(filepath.FromSlash(rel)) {
			continue
		}
		own := filepath.Join(roots[i], filepath.FromSlash(rel))
		theirs, err1 := os.Stat(filepath.Join(musicDir, filepath.FromSlash(uri)))
		mine, err2 := os.Stat(own)
		if err1 != nil || err2 != nil || !os.SameFile(theirs, mine) {
			continue
		}
		return o.rowAt(ctx, rule.StripBridgePrefix+rel, own, "path")
	}
	return nil
}

// rowAt is the row at a manifest path, and whether the file on disk is
// still the one the scanner read: the same size and modification time,
// the scanner's own test (GetTrackStat).
func (o *localOutputs) rowAt(ctx context.Context, path, abs, via string) *songRow {
	track, ok, err := o.track(ctx, path)
	if err != nil || !ok {
		return nil
	}
	row := &songRow{path: path, via: via, track: track}
	if fi, err := os.Stat(abs); err == nil && fi.Mode().IsRegular() &&
		fi.Size() == track.Size && fi.ModTime().UnixNano() == track.MTimeNS {
		row.current = true
	}
	return row
}

// loopbackTrackID is the track id in a URL of this bridge's loopback
// track route: http, a loopback host, the route's port, /dlna/file/<id>
// and nothing more. base is the route's base URL, "" while it is not
// serving.
func loopbackTrackID(uri, base string) (string, bool) {
	if base == "" {
		return "", false
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", false
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	if u.Port() == "" || u.Port() != b.Port() || !loopbackHostName(u.Hostname()) {
		return "", false
	}
	id, ok := strings.CutPrefix(u.Path, "/dlna/file/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

func loopbackHostName(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// readingElement is the output an id names in a reading: its card, or nil
// for an output that names none.
type readingElement struct {
	pick *dacPick
}

// readingElement finds the output id names, from this reading's cards the
// way GET /v1/outputs lists them, so the ids agree. When the cards could
// not be read, the ALSA configuration that says which card a PCM name
// reaches was not had, the MPD endpoint the ids are made from is not known,
// or the reading failed, an id they could have named cannot be told from
// one they could not: it is answered, unknown, rather than refused.
func (o *localOutputs) readingElement(s *readingSnap, id string) (readingElement, bool) {
	if s.procCode != "" || s.alsaCode != "" || s.failed || s.unidentified {
		return readingElement{}, true
	}
	if len(s.picks) == 0 {
		return readingElement{}, outputID(o.identity, s.endpoint, "", "") == id
	}
	for i := range s.picks {
		if outputID(o.identity, s.endpoint, s.picks[i].dac.ID, s.picks[i].dac.USBID) == id {
			return readingElement{pick: &s.picks[i]}, true
		}
	}
	return readingElement{}, false
}

// readingView is one output's reading, cut from the box's.
func (o *localOutputs) readingView(s *readingSnap, id string) (OutputReading, bool) {
	elem, found := o.readingElement(s, id)
	if !found {
		return OutputReading{}, false
	}
	redact := func(v string) string {
		if s.confRead {
			v = s.conf.Redact(v)
		}
		return conf.RedactSecrets(v, s.password)
	}

	var reasons []string
	switch {
	case s.failed:
		reasons = []string{signalpath.ReadingFailed}
	default:
		if s.mpdCode != "" {
			reasons = append(reasons, s.mpdCode)
		}
		switch {
		case s.procCode != "":
			reasons = append(reasons, s.procCode)
		case s.alsaCode != "":
			reasons = append(reasons, s.alsaCode)
		case elem.pick == nil && !s.unidentified:
			reasons = append(reasons, signalpath.NoCard)
		}
	}
	state := "whole"
	if len(reasons) > 0 {
		state = "unknown"
	} else if settle := readingSettling(s); len(settle) > 0 {
		state, reasons = "settling", settle
	}
	verdict := signalpath.WithheldFor(reasons...)
	if state == "whole" {
		verdict = signalpath.Judge(readingFacts(s, elem.pick))
	}

	age := o.now().Sub(s.at).Milliseconds()
	if age < 0 {
		age = 0
	}
	out := OutputReading{
		OutputID:     id,
		TakenAt:      s.at.UTC(),
		AgeMs:        age,
		State:        state,
		StateReasons: signalpath.Sorted(reasons),
		Verdict:      readingVerdict(verdict),
	}
	if s.failed {
		return out, true
	}
	sub, _ := pickSubstream(s.cards, elem.pick)
	sh := shownOutput(s, elem.pick, sub)
	if s.mpdCode == "" {
		out.MPD = readingMPD(s, sh, redact)
		out.Song = readingSong(s, redact)
	}
	if elem.pick != nil && s.procCode == "" {
		out.DAC = readingDAC(s, elem.pick, sh)
	}
	return out, true
}

// readingVerdict is the verdict's wire form.
func readingVerdict(v signalpath.Verdict) ReadingVerdict {
	out := ReadingVerdict{Value: v.Value, Reasons: v.Reasons, BitExactness: v.BitExactness, Warnings: []ReadingWarning{}}
	for _, w := range v.Warnings {
		out.Warnings = append(out.Warnings, ReadingWarning{Code: w.Code, Source: w.Source})
	}
	return out
}

// readingSettling is the settling codes: the two status reads; the song
// currentsong named between them, which while MPD plays must be the song
// both reads name, by an id it gives; and what MPD reports of its outputs
// and settings, which must be the same at both ends of the /proc read.
func readingSettling(s *readingSnap) []string {
	status := func(r mpd.Reply) signalpath.Status {
		state, _ := r.Get("state")
		song, _ := r.Get("songid")
		elapsed, _ := r.Get("elapsed")
		return signalpath.Status{State: state, SongID: song, Elapsed: elapsed}
	}
	codes := signalpath.Settling(status(s.before), status(s.after), s.between)
	id, ok := s.current.Get("Id")
	after, _ := s.after.Get("songid")
	state, _ := s.after.Get("state")
	if (ok || state == "play") && (!ok || after != id) {
		codes = append(codes, signalpath.SongChanged)
	}
	if !sameSettings(s) {
		codes = append(codes, signalpath.SettingsChanged)
	}
	return signalpath.Sorted(codes)
}

// sameSettings reports whether what MPD reports of the settings the
// verdict reads was the same before the /proc read and after it: the
// outputs reply whole (an output switched on or off, an attribute set with
// outputset), replay_gain_status, and status's audio, xfade and
// mixrampdelay. A reading judges them from one read; two that agree around
// the /proc read are what held while it ran.
func sameSettings(s *readingSnap) bool {
	for _, key := range []string{"audio", "xfade", "mixrampdelay"} {
		a, okA := s.before.Get(key)
		b, okB := s.after.Get(key)
		if a != b || okA != okB {
			return false
		}
	}
	return slices.Equal(s.gain.Pairs, s.gainAfter.Pairs) && slices.Equal(s.outputs.Pairs, s.outputsAfter.Pairs)
}

// outputThreadName is the name the thread of MPD's output called output
// has, as a /proc status file shows it: "output:" and the output's name
// (src/output/Thread.cxx, FmtThreadName("output:{}")), cut to the 15 bytes
// a task's name holds (FmtThreadName formats into 16 bytes, TASK_COMM_LEN),
// with the backslash and the newline the status file escapes escaped, and
// without the white space at either end the reader trims.
func outputThreadName(output string) string {
	n := "output:" + output
	if len(n) > 15 {
		n = n[:15]
	}
	n = strings.ReplaceAll(n, `\`, `\\`)
	n = strings.ReplaceAll(n, "\n", `\n`)
	return strings.TrimSpace(n)
}

// holders is every enabled output MPD reports whose output thread is named
// as the thread holding sub is: the output whose samples reach the card,
// when there is exactly one. Names are unique among a daemon's outputs, so
// two are two names alike in their first bytes.
func holders(s *readingSnap, sub *alsa.Substream) []mpd.ServerOutput {
	if sub == nil || sub.Owner == nil || sub.Owner.Thread == nil {
		return nil
	}
	var out []mpd.ServerOutput
	for _, lo := range mpd.ServerOutputs(s.outputs) {
		if lo.Enabled && outputThreadName(lo.Name) == *sub.Owner.Thread {
			out = append(out, lo)
		}
	}
	return out
}

// namedBlock is mpd.conf's audio_output block named name.
func namedBlock(s *readingSnap, name string) (conf.AudioOutput, bool) {
	for _, b := range s.conf.Outputs {
		if b.Name == name {
			return b, true
		}
	}
	return conf.AudioOutput{}, false
}

// namedChain is the alsa output named name, with the chain its device
// takes.
func namedChain(s *readingSnap, name string) (*outputChain, bool) {
	for i := range s.chains {
		if s.chains[i].conf.Name == name {
			return &s.chains[i], true
		}
	}
	return nil, false
}

// holderOutput is the output whose thread holds the card, as the verdict
// and its warnings read it: its live plugin, allowed_formats and dop; and,
// when mpd.conf was read and has its block, that block's mixer, format and
// filters, the block's allowed_formats and dop where MPD reports none, and
// the first stage that changes samples on its device's way as the ALSA
// files read. What the block says is information: the verdict claims none
// of it, and only the dop it falls back to can withhold one.
func holderOutput(s *readingSnap, h mpd.ServerOutput) signalpath.Output {
	out := signalpath.Output{Plugin: h.Plugin}
	liveFormats, haveFormats := h.Attribute("allowed_formats")
	liveDoP, haveDoP := h.Attribute("dop")
	if haveFormats {
		out.AllowedFormats, out.AllowedFormatsLive = liveFormats, true
	}
	if haveDoP {
		out.DoP, _ = signalpath.ParseBool(liveDoP)
	}
	if !s.confRead {
		return out
	}
	block, ok := namedBlock(s, h.Name)
	if !ok {
		return out
	}
	out.Configured = true
	out.MixerType = signalpath.EffectiveMixerType(block.MixerType, block.MixerEnabled, s.conf.MixerType)
	out.Format, out.Filters = block.Format, block.Filters
	if !haveFormats {
		out.AllowedFormats = block.AllowedFormats
	}
	if !haveDoP {
		out.DoP, _ = signalpath.ParseBool(block.DOP)
	}
	if oc, ok := namedChain(s, h.Name); ok {
		out.SampleStage = oc.stage
	}
	return out
}

// shown is the output a reading describes for a card, information beside
// the verdict: mpd.conf's block, MPD's live element of the same name, and
// the chain its device resolves to.
type shown struct {
	block conf.AudioOutput
	live  *mpd.ServerOutput
	chain []string
}

// shownOutput is the output whose thread holds the card; with nothing held,
// the one enabled alsa output MPD reports whose chain reaches the card;
// nil when there is no such one output.
func shownOutput(s *readingSnap, pick *dacPick, sub *alsa.Substream) *shown {
	if !s.confRead || pick == nil {
		return nil
	}
	hs := holders(s, sub)
	switch len(hs) {
	case 1:
		block, ok := namedBlock(s, hs[0].Name)
		if !ok {
			return nil
		}
		sh := &shown{block: block, live: &hs[0]}
		if oc, ok := namedChain(s, hs[0].Name); ok && oc.ok {
			sh.chain = oc.chain.Stages
		}
		return sh
	case 0:
	default:
		return nil
	}
	var found *shown
	for i := range s.chains {
		oc := &s.chains[i]
		if !oc.enabled || !oc.ok || oc.chain.Card != pick.dac.Card || oc.chain.Device != pick.pcm {
			continue
		}
		if found != nil {
			return nil
		}
		found = &shown{block: oc.conf, live: oc.live, chain: oc.chain.Stages}
	}
	return found
}

// shownDoP is whether the output sends DSD as DoP: the live dop attribute,
// which outputset changes at run time, else the file's.
func shownDoP(sh *shown) bool {
	if sh.live != nil {
		if v, ok := sh.live.Attribute("dop"); ok {
			on, _ := signalpath.ParseBool(v)
			return on
		}
	}
	on, _ := signalpath.ParseBool(sh.block.DOP)
	return on
}

// shownAllowedFormats is the live allowed_formats attribute, else the
// file's.
func shownAllowedFormats(sh *shown) string {
	if sh.live != nil {
		if v, ok := sh.live.Attribute("allowed_formats"); ok {
			return v
		}
	}
	return sh.block.AllowedFormats
}

func shownMixerType(s *readingSnap, sh *shown) string {
	return signalpath.EffectiveMixerType(sh.block.MixerType, sh.block.MixerEnabled, s.conf.MixerType)
}

// readingFacts is what the verdict judges, and its warnings read, for one
// output.
func readingFacts(s *readingSnap, pick *dacPick) signalpath.Facts {
	state, _ := s.after.Get("state")
	audio, _ := s.after.Get("audio")
	mode, _ := s.gain.Get("replay_gain_mode")
	xfade, _ := s.after.Get("xfade")
	mixramp, _ := s.after.Get("mixrampdelay")
	volume, _ := s.after.Get("volume")
	f := signalpath.Facts{
		State:  state,
		Audio:  audio,
		Source: sourceFacts(s.row),
		MPD: signalpath.MPD{
			ReplayGainMode:      mode,
			Crossfade:           xfade,
			MixRampDelay:        mixramp,
			Volume:              volume,
			ConfRead:            s.confRead,
			VolumeNormalization: s.conf.VolumeNormalization,
			AudioOutputFormat:   s.conf.AudioOutputFormat,
		},
	}
	sub, ambiguous := pickSubstream(s.cards, pick)
	switch {
	case ambiguous:
		f.PCM.Ambiguous = true
	case sub != nil:
		f.PCM.Open = true
		if sub.Status != nil {
			f.PCM.State = sub.Status.State
		}
		if h := sub.HWParams; h != nil {
			f.PCM.Format = h.Format
			if h.Rate != nil {
				f.PCM.Rate = *h.Rate
			}
			if h.Channels != nil {
				f.PCM.Channels = *h.Channels
			}
		}
	}
	var owner *alsa.Owner
	if sub != nil && !ambiguous {
		owner = sub.Owner
	}
	if owner != nil {
		if owner.Process != nil {
			f.PCM.Owner = *owner.Process
		}
		if owner.Thread != nil {
			f.PCM.Thread = *owner.Thread
		}
	}
	hs := holders(s, sub)
	f.MPD.Holders = len(hs)
	if len(hs) == 1 {
		f.MPD.Output = holderOutput(s, hs[0])
	}
	return f
}

// sourceFacts is the file's own format, from its row.
func sourceFacts(row *songRow) signalpath.Source {
	if row == nil {
		return signalpath.Source{}
	}
	t := row.track
	src := signalpath.Source{
		Known:   row.current,
		Changed: !row.current,
		Codec:   t.Codec,
		Lossy:   manifest.IsLossyCodec(t.Codec),
		DSD:     (t.IsDSD != nil && *t.IsDSD) || strings.EqualFold(t.Codec, "DSF") || strings.EqualFold(t.Codec, "DFF"),
	}
	if t.SampleRate != nil && *t.SampleRate > 0 && *t.SampleRate < math.MaxInt32 {
		src.SampleRate = int(math.Round(*t.SampleRate))
	}
	if t.BitsPerSample != nil {
		src.Bits = *t.BitsPerSample
	}
	if t.Channels != nil {
		src.Channels = *t.Channels
	}
	return src
}

// pickSubstream is the playback substream of pick's PCM device that is
// open: the only one, else the only one MPD holds. ambiguous is more than
// one open and not exactly one of them MPD's.
func pickSubstream(snap alsa.Snapshot, pick *dacPick) (*alsa.Substream, bool) {
	if pick == nil {
		return nil, false
	}
	var dev *alsa.Device
	for i := range snap.Cards {
		if snap.Cards[i].Index != pick.dac.Card {
			continue
		}
		for j := range snap.Cards[i].Devices {
			if snap.Cards[i].Devices[j].Device == pick.pcm {
				dev = &snap.Cards[i].Devices[j]
			}
		}
	}
	if dev == nil {
		return nil, false
	}
	var open []*alsa.Substream
	for i := range dev.Substreams {
		sub := &dev.Substreams[i]
		hwOpen := sub.HWParams != nil && !sub.HWParams.Closed
		statusOpen := sub.Status != nil && !sub.Status.Closed && sub.Status.State != ""
		if hwOpen || statusOpen {
			open = append(open, sub)
		}
	}
	switch len(open) {
	case 0:
		return nil, false
	case 1:
		return open[0], false
	}
	var mpds []*alsa.Substream
	for _, sub := range open {
		if sub.Owner != nil && sub.Owner.Process != nil && *sub.Owner.Process == "mpd" {
			mpds = append(mpds, sub)
		}
	}
	if len(mpds) == 1 {
		return mpds[0], false
	}
	return nil, true
}

// readingMPD is MPD's half of the reading: its status after the /proc
// read, the read commands between, and what mpd.conf says.
func readingMPD(s *readingSnap, sh *shown, redact func(string) string) *ReadingMPD {
	get := func(r mpd.Reply, key string) string {
		v, _ := r.Get(key)
		return redact(v)
	}
	m := &ReadingMPD{
		State:          get(s.after, "state"),
		SongID:         replyInt(s.after, "songid"),
		Elapsed:        replyFloat(s.after, "elapsed"),
		Audio:          get(s.after, "audio"),
		ReplayGainMode: get(s.gain, "replay_gain_mode"),
		Volume:         replyVolume(s.after),
		Xfade:          replyFloat(s.after, "xfade"),
		MixRampDB:      replyFloat(s.after, "mixrampdb"),
		MixRampDelay:   replyFloat(s.after, "mixrampdelay"),
		Outputs:        []ReadingMPDOutput{},
	}
	if _, ok := s.after.Get("xfade"); !ok {
		zero := 0.0
		m.Xfade = &zero
	}
	for _, lo := range mpd.ServerOutputs(s.outputs) {
		attrs := map[string]string{}
		for _, a := range lo.Attributes {
			attrs[redact(a.Key)] = redact(a.Value)
		}
		m.Outputs = append(m.Outputs, ReadingMPDOutput{
			ID: lo.ID, Name: redact(lo.Name), Plugin: redact(lo.Plugin), Enabled: lo.Enabled, Attributes: attrs,
		})
	}
	if s.confRead {
		m.Config = &ReadingMPDConfig{
			VolumeNormalization: redact(s.conf.VolumeNormalization),
			AudioOutputFormat:   redact(s.conf.AudioOutputFormat),
		}
		if sh != nil {
			m.Config.Output = &ReadingMPDConfOutput{
				Name:           redact(sh.block.Name),
				Device:         redact(sh.block.Device),
				Chain:          append([]string{}, sh.chain...),
				MixerType:      shownMixerType(s, sh),
				MixerControl:   redact(sh.block.MixerControl),
				DoP:            shownDoP(sh),
				Format:         redact(sh.block.Format),
				AllowedFormats: redact(shownAllowedFormats(sh)),
				Filters:        redact(sh.block.Filters),
			}
		}
	}
	return m
}

// readingSong is the song MPD is on, null when it is on none. A URL's user
// information is left out.
func readingSong(s *readingSnap, redact func(string) string) *ReadingSong {
	uri, ok := s.current.Get("file")
	if !ok || uri == "" {
		return nil
	}
	format, _ := s.current.Get("Format")
	song := &ReadingSong{URI: redact(withoutUserinfo(uri)), Format: redact(format)}
	if r := s.row; r != nil {
		t := r.track
		song.Row = &ReadingBridgeRow{
			Path: r.path, Via: r.via, Current: r.current, Codec: t.Codec,
			SampleRate: t.SampleRate, BitsPerSample: t.BitsPerSample, Channels: t.Channels, IsDSD: t.IsDSD,
		}
	}
	return song
}

// readingDAC is the card and the substream read on it, and the DAC's own
// mixer when MPD drives it as a hardware mixer.
func readingDAC(s *readingSnap, pick *dacPick, sh *shown) *ReadingDAC {
	d := &ReadingDAC{Card: pick.dac.Card, ID: pick.dac.ID, Device: pick.pcm}
	if sub, ambiguous := pickSubstream(s.cards, pick); sub != nil && !ambiguous {
		n := sub.Subdevice
		d.Subdevice = &n
		if sub.Status != nil {
			d.State = sub.Status.State
		}
		if h := sub.HWParams; h != nil {
			d.Format, d.Rate, d.Channels = h.Format, h.Rate, h.Channels
		}
		if ow := sub.Owner; ow != nil {
			d.Owner = &ReadingOwner{TID: ow.TID, Thread: ow.Thread, PID: ow.PID, Process: ow.Process}
		}
	}
	if sh != nil && shownMixerType(s, sh) == "hardware" {
		control := sh.block.MixerControl
		if control == "" {
			control = "PCM"
		}
		d.Mixer = &ReadingMixer{Control: conf.RedactSecrets(s.conf.Redact(control), s.password)}
		if s.mpdCode == "" {
			d.Mixer.Volume = replyVolume(s.after)
		}
	}
	return d
}

func replyInt(r mpd.Reply, key string) *int {
	v, ok := r.Get(key)
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return nil
	}
	return &n
}

// replyFloat is a number MPD printed, nil when it printed none, or one
// JSON cannot carry (nan, inf).
func replyFloat(r mpd.Reply, key string) *float64 {
	v, ok := r.Get(key)
	if !ok {
		return nil
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	return &n
}

// replyVolume is status volume: 0 to 100, nil when MPD has no mixer to
// read (it then prints none, or -1 before 0.23).
func replyVolume(r mpd.Reply) *int {
	n := replyInt(r, "volume")
	if n == nil || *n < 0 || *n > 100 {
		return nil
	}
	return n
}

// withoutUserinfo is uri without the user name and password a URL may
// carry before its host; anything else is as MPD names it.
func withoutUserinfo(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.User == nil || u.Scheme == "" {
		return uri
	}
	prefix := u.Scheme + "://"
	if len(uri) < len(prefix) || !strings.EqualFold(uri[:len(prefix)], prefix) {
		return uri
	}
	rest := uri[len(prefix):]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	at := strings.LastIndex(rest[:end], "@")
	if at < 0 {
		return uri
	}
	return uri[:len(prefix)] + rest[at+1:]
}

func (s *Server) outputReading(w http.ResponseWriter, r *http.Request) {
	if s.localOutputs == nil || !s.localOutputs.Active(r.Context()) {
		writeError(w, http.StatusNotFound, "local_outputs_not_supported",
			"local outputs are not enabled on this bridge")
		return
	}
	reading, found, err := s.localOutputs.Reading(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge encountered an internal error", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "output_not_found",
			"no output with that id on this bridge")
		return
	}
	writeJSON(w, http.StatusOK, reading)
}
