//go:build !linux

package atomicwrite

// platformMountID is no ID: no platform but Linux gives the strict writer a
// mount's ID. A mount of another filesystem is still refused there by its
// device; one that shows no device of its own, as a bind mount does, is not
// seen. The strict writer's one caller, the player, runs on Linux alone.
func platformMountID(string) (uint64, bool, error) { return 0, false, nil }

// platformMountTable is no table: no platform but Linux keeps one the probe
// reads.
func platformMountTable() (map[string]bool, error) { return nil, nil }
