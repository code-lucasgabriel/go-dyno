// Package redline ramps load until the service stops keeping up, then explains why.
package redline

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/code-lucasgabriel/go-dyno/dyno"
	"github.com/code-lucasgabriel/go-dyno/internal/collect"
	"github.com/code-lucasgabriel/go-dyno/internal/config"
	"github.com/code-lucasgabriel/go-dyno/internal/extract"
	"github.com/code-lucasgabriel/go-dyno/internal/load"
	"github.com/code-lucasgabriel/go-dyno/internal/proc"
	"github.com/code-lucasgabriel/go-dyno/internal/results"
)

type ProcStep struct {
	CPUCores    float64 `json:"cpu_cores"`   // CPU seconds per second
	Utilisation float64 `json:"utilisation"` // of the process CPU limit (0 if no limit)
	Throttled   float64 `json:"throttled_periods"`
	Goroutines  int     `json:"goroutines"`
}

type Step struct {
	Rate         float64             `json:"rate"`
	Achieved     float64             `json:"achieved"` // successful requests per second, as the client saw them
	Served       float64             `json:"served"`   // requests per second the entry process completed (its own count)
	ErrorRate    float64             `json:"error_rate"`
	P50          float64             `json:"p50_ms"`
	P99          float64             `json:"p99_ms"`
	Mean         float64             `json:"mean_ms"`
	LateRatio    float64             `json:"late_ratio"`
	LoadgenCores float64             `json:"loadgen_cores"`
	Procs        map[string]ProcStep `json:"procs"`
	Pass         bool                `json:"pass"`
	Reason       string              `json:"reason,omitempty"`
	Dir          string              `json:"dir"`
}

type Finding struct {
	Kind     string `json:"kind"` // cpu_bound, throttling, loadgen, concurrency_cap, single_core, none
	Detail   string `json:"detail"`
	Evidence string `json:"evidence,omitempty"`
}

type Result struct {
	Scenario  string    `json:"scenario"`
	Knee      float64   `json:"knee"` // highest throughput that met the limits
	Limits    string    `json:"limits"`
	P99Ms     float64   `json:"p99_limit_ms"`
	Steps     []Step    `json:"steps"`
	Diagnosis []Finding `json:"diagnosis"`
}

// Run sweeps rates from cfg.Redline.Start, multiplying by Factor until a step fails,
// then bisects between the last passing and first failing rate.
func Run(ctx context.Context, cfg *config.Config, out string, logf func(string, ...any)) (Result, error) {
	rl := cfg.Redline
	dir := filepath.Join(out, "redline-"+cfg.Scenario)
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}
	proc.Reap(ctx, cfg, logf)
	if err := proc.Shell(ctx, cfg.Dir, cfg.Hooks.BeforeAll); err != nil {
		return Result{}, err
	}
	if err := proc.Build(ctx, cfg, logf); err != nil {
		return Result{}, err
	}
	if err := proc.Shell(ctx, cfg.Dir, cfg.Hooks.BeforeRep); err != nil {
		return Result{}, err
	}
	var running []*proc.Running
	defer func() {
		for _, r := range running {
			r.Stop()
		}
	}()
	for _, p := range cfg.Processes {
		r, err := proc.Start(ctx, cfg, p, "redline-"+cfg.Scenario, filepath.Join(dir, p.Name+".log"))
		if err != nil {
			return Result{}, err
		}
		running = append(running, r)
	}

	res := Result{Scenario: cfg.Scenario, P99Ms: float64(rl.P99.Milliseconds()), Limits: fmt.Sprintf("p99 ≤ %v, errors ≤ %.1f%%, throughput ≥ %.0f%% of target", rl.P99, rl.MaxErrorRate*100, rl.MinAchieved*100)}
	run := func(rate float64) (Step, error) {
		logf("redline %s: %.0f req/s", cfg.Scenario, rate)
		s, err := step(ctx, cfg, rate, dir)
		if err == nil {
			logf("  achieved %.0f req/s (server completed %.0f), p99 %.1f ms, errors %.2f%% → %s", s.Achieved, s.Served, s.P99, s.ErrorRate*100, map[bool]string{true: "pass", false: "FAIL: " + s.Reason}[s.Pass])
		}
		return s, err
	}
	var lastPass, firstFail *Step
	for rate := rl.Start; rate <= rl.Max; rate *= rl.Factor {
		s, err := run(rate)
		if err != nil {
			return res, err
		}
		res.Steps = append(res.Steps, s)
		if !s.Pass {
			firstFail = &res.Steps[len(res.Steps)-1]
			break
		}
		lastPass = &res.Steps[len(res.Steps)-1]
	}
	if lastPass != nil && firstFail != nil {
		lo, hi := lastPass.Rate, firstFail.Rate
		for i := 0; i < rl.Refine; i++ {
			mid := math.Round(math.Sqrt(lo * hi))
			if mid <= lo || mid >= hi {
				break
			}
			s, err := run(mid)
			if err != nil {
				return res, err
			}
			res.Steps = append(res.Steps, s)
			if s.Pass {
				lo = mid
			} else {
				hi = mid
			}
		}
	}
	sort.Slice(res.Steps, func(i, j int) bool { return res.Steps[i].Rate < res.Steps[j].Rate })
	for _, s := range res.Steps {
		if s.Pass && s.Achieved > res.Knee {
			res.Knee = s.Achieved
		}
	}
	res.Diagnosis = Diagnose(res.Steps, cfg, dir)
	return res, results.WriteJSON(filepath.Join(dir, "redline.json"), res)
}

