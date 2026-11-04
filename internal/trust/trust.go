// Package trust decides whether a run measured what it was meant to, and grades it.
package trust

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"

	"github.com/code-lucasgabriel/go-dyno/internal/load"
	"github.com/code-lucasgabriel/go-dyno/internal/results"
)

const (
	Pass = "pass"
	Warn = "warn"
	Fail = "fail"
	Skip = "skip"
)

// Evaluate runs the per-run checks on an extracted run folder and writes checks.json.
// A run with any failing check is marked FAILED in meta.json: it stays on disk
// but is left out of every aggregate.
func Evaluate(dir string) (results.Trust, error) {
	r, err := results.LoadRun(dir)
	if err != nil {
		return results.Trust{}, err
	}
	m := &r.Meta
	var lg load.Summary
	results.ReadJSON(filepath.Join(dir, "loadgen.json"), &lg)
	c := m.Checks
	var cs []results.Check
	add := func(name, status string, v, lim float64, f string, a ...any) {
		cs = append(cs, results.Check{Name: name, Status: status, Value: round(v), Limit: lim, Detail: fmt.Sprintf(f, a...)})
	}

	if len(m.Errors) > 0 {
		add("captures", Fail, float64(len(m.Errors)), 0, "%s", strings.Join(m.Errors, "; "))
	} else {
		add("captures", Pass, 0, 0, "all profiles and scrapes captured")
	}

	// error rate after warm-up
	reqs, _ := results.ReadCSV(r.Tidy("requests.csv.gz"))
	var n, bad, inCPU float64
	for _, row := range reqs {
		if row["in_window"] != "1" {
			continue
		}
		n++
		if row["ok"] != "1" {
			bad++
		}
		if row["in_cpu_window"] == "1" {
			inCPU++
		}
	}
	switch {
	case n == 0:
		add("error_rate", Fail, 0, c.MaxErrorRate, "no requests after warm-up")
	default:
		er := bad / n
		add("error_rate", status(er <= c.MaxErrorRate, Fail), er, c.MaxErrorRate, "%.0f of %.0f requests failed after warm-up", bad, n)
	}

	// requests in the CPU window vs the target rate
	window := m.Times.CPU1 - m.Times.CPU0
	if target := m.Rate * window; target > 0 {
		dev := math.Abs(inCPU-target) / target
		add("requests_on_target", status(dev <= c.RequestTolerance, Fail), dev, c.RequestTolerance,
			"%.0f requests sent in the CPU window, target %.0f", inCPU, target)
	}

	entry := m.Entry()
	for _, row := range r.Rows {
		p := row["process"]
		srv, lgn := results.F(row, "requests_server"), results.F(row, "requests_loadgen")
		if p == entry {
			if srv > 0 && lgn > 0 {
				dev := math.Abs(srv-lgn) / lgn
				add("server_count:"+p, status(dev <= c.RequestTolerance, Warn), dev, c.RequestTolerance,
					"server counted %.0f, load generator %.0f", srv, lgn)
			} else {
				add("server_count:"+p, Skip, 0, 0, "service doesn't count requests; dividing by the load generator's count")
			}
		}
		if row["throttled_periods"] == "" {
			add("throttling:"+p, Skip, 0, 0, "no cgroup counters (not in a container, or no dyno stats)")
		} else if th := results.F(row, "throttled_periods"); th > 0 && !c.AllowThrottling {
			add("throttling:"+p, Warn, th, 0, "%.0f throttled CFS periods in the CPU window", th)
		} else {
			add("throttling:"+p, Pass, th, 0, "no CPU throttling")
		}
		osCPU, pp := results.F(row, "cpu_s_os"), results.F(row, "cpu_s_pprof")
		if osCPU > 0 && pp > 0 {
			dev := math.Abs(pp-osCPU) / osCPU
			add("cpu_agreement:"+p, status(dev <= c.CPUAgreement, Warn), dev, c.CPUAgreement,
				"pprof %.2fs vs %s %.2fs", pp, row["cpu_source"], osCPU)
		} else {
			add("cpu_agreement:"+p, Skip, 0, c.CPUAgreement, "missing OS or pprof CPU")
		}
		switch row["goos"] {
		case "linux":
			add("linux:"+p, Pass, 0, 0, "profiled on Linux")
		case "":
			add("linux:"+p, Skip, 0, 0, "OS unknown")
		default:
			add("linux:"+p, Warn, 0, 0, "profiled on %s: the kernel can dominate the profile; run in Linux containers", row["goos"])
		}
	}

	// load generator health
	if lg.Planned > 0 {
		lr := float64(lg.Late) / float64(lg.Planned)
		add("loadgen_schedule", status(lr <= c.MaxLateRatio, Warn), lr, c.MaxLateRatio, "%d of %d requests sent late (>10 ms)", lg.Late, lg.Planned)
		if lg.Dropped > 0 {
			add("loadgen_dropped", Fail, float64(lg.Dropped), 0, "%d requests not sent: max_in_flight reached (service too slow for this rate)", lg.Dropped)
		}
		if lg.Cores > 0 && lg.End > lg.Start {
			u := lg.CPUSeconds / (lg.End - lg.Start) / float64(lg.Cores)
			add("loadgen_cpu", status(u <= 0.8, Warn), u, 0.8, "load generator used %.0f%% of host CPU", u*100)
		}
	}

	t := Grade(cs)
	t.RunID = m.RunID
	if err := results.WriteJSON(filepath.Join(dir, "checks.json"), t); err != nil {
		return t, err
	}
	want := "ok"
	if t.Failed {
		want = "FAILED"
	}
	if m.Status != want {
		m.Status = want
		results.WriteJSON(filepath.Join(dir, "meta.json"), m)
	}
	return t, nil
}

