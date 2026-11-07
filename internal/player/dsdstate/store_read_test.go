package dsdstate

import (
	"sync"
	"testing"
	"time"
)

// TestTheStateIsReadWithoutWaitingOnAWrite (B762 round 2): a write holds the
// store's lock through its durable write, whose fsyncs a stalled disk holds
// as long as it likes; State answers meanwhile, the state the last write
// left, and the one the held write makes once it lands. Serve's player's
// supervisor reads it at each snapshot (its migration's waits), and the
// effective policy's refresh at each reading: neither waits on a write.
func TestTheStateIsReadWithoutWaitingOnAWrite(t *testing.T) {
	came, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	let := sync.OnceFunc(func() { close(gate) })
	defer let()
	s := openAt(t, t.TempDir(), options{write: func(string, []byte) error {
		once.Do(func() { close(came) })
		<-gate
		return nil
	}})
	was := s.State().DefaultMode
	if was == ModeOff {
		t.Fatalf("the defaults' mode is %q, which the test's write makes", was)
	}
	wrote := make(chan error, 1)
	go func() {
		wrote <- s.Update(func(st *State) error {
			st.DefaultMode = ModeOff
			return nil
		})
	}()
	select {
	case <-came:
	case <-time.After(time.Minute):
		t.Fatal("the write never came to its durable write")
	}
	read := make(chan State, 1)
	go func() { read <- s.State() }()
	select {
	case st := <-read:
		if st.DefaultMode != was {
			t.Errorf("State answered %q while the write was held, want the state the last write left (%q)", st.DefaultMode, was)
		}
	case <-time.After(time.Minute):
		t.Fatal("State waited on the write that holds the store's lock")
	}
	let()
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if st := s.State(); st.DefaultMode != ModeOff {
		t.Errorf("State answers %q once the write landed, want %q", st.DefaultMode, ModeOff)
	}
}
