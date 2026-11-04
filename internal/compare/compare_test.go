package compare

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/code-lucasgabriel/go-dyno/internal/results"
)

// runs writes one folder per CPU figure: 1000 requests, cpu_s_os = cpu µs × 1000 / 1e6.
func runs(t *testing.T, cpuUS ...float64) string {
	root := t.TempDir()
	for i, c := range cpuUS {
		id := fmt.Sprintf("s-r%d", i+1)
		dir := filepath.Join(root, id)
		os.MkdirAll(filepath.Join(dir, "tidy"), 0o755)
		results.WriteJSON(filepath.Join(dir, "meta.json"), results.Meta{RunID: id, Scenario: "s", Rep: i + 1, Status: "ok",
			Processes: []results.ProcMeta{{Name: "api", Entry: true}}})
		os.WriteFile(filepath.Join(dir, "tidy", "run.csv"), []byte(fmt.Sprintf(
			"process,requests_server,cpu_s_os,alloc_bytes,window_s,cpus\napi,1000,%g,%g,10,2\n", c*1000/1e6, c*1000)), 0o644)
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		gz.Write([]byte("t,route,latency_ms,ok,status,in_window,in_cpu_window\n"))
		for j := 0; j < 100; j++ {
			fmt.Fprintf(gz, "%d,/x,%g,1,200,1,1\n", j, 1+float64(j%10)/10)
		}
		gz.Close()
		os.WriteFile(filepath.Join(dir, "tidy", "requests.csv.gz"), b.Bytes(), 0o644)
	}
	return root
}

func verdict(r Report, name string) string {
	for _, m := range r.Metrics {
		if m.Name == name {
			return m.Verdict
		}
	}
	return "missing"
}

func TestRegression(t *testing.T) {
	base := runs(t, 100, 101, 99)
	head := runs(t, 120, 121, 119)
	r, err := Run(base, head, Default)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Regressed || verdict(r, "cpu_per_request") != "regressed" {
		t.Fatalf("expected CPU regression: %+v", r.Metrics)
	}
	if verdict(r, "p99_latency") != "same" {
		t.Errorf("latency unchanged, got %s", verdict(r, "p99_latency"))
	}
	if !strings.Contains(r.Markdown(), "regressed") {
		t.Error("markdown should name the regression")
	}
}

func TestNoiseAbsorbsSmallChange(t *testing.T) {
	base := runs(t, 80, 100, 120) // 40% spread
	head := runs(t, 90, 110, 130) // +10% median
	r, _ := Run(base, head, Default)
	if r.Regressed {
		t.Fatalf("a change inside the run-to-run spread is noise: %+v", r.Metrics)
	}
}

func TestImprovement(t *testing.T) {
	r, _ := Run(runs(t, 100, 100, 100), runs(t, 50, 50, 50), Default)
	if r.Regressed || verdict(r, "cpu_per_request") != "improved" {
		t.Fatalf("expected improvement: %+v", r.Metrics)
	}
}
