// Package runner performs measurement runs: fresh processes, load, a fixed capture window.
package runner

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/code-lucasgabriel/go-dyno/internal/collect"
	"github.com/code-lucasgabriel/go-dyno/internal/config"
	"github.com/code-lucasgabriel/go-dyno/internal/extract"
	"github.com/code-lucasgabriel/go-dyno/internal/load"
	"github.com/code-lucasgabriel/go-dyno/internal/proc"
	"github.com/code-lucasgabriel/go-dyno/internal/results"
	"github.com/code-lucasgabriel/go-dyno/internal/trust"
)

// Version is set by the CLI.
var Version = "dev"

type Options struct {
	Out   string // results root
	Label string // build label, e.g. base / head
	SHA   string // commit; detected with git when empty
	Reps  int    // overrides cfg.Reps when > 0
	Log   func(format string, a ...any)
}

// Result of one repetition.
type Result struct {
	RunID string
	Dir   string
	Trust results.Trust
}

// Run builds images once, then performs every repetition of the scenario.
func Run(ctx context.Context, cfg *config.Config, o Options) ([]Result, error) {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	if o.SHA == "" {
		o.SHA = gitSHA(cfg.Dir)
	}
	reps := cfg.Reps
	if o.Reps > 0 {
		reps = o.Reps
	}
	proc.Reap(ctx, cfg, o.Log)
	if err := proc.Shell(ctx, cfg.Dir, cfg.Hooks.BeforeAll); err != nil {
		return nil, err
	}
	if err := proc.Build(ctx, cfg, o.Log); err != nil {
		return nil, err
	}
	var out []Result
	for rep := 1; rep <= reps; rep++ {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		id := fmt.Sprintf("%s-r%d", cfg.Scenario, rep)
		o.Log("run %s (%d/%d)", id, rep, reps)
		res, err := one(ctx, cfg, o, id, rep)
		if err != nil {
			return out, fmt.Errorf("%s: %w", id, err)
		}
		o.Log("  %s: grade %s (%d)%s", id, res.Trust.Grade, res.Trust.Score, failNote(res.Trust))
		out = append(out, res)
	}
	return out, proc.Shell(ctx, cfg.Dir, cfg.Hooks.AfterAll)
}

func failNote(t results.Trust) string {
	var bad []string
	for _, c := range t.Checks {
		if c.Status == trust.Fail || c.Status == trust.Warn {
			bad = append(bad, c.Status+" "+c.Name+": "+c.Detail)
		}
	}
	if len(bad) == 0 {
		return ""
	}
	return "\n    " + strings.Join(bad, "\n    ")
}

