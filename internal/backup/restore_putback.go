package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/atlasharvest"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// ErrRestoreStopped is a database-only restore, or its recovery, that stopped
// with its journal kept: an entry where none was due, an original whose
// content is not what the journal recorded, a barrier that failed. Its words
// say why and where every original is; `bridge restore --database-only`
// takes it up again once what it names is resolved, and docs/backup.md says
// how to put the originals back by hand.
var ErrRestoreStopped = errors.New("the restore of the library database stopped, and its journal is kept")

// ErrRestoreByAnotherVersion is the recovery's refusal of a journal another
// bridge version wrote: it moves nothing.
var ErrRestoreByAnotherVersion = errors.New("the unfinished restore of the library database was begun by another bridge version")

// RestoreStoppedError is ErrRestoreStopped with its reason and the place of
// every original.
type RestoreStoppedError struct {
	// Phase is the journal's phase as it stands.
	Phase string
	// Why is what stopped it.
	Why string
	// Where says where each original is, one line each.
	Where []string
}

func (e *RestoreStoppedError) Error() string {
	s := fmt.Sprintf("%s (phase %s): %s", ErrRestoreStopped, e.Phase, e.Why)
	if len(e.Where) > 0 {
		s += "; " + strings.Join(e.Where, "; ")
	}
	return s
}

// Is reads the error as ErrRestoreStopped.
func (e *RestoreStoppedError) Is(target error) bool { return target == ErrRestoreStopped }

// placedOriginal is one original and what stands at its live name and at its
// name in the run's folder.
type placedOriginal struct {
	fact FileFact
	live entryFact
	arch entryFact
}

// restoreLayout is everything the put-back reads before it moves anything:
// each original's two places, and the staging name.
type restoreLayout struct {
	dataDir, archive, stage string
	stageAt                 entryFact
	originals               []placedOriginal
}

// readLayout reads every name the put-back may move or check: each
// original's live name and its name in the run's folder, and the staging
// name, each regular file hashed.
func readLayout(dataDir string, j *RestoreJournal) (*restoreLayout, error) {
	l := &restoreLayout{dataDir: dataDir, archive: j.ArchiveDir(dataDir), stage: manifest.RestoreStagingPath(dataDir)}
	var err error
	if l.stageAt, err = readEntry(l.stage); err != nil {
		return nil, fmt.Errorf("read the staging name: %w", err)
	}
	for _, name := range originalNames() {
		p := placedOriginal{fact: j.Original(name)}
		if p.live, err = readEntry(filepath.Join(dataDir, name)); err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		if p.arch, err = readEntry(filepath.Join(l.archive, name)); err != nil {
			return nil, fmt.Errorf("read %s in %s: %w", name, l.archive, err)
		}
		l.originals = append(l.originals, p)
	}
	return l, nil
}

// where says, for each original, where it is: what its two names hold.
func (l *restoreLayout) where() []string {
	var out []string
	for _, p := range l.originals {
		if !p.fact.Present {
			out = append(out, fmt.Sprintf("%s: there was none (its name holds %s, the folder of originals %s)", p.fact.Name, p.live, p.arch))
			continue
		}
		out = append(out, fmt.Sprintf("%s (%d bytes): its name holds %s, and %s holds %s", p.fact.Name, p.fact.Size, p.live,
			filepath.Join(l.archive, p.fact.Name), p.arch))
	}
	return out
}

// putBackPlan is what the put-back will do, decided before anything moves.
type putBackPlan struct {
	// installedLive: the live database is the prepared copy, which goes back
	// to the staging name before any original returns.
	installedLive bool
	// back are the originals in the run's folder, the database first.
	back []string
}

