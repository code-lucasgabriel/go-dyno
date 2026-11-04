// Package extract turns a run folder into tidy CSV tables (see skill/references/notebook-report.md).
package extract

import (
	"bufio"
	"compress/gzip"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/pprof/profile"

	"github.com/code-lucasgabriel/go-dyno/dyno"
	"github.com/code-lucasgabriel/go-dyno/internal/results"
)

// Run writes <dir>/tidy/*.csv for one run folder.
func Run(dir string) error {
	var m results.Meta
	if err := results.ReadJSON(filepath.Join(dir, "meta.json"), &m); err != nil {
		return err
	}
	out := filepath.Join(dir, "tidy")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	P := func(name string) string { return filepath.Join(dir, name) }
	t := m.Times

	// requests: relative time, warm-up and CPU-window flags
	nLoadgen := 0.0
	if raw, err := results.ReadCSV(P("requests.csv.gz")); err == nil && raw != nil {
		w, err := newCSV(filepath.Join(out, "requests.csv.gz"), "t", "route", "latency_ms", "ok", "status", "in_window", "in_cpu_window")
		if err != nil {
			return err
		}
		for _, row := range raw {
			ts := results.F(row, "t")
			incpu := ts >= t.CPU0 && ts < t.CPU1
			if incpu && row["ok"] == "1" {
				nLoadgen++
			}
			w.row(f3(ts-t.LoadStart), row["route"], row["latency_ms"], row["ok"], row["status"],
				b01(ts >= t.LoadStart+m.Warmup), b01(incpu))
		}
		if err := w.close(); err != nil {
			return err
		}
	}
	if raw, err := results.ReadCSV(P("counters-raw.csv")); err == nil && raw != nil {
		w, _ := newCSV(filepath.Join(out, "counters.csv"), "t", "metric", "value", "tags")
		for _, row := range raw {
			w.row(f3(results.F(row, "t")-t.LoadStart), row["metric"], row["value"], row["tags"])
		}
		w.close()
	}

	runRows := [][]string{}
	paramKeys := sortedKeys(m.Params)
	runHead := append([]string{"run_id", "scenario", "rep", "label", "sha", "process", "entry", "cpus", "rate"}, paramKeys...)
	runHead = append(runHead, "window_s", "requests_server", "requests_loadgen", "cpu_s_os", "cpu_source", "cpu_s_pprof",
		"throttled_periods", "alloc_bytes", "gc_cycles", "gc_cpu_s", "warmup_s", "load_s", "goos", "status")

	cpuW, _ := newCSV(filepath.Join(out, "cpu_functions.csv"), "process", "function", "flat_s", "cum_s", "flat_share", "cum_share")
	allocW, _ := newCSV(filepath.Join(out, "alloc_functions.csv"), "process", "function", "flat_bytes", "cum_bytes", "flat_share", "cum_share")
	flameW, _ := newCSV(filepath.Join(out, "flame.csv"), "process", "stack", "seconds")
	gcW, _ := newCSV(filepath.Join(out, "gc.csv"), "process", "t", "cpu_ms", "idle_ms", "heap_before_mb", "heap_after_mb")
	focusW, _ := newCSV(filepath.Join(out, "focus.csv"), "process", "label", "regex", "share")
	grW, _ := newCSV(filepath.Join(out, "goroutines.csv"), "process", "state", "top", "count")
	var rtRows, growthRows [][]string

	for _, pm := range m.Processes {
		p := pm.Name
		var s0, s1 dyno.Stats
		e0 := results.ReadJSON(P("stats-"+p+"-0.json"), &s0)
		e1 := results.ReadJSON(P("stats-"+p+"-1.json"), &s1)
		haveStats := e0 == nil && e1 == nil

		cpuPprof := 0.0
		if prof, err := readProfile(P(p + "-cpu.pb.gz")); err == nil {
			idx := sampleIndex(prof, "cpu", "samples")
			scale := 1.0
			if prof.SampleType[idx].Unit == "nanoseconds" {
				scale = 1e-9
			}
			ft := functionTable(prof, idx, scale)
			cpuPprof = ft.total
			ft.write(cpuW, p)
			for _, st := range folded(prof, idx, scale) {
				flameW.row(p, st.stack, g(st.v))
			}
			for _, f := range m.Focus {
				if f.Process != p {
					continue
				}
				rx := regexp.MustCompile(f.Regex)
				share := ""
				if ft.total > 0 {
					share = g(focusTotal(prof, idx, scale, rx) / ft.total)
				}
				focusW.row(p, f.Label, f.Regex, share)
			}
		}
		if prof, err := readProfile(P(p + "-allocs.pb.gz")); err == nil {
			functionTable(prof, sampleIndex(prof, "alloc_space", ""), 1).write(allocW, p)
		}

		// GC cycles from the gctrace log
		gcCPU, nGC := 0.0, 0.0
		for _, c := range gctrace(P(p + ".log")) {
			gcW.row(p, g(c.t), g(c.cpuMs), g(c.idleMs), strconv.Itoa(c.before), strconv.Itoa(c.after))
			if w := pm.Start + c.t; w >= t.CPU0 && w < t.CPU1 {
				gcCPU += c.cpuMs / 1000
				nGC++
			}
		}

		row := map[string]string{
			"run_id": m.RunID, "scenario": m.Scenario, "rep": strconv.Itoa(m.Rep), "label": m.Label, "sha": m.SHA,
			"process": p, "entry": b01(pm.Entry), "cpus": g(pm.CPUs), "rate": g(m.Rate),
			"window_s": g(t.CPU1 - t.CPU0), "requests_loadgen": g(nLoadgen), "cpu_s_pprof": g(cpuPprof),
			"warmup_s": g(m.Warmup), "load_s": g(t.LoadEnd - t.LoadStart), "status": m.Status,
		}
		for k, v := range m.Params {
			row[k] = v
		}
		if haveStats {
			row["requests_server"] = g(s1.Requests - s0.Requests)
			row["alloc_bytes"] = g(s1.TotalAlloc - s0.TotalAlloc)
			row["goos"] = s1.GOOS
			if s0.Cgroup != nil && s1.Cgroup != nil {
				row["cpu_s_os"] = g(s1.Cgroup.CPUSeconds - s0.Cgroup.CPUSeconds)
				row["cpu_source"] = "cgroup"
				row["throttled_periods"] = g(s1.Cgroup.ThrottledPeriods - s0.Cgroup.ThrottledPeriods)
			} else {
				row["cpu_s_os"] = g(s1.CPUSeconds - s0.CPUSeconds)
				row["cpu_source"] = map[string]string{"dyno": "rusage", "prometheus": "prometheus"}[s1.Source]
			}
			if s1.Source == "dyno" {
				row["gc_cpu_s"] = g(s1.GCCPUSeconds - s0.GCCPUSeconds)
				row["gc_cycles"] = g(s1.NumGC - s0.NumGC)
			} else {
				row["gc_cpu_s"] = g(gcCPU)
				row["gc_cycles"] = g(nGC)
			}
		}
		r := make([]string, len(runHead))
		for i, h := range runHead {
			r[i] = row[h]
		}
		runRows = append(runRows, r)

		// goroutine dump, grouped
		for _, gr := range Goroutines(P(p + "-goroutine.txt")) {
			grW.row(p, gr.State, gr.Top, strconv.Itoa(gr.Count))
		}

		// leak detection: runtime series and heap growth between snapshots after warm-up
		if rt, err := results.ReadCSV(P("runtime-" + p + ".csv")); err == nil {
			for _, x := range rt {
				rtRows = append(rtRows, []string{p, x["t"], x["goroutines"], x["heap_inuse"], x["heap_alloc"], x["heap_sys"], x["num_gc"]})
			}
		}
		snaps, _ := filepath.Glob(P("heapsnap-" + p + "-*.pb.gz"))
		sort.Strings(snaps)
		var after []string
		for _, sn := range snaps {
			if snapTime(sn) >= m.Warmup {
				after = append(after, sn)
			}
		}
		if len(after) >= 2 {
			a, e1 := readProfile(after[0])
			b, e2 := readProfile(after[len(after)-1])
			if e1 == nil && e2 == nil {
				fa := functionTable(a, sampleIndex(a, "inuse_space", ""), 1)
				fb := functionTable(b, sampleIndex(b, "inuse_space", ""), 1)
				for fn := range union(fa.flat, fb.flat, fa.cum, fb.cum) {
					df, dc := fb.flat[fn]-fa.flat[fn], fb.cum[fn]-fa.cum[fn]
					if df != 0 || dc != 0 {
						growthRows = append(growthRows, []string{p, g(snapTime(after[0])), g(snapTime(after[len(after)-1])), fn, g(df), g(dc)})
					}
				}
			}
		}
	}
	for _, w := range []*csvw{cpuW, allocW, flameW, gcW, focusW, grW} {
		if err := w.close(); err != nil {
			return err
		}
	}
	if err := writeAll(filepath.Join(out, "run.csv"), runHead, runRows); err != nil {
		return err
	}
	if len(rtRows) > 0 {
		writeAll(filepath.Join(out, "runtime.csv"), []string{"process", "t", "goroutines", "heap_inuse", "heap_alloc", "heap_sys", "num_gc"}, rtRows)
	}
	if len(growthRows) > 0 {
		sort.Slice(growthRows, func(i, j int) bool { return abs(pf(growthRows[i][4])) > abs(pf(growthRows[j][4])) })
		writeAll(filepath.Join(out, "heap_growth.csv"), []string{"process", "t_first", "t_last", "function", "flat_bytes_delta", "cum_bytes_delta"}, growthRows)
	}
	return nil
}

