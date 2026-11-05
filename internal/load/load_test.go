package load

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/code-lucasgabriel/go-dyno/internal/config"
)

// A stand-in k6 that writes the CSV a real k6 writes with K6_CSV_TIME_FORMAT=unix_micro,
// after burning a little CPU.
const fakeK6 = `#!/bin/sh
[ "$K6_CSV_TIME_FORMAT" = unix_micro ] || { echo "K6_CSV_TIME_FORMAT=$K6_CSV_TIME_FORMAT" >&2; exit 1; }
for a in "$@"; do case "$a" in csv=*) out="${a#csv=}";; esac; done
i=0; while [ $i -lt 100000 ]; do i=$((i+1)); done
{
echo "metric_name,timestamp,metric_value,expected_response,name,status,extra_tags"
echo "http_req_duration,1700000000250000,250.000000,true,get,200,"
echo "http_req_duration,1700000001500000,1.500000,false,put,500,"
echo "dropped_iterations,1700000002000000,3,,,,"
echo "app_fail,1700000002123456,1,,,,kind=setup"
} | gzip > "$out"
`

func TestRunK6(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "k6"), []byte(fakeK6), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := &config.Config{Dir: t.TempDir(), Load: config.Load{BaseURL: "http://x", K6: &config.K6{
		Script: "s.js", LatencyMetric: "http_req_duration", RouteTag: "name", OKTag: "expected_response", CounterPrefix: "app_",
	}}}
	var got []Sample
	var counters [][]string
	sum, err := RunK6(context.Background(), cfg, 10, time.Second, t.TempDir(), func(s Sample) { got = append(got, s) }, func(r []string) { counters = append(counters, r) })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("samples = %+v", got)
	}
	// sub-second timestamps, moved from completion back to send time
	if math.Abs(got[0].T-1700000000.0) > 1e-6 || math.Abs(got[1].T-1700000001.4985) > 1e-6 {
		t.Errorf("send times = %.6f, %.6f", got[0].T, got[1].T)
	}
	if !got[0].OK || got[1].OK || got[1].Status != 500 || got[0].Route != "get" {
		t.Errorf("samples = %+v", got)
	}
	if sum.Sent != 2 || sum.Dropped != 3 || sum.Planned != 5 {
		t.Errorf("summary = %+v", sum)
	}
	if len(counters) != 1 || counters[0][0] != "1700000002.123" {
		t.Errorf("counters = %v", counters)
	}
	if sum.CPUSeconds <= 0 {
		t.Errorf("k6 CPU = %v, want > 0", sum.CPUSeconds)
	}
}
