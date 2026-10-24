package api

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/mpd"
	"github.com/acoseac/1-bit-bridge/internal/mpd/conf"
)

// processConfig chooses information from the process whose output thread the
// reading tied to MPD. Multiple process ids cannot identify one daemon: keep the
// configured fallback. Never read a command line without that owner tie, or
// substitute the host's /proc for an absent injected root.
func (o *localOutputs) processConfig(s *readingSnap, opt mpd.Options) (root, logical, source string, ok bool) {
	if s.procCode != "" || s.mpdCode != "" || s.cards.ProcRoot == "" {
		return
	}
	pid := 0
	for _, card := range s.cards.Cards {
		for _, dev := range card.Devices {
			for _, sub := range dev.Substreams {
				ow := sub.Owner
				if ow == nil || ow.PID == nil || *ow.PID <= 0 || ow.Process == nil || *ow.Process != "mpd" || len(holders(s, &sub)) != 1 {
					continue
				}
				if pid != 0 && pid != *ow.PID {
					return "", "", "", false
				}
				pid = *ow.PID
			}
		}
	}
	if pid == 0 {
		return
	}
	b, err := o.cmdline(s.cards.ProcRoot, "/"+strconv.Itoa(pid)+"/cmdline", 64<<10)
	if err != nil {
		return
	}
	logical, valid := mpdConfigArgument(b)
	if !valid {
		return "", "", "", false
	}
	root = alsa.ConfigRoot(s.cards.ProcRoot)
	if logical != "" {
		return root, logical, "process_cmdline", true
	}
	loc := conf.Locate{Root: root, Home: opt.Home, XDGConfigHome: opt.XDGConfigHome}
	if logical, _, found := loc.Find(); found {
		return root, logical, "mpd_default", true
	}
	return "", "", "", false
}

// mpdConfigArgument reads proc's NUL-separated argv, not shell text. MPD's
// command line is OPTION... CONF_FILE (src/CommandLine.cxx ParseCommandLine).
// All modeled options are flags; unknown options and relative filenames are
// deliberately refused rather than interpreting an option's value as a file.
// Resolving a relative argument would require MPD's startup working directory,
// which its current /proc cwd need not be after daemonizing.
func mpdConfigArgument(b []byte) (string, bool) {
	if len(b) == 0 || len(b) > 64<<10 || b[len(b)-1] != 0 {
		return "", false
	}
	argv := bytes.Split(b[:len(b)-1], []byte{0})
	if len(argv) == 0 || len(argv[0]) == 0 {
		return "", false
	}
	var config string
	endOptions := false
	for _, arg := range argv[1:] {
		a := string(arg)
		if a == "" {
			return "", false
		}
		if !endOptions && a == "--" {
			endOptions = true
			continue
		}
		if !endOptions && strings.HasPrefix(a, "-") {
			switch a {
			case "--no-daemon", "--systemd", "--stderr", "--verbose", "--kill", "--version", "--help", "-v", "-V", "-h":
				continue
			default:
				return "", false
			}
		}
		if config != "" || !strings.HasPrefix(a, "/") || strings.Contains(a, `\`) {
			return "", false
		}
		config = a
		// Options after the positional filename are not modeled.
		endOptions = true
	}
	return config, true
}