// classify decides the put-back from what the layout holds (backlog B653
// PR 2b1 §4.4 step 2), before anything moves: each original recorded there
// must be in the run's folder with its recorded size and SHA-256 and its live
// name empty (or, for the database alone, holding the prepared copy while
// the staging name is empty), or at its live name with its recorded content
// and not in the folder (never moved, or put back already); each recorded
// absent must be absent from both, but for a live database that is the
// prepared copy. The live database is the installed copy only while its
// original is in the folder or was recorded absent: once the original is
// back it is the original, whatever bytes the two share. Anything else
// stops, naming every original's place: an entry planted at a live name, a
// copy of an original beside its archived self, a second database, a
// sidecar beside the prepared copy, an archived entry that is no regular
// file, other content, an original in neither place.
func classify(l *restoreLayout, j *RestoreJournal) (*putBackPlan, error) {
	stop := func(why string, args ...any) error {
		return &RestoreStoppedError{Phase: j.Phase, Why: fmt.Sprintf(why, args...), Where: l.where()}
	}
	plan := &putBackPlan{}
	dbName := originalNames()[0]
	liveAlready := 0
	for _, p := range l.originals {
		name := p.fact.Name
		installed := name == dbName && p.live.matches(j.Prepared.Size, j.Prepared.SHA256)
		if !p.fact.Present {
			switch {
			case p.arch.kind != entryAbsent:
				return nil, stop("%s holds %s, where the restore moved none", filepath.Join(l.archive, name), p.arch)
			case p.live.kind == entryAbsent:
			case installed:
				plan.installedLive = true
			default:
				return nil, stop("%s holds %s, where the restore recorded none", name, p.live)
			}
			continue
		}
		switch {
		case p.arch.matches(p.fact.Size, p.fact.SHA256):
			switch {
			case p.live.kind == entryAbsent:
			case installed:
				plan.installedLive = true
			default:
				return nil, stop("%s holds %s while its original is in %s", name, p.live, l.archive)
			}
			plan.back = append(plan.back, name)
		case p.arch.kind != entryAbsent:
			return nil, stop("%s holds %s, which is not the original the restore moved there", filepath.Join(l.archive, name), p.arch)
		case p.live.matches(p.fact.Size, p.fact.SHA256):
			if name != dbName {
				liveAlready++
			}
		case p.live.kind == entryAbsent:
			return nil, stop("the original %s is neither at its name nor in %s", name, l.archive)
		default:
			return nil, stop("%s holds %s, which is not the original the restore recorded", name, p.live)
		}
	}
	if plan.installedLive {
		if l.stageAt.kind != entryAbsent {
			return nil, stop("the staging name %s holds %s while the prepared copy is at %s", filepath.Base(l.stage), l.stageAt, dbName)
		}
		if liveAlready > 0 {
			return nil, stop("a sidecar of the original database is beside the prepared copy at %s", dbName)
		}
	} else if l.stageAt.kind != entryAbsent && !l.stageAt.matches(j.Prepared.Size, j.Prepared.SHA256) {
		return nil, stop("the staging name %s holds %s, which is not the prepared copy", filepath.Base(l.stage), l.stageAt)
	}
	return plan, nil
}

