package main

import "testing"

func TestWordErrorRate(t *testing.T) {
	cases := []struct {
		ref, hyp string
		want     int
	}{
		{"List the files.", "list the files", 0},
		{"list the files", "list files", 1},                             // deletion
		{"list the files", "list all the files", 1},                     // insertion
		{"create a branch called auth", "create a bench called off", 2}, // substitutions
		{"", "anything", 1},
		{"what's changed", "whats changed", 1}, // apostrophes are part of the word
	}
	for _, c := range cases {
		if got := editDistance(words(c.ref), words(c.hyp)); got != c.want {
			t.Errorf("edits(%q, %q) = %d, want %d", c.ref, c.hyp, got, c.want)
		}
	}
}
