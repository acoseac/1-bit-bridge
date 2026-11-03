package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// playerDACCheckName is the doctor's line on whether the bridge's own player
// may open its DAC's node (B676).
const playerDACCheckName = "player-dac"

// dacAccessBound bounds the line's reading of the box, as the console's
// readings are bounded (consoleReadBound).
const dacAccessBound = 3 * time.Second

// errDACReadingUnderWay is a reading of the box an earlier line started that
// has not returned yet: one stuck in the kernel holds the next back.
var errDACReadingUnderWay = errors.New("an earlier reading of the box has not returned yet")

// dacNode is what lstat says of a DAC's node: the group that owns it, and
// whether it is a character device, the only kind the player opens.
type dacNode struct {
	gid  uint32
	char bool
}

// dacNodeProbe is what the line asks of a DAC's node, neither of which opens
// it: lstat's answer (statDACNode), and whether this process may open it for
// playback (accessForPlayback).
type dacNodeProbe struct {
	stat   func(path string) (dacNode, error)
	access func(path string) error
}

// hostDACNodes is how the line asks the host's own DAC nodes, named here
// alone, as hostTrees names the host's trees: this package's TestMain
// replaces it, before any test runs and never again, with a probe that
// refuses and records each node it is asked about, and the binary fails on
// one, so no test asks the box's own /dev/snd.
var hostDACNodes = dacNodeProbe{stat: statDACNode, access: accessForPlayback}

// dacCreds are the credentials an access is judged by: the effective user
// and its name ("" when the user database names none), the effective group
// and the supplementary groups.
type dacCreds struct {
	euid   int
	name   string
	egid   uint32
	groups []uint32
}

// in reports whether gid is the effective group or a supplementary one.
func (c dacCreds) in(gid uint32) bool { return c.egid == gid || slices.Contains(c.groups, gid) }

// dacService is the running bridge as /proc shows it: its pid, the user it
// runs as (its effective uid), and its effective and supplementary groups.
type dacService struct {
	pid    int
	euid   int
	egid   uint32
	groups []uint32
}

// in reports whether gid is the running bridge's effective group or a
// supplementary one.
func (s dacService) in(gid uint32) bool { return s.egid == gid || slices.Contains(s.groups, gid) }

// dacAccessHost is everything the line reads of the host, each part a seam a
// test stands in for; cliDACHost and servingDACHost are production's.
type dacAccessHost struct {
	// goos is the host: the line is Linux's alone.
	goos string
	// serving is a line the serving process makes of itself (the console's
	// doctor): the process it checks is the bridge, so no running bridge is
	// looked for beside it.
	serving bool
	// boot is the binding the serving player started with, nil for none or
	// for bridge doctor: the console's line says when the config names
	// another, which the player opens after a restart.
	boot *config.PlayerDevice
	// locate resolves a binding to the unit it names now, through the bounded
	// binding machinery (playback.BindingLocator over the box's trees).
	locate func(ctx context.Context, d config.PlayerDevice) (playback.Unit, error)
	// nodes asks a node, opening nothing.
	nodes dacNodeProbe
	// groupName names a gid, "" where the group database has no name for it.
	groupName func(gid uint32) string
	// creds are this process's credentials.
	creds func() (dacCreds, error)
	// accountGroups are the gids the user named belongs to in the group
	// database, its primary group among them: the account's, which a login
	// started now would have, never a process's.
	accountGroups func(name string) ([]uint32, error)
	// service is the bridge that serves the data directory now, read from its
	// pid file and /proc (bridge doctor alone, nil for the console's), false
	// when none runs or it cannot be read.
	service func(dataDir string) (dacService, bool)
	// sudo reports whether the box has sudo, which the fix's commands are
	// written with; without it they are root's to run as they stand.
	sudo func() bool
	// slot is the one reading of the box under way, taken before a reading
	// starts and let go when it returns, which a reading stuck in the kernel
	// may do after the line has gone; nil holds nothing back.
	slot chan struct{}
}

