package doctor

// socketCgroups returns the cgroup each socket listening on port was created
// in, as the id of that cgroup's directory on the cgroup2 mount (its inode
// number), keyed by the socket's fd-link text, `socket:[<inode>]`. A socket
// it has no cgroup for is absent. nil means nothing asks at all.
type socketCgroups func(port int) (map[string]uint64, error)

// cgroupsNotOf is the census's third accounting, for the listeners that
// neither a readable holder nor another uid accounts for (rest). It counts
// nothing yet: the accounting lands in the change after this one.
func cgroupsNotOf(procRoot string, pid int, rest []string, port int, cgroupsOf socketCgroups) (cgroups []string, pidCgroup string, all bool) {
	return nil, "", false
}
