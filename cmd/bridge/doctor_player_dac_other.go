//go:build !linux

package main

import "errors"

// errNoDACProbe is the answer off Linux, where the player does not run and
// the player-dac line is not shown.
var errNoDACProbe = errors.New("the player's DAC is asked on Linux alone")

func statDACNode(string) (dacNode, error) { return dacNode{}, errNoDACProbe }

func accessForPlayback(string) error { return errNoDACProbe }