// ---- profiles

func readProfile(path string) (*profile.Profile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return profile.Parse(f)
}

func sampleIndex(p *profile.Profile, want, fallback string) int {
	for i, st := range p.SampleType {
		if st.Type == want {
			return i
		}
	}
	for i, st := range p.SampleType {
		if st.Type == fallback {
			return i
		}
	}
	return len(p.SampleType) - 1
}

// frames returns function names leaf first, inlined frames expanded.
func frames(s *profile.Sample) []string {
	var out []string
	for _, loc := range s.Location {
		if len(loc.Line) == 0 {
			out = append(out, fmt.Sprintf("0x%x", loc.Address))
			continue
		}
		for _, ln := range loc.Line {
			if ln.Function != nil {
				out = append(out, ln.Function.Name)
			}
		}
	}
	return out
}

type ftable struct {
	flat, cum map[string]float64
	total     float64
}

func functionTable(p *profile.Profile, idx int, scale float64) ftable {
	ft := ftable{flat: map[string]float64{}, cum: map[string]float64{}}
	for _, s := range p.Sample {
		v := float64(s.Value[idx]) * scale
		fr := frames(s)
		if len(fr) == 0 {
			continue
		}
		ft.total += v
		ft.flat[fr[0]] += v
		seen := map[string]bool{}
		for _, f := range fr {
			if !seen[f] {
				seen[f] = true
				ft.cum[f] += v
			}
		}
	}
	return ft
}

