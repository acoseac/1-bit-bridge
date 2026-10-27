package alsactl

import (
	"testing"
)

type fakeDevice struct{ closed bool }

func (d *fakeDevice) Controls() ([]Control, error) {
	return []Control{{Name: "Playback Volume", Count: 2, Values: []byte{1, 2}}}, nil
}
func (d *fakeDevice) Close() error { d.closed = true; return nil }

// TestControlReaderUsesInjectedDevice covers the shipping reader's lifetime.
func TestControlReaderUsesInjectedDevice(t *testing.T) {
	d := &fakeDevice{}
	r := Reader{Open: func(int) (Device, error) { return d, nil }}
	s, e := r.Read(5)
	if e != nil || len(s.Controls) != 1 || !d.closed {
		t.Fatalf("snapshot %+v closed %v: %v", s, d.closed, e)
	}
}

// TestControlDeviceTripwire refuses an accidentally unmocked path before open.
func TestControlDeviceTripwire(t *testing.T) {
	calls := 0
	remove := GuardNodeOpens(func(string) { calls++ })
	defer remove()
	if _, e := productionOpen(5); e == nil || calls != 1 {
		t.Fatalf("guard calls %d: %v", calls, e)
	}
}