// putBack is the one routine that puts a database-only restore's originals
// back (§4.4), for a failure in its swap and for the bounded path alike: it
// reads and classifies every name first (classify), changing nothing where
// that stops; then writes `puttingBack` durably, unless the journal says it
// already; moves a live prepared copy aside to the staging name; moves each
// original in the run's folder back to its name, the database first and then
// its sidecars, each by the no-replace rename, the folder and the data dir
// synced after each group; then reads the whole set again, and only once
// every original recorded there is at its name with its content and every
// one recorded absent is absent, removes the journal. From there the
// put-back is finished: what is left (the staged copy, the run's record and
// folder) is tidied by name, and a failure there is said, never a stop.
// Every stop keeps the journal and names every original's place. It reads
// no snapshot, opens no database, and takes no lock its caller does not hold.
func putBack(dataDir string, j *RestoreJournal, warn func(string)) error {
	l, err := readLayout(dataDir, j)
	if err != nil {
		return &RestoreStoppedError{Phase: j.Phase, Why: err.Error()}
	}
	plan, err := classify(l, j)
	if err != nil {
		return err
	}
	if j.Phase != PhasePuttingBack {
		if err := setJournalPhase(dataDir, j, PhasePuttingBack); err != nil {
			return &RestoreStoppedError{Phase: j.Phase, Why: err.Error(), Where: l.where()}
		}
	}
	step("P:published")
	stopped := func(err error) error {
		after, rerr := readLayout(dataDir, j)
		if rerr != nil {
			return &RestoreStoppedError{Phase: j.Phase, Why: err.Error()}
		}
		return &RestoreStoppedError{Phase: j.Phase, Why: err.Error(), Where: after.where()}
	}
	dbName := originalNames()[0]
	if plan.installedLive {
		if err := fsutil.RenameNoReplace(filepath.Join(dataDir, dbName), l.stage); err != nil {
			return stopped(fmt.Errorf("move the prepared copy aside to %s: %w", filepath.Base(l.stage), err))
		}
		if err := restoreBarrier(dataDir, dataDir); err != nil {
			return stopped(err)
		}
		step("P:aside")
	}
	for _, group := range [][]string{{dbName}, sidecarOriginalNames()} {
		moved := false
		for _, name := range group {
			if !slices.Contains(plan.back, name) {
				continue
			}
			if err := fsutil.RenameNoReplace(filepath.Join(l.archive, name), filepath.Join(dataDir, name)); err != nil {
				return stopped(fmt.Errorf("put %s back: %w", name, err))
			}
			moved = true
			step("P:back:" + name)
		}
		if !moved {
			continue
		}
		if err := restoreBarrier(dataDir, l.archive); err != nil {
			return stopped(err)
		}
		if err := restoreBarrier(dataDir, dataDir); err != nil {
			return stopped(err)
		}
	}
	for _, name := range originalNames() {
		f := j.Original(name)
		at, err := readEntry(filepath.Join(dataDir, name))
		if err != nil {
			return stopped(fmt.Errorf("read %s back: %w", name, err))
		}
		if f.Present && !at.matches(f.Size, f.SHA256) || !f.Present && at.kind != entryAbsent {
			return stopped(fmt.Errorf("%s holds %s once put back, not what the restore recorded", name, at))
		}
	}
	if err := removeJournal(dataDir); err != nil {
		return stopped(err)
	}
	step("P:journal")
	tidyAfterRestore(dataDir, warn)
	tidyRun(l.archive, warn)
	return nil
}

// tidyRun removes a put-back run's record and its folder where the record is
// all the folder holds; otherwise both stay. A failure is said.
func tidyRun(archive string, warn func(string)) {
	entries, err := os.ReadDir(archive)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil || len(entries) != 1 || entries[0].Name() != originalsRecordName || !entries[0].Type().IsRegular() {
		return
	}
	if err := os.Remove(filepath.Join(archive, originalsRecordName)); err != nil {
		warn(fmt.Sprintf("restore: the put-back is finished; its record %s was left (%v)", filepath.Join(archive, originalsRecordName), err))
		return
	}
	if err := os.Remove(archive); err != nil {
		warn(fmt.Sprintf("restore: the put-back is finished; its empty folder %s was left (%v)", archive, err))
	}
}

// stagedNames are the regular files a database-only restore leaves by name in
// the data dir: the staged copy and its sidecars. Their lock files
// (bridge.db-restore-open.lock and -migrate.lock) are never among them.
func stagedNames(dataDir string) []string {
	stage := manifest.RestoreStagingPath(dataDir)
	return []string{stage, stage + "-wal", stage + "-shm", stage + "-journal"}
}

// stagedPrefixes are the prefixes of the restore's temporary files in the
// data dir: a copy of the snapshot's database in flight, and a journal or a
// record staged before its rename.
func stagedPrefixes() []string {
	return []string{filepath.Base(manifest.RestoreStagingPath(".")) + ".tmp-", stagedJSONPrefix()}
}

// removeStaged removes, by name, every regular file the restore stages in
// the data dir (stagedNames, stagedPrefixes): never a lock file, a link or a
// directory. It answers the first failure.
func removeStaged(dataDir string) error {
	var first error
	remove := func(path string) {
		fi, err := os.Lstat(path)
		if err != nil || !fi.Mode().IsRegular() {
			return
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) && first == nil {
			first = err
		}
	}
	for _, p := range stagedNames(dataDir) {
		remove(p)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if first == nil {
			first = err
		}
		return first
	}
	for _, e := range entries {
		for _, prefix := range stagedPrefixes() {
			if strings.HasPrefix(e.Name(), prefix) {
				remove(filepath.Join(dataDir, e.Name()))
			}
		}
	}
	return first
}