func (ft ftable) write(w *csvw, proc string) {
	fns := make([]string, 0, len(ft.cum))
	for f := range ft.cum {
		fns = append(fns, f)
	}
	sort.Slice(fns, func(i, j int) bool {
		if ft.flat[fns[i]] != ft.flat[fns[j]] {
			return ft.flat[fns[i]] > ft.flat[fns[j]]
		}
		return ft.cum[fns[i]] > ft.cum[fns[j]]
	})
	for _, f := range fns {
		fs, cs := "", ""
		if ft.total > 0 {
			fs, cs = g(ft.flat[f]/ft.total), g(ft.cum[f]/ft.total)
		}
		w.row(proc, f, g(ft.flat[f]), g(ft.cum[f]), fs, cs)
	}
}

type stack struct {
	stack string
	v     float64
}

// folded merges samples into unique root→leaf stacks.
func folded(p *profile.Profile, idx int, scale float64) []stack {
	m := map[string]float64{}
	for _, s := range p.Sample {
		fr := frames(s)
		for i, j := 0, len(fr)-1; i < j; i, j = i+1, j-1 {
			fr[i], fr[j] = fr[j], fr[i]
		}
		m[strings.Join(fr, ";")] += float64(s.Value[idx]) * scale
	}
	out := make([]stack, 0, len(m))
	for k, v := range m {
		out = append(out, stack{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].v > out[j].v })
	return out
}

