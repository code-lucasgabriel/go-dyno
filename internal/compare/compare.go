// Package compare decides whether a head build regressed against a base build,
// using the run-to-run spread as the noise floor.
package compare

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/code-lucasgabriel/go-dyno/internal/results"
)

type Thresholds struct {
	CPU   float64 // relative change in CPU per request
	Alloc float64
	P99   float64
	P50   float64
}

var Default = Thresholds{CPU: 0.05, Alloc: 0.05, P99: 0.10, P50: 0.10}

type Metric struct {
	Scenario string      `json:"scenario"`
	Process  string      `json:"process,omitempty"`
	Name     string      `json:"name"`
	Unit     string      `json:"unit"`
	Base     results.Agg `json:"base"`
	Head     results.Agg `json:"head"`
	Change   float64     `json:"change"`  // head/base - 1
	Allowed  float64     `json:"allowed"` // max(threshold, noise)
	Verdict  string      `json:"verdict"` // regressed | improved | same | n/a
}

type FuncDelta struct {
	Scenario string  `json:"scenario"`
	Process  string  `json:"process"`
	Function string  `json:"function"`
	Base     float64 `json:"base"` // flat share
	Head     float64 `json:"head"`
}

type Report struct {
	Base      string      `json:"base"`
	Head      string      `json:"head"`
	Metrics   []Metric    `json:"metrics"`
	Functions []FuncDelta `json:"functions"`
	Regressed bool        `json:"regressed"`
	Skipped   []string    `json:"skipped,omitempty"`
	Warnings  []string    `json:"warnings,omitempty"` // base and head measured under different conditions
}

// environment describes what must match for a comparison to mean anything.
func environment(rs []*results.Run) map[string]string {
	env := map[string]string{}
	for _, r := range rs {
		m := r.Meta
		env["host"] = fmt.Sprintf("%s/%s, %d CPUs", m.HostOS, m.HostArch, m.HostCPUs)
		env["rate"] = fmt.Sprintf("%g req/s", m.Rate)
		for _, p := range m.Processes {
			env["cpus:"+p.Name] = fmt.Sprintf("%g", p.CPUs)
			env["mode:"+p.Name] = p.Mode
		}
		for _, row := range r.Rows {
			env["os:"+row["process"]] = row["goos"]
		}
		for k, v := range m.Params {
			env["param:"+k] = v
		}
	}
	return env
}

// Run compares every scenario present in both result folders.
func Run(baseDir, headDir string, th Thresholds) (Report, error) {
	rep := Report{Base: baseDir, Head: headDir}
	bRuns, err := results.LoadAll(baseDir)
	if err != nil {
		return rep, err
	}
	hRuns, err := results.LoadAll(headDir)
	if err != nil {
		return rep, err
	}
	bo, bs := results.ByScenario(results.Passing(bRuns))
	_, hs := results.ByScenario(results.Passing(hRuns))
	for _, sc := range bo {
		b, h := bs[sc], hs[sc]
		if len(h) == 0 {
			rep.Skipped = append(rep.Skipped, sc+": no passing head runs")
			continue
		}
		be, he := environment(b), environment(h)
		keys := make([]string, 0, len(be))
		for k := range be {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if hv, ok := he[k]; ok && hv != be[k] {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: %s differs (base %s, head %s)", sc, k, be[k], hv))
			}
		}
		for _, p := range results.Processes(b) {
			collect := func(rs []*results.Run, f func(results.PerRequest) float64) results.Agg {
				var xs []float64
				for _, r := range rs {
					if pr, ok := r.PerRequest()[p]; ok {
						xs = append(xs, f(pr))
					}
				}
				return results.Aggregate(xs)
			}
			rep.add(sc, p, "cpu_per_request", "µs", collect(b, func(x results.PerRequest) float64 { return x.CPUus }), collect(h, func(x results.PerRequest) float64 { return x.CPUus }), th.CPU)
			rep.add(sc, p, "alloc_per_request", "B", collect(b, func(x results.PerRequest) float64 { return x.AllocB }), collect(h, func(x results.PerRequest) float64 { return x.AllocB }), th.Alloc)

			bf, hf := results.Functions(b, p, false), results.Functions(h, p, false)
			m := map[string]*FuncDelta{}
			for _, f := range bf {
				m[f.Function] = &FuncDelta{Scenario: sc, Process: p, Function: f.Function, Base: f.Flat}
			}
			for _, f := range hf {
				if d, ok := m[f.Function]; ok {
					d.Head = f.Flat
				} else {
					m[f.Function] = &FuncDelta{Scenario: sc, Process: p, Function: f.Function, Head: f.Flat}
				}
			}
			var ds []FuncDelta
			for _, d := range m {
				ds = append(ds, *d)
			}
			sort.Slice(ds, func(i, j int) bool { return math.Abs(ds[i].Head-ds[i].Base) > math.Abs(ds[j].Head-ds[j].Base) })
			if len(ds) > 10 {
				ds = ds[:10]
			}
			rep.Functions = append(rep.Functions, ds...)
		}
		lat := func(rs []*results.Run, q func(results.Latency) float64) results.Agg {
			var xs []float64
			for _, r := range rs {
				for _, l := range results.Latencies([]*results.Run{r}) {
					if l.Route == "*" {
						xs = append(xs, q(l))
					}
				}
			}
			return results.Aggregate(xs)
		}
		rep.add(sc, "", "p50_latency", "ms", lat(b, func(l results.Latency) float64 { return l.P50 }), lat(h, func(l results.Latency) float64 { return l.P50 }), th.P50)
		rep.add(sc, "", "p99_latency", "ms", lat(b, func(l results.Latency) float64 { return l.P99 }), lat(h, func(l results.Latency) float64 { return l.P99 }), th.P99)
	}
	return rep, nil
}

