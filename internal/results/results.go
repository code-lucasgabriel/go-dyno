// Package results defines the run folder layout and reads it back for analysis.
package results

import (
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/code-lucasgabriel/go-dyno/internal/config"
)

// Meta is meta.json: everything needed to interpret a run folder.
type Meta struct {
	Tool      string            `json:"tool"` // go-dyno version
	Name      string            `json:"name"`
	RunID     string            `json:"run_id"`
	Scenario  string            `json:"scenario"`
	Rep       int               `json:"rep"`
	Label     string            `json:"label,omitempty"`
	SHA       string            `json:"sha,omitempty"`
	HostOS    string            `json:"host_os"`
	HostArch  string            `json:"host_arch"`
	HostCPUs  int               `json:"host_cpus"`
	Rate      float64           `json:"rate"`
	RateSrc   config.RateSource `json:"rate_source"`
	Warmup    float64           `json:"warmup_s"`
	Params    map[string]string `json:"params,omitempty"`
	Processes []ProcMeta        `json:"processes"`
	Times     Times             `json:"times"`
	Checks    config.Checks     `json:"checks"`
	Focus     []config.Focus    `json:"focus,omitempty"`
	Status    string            `json:"status"` // ok | FAILED
	Errors    []string          `json:"errors,omitempty"`
}

type ProcMeta struct {
	Name    string  `json:"name"`
	Entry   bool    `json:"entry"`
	Mode    string  `json:"mode"`
	Image   string  `json:"image,omitempty"`
	CPUs    float64 `json:"cpus,omitempty"`
	Metrics string  `json:"metrics"`
	Start   float64 `json:"start"` // unix seconds the process was launched
}

// Times are unix seconds.
type Times struct {
	LoadStart float64 `json:"load_start"`
	LoadEnd   float64 `json:"load_end"`
	CPU0      float64 `json:"cpu0"` // CPU window start (first stats scrape)
	CPU1      float64 `json:"cpu1"` // CPU window end (second stats scrape)
}

func (m *Meta) Entry() string {
	for _, p := range m.Processes {
		if p.Entry {
			return p.Name
		}
	}
	if len(m.Processes) > 0 {
		return m.Processes[0].Name
	}
	return ""
}

func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Table is a CSV file read as rows of column -> value.
type Table []map[string]string

func ReadCSV(path string) (Table, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		r = gz
	}
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	head, err := cr.Read()
	if err != nil {
		return nil, nil
	}
	var t Table
	for {
		row, err := cr.Read()
		if err != nil {
			break
		}
		m := make(map[string]string, len(head))
		for i, h := range head {
			if i < len(row) {
				m[h] = row[i]
			}
		}
		t = append(t, m)
	}
	return t, nil
}

func F(m map[string]string, k string) float64 {
	v, err := strconv.ParseFloat(m[k], 64)
	if err != nil {
		return 0
	}
	return v
}

// Check is one validity check of a run or scenario.
type Check struct {
	Name   string  `json:"name"`
	Status string  `json:"status"` // pass | warn | fail | skip
	Value  float64 `json:"value"`
	Limit  float64 `json:"limit"`
	Detail string  `json:"detail"`
}

// Trust is checks.json.
type Trust struct {
	RunID  string  `json:"run_id"`
	Score  int     `json:"score"`
	Grade  string  `json:"grade"`
	Failed bool    `json:"failed"`
	Checks []Check `json:"checks"`
}

// Run is one run folder, loaded.
type Run struct {
	Dir      string
	Meta     Meta
	Trust    Trust
	Rows     Table // tidy/run.csv
	CPU      Table // tidy/cpu_functions.csv
	Alloc    Table
	Flame    Table
	Runtime  Table
	Growth   Table
	Goroutes Table
	Focus    Table
}

func (r *Run) Tidy(name string) string { return filepath.Join(r.Dir, "tidy", name) }

// LoadRun reads one run folder (without per-request samples).
func LoadRun(dir string) (*Run, error) {
	r := &Run{Dir: dir}
	if err := ReadJSON(filepath.Join(dir, "meta.json"), &r.Meta); err != nil {
		return nil, err
	}
	ReadJSON(filepath.Join(dir, "checks.json"), &r.Trust)
	r.Rows, _ = ReadCSV(r.Tidy("run.csv"))
	r.CPU, _ = ReadCSV(r.Tidy("cpu_functions.csv"))
	r.Alloc, _ = ReadCSV(r.Tidy("alloc_functions.csv"))
	r.Flame, _ = ReadCSV(r.Tidy("flame.csv"))
	r.Runtime, _ = ReadCSV(r.Tidy("runtime.csv"))
	r.Growth, _ = ReadCSV(r.Tidy("heap_growth.csv"))
	r.Goroutes, _ = ReadCSV(r.Tidy("goroutines.csv"))
	r.Focus, _ = ReadCSV(r.Tidy("focus.csv"))
	return r, nil
}

// LoadAll reads every run folder directly under root (folders with meta.json), sorted by id.
func LoadAll(root string) ([]*Run, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var runs []*Run
	for _, e := range ents {
		d := filepath.Join(root, e.Name())
		if st, err := os.Stat(d); err != nil || !st.IsDir() { // follows symlinked run folders
			continue
		}
		if _, err := os.Stat(filepath.Join(d, "meta.json")); err != nil {
			continue
		}
		if r, err := LoadRun(d); err == nil {
			runs = append(runs, r)
		}
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].Meta.Scenario != runs[j].Meta.Scenario {
			return runs[i].Meta.Scenario < runs[j].Meta.Scenario
		}
		return runs[i].Meta.Rep < runs[j].Meta.Rep
	})
	return runs, nil
}

// OK reports whether a run passed its checks and can be used in conclusions.
func (r *Run) OK() bool { return r.Meta.Status != "FAILED" && !r.Trust.Failed }