// focusTotal is the value of samples with any frame matching rx (pprof -focus).
func focusTotal(p *profile.Profile, idx int, scale float64, rx *regexp.Regexp) float64 {
	t := 0.0
	for _, s := range p.Sample {
		for _, f := range frames(s) {
			if rx.MatchString(f) {
				t += float64(s.Value[idx]) * scale
				break
			}
		}
	}
	return t
}

// ---- gctrace

type gcCycle struct {
	t, cpuMs, idleMs float64
	before, after    int
}

var gcRx = regexp.MustCompile(`^gc \d+ @([\d.]+)s \d+%: [\d.+]+ ms clock, ([\d.]+)\+([\d.]+)/([\d.]+)/([\d.]+)\+([\d.]+) ms cpu, (\d+)->(\d+)->(\d+) MB`)

func gctrace(path string) []gcCycle {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []gcCycle
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		m := gcRx.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		v := make([]float64, 10)
		for i := 1; i < 10; i++ {
			v[i], _ = strconv.ParseFloat(m[i], 64)
		}
		// stw sweep term + assist + background + stw mark term; idle reported apart
		out = append(out, gcCycle{t: v[1], cpuMs: v[2] + v[3] + v[4] + v[6], idleMs: v[5], before: int(v[7]), after: int(v[9])})
	}
	return out
}

// ---- goroutine dumps

// Group is a set of goroutines in the same state with the same top frames.
type Group struct {
	State string
	Top   string // top frames, leaf first, joined with " < "
	Count int
}

var grHead = regexp.MustCompile(`^goroutine \d+ \[([^\]]+)\]:`)

// Goroutines parses a debug=2 goroutine dump and groups it, largest group first.
func Goroutines(path string) []Group {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	counts := map[[2]string]int{}
	var state string
	var top []string
	flush := func() {
		if state != "" {
			counts[[2]string{state, strings.Join(top, " < ")}]++
		}
		state, top = "", nil
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if m := grHead.FindStringSubmatch(line); m != nil {
			flush()
			st, _, _ := strings.Cut(m[1], ",")
			state = strings.TrimSpace(st)
			continue
		}
		if state == "" || line == "" || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "created by ") {
			if line == "" {
				flush()
			}
			continue
		}
		if len(top) < 3 {
			fn := line
			if i := strings.LastIndex(fn, "("); i > 0 {
				fn = fn[:i]
			}
			top = append(top, fn)
		}
	}
	flush()
	out := make([]Group, 0, len(counts))
	for k, n := range counts {
		out = append(out, Group{k[0], k[1], n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Top < out[j].Top
	})
	return out
}

// ---- small helpers

var snapRx = regexp.MustCompile(`-(\d+)\.pb\.gz$`)

func snapTime(path string) float64 {
	m := snapRx.FindStringSubmatch(path)
	if m == nil {
		return -1
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	return v
}

type csvw struct {
	f  *os.File
	gz *gzip.Writer
	w  *csv.Writer
}

func newCSV(path string, head ...string) (*csvw, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	c := &csvw{f: f}
	if strings.HasSuffix(path, ".gz") {
		c.gz = gzip.NewWriter(f)
		c.w = csv.NewWriter(c.gz)
	} else {
		c.w = csv.NewWriter(f)
	}
	c.w.Write(head)
	return c, nil
}

func (c *csvw) row(v ...string) { c.w.Write(v) }

func (c *csvw) close() error {
	c.w.Flush()
	if err := c.w.Error(); err != nil {
		return err
	}
	if c.gz != nil {
		c.gz.Close()
	}
	return c.f.Close()
}

func writeAll(path string, head []string, rows [][]string) error {
	w, err := newCSV(path, head...)
	if err != nil {
		return err
	}
	for _, r := range rows {
		w.row(r...)
	}
	return w.close()
}

func g(v float64) string  { return strconv.FormatFloat(v, 'g', 10, 64) }
func f3(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }
func pf(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
func b01(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func sortedKeys(m map[string]string) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

func union(ms ...map[string]float64) map[string]bool {
	u := map[string]bool{}
	for _, m := range ms {
		for k := range m {
			u[k] = true
		}
	}
	return u
}
