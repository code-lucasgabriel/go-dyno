package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExample(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dyno.yaml")
	os.WriteFile(p, []byte(Example), 0o644)
	c, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Entry().Name != "api" || c.Processes[0].CPUs != 2 || c.Load.Routes[1].Method != "POST" {
		t.Errorf("unexpected config: %+v", c)
	}
	if min := c.Load.Warmup + c.Capture.CPU + c.Capture.Trace; c.Load.Duration < min {
		t.Errorf("duration %v shorter than warm-up + captures %v", c.Load.Duration, min)
	}
	if c.Redline.P99 != 250*time.Millisecond {
		t.Errorf("redline p99 = %v", c.Redline.P99)
	}
}

func TestValidate(t *testing.T) {
	bad := map[string]string{
		"no processes":  "load: {rate: 1, base_url: x, routes: [{path: /}]}",
		"no debug":      "processes: [{name: a, command: [x]}]\nload: {rate: 1, base_url: x, routes: [{path: /}]}",
		"docker+cmd":    "processes: [{name: a, debug: h:1, command: [x], docker: {image: i}}]\nload: {rate: 1, base_url: x, routes: [{path: /}]}",
		"no rate":       "processes: [{name: a, debug: h:1, command: [x]}]\nload: {base_url: x, routes: [{path: /}]}",
		"bad regex":     "processes: [{name: a, debug: h:1, command: [x]}]\nload: {rate: 1, base_url: x, routes: [{path: /}]}\nfocus: [{regex: '('}]",
		"bad proc name": "processes: [{name: 'a b', debug: h:1, command: [x]}]\nload: {rate: 1, base_url: x, routes: [{path: /}]}",
	}
	for name, y := range bad {
		p := filepath.Join(t.TempDir(), "c.yaml")
		os.WriteFile(p, []byte(y), 0o644)
		if _, err := Read(p); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
