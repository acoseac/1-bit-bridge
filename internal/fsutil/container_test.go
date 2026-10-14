package fsutil

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestInContainerAtReadsEitherMarker: Docker's marker, Podman's marker, or
// neither. The fixture stands in for the container root, so the verdict is
// the same on every host the suite runs on.
func TestInContainerAtReadsEitherMarker(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "dockerenv")
	podman := filepath.Join(dir, "containerenv")

	if inContainerAt(docker, podman) {
		t.Fatal("no marker is there, yet it reads as a container")
	}
	if err := os.WriteFile(podman, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !inContainerAt(docker, podman) {
		t.Error("Podman's marker alone must read as a container")
	}
	if err := os.Remove(podman); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(docker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !inContainerAt(docker, podman) {
		t.Error("Docker's marker alone must read as a container")
	}
}

// TestInContainerNamesBothRuntimes pins the production marker list: the
// image's doctor report depends on Docker's, and a Podman user's on
// Podman's.
func TestInContainerNamesBothRuntimes(t *testing.T) {
	for _, want := range []string{"/.dockerenv", "/run/.containerenv"} {
		if !slices.Contains(containerMarkers, want) {
			t.Errorf("containerMarkers = %v, missing %s", containerMarkers, want)
		}
	}
}
