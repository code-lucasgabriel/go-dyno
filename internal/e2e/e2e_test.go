// Package e2e runs go-dyno against the demo service and checks it finds each planted problem.
// Skipped with -short; takes about two minutes.
package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/code-lucasgabriel/go-dyno/internal/config"
	"github.com/code-lucasgabriel/go-dyno/internal/redline"
	"github.com/code-lucasgabriel/go-dyno/internal/report"
	"github.com/code-lucasgabriel/go-dyno/internal/results"
	"github.com/code-lucasgabriel/go-dyno/internal/runner"
)

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func setup(t *testing.T, yaml string) (*config.Config, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "demo")
	if out, err := exec.Command("go", "build", "-o", bin, "../../examples/demo").CombinedOutput(); err != nil {
		t.Fatalf("build demo: %v\n%s", err, out)
	}
	app, dbg := freePort(t), freePort(t)
	y := fmt.Sprintf(`name: e2e
cpus: 2
processes:
  - name: api
    debug: 127.0.0.1:%d
    ready: http://127.0.0.1:%d/healthz
    command: [%q]
    env: {ADDR: "127.0.0.1:%d"}
load:
  base_url: http://127.0.0.1:%d
`, dbg, app, bin, app, app) + yaml
	p := filepath.Join(dir, "dyno.yaml")
	os.WriteFile(p, []byte(y), 0o644)
	cfg, err := config.Read(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, filepath.Join(dir, "results")
}

func TestHotspot(t *testing.T) {
	cfg, out := setup(t, `  rate: 150
  warmup: 2s
  routes: [{name: hot, path: /hot}]
scenario: hot
reps: 2
capture: {cpu: 4s, trace: 1s, sample_interval: 1s, snapshot_interval: 2s}
focus: [{process: api, label: regexp, regex: "regexp"}]
`)
	res, err := runner.Run(context.Background(), cfg, runner.Options{Out: out, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("runs = %d", len(res))
	}
	for _, r := range res {
		for _, c := range r.Trust.Checks {
			if c.Status == "fail" {
				t.Errorf("%s: check %s failed: %s", r.RunID, c.Name, c.Detail)
			}
		}
	}
	s, err := report.Summarize(out)
	if err != nil {
		t.Fatal(err)
	}
	p := s.Scenarios[0].Procs[0]
	// On Linux the hotspot dominates; on a macOS host the kernel takes most of the
	// profile (which the linux check warns about), so expect less there.
	want := 0.4
	if runtime.GOOS != "linux" {
		want = 0.08
	}
	if len(p.Focus) == 0 || p.Focus[0].Share.Median < want {
		t.Errorf("regexp share of CPU = %+v, want ≥ %.2f", p.Focus, want)
	}
	if p.CPUus.Median <= 0 || p.AllocB.Median <= 0 {
		t.Errorf("per-request figures missing: %+v", p)
	}
	d, err := report.Build(out, "")
	if err != nil {
		t.Fatal(err)
	}
	html := filepath.Join(out, "report.html")
	if err := report.Write(d, html); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(html)
	if !strings.Contains(string(b), `"name":"hot"`) || strings.Contains(string(b), "/*DATA*/null") {
		t.Error("report data not embedded")
	}
}

func TestLeak(t *testing.T) {
	cfg, out := setup(t, `  rate: 100
  warmup: 3s
  duration: 15s
  routes: [{name: leak, path: /leak}]
scenario: leak
reps: 1
capture: {cpu: 3s, trace: 1s, sample_interval: 1s, snapshot_interval: 3s}
`)
	if _, err := runner.Run(context.Background(), cfg, runner.Options{Out: out, Log: t.Logf}); err != nil {
		t.Fatal(err)
	}
	s, err := report.Summarize(out)
	if err != nil {
		t.Fatal(err)
	}
	p := s.Scenarios[0].Procs[0]
	// 100 req/s, one leaked goroutine per request: ~6000/min
	if p.GoroutinesSlope < 2000 {
		t.Errorf("goroutines/min = %.0f, want a clear upward slope", p.GoroutinesSlope)
	}
	found := false
	for _, g := range p.HeapGrowth {
		if strings.Contains(g.Function, "main.leak") && g.Bytes > 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("heap growth should point at main.leak: %+v", p.HeapGrowth)
	}
	run, _ := results.LoadRun(filepath.Join(out, "leak-r1"))
	if len(run.Goroutes) == 0 {
		t.Error("goroutine groups missing")
	}
}

func TestRedline(t *testing.T) {
	cfg, out := setup(t, `  rate: 50
  timeout: 2s
  routes: [{name: work, path: /work}]
scenario: work
redline: {start: 50, factor: 2, step: 4s, warmup: 2s, p99: 150ms, refine: 1}
`)
	start := time.Now()
	r, err := redline.Run(context.Background(), cfg, out, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("redline took %v, knee %.0f, diagnosis %+v", time.Since(start), r.Knee, r.Diagnosis)
	// 4 workers × 20 ms each → at most 200 req/s
	if r.Knee < 80 || r.Knee > 230 {
		t.Errorf("knee = %.0f, want near the 200 req/s ceiling", r.Knee)
	}
	if len(r.Diagnosis) == 0 || r.Diagnosis[0].Kind != "concurrency_cap" {
		t.Fatalf("diagnosis = %+v, want concurrency_cap", r.Diagnosis)
	}
	if !strings.Contains(r.Diagnosis[0].Evidence, "main.work") {
		t.Errorf("evidence should name the waiting handler: %q", r.Diagnosis[0].Evidence)
	}
}
