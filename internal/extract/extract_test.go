package extract

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/code-lucasgabriel/go-dyno/dyno"
	"github.com/code-lucasgabriel/go-dyno/internal/config"
	"github.com/code-lucasgabriel/go-dyno/internal/results"
)

//go:noinline
func burnSHA(d time.Duration) {
	b := make([]byte, 4096)
	for end := time.Now().Add(d); time.Now().Before(end); {
		s := sha256.Sum256(b)
		b[0] = s[0]
	}
}

// fixture writes a synthetic run folder with a real CPU profile.
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	P := func(n string) string { return filepath.Join(dir, n) }
	f, _ := os.Create(P("api-cpu.pb.gz"))
	if err := pprof.StartCPUProfile(f); err != nil {
		t.Skip("CPU profiling unavailable:", err)
	}
	burnSHA(400 * time.Millisecond)
	pprof.StopCPUProfile()
	f.Close()
	a, _ := os.Create(P("api-allocs.pb.gz"))
	pprof.Lookup("allocs").WriteTo(a, 0)
	a.Close()

	t0 := 1000.0
	m := results.Meta{RunID: "s1-r1", Scenario: "s1", Rep: 1, Rate: 10, Warmup: 1, Status: "ok",
		Processes: []results.ProcMeta{{Name: "api", Entry: true, CPUs: 2, Start: t0 - 1}},
		Times:     results.Times{LoadStart: t0, LoadEnd: t0 + 4, CPU0: t0 + 1, CPU1: t0 + 3},
		Checks:    config.Checks{MaxErrorRate: .01, RequestTolerance: .05, CPUAgreement: .1, MaxCV: .05, MaxLateRatio: .01},
		Focus:     []config.Focus{{Process: "api", Label: "sha", Regex: "sha256"}}}
	results.WriteJSON(P("meta.json"), m)
	results.WriteJSON(P("stats-api-0.json"), dyno.Stats{Source: "dyno", GOOS: "linux", Requests: 100, TotalAlloc: 1000, CPUSeconds: 1, NumGC: 2, GCCPUSeconds: .1,
		Cgroup: &dyno.Cgroup{CPUSeconds: 10, ThrottledPeriods: 0}})
	results.WriteJSON(P("stats-api-1.json"), dyno.Stats{Source: "dyno", GOOS: "linux", Requests: 120, TotalAlloc: 5000, CPUSeconds: 1.4, NumGC: 4, GCCPUSeconds: .14,
		Cgroup: &dyno.Cgroup{CPUSeconds: 10.4, ThrottledPeriods: 3}})

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte("t,route,latency_ms,ok,status\n"))
	for i := 0; i < 40; i++ {
		ok := "1"
		if i == 0 {
			ok = "0"
		}
		gz.Write([]byte(strings.Join([]string{ftoa(t0 + float64(i)*0.1), "/x", "2.5", ok, "200"}, ",") + "\n"))
	}
	gz.Close()
	os.WriteFile(P("requests.csv.gz"), buf.Bytes(), 0o644)
	os.WriteFile(P("api.log"), []byte("gc 1 @1.500s 2%: 0.009+0.23+0.004 ms clock, 0.072+0.10/0.21/0.10+0.035 ms cpu, 3->4->1 MB, 4 MB goal, 0 MB stacks, 0 MB globals, 8 P\n"), 0o644)
	os.WriteFile(P("api-goroutine.txt"), []byte(`goroutine 1 [chan receive, 3 minutes]:
main.worker()
	/src/main.go:10 +0x1
created by main.main in goroutine 1
	/src/main.go:5 +0x2

goroutine 2 [chan receive]:
main.worker()
	/src/main.go:10 +0x1

goroutine 3 [running]:
main.main()
	/src/main.go:3 +0x1
`), 0o644)
	return dir
}

func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }

func TestRun(t *testing.T) {
	dir := fixture(t)
	if err := Run(dir); err != nil {
		t.Fatal(err)
	}
	rows, _ := results.ReadCSV(filepath.Join(dir, "tidy", "run.csv"))
	if len(rows) != 1 {
		t.Fatalf("run.csv rows = %d", len(rows))
	}
	r := rows[0]
	check := func(k string, want float64) {
		t.Helper()
		if got := results.F(r, k); abs(got-want) > 1e-6 {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	check("requests_server", 20)
	check("requests_loadgen", 20) // t in [1001, 1003): 20 samples, all ok
	check("cpu_s_os", 0.4)        // cgroup preferred over rusage
	check("throttled_periods", 3)
	check("alloc_bytes", 4000)
	check("gc_cpu_s", 0.04)
	if r["cpu_source"] != "cgroup" {
		t.Errorf("cpu_source = %s", r["cpu_source"])
	}

	cpu, _ := results.ReadCSV(filepath.Join(dir, "tidy", "cpu_functions.csv"))
	found := false
	for _, x := range cpu {
		if strings.Contains(x["function"], "burnSHA") && results.F(x, "cum_share") > 0.5 {
			found = true
		}
	}
	if !found {
		t.Error("burnSHA should hold most cumulative CPU")
	}
	focus, _ := results.ReadCSV(filepath.Join(dir, "tidy", "focus.csv"))
	if len(focus) != 1 || results.F(focus[0], "share") < 0.5 {
		t.Errorf("focus = %v", focus)
	}
	gc, _ := results.ReadCSV(filepath.Join(dir, "tidy", "gc.csv"))
	if len(gc) != 1 || abs(results.F(gc[0], "cpu_ms")-(0.072+0.10+0.21+0.035)) > 1e-9 || results.F(gc[0], "idle_ms") != 0.10 {
		t.Errorf("gc = %v", gc)
	}
	req, _ := results.ReadCSV(filepath.Join(dir, "tidy", "requests.csv.gz"))
	if len(req) != 40 || req[5]["in_window"] != "0" || req[15]["in_window"] != "1" {
		t.Errorf("requests flags wrong: %v %v", req[5], req[15])
	}
}

func TestGoroutines(t *testing.T) {
	dir := fixture(t)
	gs := Goroutines(filepath.Join(dir, "api-goroutine.txt"))
	if len(gs) != 2 || gs[0].Count != 2 || gs[0].State != "chan receive" || gs[0].Top != "main.worker" {
		t.Fatalf("groups = %+v", gs)
	}
}
