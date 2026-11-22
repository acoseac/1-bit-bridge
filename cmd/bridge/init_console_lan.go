package main

// Names-only shim (backlog B989's tests-first commit): the flag's check,
// which refuses nothing until the change.

func initLANAddressFlagError(public bool, lan, admin string) error { return nil }
