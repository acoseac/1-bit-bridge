//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package manifest

// platformLadderLockKindCases is empty where fsutil locks through lock_other.go
// (AIX, Solaris and the other targets without flock): its answers are not
// flock's, so flock's cases would misclassify there.
var platformLadderLockKindCases = []ladderLockKindCase{}