func one(ctx context.Context, cfg *config.Config, o Options, id string, rep int) (Result, error) {
	dir := filepath.Join(o.Out, id)
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}
	P := func(n string) string { return filepath.Join(dir, n) }
	if err := proc.Shell(ctx, cfg.Dir, cfg.Hooks.BeforeRep); err != nil {
		return Result{}, err
	}

	m := results.Meta{
		Tool: "go-dyno " + Version, Name: cfg.Name, RunID: id, Scenario: cfg.Scenario, Rep: rep,
		Label: o.Label, SHA: o.SHA, HostOS: runtime.GOOS, HostArch: runtime.GOARCH, HostCPUs: runtime.NumCPU(),
		Rate: cfg.Load.Rate, RateSrc: cfg.Load.RateSource, Warmup: cfg.Load.Warmup.Seconds(), Params: cfg.Params, Checks: cfg.Checks, Focus: cfg.Focus,
		Status: "ok",
	}
	var mu sync.Mutex
	fail := func(f string, a ...any) {
		mu.Lock()
		m.Errors = append(m.Errors, fmt.Sprintf(f, a...))
		mu.Unlock()
	}

	// fresh processes
	var running []*proc.Running
	stopAll := func() {
		for _, r := range running {
			r.Stop()
		}
		running = nil
	}
	defer stopAll()
	for _, p := range cfg.Processes {
		r, err := proc.Start(ctx, cfg, p, id, P(p.Name+".log"))
		if err != nil {
			return Result{}, err
		}
		running = append(running, r)
		pm := results.ProcMeta{Name: p.Name, Entry: p.Entry, Mode: r.Mode, CPUs: p.CPUs, Metrics: p.Metrics, Start: unix(r.Started)}
		if p.Docker != nil {
			pm.Image = p.Docker.Image
		}
		m.Processes = append(m.Processes, pm)
	}
	// a process can report its own start time; prefer it (go run compiles first)
	for i, p := range cfg.Processes {
		if s, err := collect.Stats(ctx, p); err == nil && s.StartTime > 0 {
			m.Processes[i].Start = s.StartTime
		}
	}

	// load in the background
	reqW, err := load.NewWriter(P("requests.csv.gz"))
	if err != nil {
		return Result{}, err
	}
	var cnt *csv.Writer
	if cfg.Load.K6 != nil {
		f, _ := os.Create(P("counters-raw.csv"))
		defer f.Close()
		cnt = csv.NewWriter(f)
		cnt.Write([]string{"t", "metric", "value", "tags"})
		defer cnt.Flush()
	}
	start := time.Now()
	m.Times.LoadStart = unix(start)
	loadDone := make(chan load.Summary, 1)
	go func() {
		if cfg.Load.K6 != nil {
			s, err := load.RunK6(ctx, cfg, cfg.Load.Rate, cfg.Load.Duration, dir, reqW.Write, func(r []string) { cnt.Write(r) })
			if err != nil {
				fail("%v", err)
			}
			loadDone <- s
			return
		}
		loadDone <- load.Run(ctx, cfg.Load, cfg.Load.Rate, cfg.Load.Duration, reqW.Write)
	}()

	// leak sampler: goroutines + heap every interval, heap snapshot every snapshot interval
	sctx, stopSampler := context.WithCancel(ctx)
	var swg sync.WaitGroup
	for _, p := range cfg.Processes {
		swg.Add(1)
		go func() { defer swg.Done(); sample(sctx, p, cfg.Capture, start, dir) }()
	}

	// capture window, after warm-up
	if !sleepUntil(ctx, start.Add(cfg.Load.Warmup)) {
		stopSampler()
		return Result{}, ctx.Err()
	}
	m.Times.CPU0 = unix(time.Now())
	parallel(cfg.Processes, func(p config.Process) {
		if err := scrape(ctx, p, P("stats-"+p.Name+"-0.json")); err != nil {
			fail("%s stats: %v", p.Name, err)
		}
	})
	cpuSec := int(cfg.Capture.CPU.Seconds())
	parallel(cfg.Processes, func(p config.Process) {
		if err := collect.Fetch(ctx, p, fmt.Sprintf("/debug/pprof/profile?seconds=%d", cpuSec), P(p.Name+"-cpu.pb.gz"), cfg.Capture.CPU+30*time.Second); err != nil {
			fail("%s cpu profile: %v", p.Name, err)
		}
	})
	parallel(cfg.Processes, func(p config.Process) {
		if err := scrape(ctx, p, P("stats-"+p.Name+"-1.json")); err != nil {
			fail("%s stats: %v", p.Name, err)
		}
	})
	m.Times.CPU1 = unix(time.Now())
	traceSec := int(cfg.Capture.Trace.Seconds())
	parallel(cfg.Processes, func(p config.Process) {
		grabs := [][2]string{
			{"/debug/pprof/heap", p.Name + "-heap.pb.gz"},
			{"/debug/pprof/allocs", p.Name + "-allocs.pb.gz"},
			{"/debug/pprof/goroutine?debug=2", p.Name + "-goroutine.txt"},
		}
		if traceSec > 0 {
			grabs = append(grabs, [2]string{fmt.Sprintf("/debug/pprof/trace?seconds=%d", traceSec), p.Name + "-trace.out"})
		}
		if cfg.Capture.Contention {
			grabs = append(grabs, [2]string{"/debug/pprof/mutex", p.Name + "-mutex.pb.gz"}, [2]string{"/debug/pprof/block", p.Name + "-block.pb.gz"})
		}
		for _, gb := range grabs {
			if err := collect.Fetch(ctx, p, gb[0], P(gb[1]), cfg.Capture.Trace+30*time.Second); err != nil {
				fail("%s %s: %v", p.Name, gb[0], err)
			}
		}
	})

	sum := <-loadDone
	m.Times.LoadEnd = unix(time.Now())
	reqW.Close()
	stopSampler()
	swg.Wait()
	stopAll()
	if err := proc.Shell(ctx, cfg.Dir, cfg.Hooks.AfterRep); err != nil {
		fail("%v", err)
	}

	results.WriteJSON(P("loadgen.json"), sum)
	if err := results.WriteJSON(P("meta.json"), m); err != nil {
		return Result{}, err
	}
	if err := extract.Run(dir); err != nil {
		return Result{}, fmt.Errorf("extract: %w", err)
	}
	t, err := trust.Evaluate(dir)
	if err != nil {
		return Result{}, err
	}
	writeSummary(P("meta.md"), dir, t)
	return Result{RunID: id, Dir: dir, Trust: t}, nil
}