func (r *Report) add(sc, p, name, unit string, b, h results.Agg, th float64) {
	m := Metric{Scenario: sc, Process: p, Name: name, Unit: unit, Base: b, Head: h, Verdict: "n/a"}
	if b.N > 0 && h.N > 0 && b.Median > 0 {
		m.Change = h.Median/b.Median - 1
		// a difference smaller than the spread between repetitions is noise
		m.Allowed = math.Max(th, math.Max(b.Spread(), h.Spread())/2)
		switch {
		case m.Change > m.Allowed:
			m.Verdict = "regressed"
			r.Regressed = true
		case m.Change < -m.Allowed:
			m.Verdict = "improved"
		default:
			m.Verdict = "same"
		}
	}
	r.Metrics = append(r.Metrics, m)
}

// Markdown renders the report for a pull request comment.
func (r Report) Markdown() string {
	var b strings.Builder
	verdict := "No regression"
	if r.Regressed {
		verdict = "**Regression**"
	}
	fmt.Fprintf(&b, "### go-dyno: %s\n\n", verdict)
	if len(r.Warnings) > 0 {
		b.WriteString("> ⚠️ Base and head were measured under different conditions, so differences may not come from the code:\n")
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "> - %s\n", w)
		}
		b.WriteString("\n")
	}
	b.WriteString("| scenario | process | metric | base | head | change | allowed | verdict |\n|---|---|---|---|---|---|---|---|\n")
	for _, m := range r.Metrics {
		if m.Verdict == "n/a" {
			continue
		}
		icon := map[string]string{"regressed": "🔴 regressed", "improved": "🟢 improved", "same": "same"}[m.Verdict]
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %+.1f%% | ±%.1f%% | %s |\n", m.Scenario, dash(m.Process), m.Name,
			num(m.Base.Median, m.Unit), num(m.Head.Median, m.Unit), m.Change*100, m.Allowed*100, icon)
	}
	if len(r.Functions) > 0 {
		b.WriteString("\n<details><summary>Largest changes in CPU share by function</summary>\n\n| scenario | process | function | base | head |\n|---|---|---|---|---|\n")
		for _, f := range r.Functions {
			fmt.Fprintf(&b, "| %s | %s | `%s` | %.1f%% | %.1f%% |\n", f.Scenario, f.Process, f.Function, f.Base*100, f.Head*100)
		}
		b.WriteString("\n</details>\n")
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(&b, "\n_Skipped: %s_\n", s)
	}
	b.WriteString("\nMedians over passing repetitions. A change counts only when it exceeds both the threshold and half the run-to-run spread.\n")
	return b.String()
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func num(v float64, unit string) string {
	switch {
	case v >= 100:
		return fmt.Sprintf("%.0f %s", v, unit)
	case v >= 10:
		return fmt.Sprintf("%.1f %s", v, unit)
	default:
		return fmt.Sprintf("%.2f %s", v, unit)
	}
}
