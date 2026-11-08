package integrity

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"
)

// A waveform curve proven missing is rendered again on access (backlog
// B652), and every regeneration is a decode. So a lookup never decides
// alone that its curve may be rendered: the WHOLE catalog is judged first,
// before the first regeneration and then at most once per interval, and a
// judgment that refuses regenerates nothing at all, one track at a time or
// otherwise. A lookup sees one row, and every lookup over a directory that
// was emptied by hand (an `rm`, a restore of an older tree) proves its own
// row missing: judged one row at a time, browsing the library would be the
// mass re-analysis the variant reapers' guards exist to refuse, one track
// at a time.
//
// The judgment is the variant guards' own decision over the waveform family
// (sidecarFamily), so the two cannot disagree about what a tree that holds
// sidecars looks like:
//
//   - the directory missing, holding no curve, or one that cannot be read
//     is the mount-loss probe (WaveformDirSweepBlock): what a volume that
//     is away looks like. The last curve's deletion reads the same way and
//     is refused with it, conservatively, since the directory alone cannot
//     tell the two apart;
//   - more than WaveformRegenMaxMissingPercent of the rows that record a
//     curve with no curve at either location (missing, or a partial copy at
//     the canonical path), past massDeleteFloor, while the directory still
//     holds curves, is the relocation guard (massMissingRefusal): a tree
//     emptied past the threshold;
//   - the directory asked again by identity once every row is classified,
//     since a volume that went during the judgment leaves the rows after it
//     reading as missing (backlog B203's rule).
//
// A judgment that admits names the rows it found to be regenerated (the
// rows it may admit until the next one) and the waveform directory it saw,
// whose identity the job carries and asks again before it writes and before
// it publishes (analyze.RunWaveformRepair). A row whose curve went after the
// judgment is not admitted by it: the next judgment, once the interval has
// passed, weighs it with the rest.

// WaveformRegenMaxMissingPercent is the emptied threshold: the regeneration
// guard refuses once more than this share of the rows that record a curve
// have none at either location (past massDeleteFloor rows). The 20 the
// variant watcher's default and `analyze --gc`'s analysisGCMaxOrphanPercent
// take, as a constant for the reason the latter gives: the `integrity.*`
// knob names the variants catalog, and this is not it.
const WaveformRegenMaxMissingPercent = 20

// WaveformRegenInterval is how long a judgment stands: at most one judgment
// of the catalog per interval, the hour the variant watcher sweeps at by
// default. A judgment stats every row, which on a slow volume and a large
// library is minutes, so it is not made per lookup.
const WaveformRegenInterval = time.Hour

// WaveformRefusalKind names why a judgment refuses regeneration, a key the
// doctor and the log word.
type WaveformRefusalKind string

// The kinds of refusal a judgment makes.
const (
	// WaveformRefusalDirMissing: nothing is at the waveform directory's path.
	WaveformRefusalDirMissing WaveformRefusalKind = "waveformDirMissing"
	// WaveformRefusalDirAway: the waveform directory holds no curve while the
	// catalog records curves, or stopped being the directory the judgment
	// began on: what a volume that is away looks like.
	WaveformRefusalDirAway WaveformRefusalKind = "waveformDirAway"
	// WaveformRefusalDirUnreadable: the waveform directory cannot be stat'ed
	// or read, is not a directory, or no waveform directory is known.
	WaveformRefusalDirUnreadable WaveformRefusalKind = "waveformDirUnreadable"
	// WaveformRefusalEmptied: more of the catalog's curves are gone than the
	// threshold allows, while the directory still holds curves.
	WaveformRefusalEmptied WaveformRefusalKind = "waveformDirEmptied"
	// WaveformRefusalCatalogUnread: the catalog itself could not be listed.
	WaveformRefusalCatalogUnread WaveformRefusalKind = "waveformCatalogUnread"
)

