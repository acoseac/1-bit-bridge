package transcode

import "context"

// noopFsync is the fsync substitute tests reach for when they inject
// a stub `runner` that doesn't actually write a sidecar file. With
// the default `fsyncFn = fsyncFileAndParent`, every such test would
// otherwise trip on `open for fsync: no such file or directory` —
// the helper preserves their pre-PR semantics.
//
// Defined in a non-`_test.go` file (matching the `runner` convention)
// so test files in this package can reference it without an
// awkward `_test.go`-internal symbol. Not part of the public API.
var noopFsync = func(string) error { return nil }

// okRunner is a stub `runner` that succeeds immediately with the given
// size (settings empty), for tests that need a job to complete and do not
// care what ran. failRunner is its failing twin.
func okRunner(size int64) func(context.Context, JobSpec) (RunResult, error) {
	return func(context.Context, JobSpec) (RunResult, error) {
		return RunResult{SizeBytes: size}, nil
	}
}

func failRunner(err error) func(context.Context, JobSpec) (RunResult, error) {
	return func(context.Context, JobSpec) (RunResult, error) {
		return RunResult{}, err
	}
}