// Requests returns the request count that CPU and allocations are divided by:
// the entry process's server-side count, or the load generator's when the
// service doesn't count requests.
func (r *Run) Requests() float64 {
	entry := r.Meta.Entry()
	for _, row := range r.Rows {
		if row["process"] == entry {
			if n := F(row, "requests_server"); n > 0 {
				return n
			}
			return F(row, "requests_loadgen")
		}
	}
	return 0
}

// PerRequest returns, per process, CPU µs, bytes allocated and GC share for this run.
type PerRequest struct {
	CPUus, AllocB, GCShare, Util float64
}

func (r *Run) PerRequest() map[string]PerRequest {
	out := map[string]PerRequest{}
	n := r.Requests()
	for _, row := range r.Rows {
		cpu := F(row, "cpu_s_os")
		pr := PerRequest{}
		if n > 0 {
			pr.CPUus = cpu / n * 1e6
			pr.AllocB = F(row, "alloc_bytes") / n
		}
		if cpu > 0 {
			pr.GCShare = F(row, "gc_cpu_s") / cpu
		}
		if w, c := F(row, "window_s"), F(row, "cpus"); w > 0 && c > 0 {
			pr.Util = cpu / w / c
		}
		out[row["process"]] = pr
	}
	return out
}

// Agg is a median with min–max over repetitions.
type Agg struct {
	Median float64 `json:"median"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	N      int     `json:"n"`
}

func Aggregate(xs []float64) Agg {
	if len(xs) == 0 {
		return Agg{}
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return Agg{Median: Quantile(s, 0.5), Min: s[0], Max: s[len(s)-1], N: len(s)}
}

// Spread is (max-min)/median.
func (a Agg) Spread() float64 {
	if a.Median == 0 {
		return 0
	}
	return (a.Max - a.Min) / a.Median
}

// Quantile of sorted xs, linear interpolation.
func Quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}

// Latency holds per-route latency percentiles in ms over successful requests after warm-up.
type Latency struct {
	Route string  `json:"route"`
	N     int     `json:"n"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Err   float64 `json:"error_rate"`
}

// Latencies reads tidy/requests.csv.gz of several runs and returns per-route and overall ("*") percentiles.
func Latencies(runs []*Run) []Latency {
	by := map[string][]float64{}
	errs := map[string]int{}
	tot := map[string]int{}
	for _, r := range runs {
		t, _ := ReadCSV(r.Tidy("requests.csv.gz"))
		for _, row := range t {
			if row["in_window"] != "1" {
				continue
			}
			for _, k := range []string{row["route"], "*"} {
				tot[k]++
				if row["ok"] == "1" {
					by[k] = append(by[k], F(row, "latency_ms"))
				} else {
					errs[k]++
				}
			}
		}
	}
	var out []Latency
	for k, n := range tot {
		s := by[k]
		sort.Float64s(s)
		l := Latency{Route: k, N: len(s), Err: float64(errs[k]) / float64(n)}
		if len(s) > 0 {
			l.P50, l.P95, l.P99 = Quantile(s, .5), Quantile(s, .95), Quantile(s, .99)
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Route == "*") != (out[j].Route == "*") {
			return out[i].Route == "*"
		}
		return out[i].Route < out[j].Route
	})
	return out
}

// ByScenario groups runs by scenario, in order.
func ByScenario(runs []*Run) ([]string, map[string][]*Run) {
	m := map[string][]*Run{}
	var order []string
	for _, r := range runs {
		s := r.Meta.Scenario
		if _, ok := m[s]; !ok {
			order = append(order, s)
		}
		m[s] = append(m[s], r)
	}
	return order, m
}

// Passing filters runs that passed their checks.
func Passing(runs []*Run) []*Run {
	var out []*Run
	for _, r := range runs {
		if r.OK() {
			out = append(out, r)
		}
	}
	return out
}

// Functions merges per-function tables (cpu or alloc) across runs for one process:
// function -> share of total flat, cum.
type FuncShare struct {
	Function string  `json:"function"`
	Flat     float64 `json:"flat"`
	Cum      float64 `json:"cum"`
}

func Functions(runs []*Run, process string, alloc bool) []FuncShare {
	flat, cum := map[string]float64{}, map[string]float64{}
	total := 0.0
	fk, ck := "flat_s", "cum_s"
	if alloc {
		fk, ck = "flat_bytes", "cum_bytes"
	}
	for _, r := range runs {
		t := r.CPU
		if alloc {
			t = r.Alloc
		}
		for _, row := range t {
			if row["process"] != process {
				continue
			}
			flat[row["function"]] += F(row, fk)
			cum[row["function"]] += F(row, ck)
			total += F(row, fk)
		}
	}
	var out []FuncShare
	if total == 0 {
		return out
	}
	for fn, v := range flat {
		out = append(out, FuncShare{fn, v / total, cum[fn] / total})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Flat > out[j].Flat })
	return out
}

// Processes lists process names across runs, entry first.
func Processes(runs []*Run) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range runs {
		for _, p := range r.Meta.Processes {
			if !seen[p.Name] {
				seen[p.Name] = true
				if p.Entry {
					out = append([]string{p.Name}, out...)
				} else {
					out = append(out, p.Name)
				}
			}
		}
	}
	return out
}

// Slope fits y = a + b·x by least squares and returns b.
func Slope(x, y []float64) float64 {
	n := float64(len(x))
	if n < 2 {
		return 0
	}
	var sx, sy, sxx, sxy float64
	for i := range x {
		sx += x[i]
		sy += y[i]
		sxx += x[i] * x[i]
		sxy += x[i] * y[i]
	}
	d := n*sxx - sx*sx
	if d == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / d
}
