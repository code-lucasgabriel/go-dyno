// Package report renders a results folder as one self-contained HTML page.
package report

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/code-lucasgabriel/go-dyno/internal/compare"
	"github.com/code-lucasgabriel/go-dyno/internal/extract"
	"github.com/code-lucasgabriel/go-dyno/internal/redline"
	"github.com/code-lucasgabriel/go-dyno/internal/results"
	"github.com/code-lucasgabriel/go-dyno/internal/trust"
)

//go:embed page.html
var page string

type Node struct {
	N string  `json:"n"`
	V float64 `json:"v"`           // head (or only) value
	B float64 `json:"b,omitempty"` // base share, diff trees only
	C []*Node `json:"c,omitempty"`
}

type Series struct {
	Run    string       `json:"run"`
	Points [][3]float64 `json:"points"` // t (s), goroutines, heap in use (MiB)
}

type FocusAgg struct {
	Label string      `json:"label"`
	Share results.Agg `json:"share"`
}

type Growth struct {
	Function string  `json:"function"`
	Bytes    float64 `json:"bytes"`
}

type ProcData struct {
	Name       string              `json:"name"`
	Slot       int                 `json:"slot"`
	CPU        results.Agg         `json:"cpu_us"`
	Alloc      results.Agg         `json:"alloc_b"`
	GC         results.Agg         `json:"gc_share"`
	Util       results.Agg         `json:"util"`
	Top        []results.FuncShare `json:"top"`
	TopAlloc   []results.FuncShare `json:"top_alloc"`
	Flame      *Node               `json:"flame,omitempty"`
	Focus      []FocusAgg          `json:"focus,omitempty"`
	Goroutines []extract.Group     `json:"goroutines,omitempty"`
	Growth     []Growth            `json:"growth,omitempty"`
	Leak       []Series            `json:"leak,omitempty"`
	SlopeGor   float64             `json:"slope_goroutines"` // per minute, median over runs
	SlopeHeap  float64             `json:"slope_heap_mib"`
}

type Scenario struct {
	Name    string            `json:"name"`
	Passing int               `json:"passing"`
	Total   int               `json:"total"`
	Checks  []results.Check   `json:"checks"`
	Procs   []ProcData        `json:"procs"`
	Latency []results.Latency `json:"latency"`
	Warmup  float64           `json:"warmup"`
	Load    string            `json:"load"`        // rate and where it came from
	LoadSrc string            `json:"load_source"` // measured | estimate | redline | ""
}

type RunRow struct {
	ID       string          `json:"id"`
	Scenario string          `json:"scenario"`
	Rep      int             `json:"rep"`
	Label    string          `json:"label"`
	SHA      string          `json:"sha"`
	Grade    string          `json:"grade"`
	Score    int             `json:"score"`
	Status   string          `json:"status"`
	Checks   []results.Check `json:"checks"`
}

type DiffFlame struct {
	Scenario string `json:"scenario"`
	Process  string `json:"process"`
	Tree     *Node  `json:"tree"`
}

type Data struct {
	Title     string           `json:"title"`
	Generated string           `json:"generated"`
	Host      string           `json:"host"`
	Runs      []RunRow         `json:"runs"`
	Scenarios []Scenario       `json:"scenarios"`
	Redline   []redline.Result `json:"redline,omitempty"`
	Compare   *compare.Report  `json:"compare,omitempty"`
	Diffs     []DiffFlame      `json:"diffs,omitempty"`
}

