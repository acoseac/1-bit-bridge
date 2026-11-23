package main

import (
	"context"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/consolelan"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// consoleLANAskBound bounds bridge doctor's ask of the running bridge's
// GET /api/console (the plan's §5.3): a second, and no answer is a bridge
// not running.
const consoleLANAskBound = time.Second

// consoleLANEnvVar is the field's variable, as config derives it.
const consoleLANEnvVar = "BRIDGE_ADMIN_LAN_ADDRESS"

// consoleLANFileNow is what bridge.yaml's adminLANAddress key holds as the
// file reads now: the doctor's own read (D14), apart from what the running
// bridge took at its start.
func consoleLANFileNow(path string) (key, address string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	k, err := config.LANKeyOf(raw)
	if err != nil {
		return "", ""
	}
	return string(k.State), k.Address
}

// consoleLANEnvNow is what this process's own environment sets: whether it
// sets the variable to something, and its value where the field's check
// takes it (never a value it refuses, R10).
func consoleLANEnvNow() (set bool, address string) {
	v := os.Getenv(consoleLANEnvVar)
	if v == "" {
		return false, ""
	}
	if config.ValidateConsoleLANAddress(config.ConsoleLANField, v, "") != nil {
		return true, ""
	}
	return true, v
}

// doctorConsoleLAN is bridge doctor's facts: the running bridge's report of
// its socket, asked over adminAddress within consoleLANAskBound; what the
// file holds now; what this shell's environment sets; and, for the next
// start, whether this box holds the address now. The source is never judged
// from this shell's environment (D7, D14).
func doctorConsoleLAN(ctx context.Context, cfgPath string, cfg *config.Config) *doctor.ConsoleLANFacts {
	if cfg == nil || cfg.IsPublic() {
		return nil
	}
	f := &doctor.ConsoleLANFacts{Port: consoleLANPortOf(cfg)}
	f.FileKey, f.FileAddress = consoleLANFileNow(cfgPath)
	f.EnvSet, f.EnvAddress = consoleLANEnvNow()
	askCtx, cancel := context.WithTimeout(ctx, consoleLANAskBound)
	defer cancel()
	var got struct {
		LAN *consolelan.State `json:"lan"`
	}
	err := fetchAdminJSON(askCtx, probeLoopbackAddr(adminListenAddress(cfg)), "/api/console", &got)
	switch {
	case err == nil && got.LAN != nil:
		f.Running, f.Reported, f.State = true, true, *got.LAN
	case err != nil && strings.Contains(err.Error(), "HTTP 404"):
		f.Running = true
	case err == nil:
		f.Running = true
	}
	if !f.Running {
		next := f.EnvAddress
		if next == "" && f.FileKey == consolelan.FileKeyAddress {
			next = f.FileAddress
		}
		f.HeldNow = consoleLANHeldNow(next)
	}
	return f
}

// consoleLANHeldNow says whether this box holds a now, by one whole reading
// of its interfaces: "yes", "no", or "" where a is none or the reading fails.
func consoleLANHeldNow(a string) string {
	addr, err := netip.ParseAddr(a)
	if err != nil {
		return ""
	}
	ifs, err := consoleLANReadInterfaces()
	if err != nil {
		return ""
	}
	for _, in := range ifs {
		for _, p := range in.Addrs {
			if p.Addr().Unmap().WithZone("") == addr {
				return "yes"
			}
		}
	}
	return "no"
}

// servingConsoleLAN is the console's doctor's facts: the serving process's
// own socket state, its environment the service's, and the file as it reads
// now.
func servingConsoleLAN(cfgPath string, public bool, port int, st admin.ConsoleLANState) *doctor.ConsoleLANFacts {
	if public {
		return nil
	}
	f := &doctor.ConsoleLANFacts{Port: port, Running: true, Reported: true, State: st, Console: true}
	f.FileKey, f.FileAddress = consoleLANFileNow(cfgPath)
	f.EnvSet, f.EnvAddress = consoleLANEnvNow()
	return f
}