func step(ctx context.Context, cfg *config.Config, rate float64, root string) (Step, error) {
	rl := cfg.Redline
	dir := filepath.Join(root, fmt.Sprintf("step-%06.0f", rate))
	os.MkdirAll(dir, 0o755)
	s := Step{Rate: rate, Procs: map[string]ProcStep{}, Dir: filepath.Base(dir)}
	var mu sync.Mutex
	var lat []float64
	var n, bad float64
	var t0, t1 float64
	sink := func(x load.Sample) {
		if t0 == 0 || x.T < t0 || x.T >= t1 {
			return
		}
		mu.Lock()
		n++
		if x.OK {
			lat = append(lat, x.LatencyMs)
		} else {
			bad++
		}
		mu.Unlock()
	}
	start := time.Now()
	t0 = unix(start.Add(rl.Warmup))
	t1 = unix(start.Add(rl.Warmup + rl.Step))
	done := make(chan load.Summary, 1)
	var loadErr error
	go func() {
		if cfg.Load.K6 != nil {
			var sum load.Summary
			sum, loadErr = load.RunK6(ctx, cfg, rate, rl.Warmup+rl.Step, dir, sink, func([]string) {})
			done <- sum
			return
		}
		done <- load.Run(ctx, cfg.Load, rate, rl.Warmup+rl.Step, sink)
	}()

	time.Sleep(time.Until(start.Add(rl.Warmup)))
	s0 := stats(ctx, cfg)
	var wg sync.WaitGroup
	for _, p := range cfg.Processes {
		wg.Add(2)
		go func() {
			defer wg.Done()
			collect.Fetch(ctx, p, fmt.Sprintf("/debug/pprof/profile?seconds=%d", int(rl.Step.Seconds())), filepath.Join(dir, p.Name+"-cpu.pb.gz"), rl.Step+30*time.Second)
		}()
		go func() {
			defer wg.Done()
			time.Sleep(rl.Step / 2)
			collect.Fetch(ctx, p, "/debug/pprof/goroutine?debug=2", filepath.Join(dir, p.Name+"-goroutine.txt"), 30*time.Second)
		}()
	}
	time.Sleep(time.Until(start.Add(rl.Warmup + rl.Step)))
	s1 := stats(ctx, cfg)
	window := rl.Step.Seconds()
	sum := <-done
	wg.Wait()
	if loadErr != nil {
		return s, loadErr
	}

	// completed requests sent inside the window
	s.Achieved = float64(len(lat)) / window
	if n > 0 {
		s.ErrorRate = bad / n
	}
	sort.Float64s(lat)
	if len(lat) > 0 {
		s.P50, s.P99 = results.Quantile(lat, .5), results.Quantile(lat, .99)
		t := 0.0
		for _, v := range lat {
			t += v
		}
		s.Mean = t / float64(len(lat))
	}
	if sum.Planned > 0 {
		s.LateRatio = float64(sum.Late) / float64(sum.Planned)
	}
	if d := sum.End - sum.Start; d > 0 {
		s.LoadgenCores = sum.CPUSeconds / d
	}
	for _, p := range cfg.Processes {
		a, b := s0[p.Name], s1[p.Name]
		if a == nil || b == nil {
			continue
		}
		if p.Entry && b.Time > a.Time {
			s.Served = (b.Requests - a.Requests) / (b.Time - a.Time)
		}
		ps := ProcStep{Goroutines: b.Goroutines}
		cpu := b.CPUSeconds - a.CPUSeconds
		if a.Cgroup != nil && b.Cgroup != nil {
			cpu = b.Cgroup.CPUSeconds - a.Cgroup.CPUSeconds
			ps.Throttled = b.Cgroup.ThrottledPeriods - a.Cgroup.ThrottledPeriods
		}
		ps.CPUCores = cpu / (b.Time - a.Time)
		if p.CPUs > 0 {
			ps.Utilisation = ps.CPUCores / p.CPUs
		}
		s.Procs[p.Name] = ps
	}
	var why []string
	if s.ErrorRate > rl.MaxErrorRate {
		why = append(why, fmt.Sprintf("errors %.1f%%", s.ErrorRate*100))
	}
	if len(lat) == 0 {
		why = append(why, "no successful requests")
	} else if s.P99 > float64(rl.P99.Milliseconds()) {
		why = append(why, fmt.Sprintf("p99 %.0f ms", s.P99))
	}
	if s.Achieved < rl.MinAchieved*rate {
		why = append(why, fmt.Sprintf("throughput %.0f of %.0f req/s", s.Achieved, rate))
	}
	s.Pass = len(why) == 0
	s.Reason = strings.Join(why, ", ")
	results.WriteJSON(filepath.Join(dir, "step.json"), s)
	return s, nil
}