// hostDACAccess is the line's host over trees and the host's own nodes,
// users and groups, for either doctor.
func hostDACAccess(trees outputTrees) dacAccessHost {
	return dacAccessHost{
		goos: runtime.GOOS,
		locate: func(ctx context.Context, d config.PlayerDevice) (playback.Unit, error) {
			return playback.BindingLocator{Device: d, Observer: trees.observer()}.Locate(ctx)
		},
		nodes:         hostDACNodes,
		groupName:     lookupGroupName,
		creds:         processCreds,
		accountGroups: lookupAccountGroups,
		sudo:          func() bool { _, err := exec.LookPath("sudo"); return err == nil },
		slot:          make(chan struct{}, 1),
	}
}

// cliDACHost is the line's host for bridge doctor: this process's own
// credentials, and, beside them, the running bridge's, read from its pid
// file and its /proc entry under trees' /proc.
func cliDACHost(trees outputTrees) dacAccessHost {
	h := hostDACAccess(trees)
	h.service = func(dataDir string) (dacService, bool) {
		return readServingBridge(trees.proc, filepath.Join(dataDir, "server.pid"))
	}
	return h
}

// servingDACHost is the line's host for the console's doctor, which runs in
// the serving process: its own credentials, the bridge's, and boot, the
// binding its player started with (nil for a player serve did not start).
func servingDACHost(trees outputTrees, boot *config.PlayerDevice) dacAccessHost {
	h := hostDACAccess(trees)
	h.serving, h.boot = true, boot
	return h
}

// doctorDACHost is the host bridge doctor's line reads (cliDACHost over
// hostTrees), a seam so a test stands in for a Linux box with a DAC.
// Production never changes it, and a test that does must not call
// t.Parallel.
var doctorDACHost = func() dacAccessHost { return cliDACHost(hostTrees) }

// doctorPlayerDAC is doctor.Deps.PlayerDAC for cfg over host, the one source
// bridge doctor and the console's doctor read (buildDoctorDepsWith): its
// line, or none where cfg does not ask for the player or the host is not
// Linux.
func doctorPlayerDAC(cfg *config.Config, host dacAccessHost) func(context.Context) []doctor.Check {
	return func(ctx context.Context) []doctor.Check {
		if c, ok := playerDACCheck(ctx, cfg, host); ok {
			return []doctor.Check{c}
		}
		return nil
	}
}

// dacConnectHint is the fix for a DAC the binding finds no node of now.
const dacConnectHint = "connect the DAC and power it on, then run bridge doctor again"

// playerDACCheck is the doctor's word on whether the process it checks may
// open the bridge's own player's DAC node for playback (B676): bridge
// doctor's own process, beside the running bridge's groups, or, in the
// console's doctor, the serving process itself. On Linux, for a config that
// asks for the player: the binding resolved to the unit it names now, on one
// bounded reading of the box; the node of its endpoint (alsapcm's, the one
// the player opens) stat'ed for its group, named by its gid ("audio" only
// where the group database names none); and the kernel asked, opening
// nothing, whether this process may open it for reading and writing, as the
// player's open does, by its effective credentials and the node's ACL
// (accessForPlayback). A node refused is a fail naming the user, the group
// and the fix: the user not in the group, and no ACL letting it; in the
// group, but in a process started before it joined (its groups are the ones
// it started with); or refused despite the group, by the node's mode, its
// ACL or a security module. A node allowed is ok, which says the node lets
// the process open it now, never that the DAC is free or plays; in bridge
// doctor, a warn where its own access comes through the group and the
// running bridge is not in it, since a login started after the change has
// the group while a lingering service keeps the groups its user manager
// started with. A DAC that is not connected, a binding that names none now
// (unplugged, powered off, ambiguous), a node that is not there, a reading
// that cannot be had, a kernel or a policy that answers the check with
// ENOSYS or EPERM, bridge doctor run as root, or as another user than the
// running bridge, are not checked, a warn, never a fail and never ok. No
// line off Linux, nor for a config that leaves the player off.
func playerDACCheck(ctx context.Context, cfg *config.Config, h dacAccessHost) (doctor.Check, bool) {
	return doctor.Check{}, false
}

