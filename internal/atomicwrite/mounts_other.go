//go:build !linux

package atomicwrite

// platformMountTable is no table: no platform but Linux keeps one the probe
// reads. A mount of another filesystem is still refused there by its
// device; one that shows no device of its own, as a bind mount does, is not
// seen. The strict writer's one caller, the player, runs on Linux alone.
func platformMountTable() (map[string]bool, error) { return nil, nil }