func stats(ctx context.Context, cfg *config.Config) map[string]*dyno.Stats {
	out := map[string]*dyno.Stats{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range cfg.Processes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s, err := collect.Stats(ctx, p); err == nil {
				mu.Lock()
				out[p.Name] = &s
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out
}

// idle states that don't indicate queueing for work
var idle = map[string]bool{"IO wait": true, "running": true, "runnable": true, "syscall": true, "sleep": true}

// Diagnose explains the first failing step (or the last step, if none failed).
func Diagnose(steps []Step, cfg *config.Config, dir string) []Finding {
	if len(steps) == 0 {
		return nil
	}
	var fail *Step
	for i := range steps {
		if !steps[i].Pass {
			fail = &steps[i]
			break
		}
	}
	if fail == nil {
		last := steps[len(steps)-1]
		return []Finding{{Kind: "none", Detail: fmt.Sprintf("met the limits at every step up to %.0f req/s; raise redline.max to find the ceiling", last.Rate)}}
	}
	var fs []Finding
	names := make([]string, 0, len(fail.Procs))
	for n := range fail.Procs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := fail.Procs[n]
		if p.Throttled > 0 {
			fs = append(fs, Finding{Kind: "throttling", Detail: fmt.Sprintf("%s was throttled by its CPU limit (%.0f periods)", n, p.Throttled),
				Evidence: fmt.Sprintf("at %.0f req/s", fail.Rate)})
		}
		if p.Utilisation >= 0.9 {
			fs = append(fs, Finding{Kind: "cpu_bound", Detail: fmt.Sprintf("%s is CPU-bound: %.2f of %.2g cores", n, p.CPUCores, p.CPUCores/p.Utilisation),
				Evidence: "profile: " + filepath.Join(fail.Dir, n+"-cpu.pb.gz")})
		} else if p.CPUCores >= 0.85 && p.CPUCores <= 1.15 && (p.Utilisation == 0 || p.Utilisation < 0.6) {
			fs = append(fs, Finding{Kind: "single_core", Detail: fmt.Sprintf("%s uses about one core (%.2f) while more are available: a single goroutine may be the bottleneck", n, p.CPUCores),
				Evidence: "look for one hot loop in " + filepath.Join(fail.Dir, n+"-cpu.pb.gz")})
		}
	}
	if fail.LoadgenCores >= 0.8*float64(runtime.NumCPU()) || fail.LateRatio > 0.05 {
		fs = append(fs, Finding{Kind: "loadgen", Detail: fmt.Sprintf("the load generator fell behind (%.0f%% of requests late, %.1f cores used)", fail.LateRatio*100, fail.LoadgenCores),
			Evidence: "run go-dyno on a separate machine or lower the rate"})
	}
	if len(fs) == 0 {
		// Little's law: a service that can hold at most N requests at once,
		// each for S seconds, completes at most N/S per second.
		base := steps[0]
		peak := 0.0
		for _, s := range steps { // the server's own count still sees completions the client gave up on
			peak = math.Max(peak, math.Max(s.Achieved, s.Served))
		}
		// queueing: either throughput fell short, or requests wait far longer than at low load
		short := fail.Achieved < cfg.Redline.MinAchieved*fail.Rate
		waiting := base.Mean > 0 && fail.Mean >= 3*base.Mean
		if base.Mean > 0 && (short || waiting) {
			n := peak * base.Mean / 1000
			f := Finding{Kind: "concurrency_cap", Detail: fmt.Sprintf("requests queue at %.0f req/s with no process CPU-bound (mean latency %.0f ms vs %.1f ms at low load): something holds at most ~%.0f requests at once (Little's law: %.0f req/s × %.1f ms)", peak, fail.Mean, base.Mean, n, peak, base.Mean)}
			if g := topWait(filepath.Join(dir, fail.Dir), cfg); g != "" {
				f.Evidence = g
			}
			fs = append(fs, f)
		} else {
			fs = append(fs, Finding{Kind: "latency", Detail: fmt.Sprintf("latency crossed the limit at %.0f req/s without a CPU, throttling or concurrency limit; check downstream dependencies", fail.Rate)})
		}
	}
	return fs
}

// topWait returns the largest group of goroutines waiting in the same place at the failing step.
func topWait(stepDir string, cfg *config.Config) string {
	best, bestN := "", 0
	for _, p := range cfg.Processes {
		for _, g := range extract.Goroutines(filepath.Join(stepDir, p.Name+"-goroutine.txt")) {
			if idle[g.State] {
				continue
			}
			if g.Count > bestN {
				best, bestN = fmt.Sprintf("%s: %d goroutines in [%s] at %s", p.Name, g.Count, g.State, g.Top), g.Count
			}
		}
	}
	return best
}

func unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }
