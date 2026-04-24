package main

import (
	"net"
	"strconv"
)

// splitHostPortRaw wraps net.SplitHostPort + Atoi into a single call
// that returns the host + int port. A separate file so the doctor
// command can call it without pulling the whole http stack in.
func splitHostPortRaw(addr string) (string, int, error) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return "", 0, err
	}
	return h, n, nil
}