// WaveformRefusalKinds is every kind a judgment can refuse as, for a caller
// that has to word each one (the doctor) and a test that holds it to that.
func WaveformRefusalKinds() []WaveformRefusalKind {
	return []WaveformRefusalKind{
		WaveformRefusalDirMissing, WaveformRefusalDirAway, WaveformRefusalDirUnreadable,
		WaveformRefusalEmptied, WaveformRefusalCatalogUnread,
	}
}

// WaveformJudgeOptions bounds one judgment.
type WaveformJudgeOptions struct {
	// MaxMissingPercent is the emptied threshold (WaveformRegenMaxMissingPercent
	// in production).
	MaxMissingPercent int
	// MaxRows stops the classification after this many rows, 0 for none:
	// the doctor's bound, since `/api/doctor` runs on a settings-page
	// render. A judgment that stopped refuses only where the rows it looked
	// at already prove the refusal (missing rows can only be added by the
	// rest), and is Undecided otherwise: it never admits.
	MaxRows int
}

// WaveformJudgment is one look at the whole waveform catalog.
type WaveformJudgment struct {
	// Dir is the waveform directory judged.
	Dir string
	// Rows is how many rows record a curve; Looked is how many of them the
	// judgment classified, Rows unless MaxRows stopped it.
	Rows, Looked int
	// Present, Relocated, Missing, Mismatched and Unknown partition the
	// rows looked at by LocateWaveform's verdict.
	Present, Relocated, Missing, Mismatched, Unknown int
	// Refusal is the kind of refusal, "" when the judgment does not refuse;
	// Reason says what was seen, in the operator's words.
	Refusal WaveformRefusalKind
	Reason  string
	// Undecided is a judgment MaxRows stopped before its rows could tell:
	// neither a refusal nor an admission.
	Undecided bool
	// Cancelled is a judgment its context ended: it decided nothing.
	Cancelled bool
	// At is when the judgment ended, on the guard's clock; zero from
	// JudgeWaveformCatalog alone.
	At time.Time

	// identity is the waveform directory the judgment saw, from the handle
	// its probe read it with, and repairable the rows it found to be
	// regenerated, by source path. Set only when the judgment admits.
	identity   os.FileInfo
	repairable map[string]struct{}
}

// Admits reports whether the judgment lets a row it found to be regenerated
// be regenerated. A catalog that records no curve admits, and has no row to
// admit.
func (j WaveformJudgment) Admits() bool {
	return j.Refusal == "" && !j.Undecided && !j.Cancelled
}

// Repairable reports whether the judgment admits sourcePath's curve.
func (j WaveformJudgment) Repairable(sourcePath string) bool {
	if !j.Admits() {
		return false
	}
	_, ok := j.repairable[sourcePath]
	return ok
}

// Identity is the waveform directory an admitting judgment saw, nil
// otherwise: what a job carries and asks again (fsutil.DirStillIs).
func (j WaveformJudgment) Identity() os.FileInfo {
	if !j.Admits() {
		return nil
	}
	return j.identity
}

// ToRegenerate is how many of the rows looked at have no curve at either
// location: missing, or a partial copy at the canonical path.
func (j WaveformJudgment) ToRegenerate() int { return j.Missing + j.Mismatched }