// Build collects everything the page shows. base is optional.
func Build(root, base string) (*Data, error) {
	runs, err := results.LoadAll(root)
	if err != nil {
		return nil, err
	}
	d := &Data{Title: filepath.Base(absPath(root)), Generated: time.Now().Format("2006-01-02 15:04")}
	if len(runs) > 0 {
		m := runs[0].Meta
		d.Title = m.Name
		d.Host = m.HostOS + "/" + m.HostArch + ", " + itoa(m.HostCPUs) + " CPUs · " + m.Tool
	}
	for _, r := range runs {
		d.Runs = append(d.Runs, RunRow{r.Meta.RunID, r.Meta.Scenario, r.Meta.Rep, r.Meta.Label, r.Meta.SHA, r.Trust.Grade, r.Trust.Score, r.Meta.Status, r.Trust.Checks})
	}
	order, by := results.ByScenario(runs)
	for _, sc := range order {
		all := by[sc]
		pass := results.Passing(all)
		maxCV := 0.05
		if len(all) > 0 && all[0].Meta.Checks.MaxCV > 0 {
			maxCV = all[0].Meta.Checks.MaxCV
		}
		s := Scenario{Name: sc, Passing: len(pass), Total: len(all), Checks: trust.Scenario(all, maxCV), Latency: results.Latencies(pass)}
		if len(all) > 0 {
			m := all[0].Meta
			s.Warmup, s.Load, s.LoadSrc = m.Warmup, m.RateSrc.Describe(m.Rate), m.RateSrc.Kind
		}
		for i, p := range results.Processes(all) {
			s.Procs = append(s.Procs, procData(pass, p, i, s.Warmup))
		}
		d.Scenarios = append(d.Scenarios, s)
	}
	rl, _ := filepath.Glob(filepath.Join(root, "redline-*", "redline.json"))
	for _, f := range rl {
		var r redline.Result
		if results.ReadJSON(f, &r) == nil {
			d.Redline = append(d.Redline, r)
		}
	}
	if base != "" {
		c, err := compare.Run(base, root, compare.Default)
		if err != nil {
			return nil, err
		}
		d.Compare = &c
		bRuns, _ := results.LoadAll(base)
		_, bBy := results.ByScenario(results.Passing(bRuns))
		_, hBy := results.ByScenario(results.Passing(runs))
		for _, sc := range order {
			if len(bBy[sc]) == 0 || len(hBy[sc]) == 0 {
				continue
			}
			for _, p := range results.Processes(hBy[sc]) {
				if t := diffTree(merged(bBy[sc], p), merged(hBy[sc], p)); t != nil {
					d.Diffs = append(d.Diffs, DiffFlame{sc, p, t})
				}
			}
		}
	}
	return d, nil
}

func procData(pass []*results.Run, p string, slot int, warmup float64) ProcData {
	pd := ProcData{Name: p, Slot: slot}
	var cpu, al, gc, ut, sg, sh []float64
	for _, r := range pass {
		if pr, ok := r.PerRequest()[p]; ok {
			cpu, al, gc, ut = append(cpu, pr.CPUus), append(al, pr.AllocB), append(gc, pr.GCShare), append(ut, pr.Util)
		}
		var ts, gs, hs []float64
		ser := Series{Run: r.Meta.RunID}
		for _, row := range r.Runtime {
			if row["process"] != p {
				continue
			}
			t := results.F(row, "t")
			ser.Points = append(ser.Points, [3]float64{t, results.F(row, "goroutines"), results.F(row, "heap_inuse") / (1 << 20)})
			if t >= warmup {
				ts, gs, hs = append(ts, t/60), append(gs, results.F(row, "goroutines")), append(hs, results.F(row, "heap_inuse")/(1<<20))
			}
		}
		if len(ser.Points) > 0 {
			pd.Leak = append(pd.Leak, ser)
		}
		if len(ts) >= 3 {
			sg, sh = append(sg, results.Slope(ts, gs)), append(sh, results.Slope(ts, hs))
		}
	}
	pd.CPU, pd.Alloc, pd.GC, pd.Util = results.Aggregate(cpu), results.Aggregate(al), results.Aggregate(gc), results.Aggregate(ut)
	pd.SlopeGor, pd.SlopeHeap = results.Aggregate(sg).Median, results.Aggregate(sh).Median
	pd.Top = head(results.Functions(pass, p, false), 15)
	pd.TopAlloc = head(results.Functions(pass, p, true), 10)
	pd.Flame = tree(merged(pass, p), 0.002)

	focus := map[string][]float64{}
	var labels []string
	for _, r := range pass {
		for _, row := range r.Focus {
			if row["process"] == p {
				if _, ok := focus[row["label"]]; !ok {
					labels = append(labels, row["label"])
				}
				focus[row["label"]] = append(focus[row["label"]], results.F(row, "share"))
			}
		}
	}
	for _, l := range labels {
		pd.Focus = append(pd.Focus, FocusAgg{l, results.Aggregate(focus[l])})
	}
	if len(pass) > 0 {
		last := pass[len(pass)-1]
		for _, row := range last.Goroutes {
			if row["process"] == p && len(pd.Goroutines) < 8 {
				pd.Goroutines = append(pd.Goroutines, extract.Group{State: row["state"], Top: row["top"], Count: int(results.F(row, "count"))})
			}
		}
		for _, row := range last.Growth {
			if row["process"] == p && len(pd.Growth) < 10 {
				pd.Growth = append(pd.Growth, Growth{row["function"], results.F(row, "flat_bytes_delta")})
			}
		}
	}
	return pd
}

