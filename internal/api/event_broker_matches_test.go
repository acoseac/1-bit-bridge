package api

import "testing"

// TestSubscriberMatches pins the topic prefix-match semantics that F7's
// zero-alloc rewrite of matches() must preserve byte-for-byte: empty
// allowlist = all, exact match, "<topic>." prefix match, and crucially a
// longer NON-dotted topic ("pairingX") must NOT match "pairing".
func TestSubscriberMatches(t *testing.T) {
	cases := []struct {
		name   string
		topics []string
		topic  string
		want   bool
	}{
		{"nil allowlist matches all", nil, "anything", true},
		{"empty allowlist matches all", []string{}, "anything", true},
		{"exact match", []string{"pairing"}, "pairing", true},
		{"prefix + dot matches", []string{"pairing"}, "pairing.abc", true},
		{"longer non-dotted does not match", []string{"pairing"}, "pairingX", false},
		{"shorter does not match", []string{"pairing"}, "pair", false},
		{"unrelated does not match", []string{"pairing"}, "stats", false},
		{"second allowlist entry matches", []string{"stats", "pairing"}, "pairing.1", true},
	}
	for _, c := range cases {
		sub := &subscriber{topics: c.topics}
		if got := sub.matches(c.topic); got != c.want {
			t.Errorf("%s: matches(%q) with topics %v = %v, want %v",
				c.name, c.topic, c.topics, got, c.want)
		}
	}
}
