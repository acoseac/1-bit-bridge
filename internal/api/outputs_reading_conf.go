package api

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/mpd/conf"
)

// processConfig selects only for the card being described. Owner.PID is the
// Tgid resolved by alsa.Reader, not status's owner_pid (Owner.TID).
// Every proc/config operation runs inside readConf's bounded step.
func (o *localOutputs) processConfig(s *readingSnap, pick *dacPick) (root, logical, source string, ok bool) {
	if s.procCode != "" || s.mpdCode != "" || s.cards.ProcRoot == "" {
		return
	}
	card := pickCard(s.cards, pick)
	if card == nil || card.Partial {
		return
	}
	sub, ambiguous := pickSubstream(s.cards, pick)
	if ambiguous || sub == nil || len(holders(s, sub)) != 1 {
		return
	}
	ow := sub.Owner
	if ow == nil || ow.PID == nil || *ow.PID <= 0 || ow.Process == nil || *ow.Process != "mpd" {
		return
	}
	pid := *ow.PID
	// A second MPD on this card makes ownership ambiguous even on another PCM.
	for _, dev := range card.Devices {
		for _, other := range dev.Substreams {
			if owner := other.Owner; owner != nil && owner.Process != nil && *owner.Process == "mpd" && (owner.PID == nil || *owner.PID != pid) {
				return "", "", "", false
			}
		}
	}
	proc := s.cards.ProcRoot
	prefix := "/" + strconv.Itoa(pid)
	before, valid := processStart(proc, prefix)
	if !valid {
		return
	}
	b, err := o.cmdline(proc, prefix+"/cmdline", 64<<10)
	after, valid := processStart(proc, prefix)
	if err != nil || !valid || before != after {
		return
	}
	logical, valid = mpdConfigArgument(b)
	if !valid || !sameProcessRoot(proc, prefix) {
		return "", "", "", false
	}
	root = alsa.ConfigRoot(proc)
	if logical == "--no-config" {
		return root, "", "mpd_no_config", true
	}
	if logical != "" {
		return root, logical, "process_cmdline", true
	}
	// Do not substitute the bridge's environment, even for the same uid:
	// services commonly set a different HOME. Never retain or emit environ.
	home, xdg, valid := processEnvironment(proc, prefix)
	if !valid {
		return "", "", "", false
	}
	loc := conf.Locate{Root: root, Home: home, XDGConfigHome: xdg}
	if logical, _, found := loc.Find(); found {
		return root, logical, "mpd_default", true
	}
	return "", "", "", false
}

func processStart(proc, prefix string) (string, bool) {
	b, err := conf.ReadLogical(proc, prefix+"/stat", 64<<10)
	if err != nil {
		return "", false
	}
	// comm is parenthesized and may contain spaces or ')'; field 3 follows
	// the last closing parenthesis. starttime is field 22 (index 19 here).
	end := bytes.LastIndexByte(b, ')')
	if end < 0 {
		return "", false
	}
	fields := strings.Fields(string(b[end+1:]))
	if len(fields) < 20 {
		return "", false
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	return fields[19], err == nil && start > 0
}

func sameProcessRoot(proc, prefix string) bool {
	daemon, errD := os.Stat(filepath.Join(proc, strings.TrimPrefix(prefix, "/"), "root"))
	bridge, errB := os.Stat(filepath.Join(proc, "self", "root"))
	if errD == nil && errB == nil {
		return os.SameFile(daemon, bridge)
	}
	a, okA := mountRoot(proc, prefix+"/mountinfo")
	b, okB := mountRoot(proc, "/self/mountinfo")
	return okA && okB && a == b
}

func mountRoot(proc, logical string) (string, bool) {
	b, err := conf.ReadLogical(proc, logical, 1<<20)
	if err != nil {
		return "", false
	}
	var root string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || f[4] != "/" {
			continue
		}
		if root != "" {
			return "", false
		}
		if _, _, ok := strings.Cut(f[2], ":"); !ok || !strings.HasPrefix(f[3], "/") {
			return "", false
		}
		root = f[2] + " " + f[3]
	}
	return root, root != ""
}

func processEnvironment(proc, prefix string) (home, xdg string, ok bool) {
	b, err := conf.ReadLogical(proc, prefix+"/environ", 64<<10)
	if err != nil || (len(b) > 0 && b[len(b)-1] != 0) {
		return "", "", false
	}
	seen := map[string]bool{}
	for _, entry := range bytes.Split(bytes.TrimSuffix(b, []byte{0}), []byte{0}) {
		key, value, found := strings.Cut(string(entry), "=")
		if !found {
			if len(entry) == 0 && len(b) == 0 {
				continue
			}
			return "", "", false
		}
		if key != "HOME" && key != "XDG_CONFIG_HOME" {
			continue
		}
		if seen[key] {
			return "", "", false
		}
		seen[key] = true
		if value != "" && !validConfigPath(value) {
			return "", "", false
		}
		if key == "HOME" {
			home = value
		} else {
			xdg = value
		}
	}
	return home, xdg, true
}

// validConfigPath refuses ambiguous argv spellings rather than silently
// cleaning them before opening or exposing them as provenance.
func validConfigPath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return false
	}
	if p == "/" {
		return true
	}
	for _, part := range strings.Split(p[1:], "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// mpdConfigArgument models MPD 0.24.4 CommandLine.cxx and OptionParser.cxx:
// options anywhere, long flag=value accepted, no '--' terminator. The
// --no-config sentinel means no file, including when an operand is supplied.
// Relative files are unprovable after daemonization changes the cwd.
func mpdConfigArgument(b []byte) (string, bool) {
	if len(b) == 0 || len(b) > 64<<10 || b[len(b)-1] != 0 {
		return "", false
	}
	argv := bytes.Split(b[:len(b)-1], []byte{0})
	if len(argv) == 0 || path.Base(string(argv[0])) != "mpd" {
		return "", false
	}
	var config string
	noConfig := false
	operands := 0
	for _, arg := range argv[1:] {
		a := string(arg)
		if a == "" {
			return "", false
		}
		if strings.HasPrefix(a, "-") {
			option := a
			if strings.HasPrefix(a, "--") {
				option, _, _ = strings.Cut(a, "=")
			}
			switch option {
			case "--no-config":
				noConfig = true
			case "--kill", "--no-daemon", "--systemd", "--stdout", "--stderr", "-v", "--verbose", "-V", "--version", "-h", "--help", "-?":
			default:
				return "", false
			}
			continue
		}
		operands++
		config = a
	}
	// MPD returns before inspecting remaining arguments in no-config mode.
	if noConfig {
		return "--no-config", true
	}
	if operands > 1 || (config != "" && !validConfigPath(config)) {
		return "", false
	}
	return config, true
}
