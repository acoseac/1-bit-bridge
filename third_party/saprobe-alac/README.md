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
imports both), `go.mod`, `go.sum`, `LICENSE`, `tests/packet_bounds_test.go`,
`internal/alac/matrix_bench_test.go`, and `tests/bitstream_overrun_test.go`
(ours: the patch's regression tests, written for upstream). The rest of the upstream
repository (its CI, documentation, examples, and the tests that need
CoreAudio, alacconvert or its test framework) is not carried.
`go -C third_party/saprobe-alac test ./...` runs the tests here; the
bridge's `make test` does not, because this directory is a module of its
own.

## Modifications

Every file here is upstream's at that revision except these two, and
`ops/upstream/saprobe-alac.patch` in the bridge repository is the change as
a diff against it, for upstream (`ops/upstream/saprobe-alac.md` is the
report):

- `internal/alac/golomb.go`, modified: `DynDecomp` checks every code against
  the packet's own length, not the four bytes of padding the bit buffer
  adds after it. It checks an escape's value before reading it, a normal
  code's length before taking it, and a zero run's count before and after
  `dynGet`. A code that would end past the packet returns
  `ErrBitstreamOverrun`, where revision 1b6eb83 decoded it from the padding
  with no error, or panicked reading past the padding too. The file's
  header says that it was changed; that note is the only difference
  between this copy and 1b6eb83 with the patch applied.
- `tests/bitstream_overrun_test.go`, added: the packets the change refuses,
  and a code that ends exactly at the packet's end, which still decodes.