// JudgeWaveformCatalog judges whether a curve proven missing may be rendered
// again, from the whole catalog: rows is every row that records a curve
// (its waveform path not empty), dir the waveform directory now. The
// package's comment above is its rules. It reads, and writes nothing.
func JudgeWaveformCatalog(ctx context.Context, dir string, rows []WaveformSnapshot, opts WaveformJudgeOptions) WaveformJudgment {
	j := WaveformJudgment{Dir: dir, Rows: len(rows)}
	if dir == "" {
		j.Refusal, j.Reason = WaveformRefusalDirUnreadable, "no waveform directory is known"
		return j
	}
	if len(rows) == 0 {
		// Nothing recorded, so nothing to regenerate and nothing to refuse
		// (the mount-loss probe protects rows, and there are none): an
		// admission with no row in it.
		return j
	}
	block := WaveformDirSweepBlock(dir)
	if block.Reason != "" {
		j.Refusal, j.Reason = refusalOfBlock(block)
		return j
	}
	repairable := make(map[string]struct{})
	for _, r := range rows {
		if ctx.Err() != nil {
			j.Cancelled = true
			return j
		}
		if opts.MaxRows > 0 && j.Looked >= opts.MaxRows {
			break
		}
		j.Looked++
		d := DecideWaveform(dir, r)
		switch d.Verdict {
		case SidecarPresent:
			j.Present++
		case SidecarRelocated:
			j.Relocated++
		case SidecarMissing:
			j.Missing++
		case SidecarMismatched:
			j.Mismatched++
		default:
			j.Unknown++
		}
		if d.Regenerates() {
			repairable[r.SourcePath] = struct{}{}
		}
	}
	// The probe proved the directory was there when the judgment began and
	// says nothing about the rows after it: a volume that went during the
	// classification leaves every later row reading as missing, which the
	// ratio below would read as a tree emptied by hand.
	if changed := dirChanged(dir, block.Info, waveformSidecars, "check"); changed != "" {
		j.Refusal, j.Reason = WaveformRefusalDirAway, changed
		return j
	}
	if reason := massMissingRefusal(dir, waveformSidecars, j.ToRegenerate(), j.Rows, opts.MaxMissingPercent); reason != "" {
		// Sound on a judgment MaxRows stopped too: the rows it did not look
		// at can only add missing ones, and the denominator is the whole
		// catalog either way.
		j.Refusal, j.Reason = WaveformRefusalEmptied, reason
		return j
	}
	// Once more, after the ratio's own walk of the tree: a volume that went
	// in between leaves that walk on the local directory, and admitting on
	// it would hand the jobs an identity no longer at the path. The jobs ask
	// again in any case, before they write.
	if changed := dirChanged(dir, block.Info, waveformSidecars, "check"); changed != "" {
		j.Refusal, j.Reason = WaveformRefusalDirAway, changed
		return j
	}
	if j.Looked < j.Rows {
		j.Undecided = true
		return j
	}
	j.identity, j.repairable = block.Info, repairable
	return j
}

// refusalOfBlock is the refusal a waveform directory probe that did not
// pass reads as.
func refusalOfBlock(b SidecarDirBlock) (WaveformRefusalKind, string) {
	switch {
	case b.Missing:
		return WaveformRefusalDirMissing, b.Reason
	case b.Empty:
		return WaveformRefusalDirAway, b.Reason
	case b.Reason == "":
		return WaveformRefusalDirUnreadable, "the waveform directory could not be read"
	}
	return WaveformRefusalDirUnreadable, b.Reason
}

// WaveformRegenConfig is what a WaveformRegenGuard needs.
type WaveformRegenConfig struct {
	// List is the catalog: every row that records a curve.
	List func(ctx context.Context) ([]WaveformSnapshot, error)
	// Dir is the waveform directory now, asked at every Ask and judgment.
	Dir func() string
	// Admit is told, on the judgment's goroutine and after it is recorded,
	// each row asked for while it ran that it admits, with the directory it
	// saw: the caller enqueues it, after checking again whatever it checked
	// when it asked. Never called once Close has begun.
	Admit func(sourcePath, dir string, identity os.FileInfo)
	// Interval is how long a judgment stands (WaveformRegenInterval).
	Interval time.Duration
	// MaxMissingPercent is the emptied threshold
	// (WaveformRegenMaxMissingPercent).
	MaxMissingPercent int
	// Now is the clock a judgment's age is read on; time.Now when nil.
	Now func() time.Time
	// OnJudged, a test seam, is told each judgment once it is recorded,
	// before the rows it admits are.
	OnJudged func(WaveformJudgment)
}