// tidyAfterRestore removes the staged leftovers once a restore, or its
// put-back, is finished (its journal removed): a failure is said, and the
// next restore removes them by name.
func tidyAfterRestore(dataDir string, warn func(string)) {
	if err := removeStaged(dataDir); err != nil {
		warn(fmt.Sprintf("restore: it is finished; a staged file was left in the data directory (%v), which the next restore removes", err))
	}
}

// harvestStateIn is the harvest state the database-only restore rewinds in
// dataDir.
func harvestStateIn(dataDir string) string { return filepath.Join(dataDir, AtlasHarvestFileName) }

// rewindHarvest is C6's rewind of the harvest's sync position (§4.3, C6): the
// state opened as serve opens it, reset (atlasharvest's ResetSyncPosition,
// which makes no file where none stands), and then a strict barrier, written
// or not: a file that stands is synced through a handle of its own, and the
// data dir synced, so the rewind is durable before the journal goes. Where
// no file stands, the directory alone is synced.
func rewindHarvest(dataDir, path string) error {
	st, err := atlasharvest.OpenStateStore(path)
	if err != nil {
		return fmt.Errorf("open the harvest state: %s", fsutil.ErrorWithoutPath(err))
	}
	present, err := st.ResetSyncPosition()
	if err != nil {
		return fmt.Errorf("rewind the harvest's sync position: %s", fsutil.ErrorWithoutPath(err))
	}
	if present {
		if err := syncFileAt(path); err != nil {
			return fmt.Errorf("sync the harvest state: %s", fsutil.ErrorWithoutPath(err))
		}
	}
	return restoreBarrier(dataDir, filepath.Dir(path))
}

