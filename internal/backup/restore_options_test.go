package backup

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestRestoreDefaultsToCryptographicRandomness(t *testing.T) {
	fixed := bytes.NewReader([]byte{0, 0, 0})
	if (RestoreOptions{}).randomSource() != rand.Reader {
		t.Fatal("default is not crypto/rand.Reader")
	}
	if (RestoreOptions{Random: fixed}).randomSource() != fixed {
		t.Fatal("test source ignored")
	}
}