// WaveformAnswerKind is a WaveformRegenGuard's answer to a lookup.
type WaveformAnswerKind uint8

const (
	// WaveformAdmitted: the standing judgment admits the row; enqueue it.
	WaveformAdmitted WaveformAnswerKind = iota
	// WaveformJudging: no judgment stands, and one is under way (begun by
	// this ask or an earlier one): the row is told to Admit if it admits it.
	WaveformJudging
	// WaveformRefused: the standing judgment refuses regeneration.
	WaveformRefused
	// WaveformNotJudged: the standing judgment found the row's curve where
	// it belongs; it went after that judgment, and the next one, once the
	// interval has passed, weighs it.
	WaveformNotJudged
	// WaveformClosed: the guard is closed.
	WaveformClosed
)

// WaveformAnswer is WaveformRegenGuard.Ask's answer.
type WaveformAnswer struct {
	Kind WaveformAnswerKind
	// Dir and Identity are the waveform directory the admitting judgment
	// saw, for WaveformAdmitted.
	Dir      string
	Identity os.FileInfo
	// Judgment is the judgment the answer is from, but for WaveformJudging
	// and WaveformClosed.
	Judgment WaveformJudgment
}

// WaveformRegenGuard holds the catalog's judgment for the lookups that prove
// a curve missing, and makes a judgment, on a goroutine of its own, when
// none stands: before the first regeneration, and then at most once per
// interval. One judgment runs at a time.
type WaveformRegenGuard struct {
	cfg    WaveformRegenConfig
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu sync.Mutex
	// last is the latest judgment that decided something; nil before the
	// first.
	last *WaveformJudgment
	// judging is whether a judgment is under way, pending the rows asked
	// for while none stood.
	judging bool
	pending map[string]struct{}
	closed  bool
	// refusal is the log latch of the refusals, the variant watcher's
	// (refusalLatch): one WARN when a streak starts or changes kind, then at
	// most one a day, and one Info line when a judgment admits after one.
	// Touched only under mu.
	refusal refusalLatch[WaveformRefusalKind]
}

// NewWaveformRegenGuard builds a guard whose judgments run on a context
// derived from parent; Close ends them.
func NewWaveformRegenGuard(parent context.Context, cfg WaveformRegenConfig) *WaveformRegenGuard {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Interval <= 0 {
		cfg.Interval = WaveformRegenInterval
	}
	ctx, cancel := context.WithCancel(parent)
	return &WaveformRegenGuard{cfg: cfg, ctx: ctx, cancel: cancel, pending: make(map[string]struct{})}
}

// Ask answers a lookup that proved sourcePath's curve missing (or a partial
// copy): admitted by the judgment that stands, refused by it, not judged by
// it, or to be told to Admit once the judgment under way answers. Cheap: it
// never waits for a judgment.
func (g *WaveformRegenGuard) Ask(sourcePath string) WaveformAnswer {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return WaveformAnswer{Kind: WaveformClosed}
	}
	dir := g.cfg.Dir()
	if j := g.last; j != nil && j.Dir == dir && g.cfg.Now().Sub(j.At) < g.cfg.Interval {
		switch {
		case !j.Admits():
			return WaveformAnswer{Kind: WaveformRefused, Judgment: *j}
		case j.Repairable(sourcePath):
			return WaveformAnswer{Kind: WaveformAdmitted, Dir: j.Dir, Identity: j.identity, Judgment: *j}
		}
		return WaveformAnswer{Kind: WaveformNotJudged, Judgment: *j}
	}
	g.pending[sourcePath] = struct{}{}
	if !g.judging {
		g.judging = true
		g.wg.Add(1)
		go g.judge(dir)
	}
	return WaveformAnswer{Kind: WaveformJudging}
}

// Wait waits for the judgment under way, and the admissions it tells, to end:
// once it returns, every row an ask so far was answered WaveformJudging for
// has been admitted or not.
func (g *WaveformRegenGuard) Wait() {
	g.wg.Wait()
}

