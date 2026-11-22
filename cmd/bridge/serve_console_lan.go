package main

import (
	"context"
	"net"
	"syscall"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/consolelan"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
)

// Names-only shim (backlog B989's tests-first commit): the seams serve's
// plan will take, production's wired, and no plan built.

type consoleLANSeams struct {
	read      func() ([]consolelan.Interface, error)
	hostTable func(ipv6 bool) (consolelan.Table, error)
	hold      func(index int, ipv6 bool) func(network, address string, c syscall.RawConn) error
	listen    func(ctx context.Context, lc *net.ListenConfig, network, address string) (net.Listener, error)
	localName func() string
	goos      string
	tick      <-chan time.Time
}

func consoleLANReadInterfaces() ([]consolelan.Interface, error) {
	return consoleLANInterfacesOf(lan.ReadInterfaces())
}

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

func consoleLANProduction(responder func() bool) consoleLANSeams {
	return consoleLANSeams{
		read:      consoleLANReadInterfaces,
		hostTable: consolelan.HostModel,
		hold:      consolelan.Hold,
		localName: func() string { return "" },
	}
}