// Grade scores a list of checks: 100, minus 40 per failure and 10 per warning.
// A: ≥90 and no failure; B: ≥75; C: ≥50; F: any failure or below 50.
func Grade(cs []results.Check) results.Trust {
	score := 100
	failed := false
	for _, c := range cs {
		switch c.Status {
		case Fail:
			score -= 40
			failed = true
		case Warn:
			score -= 10
		}
	}
	if score < 0 {
		score = 0
	}
	g := "F"
	switch {
	case failed:
	case score >= 90:
		g = "A"
	case score >= 75:
		g = "B"
	case score >= 50:
		g = "C"
	}
	return results.Trust{Score: score, Grade: g, Failed: failed, Checks: cs}
}

// Scenario checks a scenario across its passing repetitions: enough reps, and
// a run-to-run spread of CPU per request small enough to trust differences.
func Scenario(runs []*results.Run, maxCV float64) []results.Check {
	pass := results.Passing(runs)
	var cs []results.Check
	st := Pass
	if len(pass) < 5 {
		st = Warn
	}
	cs = append(cs, results.Check{Name: "repetitions", Status: st, Value: float64(len(pass)), Limit: 5,
		Detail: fmt.Sprintf("%d passing of %d runs", len(pass), len(runs))})
	if len(pass) < 2 {
		return cs
	}
	for _, p := range results.Processes(pass) {
		var xs []float64
		for _, r := range pass {
			if v := r.PerRequest()[p].CPUus; v > 0 {
				xs = append(xs, v)
			}
		}
		if len(xs) < 2 {
			continue
		}
		cv := cv(xs)
		cs = append(cs, results.Check{Name: "noise:" + p, Status: status(cv <= maxCV, Warn), Value: round(cv), Limit: maxCV,
			Detail: fmt.Sprintf("CPU per request varies %.1f%% between runs (coefficient of variation)", cv*100)})
	}
	return cs
}

func cv(xs []float64) float64 {
	m := 0.0
	for _, x := range xs {
		m += x
	}
	m /= float64(len(xs))
	v := 0.0
	for _, x := range xs {
		v += (x - m) * (x - m)
	}
	v /= float64(len(xs) - 1)
	if m == 0 {
		return 0
	}
	return math.Sqrt(v) / m
}

func status(ok bool, otherwise string) string {
	if ok {
		return Pass
	}
	return otherwise
}

func round(v float64) float64 { return math.Round(v*1e4) / 1e4 }