// dacNotLocated is the line for a binding that resolved to no unit to check:
// not checked, by why.
func dacNotLocated(err error, notChecked func(why, hint string) (doctor.Check, bool)) (doctor.Check, bool) {
	var ue *playback.UnitError
	switch {
	case errors.Is(err, errDACReadingUnderWay):
		return notChecked("an earlier reading of the box's sound cards has not returned yet", "run bridge doctor again in a moment")
	case errors.Is(err, context.DeadlineExceeded):
		return notChecked(fmt.Sprintf("the box's sound cards could not be read within %v", dacAccessBound), "")
	case errors.As(err, &ue) && ue.State != "":
		return notChecked(fmt.Sprintf("the bound DAC is not connected now (%s: %s)", ue.State, ue.Reason), dacConnectHint)
	case errors.As(err, &ue):
		return notChecked("the box's sound cards could not be read: "+ue.Reason, "")
	}
	return notChecked("the bound DAC could not be found: "+err.Error(), "")
}

// locateWithin resolves d through h.locate within dacAccessBound, on a
// goroutine of its own and one at a time (h.slot): a reading stuck in the
// kernel is abandoned to return on its own, its slot let go then, and a
// panic in it is the reading's failure.
func (h dacAccessHost) locateWithin(ctx context.Context, d config.PlayerDevice) (playback.Unit, error) {
	ctx, cancel := context.WithTimeout(ctx, dacAccessBound)
	defer cancel()
	if h.slot != nil {
		select {
		case h.slot <- struct{}{}:
		default:
			return playback.Unit{}, errDACReadingUnderWay
		}
	}
	type answer struct {
		u   playback.Unit
		err error
	}
	done := make(chan answer, 1)
	go func() {
		if h.slot != nil {
			defer func() { <-h.slot }()
		}
		defer func() {
			if r := recover(); r != nil {
				done <- answer{err: errors.New("the reading of the box failed")}
			}
		}()
		u, err := h.locate(ctx, d)
		done <- answer{u, err}
	}()
	select {
	case a := <-done:
		return a.u, a.err
	case <-ctx.Done():
		return playback.Unit{}, ctx.Err()
	}
}

// dacJudgement is what an access check answered is judged with.
type dacJudgement struct {
	h       dacAccessHost
	cr      dacCreds
	svc     dacService
	running bool
	node    string
	gid     uint32
	group   string
	dev     config.PlayerDevice
}

// who is the process checked, as a person reads it.
func (j dacJudgement) who() string {
	if j.h.serving {
		return "the bridge, as " + userWord(j.cr)
	}
	return userWord(j.cr) + " (this account)"
}

// proc is the process checked, as its groups' owner.
func (j dacJudgement) proc() string {
	if j.h.serving {
		return "the bridge"
	}
	return "this login"
}

// allowed is the line for a node the kernel lets the process checked open.
func (j dacJudgement) allowed(line func(doctor.Status, string, string) (doctor.Check, bool)) (doctor.Check, bool) {
	how := "through the " + j.group + " group"
	if !j.cr.in(j.gid) {
		how = "through an ACL or the node's mode, not the " + j.group + " group"
	}
	summary := fmt.Sprintf("%s may open %s, the DAC's node, for playback, %s (asked with faccessat2, which opens nothing: whether the DAC is free, and plays, the player finds as it opens it)",
		j.who(), j.node, how)
	if !j.h.serving {
		switch {
		case j.running && j.cr.in(j.gid) && !j.svc.in(j.gid):
			return line(doctor.Warn, fmt.Sprintf("%s may open %s through the %s group, but the running bridge (pid %d) is not in %s: it keeps the groups its user manager started with, so its player may not open the DAC",
				j.who(), j.node, j.group, j.svc.pid, j.group), j.restartHint("restart"))
		case j.running && j.svc.in(j.gid):
			summary += fmt.Sprintf("; the running bridge (pid %d) is in %s too", j.svc.pid, j.group)
		case j.running:
			summary += fmt.Sprintf("; the running bridge (pid %d) runs as the same user", j.svc.pid)
		default:
			summary += "; no running bridge was found to compare with"
		}
	}
	return line(doctor.OK, summary+j.bootNote(), "")
}

// denied is the line for a node the kernel refuses the process checked: a
// fail by the cause, or, in bridge doctor, a warn for a login that started
// before its user joined the group beside a running bridge that is in it.
func (j dacJudgement) denied(line func(doctor.Status, string, string) (doctor.Check, bool)) (doctor.Check, bool) {
	user, group := userWord(j.cr), j.group
	var account []uint32
	var err error = errors.New("the account has no name")
	if j.cr.name != "" {
		account, err = j.h.accountGroups(j.cr.name)
	}
	inAccount := err == nil && slices.Contains(account, j.gid)
	switch {
	case j.cr.in(j.gid):
		return line(doctor.Fail, fmt.Sprintf("the player cannot open its DAC: %s is refused to %s, though %s is in %s, which owns it: the node's mode or its ACL, or a security module (AppArmor, SELinux), refuses it",
			j.node, j.who(), j.proc(), group)+j.bootNote(),
			"read the node's mode and its ACL (ls -l "+j.node+", getfacl "+j.node+") and the security module's log")
	case inAccount && !j.h.serving && j.running && j.svc.in(j.gid):
		return line(doctor.Warn, fmt.Sprintf("%s may not open %s: %s is in %s, which owns it, but this login started before %s joined it; the running bridge (pid %d) is in %s",
			j.who(), j.node, user, group, user, j.svc.pid, group), "log in again, and run bridge doctor again")
	case inAccount:
		hint := j.restartHint("restart")
		if !j.h.serving {
			hint = "log in again for this shell; for the bridge's service, " + hint
		}
		return line(doctor.Fail, fmt.Sprintf("the player cannot open its DAC: %s is in %s, which owns %s, but %s started before %s joined it, and keeps the groups it started with",
			user, group, j.node, j.proc(), user)+j.bootNote(), hint)
	}
	return line(doctor.Fail, fmt.Sprintf("the player cannot open its DAC: %s, which the %s group owns, is refused to %s: %s is not in %s, and no ACL grants it access",
		j.node, group, j.who(), user, group)+j.bootNote(), j.restartHint("join"))
}

// restartHint is the fix, as the installer prints it (scripts/install.sh):
// for join, add the user to the group first; then restart the user's
// service manager, user@<uid>.service, whose services keep the groups it
// started with, so a restart of the bridge's own service alone is not enough;
// or reboot; and what the restart costs.
func (j dacJudgement) restartHint(what string) string {
	user, group := userWord(j.cr), j.group
	root := func(words ...string) string {
		if j.h.sudo != nil && j.h.sudo() {
			words = append([]string{"sudo"}, words...)
		}
		return shellLine(words...)
	}
	asRoot := ""
	if j.h.sudo == nil || !j.h.sudo() {
		asRoot = " as root"
	}
	restart := root("systemctl", "restart", "user@"+strconv.Itoa(j.cr.euid)+".service")
	cost := "restarting it stops every service of " + user + "'s, a desktop or terminal session under it included, so run it from another administrative session; the bridge's service starts again with it"
	if what == "join" {
		return fmt.Sprintf("add %s to %s, then restart %s's user manager, whose services keep the groups it started with; run%s: %s, then %s, or reboot; %s",
			user, group, user, asRoot, root("usermod", "-aG", group, user), restart, cost)
	}
	return fmt.Sprintf("restart %s's user manager, whose services keep the groups it started with; run%s: %s, or reboot; %s", user, asRoot, restart, cost)
}

// bootNote says, in the console's doctor, that the config names another DAC
// than the one the running player started with, its transport usb whether
// written or not, as the console's Player page compares them.
func (j dacJudgement) bootNote() string {
	if !j.h.serving || j.h.boot == nil || hotplug.SameBinding(usbTransport(*j.h.boot), usbTransport(j.dev)) {
		return ""
	}
	return " (this is the DAC the config names; the running player opens the one it started with until the bridge restarts)"
}

// userWord names the user of c, by name, or by uid where the user database
// names none.
func userWord(c dacCreds) string {
	if c.name != "" {
		return c.name
	}
	return "uid " + strconv.Itoa(c.euid)
}

// shellLine is words as a shell command line, each word quoted where the
// shell would read it otherwise, as the installer's command_line quotes them.
func shellLine(words ...string) string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = shellWord(w)
	}
	return strings.Join(out, " ")
}

// shellWord is w as one shell word: as it is when every byte is one the
// installer's sh_quote leaves bare, single-quoted otherwise.
func shellWord(w string) string {
	bare := w != ""
	for _, r := range w {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_./:@=+-", r)) {
			bare = false
			break
		}
	}
	if bare {
		return w
	}
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}

// lookupGroupName is the name the group database gives gid, "" for none.
func lookupGroupName(gid uint32) string {
	g, err := user.LookupGroupId(strconv.FormatUint(uint64(gid), 10))
	if err != nil {
		return ""
	}
	return g.Name
}

// processCreds are this process's effective credentials and supplementary
// groups.
func processCreds() (dacCreds, error) {
	ids, err := os.Getgroups()
	if err != nil {
		return dacCreds{}, err
	}
	c := dacCreds{euid: os.Geteuid(), egid: uint32(os.Getegid())}
	for _, id := range ids {
		c.groups = append(c.groups, uint32(id))
	}
	if u, err := user.LookupId(strconv.Itoa(c.euid)); err == nil {
		c.name = u.Username
	}
	return c, nil
}

// lookupAccountGroups are the gids of the groups the user named belongs to in
// the group database, its primary group among them.
func lookupAccountGroups(name string) ([]uint32, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	ids, err := u.GroupIds()
	if err != nil {
		return nil, err
	}
	var out []uint32
	for _, id := range ids {
		n, err := strconv.ParseUint(id, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("the group id %q is not a number", id)
		}
		out = append(out, uint32(n))
	}
	return out, nil
}

// procFileCap bounds a read of a pid file or a /proc file, each a few
// hundred bytes.
const procFileCap = 64 << 10

// readCapped is the file at path, at most procFileCap bytes of it.
func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, procFileCap))
}

// readServingBridge is the bridge that serves a data directory now, as
// procRoot shows it: the pid its pid file holds, when that process is a
// bridge serve (its command line's second word is serve), its effective uid
// and gid and its supplementary groups from its status file. False where
// the pid file is not there or holds no pid, the process is gone or is no
// serve, or /proc does not show it (hidepid).
func readServingBridge(procRoot, pidFile string) (dacService, bool) {
	raw, err := readCapped(pidFile)
	if err != nil {
		return dacService{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return dacService{}, false
	}
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	cmdline, err := readCapped(filepath.Join(dir, "cmdline"))
	if err != nil {
		return dacService{}, false
	}
	if args := bytes.Split(cmdline, []byte{0}); len(args) < 2 || string(args[1]) != "serve" {
		return dacService{}, false
	}
	status, err := readCapped(filepath.Join(dir, "status"))
	if err != nil {
		return dacService{}, false
	}
	s, ok := parseProcStatusCreds(string(status))
	if !ok {
		return dacService{}, false
	}
	s.pid = pid
	return s, true
}

// parseProcStatusCreds reads a /proc/<pid>/status file's Uid, Gid and Groups
// lines (proc(5)): the effective uid and gid, each the second of its line's
// four, and the supplementary groups. False when the Uid or the Gid line is
// missing or does not parse, or a group does not.
func parseProcStatusCreds(status string) (dacService, bool) {
	var s dacService
	var uid, gid bool
	for _, l := range strings.Split(status, "\n") {
		key, value, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		switch key {
		case "Uid", "Gid":
			if len(fields) != 4 {
				return dacService{}, false
			}
			n, err := strconv.ParseUint(fields[1], 10, 32)
			if err != nil {
				return dacService{}, false
			}
			if key == "Uid" {
				s.euid, uid = int(n), true
			} else {
				s.egid, gid = uint32(n), true
			}
		case "Groups":
			for _, f := range fields {
				n, err := strconv.ParseUint(f, 10, 32)
				if err != nil {
					return dacService{}, false
				}
				s.groups = append(s.groups, uint32(n))
			}
		}
	}
	return s, uid && gid
}
