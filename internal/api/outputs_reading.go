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
// beside it, and the bridge's verdict on it. Every key is always present;
// a list is [] and a value the reading could not establish is null.
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

// ReadingMPDConfig is what mpd.conf, read at this reading, says that the
// verdict reads.
type ReadingMPDConfig struct {
	VolumeNormalization string                `json:"volumeNormalization"`
	AudioOutputFormat   string                `json:"audioOutputFormat"`
	Output              *ReadingMPDConfOutput `json:"output"`
}

// ReadingMPDConfOutput is the enabled alsa output whose device reaches
// this output's card.
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

// ReadingVerdict is the bridge's verdict: bit_exact, bit_exact_dop or
// withheld, and a code for each reason it is withheld.
type ReadingVerdict struct {
	Value   string   `json:"value"`
	Reasons []string `json:"reasons"`
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
// localOutputs.mu. proc and row are a /proc read and a look for the song's
// row still running past an earlier reading's deadline: a reading does not
// start a second of either while one runs, so a /proc file or a mount that
// hangs holds one goroutine, not one per call.
type readingState struct {
	snap   *readingSnap
	flight *readingFlight
	proc   *procRead
	row    *rowLook
}

type readingFlight struct {
	done chan struct{}
	snap *readingSnap
}

type procRead struct {
	done chan struct{}
	snap alsa.Snapshot
	err  error
}

type rowLook struct {
	done chan struct{}
	row  *songRow
}

// readingLimits are the reading's deadlines: the whole reading, the dial,
// each MPD command, the /proc read and the look for the song's row.
type readingLimits struct {
	build, dial, command, proc, file time.Duration
}

var defaultReadingLimits = readingLimits{
	build:   3 * time.Second,
	dial:    750 * time.Millisecond,
	command: 750 * time.Millisecond,
	proc:    500 * time.Millisecond,
	file:    500 * time.Millisecond,
}

// readingSnap is one reading of the box, which every output's view is cut
// from: MPD's status before the /proc read and after it with the read
// commands between, every card, mpd.conf as it is now, and the song's row.
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

	cards    alsa.Snapshot
	procCode string
	// picks are the cards GET /v1/outputs lists from these cards, and
	// chains every alsa output of mpd.conf's with the chain its device
	// takes: both are resolved once a reading, from the ALSA configuration
	// files, so a call the cache answers reads nothing.
	picks  []dacPick
	chains []outputChain

	row *songRow
}

// outputChain is one alsa audio_output of mpd.conf: the live outputs
// element of the same name, whether it is enabled, and the chain its
// device takes to a card when there is one.
type outputChain struct {
	conf    conf.AudioOutput
	live    *mpd.ServerOutput
	enabled bool
	chain   alsa.Chain
	ok      bool
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

	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.limits.build)
	snap := o.takeReading(bctx)
	cancel()
	o.mu.Lock()
	o.reading.snap = snap
	o.reading.flight = nil
	o.readings--
	f.snap = snap
	close(f.done)
	o.mu.Unlock()
	o.armIdle()
	return snap, nil
}

// takeReading reads the box once. MPD's status is read before the /proc
// read and again after it, with currentsong, replay_gain_status and
// outputs between, so a song change, a seek or a pause across the read
// shows. mpd.conf is read again here: the player rewrites it and restarts
// MPD when its settings change, and the probe's copy can be half a minute
// old. A step that cannot be had leaves a code and the rest of the
// reading stands.
func (o *localOutputs) takeReading(ctx context.Context) *readingSnap {
	cfg := o.outputConfig()
	_, rep, opt := o.probe(ctx)
	s := &readingSnap{rep: rep, password: cfg.Password}
	s.endpoint = rep.MPD.Endpoint
	if s.endpoint == "" {
		s.endpoint = opt.Endpoint
	}
	if rep.ConfPath != "" {
		if f, err := conf.ReadFile(confRoot(opt.Root, rep.ConfPath), rep.ConfPath, rep.ConfPath); err == nil {
			s.conf, s.confRead = f, true
		}
	}

	sess, code := o.readingSession(ctx, s.endpoint, cfg.Password)
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
	s.current, _ = ask(func(c outputSession, ctx context.Context) (mpd.Reply, error) { return c.CurrentSong(ctx) })
	s.gain, _ = ask(func(c outputSession, ctx context.Context) (mpd.Reply, error) { return c.ReplayGainStatus(ctx) })
	s.outputs, _ = ask(func(c outputSession, ctx context.Context) (mpd.Reply, error) { return c.Outputs(ctx) })
	s.cards, s.procCode = o.readCards(ctx)
	s.after, _ = ask(statusCommand)
	s.before, s.between, s.mpdCode = before, o.now().Sub(first), code
	// With no card there is nothing to resolve a device to, and the ALSA
	// configuration is not read.
	if s.procCode == "" && len(s.cards.Cards) > 0 {
		s.picks = dacsFrom(s.cards, s.rep)
		s.chains = outputChains(s)
	}
	if s.mpdCode == "" {
		s.row = o.lookUpSong(ctx, s)
	}
	s.at = o.now()
	return s
}