// merged sums folded CPU stacks of one process over runs.
func merged(runs []*results.Run, p string) map[string]float64 {
	m := map[string]float64{}
	for _, r := range runs {
		for _, row := range r.Flame {
			if row["process"] == p {
				m[row["stack"]] += results.F(row, "seconds")
			}
		}
	}
	return m
}

func tree(stacks map[string]float64, minShare float64) *Node {
	if len(stacks) == 0 {
		return nil
	}
	root := &Node{N: "all"}
	idx := map[*Node]map[string]*Node{}
	for st, v := range stacks {
		root.V += v
		cur := root
		for _, f := range strings.Split(st, ";") {
			if idx[cur] == nil {
				idx[cur] = map[string]*Node{}
			}
			nx := idx[cur][f]
			if nx == nil {
				nx = &Node{N: f}
				idx[cur][f] = nx
				cur.C = append(cur.C, nx)
			}
			nx.V += v
			cur = nx
		}
	}
	prune(root, root.V*minShare)
	return root
}

func prune(n *Node, min float64) {
	kept := n.C[:0]
	for _, c := range n.C {
		if c.V >= min {
			prune(c, min)
			kept = append(kept, c)
		}
	}
	n.C = kept
	sort.Slice(n.C, func(i, j int) bool { return n.C[i].N < n.C[j].N })
}

// diffTree builds the head tree in shares, carrying the base share of each frame path.
func diffTree(base, head map[string]float64) *Node {
	bt, ht := tree(base, 0), tree(head, 0)
	if bt == nil || ht == nil {
		return nil
	}
	var walk func(h, b *Node, hTot, bTot float64)
	walk = func(h, b *Node, hTot, bTot float64) {
		h.V /= hTot
		if b != nil {
			h.B = b.V / bTot
		}
		for _, c := range h.C {
			var bc *Node
			if b != nil {
				for _, x := range b.C {
					if x.N == c.N {
						bc = x
						break
					}
				}
			}
			walk(c, bc, hTot, bTot)
		}
	}
	walk(ht, bt, ht.V, bt.V)
	prune(ht, 0.002)
	return ht
}

func head(fs []results.FuncShare, n int) []results.FuncShare {
	if len(fs) > n {
		return fs[:n]
	}
	return fs
}

// Write renders the page to out.
func Write(d *Data, out string) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	js := strings.ReplaceAll(string(b), "</", `<\/`)
	html := strings.Replace(page, "/*DATA*/null", js, 1)
	html = strings.Replace(html, "<title>go-dyno report</title>", "<title>"+escape(d.Title)+" · go-dyno</title>", 1)
	return os.WriteFile(out, []byte(html), 0o644)
}

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func itoa(n int) string { return strconv.Itoa(n) }

func absPath(p string) string { a, _ := filepath.Abs(p); return a }
