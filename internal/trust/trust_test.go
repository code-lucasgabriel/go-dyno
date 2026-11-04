package trust

import (
	"testing"

	"github.com/code-lucasgabriel/go-dyno/internal/results"
)

func TestGrade(t *testing.T) {
	cases := []struct {
		st    []string
		grade string
		score int
	}{
		{[]string{Pass, Pass, Skip}, "A", 100},
		{[]string{Pass, Warn}, "A", 90},
		{[]string{Warn, Warn}, "B", 80},
		{[]string{Warn, Warn, Warn, Warn}, "C", 60},
		{[]string{Pass, Fail}, "F", 60},
	}
	for _, c := range cases {
		var cs []results.Check
		for _, s := range c.st {
			cs = append(cs, results.Check{Status: s})
		}
		g := Grade(cs)
		if g.Grade != c.grade || g.Score != c.score || g.Failed != (c.grade == "F") {
			t.Errorf("%v: got %s %d failed=%v", c.st, g.Grade, g.Score, g.Failed)
		}
	}
}

func TestCV(t *testing.T) {
	if v := cv([]float64{10, 10, 10}); v != 0 {
		t.Errorf("cv of constants = %v", v)
	}
	if v := cv([]float64{9, 11}); v < 0.14 || v > 0.15 { // sd √2 / mean 10
		t.Errorf("cv = %v", v)
	}
}