// outputChains resolves the chain of every alsa output mpd.conf names,
// enabled as the live outputs reply switches it. An output with no device
// plays to ALSA's default PCM.
func outputChains(s *readingSnap) []outputChain {
	if !s.confRead {
		return nil
	}
	cards := make([]alsa.Card, len(s.cards.Cards))
	for i, c := range s.cards.Cards {
		cards[i] = c.Card
	}
	live := mpd.ServerOutputs(s.outputs)
	root := alsa.ConfigRoot(s.cards.ProcRoot)
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
		oc.enabled = outputEnabled(c, oc.live)
		device := strings.TrimSpace(c.Device)
		if device == "" {
			device = "default"
		}
		oc.chain, oc.ok = alsa.ResolveOutputChain(root, device, cards)
		out = append(out, oc)
	}
	return out
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
// next command, so the session is dropped. broken reports that drop.
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
	if cctx.Err() != nil {
		return reply, signalpath.MPDTimeout, true
	}
	return reply, signalpath.MPDUnreachable, true
}

// readCards reads every card once, under the /proc deadline. A read still
// running past an earlier reading's deadline is not joined and not
// doubled: that reading's /proc read is as late as its own would be, so it
// is a timeout at once. Joining it would also hand this reading cards read
// before its first status, outside the bracket.
func (o *localOutputs) readCards(ctx context.Context) (alsa.Snapshot, string) {
	o.mu.Lock()
	if o.reading.proc != nil {
		o.mu.Unlock()
		return alsa.Snapshot{}, signalpath.ProcTimeout
	}
	p := &procRead{done: make(chan struct{})}
	o.reading.proc = p
	o.mu.Unlock()
	go func() {
		snap, err := o.cards()
		o.mu.Lock()
		p.snap, p.err = snap, err
		o.reading.proc = nil
		o.mu.Unlock()
		close(p.done)
	}()
	timer := time.NewTimer(o.limits.proc)
	defer timer.Stop()
	select {
	case <-p.done:
		switch {
		case p.err == nil:
			return p.snap, ""
		case errors.Is(p.err, fs.ErrNotExist):
			// No /proc/asound: a box with no sound card, which is a card
			// missing, not a /proc that could not be read.
			return alsa.Snapshot{Cards: []alsa.CardReading{}}, ""
		}
		return alsa.Snapshot{}, signalpath.ProcUnreadable
	case <-timer.C:
	case <-ctx.Done():
	}
	return alsa.Snapshot{}, signalpath.ProcTimeout
}