// syncFileAt syncs the regular file at path through a handle of its own:
// read-only through fsutil.OpenAsFile off Windows, for writing on Windows,
// whose FlushFileBuffers needs it.
func syncFileAt(path string) error {
	var f *os.File
	var err error
	if runtime.GOOS == "windows" {
		f, err = os.OpenFile(path, os.O_RDWR, 0)
	} else {
		f, _, err = fsutil.OpenAsFile(path)
	}
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// completeRestore is C6, for a restore whose prepared copy is installed and
// recorded so (`installed`): the harvest rewound durably, and only then the
// journal removed, and then the staged leftovers. A rewind or a barrier that
// fails stops it with `installed` kept, which `bridge restore
// --database-only` runs again. Once the journal is gone the restore is
// finished; what is left is tidied, and a failure there said.
func completeRestore(dataDir string, j *RestoreJournal, harvest string, warn func(string)) error {
	step("C6:rewind")
	if err := rewindHarvest(dataDir, harvest); err != nil {
		return &RestoreStoppedError{Phase: j.Phase, Why: "the database is restored, and the harvest's sync position could not be rewound durably: " + err.Error()}
	}
	step("C6:rewound")
	if err := removeJournal(dataDir); err != nil {
		return &RestoreStoppedError{Phase: j.Phase, Why: "the database is restored, and " + err.Error()}
	}
	step("C6:journal")
	tidyAfterRestore(dataDir, warn)
	return nil
}

// FinishPlan is what the recovery found, for its caller's consent: the
// journal, what it will do, and where each original is.
type FinishPlan struct {
	Journal *RestoreJournal
	// Archive is the run's folder of originals, absolute.
	Archive string
	// Complete is true for a restore installed and recorded so, which the
	// recovery completes; false for one it puts back.
	Complete bool
	// Where says where each original is.
	Where []string
}

// PlanFinishDatabaseRestore reads the journal standing in dataDir and what
// the data dir holds, and says what `bridge restore --database-only` will do
// with it, changing nothing (§4.5 steps 1 and 2): a journal that does not
// read refuses (ErrRestoreJournalUnread), one another bridge version wrote
// refuses (ErrRestoreByAnotherVersion), an `installed` one is completed
// where the live database is the prepared copy with no sidecar beside it,
// and any other is put back where the put-back's classification takes the
// layout; every other answer stops (ErrRestoreStopped).
func PlanFinishDatabaseRestore(dataDir string) (*FinishPlan, error) {
	j, err := ReadRestoreJournal(dataDir)
	if err != nil {
		return nil, err
	}
	if j.BridgeVersion != version.ServerVersion {
		return nil, fmt.Errorf("%w: finish it with the bridge version that began it (%s); this is %s", ErrRestoreByAnotherVersion, j.BridgeVersion, version.ServerVersion)
	}
	l, err := readLayout(dataDir, j)
	if err != nil {
		return nil, &RestoreStoppedError{Phase: j.Phase, Why: err.Error()}
	}
	plan := &FinishPlan{Journal: j, Archive: l.archive, Where: l.where()}
	if j.Phase == PhaseInstalled {
		if err := checkInstalled(l, j); err != nil {
			return nil, err
		}
		plan.Complete = true
		return plan, nil
	}
	if _, err := classify(l, j); err != nil {
		return nil, err
	}
	return plan, nil
}

// checkInstalled requires, of an `installed` restore, the prepared copy at
// the database's name and no sidecar beside it.
func checkInstalled(l *restoreLayout, j *RestoreJournal) error {
	stop := func(why string, args ...any) error {
		return &RestoreStoppedError{Phase: j.Phase, Why: fmt.Sprintf(why, args...), Where: l.where()}
	}
	for i, p := range l.originals {
		if i == 0 {
			if !p.live.matches(j.Prepared.Size, j.Prepared.SHA256) {
				return stop("%s holds %s, not the prepared copy the restore installed", p.fact.Name, p.live)
			}
			continue
		}
		if p.live.kind != entryAbsent {
			return stop("%s stands beside the restored database: something opened it, or put it there", p.fact.Name)
		}
	}
	return nil
}

// FinishOptions are the recovery's: its warning sink and the harvest state
// it rewinds (harvestStateIn's where empty).
type FinishOptions struct {
	Warn         func(string)
	HarvestState string
}

// FinishDatabaseRestore is the bounded path (§4.5): with the journal of an
// unfinished database-only restore standing in dataDir, it plans again
// (PlanFinishDatabaseRestore) and requires the plan its caller consented to,
// the same journal in the same phase, then completes an `installed` restore
// (C6) or puts any other back (putBack). It holds the store's lifetime lock
// exclusively (counted on its caller's), and nothing else: it reads no
// snapshot, opens no database, takes no install marker or config lock, and
// never prepares or installs again.
func FinishDatabaseRestore(dataDir string, consented *FinishPlan, opts FinishOptions) error {
	warn := opts.Warn
	if warn == nil {
		warn = func(string) {}
	}
	hold, err := manifest.HoldStoreExclusive(manifest.DefaultDBPath(dataDir))
	if err != nil {
		return fmt.Errorf("%w; nothing was moved", err)
	}
	defer hold.Release()
	plan, err := PlanFinishDatabaseRestore(dataDir)
	if err != nil {
		return err
	}
	if consented == nil || plan.Journal.ID != consented.Journal.ID || plan.Journal.Phase != consented.Journal.Phase || plan.Complete != consented.Complete {
		return &RestoreStoppedError{Phase: plan.Journal.Phase, Why: "the unfinished restore changed after it was described; run the command again", Where: plan.Where}
	}
	if plan.Complete {
		harvest := opts.HarvestState
		if harvest == "" {
			harvest = harvestStateIn(dataDir)
		}
		return completeRestore(dataDir, plan.Journal, harvest, warn)
	}
	return putBack(dataDir, plan.Journal, warn)
}

// RestoreRemedyArchive is where the originals of the restore whose journal
// stands in dataDir are, absolute: the run's folder its journal names, read
// as the recovery reads it, else the folder of every run's originals.
func RestoreRemedyArchive(dataDir string) string {
	if j, err := ReadRestoreJournal(dataDir); err == nil {
		return j.ArchiveDir(dataDir)
	}
	return filepath.Join(dataDir, OriginalsDirName)
}
