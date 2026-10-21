# saprobe-alac, carried in the bridge

This directory is the Go module `github.com/mycophonic/saprobe-alac` at
revision `1b6eb83789544ce2e8624a679c14dbd33da7673a`
(`v1.0.0-rc2.0.20261020094436-1b6eb8378954`), licensed under the Apache
License 2.0 (`LICENSE`). The bridge's `go.mod` replaces the module with this
directory (backlog B470), so the bridge builds the copy here and never the
one in the module cache.

saprobe-alac is Mycophonic's Go port of Apple's ALAC reference decoder
(Apache License 2.0, Copyright 2011 Apple Inc.).

## What is here

Only what the bridge's build needs, and the tests of it that run offline:
the root package, `internal/alac` and `internal/mp4` (the root package
imports both), `go.mod`, `go.sum`, `LICENSE`, `tests/packet_bounds_test.go`
and `internal/alac/matrix_bench_test.go`. The rest of the upstream
repository (its CI, documentation, examples, and the tests that need
CoreAudio, alacconvert or its test framework) is not carried.
`go -C third_party/saprobe-alac test ./...` runs the tests here; the
bridge's `make test` does not, because this directory is a module of its
own.

## Modifications

None: every file here is upstream's at that revision.
