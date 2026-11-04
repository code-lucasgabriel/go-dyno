package report

import "github.com/code-lucasgabriel/go-dyno/internal/results"

// Summary is a compact, machine-readable digest of a results folder, for agents and scripts.
type Summary struct {
	Runs      []RunBrief      `json:"runs"`
	Scenarios []ScenarioBrief `json:"scenarios"`
	Redline   []RedlineBrief  `json:"redline,omitempty"`
}

type RunBrief struct {
	ID     string   `json:"id"`
	Grade  string   `json:"grade"`
	Status string   `json:"status"`
	Issues []string `json:"issues,omitempty"`
}

type ProcBrief struct {
	Name            string              `json:"name"`
	CPUus           results.Agg         `json:"cpu_us_per_request"`
	AllocB          results.Agg         `json:"alloc_bytes_per_request"`
	GCShare         results.Agg         `json:"gc_share"`
	GoroutinesSlope float64             `json:"goroutines_per_min"`
	HeapSlopeMiB    float64             `json:"heap_mib_per_min"`
	TopCPU          []results.FuncShare `json:"top_cpu"`
	TopAlloc        []results.FuncShare `json:"top_alloc"`
	HeapGrowth      []Growth            `json:"heap_growth,omitempty"`
	Focus           []FocusAgg          `json:"focus,omitempty"`
}

type ScenarioBrief struct {
	Name    string            `json:"name"`
	Passing int               `json:"passing"`
	Total   int               `json:"total"`
	Issues  []string          `json:"issues,omitempty"`
	Latency []results.Latency `json:"latency"`
	Procs   []ProcBrief       `json:"processes"`
}

type RedlineBrief struct {
	Scenario  string   `json:"scenario"`
	Knee      float64  `json:"knee"`
	Diagnosis []string `json:"diagnosis"`
}

func issues(cs []results.Check) []string {
	var out []string
	for _, c := range cs {
		if c.Status == "fail" || c.Status == "warn" {
			out = append(out, c.Status+" "+c.Name+": "+c.Detail)
		}
	}
	return out
}

// Summarize digests a results folder.
func Summarize(root string) (*Summary, error) {
	d, err := Build(root, "")
	if err != nil {
		return nil, err
	}
	s := &Summary{}
	for _, r := range d.Runs {
		s.Runs = append(s.Runs, RunBrief{r.ID, r.Grade, r.Status, issues(r.Checks)})
	}
	for _, sc := range d.Scenarios {
		b := ScenarioBrief{Name: sc.Name, Passing: sc.Passing, Total: sc.Total, Issues: issues(sc.Checks), Latency: sc.Latency}
		for _, p := range sc.Procs {
			b.Procs = append(b.Procs, ProcBrief{p.Name, p.CPU, p.Alloc, p.GC, p.SlopeGor, p.SlopeHeap, head(p.Top, 8), head(p.TopAlloc, 5), p.Growth, p.Focus})
		}
		s.Scenarios = append(s.Scenarios, b)
	}
	for _, r := range d.Redline {
		rb := RedlineBrief{Scenario: r.Scenario, Knee: r.Knee}
		for _, f := range r.Diagnosis {
			x := f.Kind + ": " + f.Detail
			if f.Evidence != "" {
				x += " (" + f.Evidence + ")"
			}
			rb.Diagnosis = append(rb.Diagnosis, x)
		}
		s.Redline = append(s.Redline, rb)
	}
	return s, nil
}
