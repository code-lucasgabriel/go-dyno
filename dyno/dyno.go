// Package dyno makes a Go service measurable by go-dyno with one call.
//
//	func main() {
//		stop, err := dyno.Start() // no-op unless DYNO_ADDR is set
//		if err != nil { log.Fatal(err) }
//		defer stop()
//		http.ListenAndServe(":8080", dyno.Middleware(mux))
//	}
//
// When DYNO_ADDR is set (for example 127.0.0.1:6060), Start serves on that
// address only:
//
//	/debug/pprof/...     the standard pprof endpoints
//	/debug/dyno/stats    process CPU, cgroup CPU and throttling, request count,
//	                     goroutines, heap and GC CPU as JSON
//
// DYNO_CONTENTION=1 also turns on mutex and block profiling. They add overhead,
// so keep them off for CPU baselines. DYNO_MUTEX_FRACTION (default 100) and
// DYNO_BLOCK_RATE (nanoseconds, default 10000) tune them.
package dyno

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/code-lucasgabriel/go-dyno/internal/rusage"
)

// StatsPath is where Start serves Stats.
const StatsPath = "/debug/dyno/stats"

// Stats is a point-in-time snapshot of the process. Counters are cumulative
// since process start; go-dyno takes two snapshots and subtracts them.
type Stats struct {
	Source     string  `json:"source"` // "dyno" or "prometheus"
	Time       float64 `json:"time"`   // unix seconds
	StartTime  float64 `json:"start_time,omitempty"`
	GOOS       string  `json:"goos,omitempty"`
	GOARCH     string  `json:"goarch,omitempty"`
	GoVersion  string  `json:"go_version,omitempty"`
	GOMAXPROCS int     `json:"gomaxprocs,omitempty"`
	NumCPU     int     `json:"num_cpu,omitempty"`

	CPUSeconds float64 `json:"cpu_seconds"` // user + system, this process
	Cgroup     *Cgroup `json:"cgroup,omitempty"`

	Requests float64 `json:"requests"`
	Errors   float64 `json:"errors"`

	Goroutines   int     `json:"goroutines"`
	HeapInuse    float64 `json:"heap_inuse"`
	HeapAlloc    float64 `json:"heap_alloc"`
	HeapSys      float64 `json:"heap_sys"`
	TotalAlloc   float64 `json:"total_alloc"`
	NumGC        float64 `json:"num_gc"`
	GCCPUSeconds float64 `json:"gc_cpu_seconds"` // assist + background + pauses, idle mark excluded
}

// Cgroup holds cgroup v2 CPU counters, when the process runs in one (Linux containers).
type Cgroup struct {
	CPUSeconds       float64 `json:"cpu_seconds"`
	ThrottledPeriods float64 `json:"throttled_periods"`
	ThrottledSeconds float64 `json:"throttled_seconds"`
}

var (
	requests atomic.Uint64
	errs     atomic.Uint64
	started  = time.Now()
)

// CountRequest records one served request. Middleware calls it for HTTP;
// call it yourself for other protocols.
func CountRequest() { requests.Add(1) }

// CountError records one failed request.
func CountError() { errs.Add(1) }

// Start serves pprof and stats on DYNO_ADDR. It does nothing when DYNO_ADDR is
// unset, so it is safe to leave in production builds.
func Start() (stop func(), err error) {
	addr := os.Getenv("DYNO_ADDR")
	if addr == "" {
		return func() {}, nil
	}
	return Listen(addr)
}

// Listen serves pprof and stats on addr, regardless of DYNO_ADDR.
func Listen(addr string) (stop func(), err error) {
	if os.Getenv("DYNO_CONTENTION") == "1" {
		runtime.SetMutexProfileFraction(envInt("DYNO_MUTEX_FRACTION", 100))
		runtime.SetBlockProfileRate(envInt("DYNO_BLOCK_RATE", 10000))
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: Handler(), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	return func() { srv.Close() }, nil
}

// Handler returns the pprof and stats endpoints, for mounting on your own private server.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc(StatsPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Snapshot())
	})
	return mux
}

// Middleware counts every request served by next, and every 5xx as an error.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		requests.Add(1)
		if rec.status >= 500 {
			errs.Add(1)
		}
	})
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int)        { r.status = code; r.ResponseWriter.WriteHeader(code) }
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush and Hijack keep handlers that type-assert http.Flusher or
// http.Hijacker on the writer working behind the middleware.
func (r *recorder) Flush() { http.NewResponseController(r.ResponseWriter).Flush() }

func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(r.ResponseWriter).Hijack()
}

var gcMetrics = []metrics.Sample{
	{Name: "/cpu/classes/gc/total:cpu-seconds"},
	{Name: "/cpu/classes/gc/mark/idle:cpu-seconds"},
}

// Snapshot returns the current Stats of this process.
func Snapshot() Stats {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s := make([]metrics.Sample, len(gcMetrics))
	copy(s, gcMetrics)
	metrics.Read(s)
	gc := f64(s[0]) - f64(s[1])
	return Stats{
		Source:       "dyno",
		Time:         float64(time.Now().UnixNano()) / 1e9,
		StartTime:    float64(started.UnixNano()) / 1e9,
		GOOS:         runtime.GOOS,
		GOARCH:       runtime.GOARCH,
		GoVersion:    runtime.Version(),
		GOMAXPROCS:   runtime.GOMAXPROCS(0),
		NumCPU:       runtime.NumCPU(),
		CPUSeconds:   rusage.CPUSeconds(),
		Cgroup:       readCgroup("/sys/fs/cgroup/cpu.stat"),
		Requests:     float64(requests.Load()),
		Errors:       float64(errs.Load()),
		Goroutines:   runtime.NumGoroutine(),
		HeapInuse:    float64(ms.HeapInuse),
		HeapAlloc:    float64(ms.HeapAlloc),
		HeapSys:      float64(ms.HeapSys),
		TotalAlloc:   float64(ms.TotalAlloc),
		NumGC:        float64(ms.NumGC),
		GCCPUSeconds: gc,
	}
}

func f64(s metrics.Sample) float64 {
	if s.Value.Kind() == metrics.KindFloat64 {
		return s.Value.Float64()
	}
	return 0
}

func readCgroup(path string) *Cgroup {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	c := &Cgroup{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		n, _ := strconv.ParseFloat(v, 64)
		switch k {
		case "usage_usec":
			c.CPUSeconds = n / 1e6
		case "nr_throttled":
			c.ThrottledPeriods = n
		case "throttled_usec":
			c.ThrottledSeconds = n / 1e6
		}
	}
	return c
}

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return n
	}
	return def
}