// Last is the judgment that stands, and whether one does.
func (g *WaveformRegenGuard) Last() (WaveformJudgment, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == nil {
		return WaveformJudgment{}, false
	}
	return *g.last, true
}

// judge makes one judgment of the catalog under dir, records it, and tells
// Admit each pending row it admits.
func (g *WaveformRegenGuard) judge(dir string) {
	defer g.wg.Done()
	var j WaveformJudgment
	rows, err := g.cfg.List(g.ctx)
	switch {
	case g.ctx.Err() != nil:
		j = WaveformJudgment{Dir: dir, Cancelled: true}
	case err != nil:
		j = WaveformJudgment{Dir: dir, Refusal: WaveformRefusalCatalogUnread,
			Reason: "the waveform catalog could not be read: " + err.Error()}
	default:
		j = JudgeWaveformCatalog(g.ctx, dir, rows, WaveformJudgeOptions{MaxMissingPercent: g.cfg.MaxMissingPercent})
	}
	j.At = g.cfg.Now()

	g.mu.Lock()
	g.judging = false
	pending := g.pending
	g.pending = make(map[string]struct{})
	if j.Cancelled || g.closed {
		// Decided nothing: the shutdown stopped it. The rows asked for are
		// asked again by their next lookup.
		g.mu.Unlock()
		return
	}
	g.last = &j
	var admitted []string
	if j.Admits() {
		for p := range pending {
			if j.Repairable(p) {
				admitted = append(admitted, p)
			}
		}
	}
	g.noteLocked(j)
	g.mu.Unlock()

	if g.cfg.OnJudged != nil {
		g.cfg.OnJudged(j)
	}
	for _, p := range admitted {
		g.mu.Lock()
		closed := g.closed
		g.mu.Unlock()
		if closed {
			return
		}
		g.cfg.Admit(p, dir, j.identity)
	}
}

// noteLocked logs a judgment through the refusal latch.
func (g *WaveformRegenGuard) noteLocked(j WaveformJudgment) {
	if j.Refusal == "" {
		if ended := g.refusal.lift(); ended != "" {
			logger.Info(msgWaveformRegenLifted,
				slog.Int("rows", j.Rows),
				slog.Int("toRegenerate", j.ToRegenerate()),
				slog.String("ended", string(ended)),
			)
		}
		return
	}
	if g.refusal.refuse(j.At, j.Refusal) {
		logger.Warn(msgWaveformRegenRefused,
			slog.String("reason", j.Reason),
			slog.String("kind", string(j.Refusal)),
			slog.Int("rows", j.Rows),
			slog.String("hint", waveformRegenRefusedHint),
		)
	}
}

// Close ends the guard: no Ask is answered but WaveformClosed, no Admit is
// told after it, and the judgment under way is cancelled and joined,
// bounded by stopGrace, as the package's other loops are.
func (g *WaveformRegenGuard) Close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	g.cancel()
	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	t := time.NewTimer(stopGrace)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		logger.Warn("waveform regeneration: a judgment of the catalog did not stop within the grace; leaving it")
	}
}

// The guard's lines.
const (
	msgWaveformRegenRefused = "waveform regeneration: refusing to render missing curves again"
	msgWaveformRegenLifted  = "waveform regeneration: no longer refusing — the waveform catalog passes the checks"
)

// waveformRegenRefusedHint is the refusal's advice.
const waveformRegenRefusedHint = "no curve is rendered again while the waveform directory is missing, holds no curve " +
	"(a volume that is away, or a directory emptied by hand), cannot be read, or has lost more than a fifth of the " +
	"curves the catalog records. Mount the volume or put the curves back; `bridge doctor` says what was seen. " +
	"If the curves really are gone, `bridge analyze --force` renders them again from source. This logs when it " +
	"starts refusing and once a day while it keeps refusing."
