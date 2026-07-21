package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/updater"
)

// TestMapUpdaterError pins the updater → admin sentinel translation
// the UpdateProvider adapter performs so handlers_api.go's
// classifyUpdateError can switch on errors.Is without importing
// internal/updater. Every updater sentinel that has an admin-side
// twin must map; anything else passes through unchanged.
func TestMapUpdaterError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"no-update", updater.ErrNoUpdate, admin.ErrUpdateNoUpdate},
		{"active-sessions", updater.ErrActiveSessions, admin.ErrUpdateActiveSessions},
		{"install-in-flight", updater.ErrInstallInFlight, admin.ErrUpdateInstallInFlight},
		{"platform-unsupported", updater.ErrInstallNotSupported, admin.ErrUpdateNotSupported},
		{"path-not-writable", updater.ErrPathNotWritable, admin.ErrUpdatePathNotWritable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The adapter must classify both the bare sentinel and a
			// wrapped instance (Install returns errors with context).
			for _, err := range []error{tc.err, fmt.Errorf("install: %w", tc.err)} {
				got := mapUpdaterError(err)
				if !errors.Is(got, tc.want) {
					t.Errorf("mapUpdaterError(%v) does not unwrap to %v", err, tc.want)
				}
			}
		})
	}

	if got := mapUpdaterError(nil); got != nil {
		t.Errorf("mapUpdaterError(nil) = %v, want nil", got)
	}
	boom := errors.New("boom")
	if got := mapUpdaterError(boom); !errors.Is(got, boom) {
		t.Errorf("mapUpdaterError(unknown) = %v, want passthrough of %v", got, boom)
	}
}
