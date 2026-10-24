package api

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/mpd/conf"
)

// processConfigHint inspects argv only for the card being described. Owner.PID is the
// Tgid resolved by alsa.Reader, not status's owner_pid (Owner.TID).
// Every proc/identity operation runs inside the hint's own bounded step.
func (o *localOutputs) processConfigHint(s *readingSnap, pick *dacPick, configuredRoot, configuredHost string) *ReadingMPDConfigHint {
	if s.procCode != "" || s.mpdCode != "" || s.cards.ProcRoot == "" {
		return nil
	}
	card := pickCard(s.cards, pick)
	if card == nil || card.Partial {
		return nil
	}
	sub, ambiguous := pickSubstream(s.cards, pick)
	if ambiguous || sub == nil || len(holders(s, sub)) != 1 {
		return nil
	}
	ow := sub.Owner
	if ow == nil || ow.PID == nil || *ow.PID <= 0 || ow.Process == nil || *ow.Process != "mpd" {
		return nil
	}
	pid := *ow.PID
	// A second MPD on this card makes ownership ambiguous even on another PCM.
	for _, dev := range card.Devices {
		for _, other := range dev.Substreams {
			if owner := other.Owner; owner != nil && owner.Process != nil && *owner.Process == "mpd" && (owner.PID == nil || *owner.PID != pid) {
				return nil
			}
		}
	}
	proc := s.cards.ProcRoot
	prefix := "/" + strconv.Itoa(pid)
	before, valid := processStart(proc, prefix)
	if !valid {
		return nil
	}
	b, err := o.cmdline(proc, prefix+"/cmdline", 64<<10)
	after, valid := processStart(proc, prefix)
	if err != nil || !valid || before != after {
		return nil
	}
	logical, valid := mpdConfigArgument(b)
	if !valid || !sameProcessRoot(proc, prefix) {
		return nil
	}
	switch logical {
	case "--no-config":
		return &ReadingMPDConfigHint{Argument: "no_config"}
	case "":
		return &ReadingMPDConfigHint{Argument: "none"}
	}
	// Compare identities in the bridge's view only. Never read the operand's
	// contents: argv and matching roots cannot prove what MPD loaded.
	root := alsa.ConfigRoot(proc)
	if processRelativeConfig(logical) {
		return nil
	}
	host, ok := hintOperandHost(root, logical)
	if !ok {
		return nil
	}
	physical, argument, err := conf.StatHost(root, host)
	_, configured, confErr := conf.StatHost(configuredRoot, configuredHost)
	if err != nil || confErr != nil || processRelativeConfig(physical) || fsutil.NotAFile(argument.Mode()) != "" || fsutil.NotAFile(configured.Mode()) != "" {
		return nil
	}
	if os.SameFile(argument, configured) {
		return &ReadingMPDConfigHint{Argument: "same"}
	}
	return &ReadingMPDConfigHint{Argument: "different", Path: logical}
}

// processRelativeConfig names process-local views that the bridge cannot
// interpret as MPD's operand, even when their file identities are available.
func processRelativeConfig(p string) bool {
	return p == "/proc" || strings.HasPrefix(p, "/proc/") || p == "/dev/fd" || strings.HasPrefix(p, "/dev/fd/") || p == "/dev/stdin" || p == "/dev/stdout" || p == "/dev/stderr"
}

// hintOperandHost walks every operand prefix and link target before following
// it. EvalSymlinks alone loses an intermediate process-local name once a magic
// link ends at an ordinary file. The root itself is trusted, as in conf.StatHost;
// everything below it stays confined, with at most 40 symlink hops. A relative
// target's .. is processed only after preceding links, never cleaned over them.
func hintOperandHost(root, logical string) (string, bool) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	namedRoot := root
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return "", false
	}
	defer func() { _ = dir.Close() }()
	pending := strings.Split(strings.TrimPrefix(logical, "/"), "/")
	var parts []string
	hops := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(parts) == 0 {
				// The host's /.. is /; an injected root still cannot be left.
				if root == "/" {
					continue
				}
				return "", false
			}
			parts = parts[:len(parts)-1]
			continue
		}
		candidate := strings.Join(append(append([]string(nil), parts...), part), "/")
		if processRelativeConfig("/" + candidate) {
			return "", false
		}
		info, err := dir.Lstat(filepath.FromSlash(candidate))
		if err != nil {
			return "", false
		}
		if info.Mode()&os.ModeSymlink == 0 {
			if len(pending) > 0 && !info.IsDir() {
				return "", false
			}
			parts = append(parts, part)
			continue
		}
		hops++
		if hops > 40 {
			return "", false
		}
		target, err := dir.Readlink(filepath.FromSlash(candidate))
		if err != nil {
			return "", false
		}
		if filepath.IsAbs(target) {
			// Absolute links retain host semantics, like the conf reader. Never
			// reinterpret an outside target as a file under an injected root.
			if processRelativeConfig(filepath.ToSlash(target)) {
				return "", false
			}
			// Do not clean a link/.. sequence while removing the canonical root.
			if target == root || target == namedRoot {
				target = ""
			} else {
				prefix := strings.TrimRight(root, string(filepath.Separator)) + string(filepath.Separator)
				if !strings.HasPrefix(target, prefix) {
					prefix = strings.TrimRight(namedRoot, string(filepath.Separator)) + string(filepath.Separator)
					if !strings.HasPrefix(target, prefix) {
						return "", false
					}
				}
				target = strings.TrimPrefix(target, prefix)
			}
			parts = nil
		} else if filepath.VolumeName(target) != "" {
			return "", false
		}
		pending = append(strings.Split(filepath.ToSlash(target), "/"), pending...)
	}
	return filepath.Join(root, filepath.FromSlash(strings.Join(parts, "/"))), true
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

// validConfigPath refuses ambiguous argv spellings rather than silently
// cleaning them before comparing identities or emitting a hint.
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
