package lrucache

import (
	"sync"
	"testing"
)

func TestSetGet(t *testing.T) {
	c := New[string, int](3)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("c", 3)
	for k, want := range map[string]int{"a": 1, "b": 2, "c": 3} {
		if got, ok := c.Get(k); !ok || got != want {
			t.Errorf("Get(%q) = (%d, %v), want (%d, true)", k, got, ok, want)
		}
	}
}

func TestEvictionOrder(t *testing.T) {
	c := New[string, int](3)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("c", 3)
	// Access "a" to promote it.
	if _, ok := c.Get("a"); !ok {
		t.Fatalf("expected a to be present")
	}
	// Insert "d" — should evict "b" (now LRU), not "a".
	c.Set("d", 4)
	if _, ok := c.Get("b"); ok {
		t.Errorf("expected b to be evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Errorf("expected a to survive (was promoted)")
	}
	if c.Len() != 3 {
		t.Errorf("Len = %d, want 3", c.Len())
	}
}

func TestUpdateNoChurn(t *testing.T) {
	c := New[string, int](2)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("a", 99) // update — no eviction
	if got, _ := c.Get("a"); got != 99 {
		t.Errorf("Get(a) = %d, want 99", got)
	}
	if _, ok := c.Get("b"); !ok {
		t.Errorf("b should still be present after update of a")
	}
	if c.Len() != 2 {
		t.Errorf("Len = %d, want 2 (update should not grow)", c.Len())
	}
}

func TestHasDoesNotPromote(t *testing.T) {
	c := New[string, int](2)
	c.Set("a", 1)
	c.Set("b", 2)
	// Has on "a" should NOT promote it.
	if !c.Has("a") {
		t.Fatalf("Has(a) should be true")
	}
	c.Set("c", 3) // should evict the actual LRU = "a"
	if c.Has("a") {
		t.Errorf("a should have been evicted (Has must not promote)")
	}
	if !c.Has("b") || !c.Has("c") {
		t.Errorf("b and c should both be present")
	}
}

func TestCapZeroOrNegative(t *testing.T) {
	c := New[string, int](0)
	c.Set("a", 1)
	c.Set("b", 2)
	if c.Len() > 1 {
		t.Errorf("zero-cap should clamp to 1, Len = %d", c.Len())
	}
}

func TestConcurrentAccess(t *testing.T) {
	// Smoke test — race detector is the real assertion.
	c := New[int, int](100)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				k := (base*1000 + j) % 200
				c.Set(k, j)
				_, _ = c.Get(k)
				_ = c.Has(k)
			}
		}(i)
	}
	wg.Wait()
	if c.Len() > 100 {
		t.Errorf("Len = %d, want <= 100", c.Len())
	}
}