// lookUpSong names the song MPD is on as one of this bridge's rows, under
// the look's deadline: os.Stat on a dead network mount does not return.
// A look still running from an earlier reading is not doubled, and the
// song is then not named.
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
	o.mu.Lock()
	if o.reading.row != nil {
		o.mu.Unlock()
		return nil
	}
	look := &rowLook{done: make(chan struct{})}
	o.reading.row = look
	o.mu.Unlock()
	lctx, cancel := context.WithTimeout(ctx, o.limits.file)
	defer cancel()
	go func() {
		row := o.findRow(lctx, uri, musicDir, roots, base)
		o.mu.Lock()
		look.row = row
		o.reading.row = nil
		o.mu.Unlock()
		close(look.done)
	}()
	select {
	case <-look.done:
		return look.row
	case <-lctx.Done():
		return nil
	}
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
// not be read, an id they could have named cannot be told from one they
// could not: it is answered, unknown, rather than refused.
func (o *localOutputs) readingElement(s *readingSnap, id string) (readingElement, bool) {
	if s.procCode != "" {
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
	feeding := feedingOutputs(s, elem.pick)

	var reasons []string
	if s.mpdCode != "" {
		reasons = append(reasons, s.mpdCode)
	}
	if s.procCode != "" {
		reasons = append(reasons, s.procCode)
	} else if elem.pick == nil {
		reasons = append(reasons, signalpath.NoCard)
	}
	state := "whole"
	if len(reasons) > 0 {
		state = "unknown"
	} else if settle := readingSettling(s); len(settle) > 0 {
		state, reasons = "settling", settle
	}
	verdict := signalpath.WithheldFor(reasons...)
	if state == "whole" {
		verdict = signalpath.Judge(readingFacts(s, elem.pick, feeding))
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
		Verdict:      ReadingVerdict{Value: verdict.Value, Reasons: verdict.Reasons},
	}
	if s.mpdCode == "" {
		out.MPD = readingMPD(s, feeding, redact)
		out.Song = readingSong(s, redact)
	}
	if elem.pick != nil && s.procCode == "" {
		out.DAC = readingDAC(s, elem.pick, feeding)
	}
	return out, true
}

// readingSettling is the settling codes: the two status reads, and the
// song currentsong named between them.
func readingSettling(s *readingSnap) []string {
	status := func(r mpd.Reply) signalpath.Status {
		state, _ := r.Get("state")
		song, _ := r.Get("songid")
		elapsed, _ := r.Get("elapsed")
		return signalpath.Status{State: state, SongID: song, Elapsed: elapsed}
	}
	codes := signalpath.Settling(status(s.before), status(s.after), s.between)
	if id, ok := s.current.Get("Id"); ok {
		if after, _ := s.after.Get("songid"); after != id {
			codes = append(codes, signalpath.SongChanged)
		}
	}
	return signalpath.Sorted(codes)
}

// feedingOutput is an enabled alsa output of MPD's whose device reaches
// the card: mpd.conf's block, the live outputs element of the same name,
// and the chain.
type feedingOutput struct {
	conf  conf.AudioOutput
	live  *mpd.ServerOutput
	chain alsa.Chain
}

// feedingOutputs is every enabled alsa output, as mpd.conf names them and
// the live outputs reply switches them, whose device reaches pick's card
// and PCM device through a chain the bridge resolves.
func feedingOutputs(s *readingSnap, pick *dacPick) []feedingOutput {
	if pick == nil {
		return nil
	}
	var out []feedingOutput
	for _, oc := range s.chains {
		if !oc.enabled || !oc.ok || oc.chain.Card != pick.dac.Card || oc.chain.Device != pick.pcm {
			continue
		}
		out = append(out, feedingOutput{conf: oc.conf, live: oc.live, chain: oc.chain})
	}
	return out
}

// outputEnabled is the live outputenabled when MPD answered for the
// output, otherwise the file's enabled, which MPD takes as yes when unset.
func outputEnabled(c conf.AudioOutput, live *mpd.ServerOutput) bool {
	if live != nil {
		return live.Enabled
	}
	if on, ok := signalpath.ParseBool(c.Enabled); ok {
		return on
	}
	return true
}

// outputDoP is whether the output sends DSD as DoP: the live dop
// attribute, which outputset changes at run time, else the file's.
func outputDoP(f feedingOutput) bool {
	if f.live != nil {
		if v, ok := f.live.Attribute("dop"); ok {
			on, _ := signalpath.ParseBool(v)
			return on
		}
	}
	on, _ := signalpath.ParseBool(f.conf.DOP)
	return on
}

// outputAllowedFormats is the live allowed_formats attribute, else the
// file's.
func outputAllowedFormats(f feedingOutput) string {
	if f.live != nil {
		if v, ok := f.live.Attribute("allowed_formats"); ok {
			return v
		}
	}
	return f.conf.AllowedFormats
}

func outputMixerType(s *readingSnap, f feedingOutput) string {
	return signalpath.EffectiveMixerType(f.conf.MixerType, f.conf.MixerEnabled, s.conf.MixerType)
}

// readingFacts is what the verdict judges for one output.
func readingFacts(s *readingSnap, pick *dacPick, feeding []feedingOutput) signalpath.Facts {
	state, _ := s.after.Get("state")
	audio, _ := s.after.Get("audio")
	mode, _ := s.gain.Get("replay_gain_mode")
	xfade, _ := s.after.Get("xfade")
	mixramp, _ := s.after.Get("mixrampdelay")
	f := signalpath.Facts{
		State:  state,
		Audio:  audio,
		Source: sourceFacts(s.row),
		MPD: signalpath.MPD{
			ReplayGainMode:      mode,
			Crossfade:           xfade,
			MixRampDelay:        mixramp,
			ConfigRead:          s.confRead,
			VolumeNormalization: s.conf.VolumeNormalization,
			AudioOutputFormat:   s.conf.AudioOutputFormat,
			Outputs:             len(feeding),
		},
	}
	if len(feeding) == 1 {
		one := feeding[0]
		f.MPD.Output = signalpath.Output{
			MixerType:      outputMixerType(s, one),
			Format:         one.conf.Format,
			AllowedFormats: outputAllowedFormats(one),
			Filters:        one.conf.Filters,
			DoP:            outputDoP(one),
			Stages:         one.chain.Stages,
		}
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
		if sub.Owner != nil && sub.Owner.Process != nil {
			f.PCM.Owner = *sub.Owner.Process
		}
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
func readingMPD(s *readingSnap, feeding []feedingOutput, redact func(string) string) *ReadingMPD {
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
		if len(feeding) == 1 {
			one := feeding[0]
			m.Config.Output = &ReadingMPDConfOutput{
				Name:           redact(one.conf.Name),
				Device:         redact(one.conf.Device),
				Chain:          append([]string{}, one.chain.Stages...),
				MixerType:      outputMixerType(s, one),
				MixerControl:   redact(one.conf.MixerControl),
				DoP:            outputDoP(one),
				Format:         redact(one.conf.Format),
				AllowedFormats: redact(outputAllowedFormats(one)),
				Filters:        redact(one.conf.Filters),
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
func readingDAC(s *readingSnap, pick *dacPick, feeding []feedingOutput) *ReadingDAC {
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
	if len(feeding) == 1 && outputMixerType(s, feeding[0]) == "hardware" {
		control := feeding[0].conf.MixerControl
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