func scrape(ctx context.Context, p config.Process, path string) error {
	s, err := collect.Stats(ctx, p)
	if err != nil {
		return err
	}
	return results.WriteJSON(path, s)
}

func sample(ctx context.Context, p config.Process, c config.Capture, start time.Time, dir string) {
	f, err := os.Create(filepath.Join(dir, "runtime-"+p.Name+".csv"))
	if err != nil {
		return
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{"t", "goroutines", "heap_inuse", "heap_alloc", "heap_sys", "num_gc"})
	nextSnap := time.Duration(0)
	tick := time.NewTicker(c.SampleInterval)
	defer tick.Stop()
	for {
		t := time.Since(start)
		if s, err := collect.Stats(ctx, p); err == nil {
			w.Write([]string{fmt.Sprintf("%.1f", t.Seconds()), fmt.Sprint(s.Goroutines), g(s.HeapInuse), g(s.HeapAlloc), g(s.HeapSys), g(s.NumGC)})
			w.Flush()
		}
		if t >= nextSnap {
			collect.Fetch(ctx, p, "/debug/pprof/heap", filepath.Join(dir, fmt.Sprintf("heapsnap-%s-%04d.pb.gz", p.Name, int(t.Seconds()))), 30*time.Second)
			nextSnap += c.SnapshotInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func writeSummary(path, dir string, t results.Trust) {
	r, err := results.LoadRun(dir)
	if err != nil {
		return
	}
	m := r.Meta
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", m.RunID)
	if m.Status == "FAILED" {
		b.WriteString("**FAILED** — kept for inspection, excluded from aggregates.\n\n")
	}
	fmt.Fprintf(&b, "- grade: %s (%d)\n- scenario: %s, rep %d, label %q, sha %s\n", t.Grade, t.Score, m.Scenario, m.Rep, m.Label, m.SHA)
	fmt.Fprintf(&b, "- host: %s/%s, %d CPUs; tool %s\n", m.HostOS, m.HostArch, m.HostCPUs, m.Tool)
	fmt.Fprintf(&b, "- load: %s; warm-up %.0fs, CPU window %.1fs, %.0f requests counted\n", m.RateSrc.Describe(m.Rate), m.Warmup, m.Times.CPU1-m.Times.CPU0, r.Requests())
	for p, pr := range r.PerRequest() {
		fmt.Fprintf(&b, "- %s: %.1f µs CPU/request, %.0f B allocated/request, GC %.1f%% of CPU\n", p, pr.CPUus, pr.AllocB, pr.GCShare*100)
	}
	for k, v := range m.Params {
		fmt.Fprintf(&b, "- %s = %s\n", k, v)
	}
	b.WriteString("\n## Checks\n\n| check | status | detail |\n|---|---|---|\n")
	for _, c := range t.Checks {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", c.Name, c.Status, c.Detail)
	}
	os.WriteFile(path, []byte(b.String()), 0o644)
}

// Parallel runs f for every process concurrently and waits.
func parallel(ps []config.Process, f func(config.Process)) {
	var wg sync.WaitGroup
	for _, p := range ps {
		wg.Add(1)
		go func() { defer wg.Done(); f(p) }()
	}
	wg.Wait()
}

func sleepUntil(ctx context.Context, t time.Time) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Until(t)):
		return true
	}
}

func gitSHA(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }
func g(v float64) string       { return fmt.Sprintf("%.0f", v) }
