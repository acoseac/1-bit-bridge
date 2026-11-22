package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/consolelan"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
)

// The console on the home network (backlog B989): serve's plan for the
// second socket, built once from the boot snapshot, with the production
// reading, hold and Windows table wired in, each a seam a test replaces.

// consoleLANSeams are the reading, the hold, the Windows table and the bind
// serve's plan uses; the zero value is production's (consoleLANProduction).
type consoleLANSeams struct {
	read      func() ([]consolelan.Interface, error)
	hostTable func(ipv6 bool) (consolelan.Table, error)
	hold      func(index int, ipv6 bool) func(network, address string, c syscall.RawConn) error
	listen    func(ctx context.Context, lc *net.ListenConfig, network, address string) (net.Listener, error)
	localName func() string
	goos      string
	tick      <-chan time.Time
}

// consoleLANReadInterfaces is the console's reading of the box: lan's whole
// or failing reading (lan.ReadInterfaces), as consolelan's interfaces.
func consoleLANReadInterfaces() ([]consolelan.Interface, error) {
	return consoleLANInterfacesOf(lan.ReadInterfaces())
}

// consoleLANInterfacesOf converts a lan reading.
func consoleLANInterfacesOf(ifs []lan.Interface, err error) ([]consolelan.Interface, error) {
	if err != nil {
		return nil, err
	}
	out := make([]consolelan.Interface, 0, len(ifs))
	for _, in := range ifs {
		out = append(out, consolelan.Interface{Name: in.Name, Index: in.Index, Flags: in.Flags, Addrs: in.Addrs})
	}
	return out, nil
}

// consoleLANProduction is serve's default seams: lan's reading,
// consolelan's hold and Windows table, and the box's .local name while the
// bridge's mDNS responder runs (I10). responder answers whether it runs.
func consoleLANProduction(responder func() bool) consoleLANSeams {
	return consoleLANSeams{
		read:      consoleLANReadInterfaces,
		hostTable: consolelan.HostModel,
		hold:      consolelan.Hold,
		localName: func() string {
			if responder == nil || !responder() {
				return ""
			}
			return advertise.LocalName()
		},
	}
}

// over is s's fields over base's: each one a test set replaces production's.
func (s consoleLANSeams) over(base consoleLANSeams) consoleLANSeams {
	if s.read != nil {
		base.read = s.read
	}
	if s.hostTable != nil {
		base.hostTable = s.hostTable
	}
	if s.hold != nil {
		base.hold = s.hold
	}
	if s.listen != nil {
		base.listen = s.listen
	}
	if s.localName != nil {
		base.localName = s.localName
	}
	if s.goos != "" {
		base.goos = s.goos
	}
	if s.tick != nil {
		base.tick = s.tick
	}
	return base
}

// consoleLANPlanFor is serve's plan from its boot snapshot, nil where no
// plan applies: a public bridge (Validate refuses the field there), or a
// loopback one whose effective address is unset and whose file holds no key
// (an empty key's plan is kept, its socket off, for what the key costs the
// rollback). The key's presence and the file's own value are what the boot
// read found in bridge.yaml's bytes (config.LANKeyOf at the load, aliases
// and merge keys resolved: I1), never the file as it reads now.
func consoleLANPlanFor(cfg *config.Config, seams consoleLANSeams) *admin.ConsoleLANPlan {
	if cfg == nil || cfg.IsPublic() {
		return nil
	}
	key := cfg.FileLANKey()
	if cfg.AdminLANAddress == "" && !key.InFile() {
		return nil
	}
	plan := &admin.ConsoleLANPlan{
		FileKey:   key,
		Read:      seams.read,
		HostTable: seams.hostTable,
		Hold:      seams.hold,
		Listen:    seams.listen,
		LocalName: seams.localName,
		GOOS:      seams.goos,
		Tick:      seams.tick,
	}
	if _, port, ok := splitHostPort(adminListenAddress(cfg)); ok {
		plan.Port = port
	}
	if cfg.AdminLANAddress != "" {
		// Validate took it as a private IP alone; a parse that fails here is
		// a config Validate never passed, and opens nothing.
		if a, err := netip.ParseAddr(cfg.AdminLANAddress); err == nil {
			plan.Address = a
			plan.Source = cfg.ConsoleLANSource()
		}
	}
	return plan
}

// consoleLANBanner writes the serve banner's home-network line (the plan's
// §5.4) for the state the first attempt left, and the rollback sentence where
// the file held the key at the start; nothing where the plan says nothing.
func consoleLANBanner(w io.Writer, st admin.ConsoleLANState, port int) {
	switch st.State {
	case admin.ConsoleLANListening:
		where, _ := consolelan.SourceWords(st.Source, st.FileKeyAtBoot, st.FileAddressAtBoot)
		fmt.Fprintf(w, "Home network: %s/ (no password: anyone on your network can use it), %s\n", st.URL, where)
	case admin.ConsoleLANWaiting:
		line := "Home network: not open yet: " + consolelan.ReasonWords(st.Reason, st.Address, port, st.Interface)
		if consolelan.Retried(st.Reason) {
			line += "; tried again every 5 s"
		}
		fmt.Fprintln(w, line)
	}
	if st.State != admin.ConsoleLANOff && st.Source == consolelan.SourceEnvironment && !st.KeyInFile() {
		fmt.Fprintln(w, "Home network: "+consolelan.EnvironmentRollbackWords)
	}
	if st.KeyInFile() {
		fmt.Fprintln(w, "Home network: "+consolelan.RollbackWords)
	}
}

// consoleLANPortOf is adminAddress's port, 0 where it does not read.
func consoleLANPortOf(cfg *config.Config) int {
	_, port, ok := splitHostPort(adminListenAddress(cfg))
	if !ok {
		return 0
	}
	return port
}

// consoleLANURLText is the console's URL at a and port as text.
func consoleLANURLText(a string, port int) string {
	return "http://" + net.JoinHostPort(a, strconv.Itoa(port))
}
