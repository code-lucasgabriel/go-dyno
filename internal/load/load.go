// Package load generates open-loop HTTP load, or drives k6, and reports every request.
package load

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/code-lucasgabriel/go-dyno/internal/config"
	"github.com/code-lucasgabriel/go-dyno/internal/rusage"
)

// Sample is one request.
type Sample struct {
	T         float64 // unix seconds when the request was sent
	Route     string
	LatencyMs float64
	OK        bool
	Status    int // 0 for transport errors and timeouts
}

// Summary describes how well the load generator kept its schedule.
type Summary struct {
	Tool       string  `json:"tool"` // builtin or k6
	Rate       float64 `json:"rate"`
	Start      float64 `json:"start"`
	End        float64 `json:"end"`
	Planned    int64   `json:"planned"`
	Sent       int64   `json:"sent"`
	Dropped    int64   `json:"dropped"` // not sent: max_in_flight reached
	Late       int64   `json:"late"`    // sent >10 ms after schedule
	CPUSeconds float64 `json:"cpu_seconds"`
	Cores      int     `json:"cores"`
}

const lateAfter = 10 * time.Millisecond

// Run sends load at rate for dur. sink is called for every completed request,
// from a single goroutine.
func Run(ctx context.Context, l config.Load, rate float64, dur time.Duration, sink func(Sample)) Summary {
	tr := &http.Transport{
		MaxIdleConns:        l.MaxInFlight,
		MaxIdleConnsPerHost: l.MaxInFlight,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, Timeout: l.Timeout}

	routes := l.Routes
	total := 0
	for _, r := range routes {
		total += r.Weight
	}
	bodies := make([][][]byte, len(routes))
	for i, r := range routes {
		if r.BodyRandomBytes > 0 { // a pool of random bodies: incompressible, cheap to reuse
			for j := 0; j < 64; j++ {
				b := make([]byte, r.BodyRandomBytes)
				for k := range b {
					b[k] = byte(rand.Uint32())
				}
				bodies[i] = append(bodies[i], b)
			}
		}
	}
	pick := func() int {
		n := rand.IntN(total)
		for i, r := range routes {
			if n < r.Weight {
				return i
			}
			n -= r.Weight
		}
		return 0
	}

	samples := make(chan Sample, 4096)
	done := make(chan struct{})
	go func() {
		for s := range samples {
			sink(s)
		}
		close(done)
	}()

	sum := Summary{Tool: "builtin", Rate: rate, Cores: runtime.NumCPU()}
	cpu0 := rusage.CPUSeconds()
	sem := make(chan struct{}, l.MaxInFlight)
	var wg sync.WaitGroup
	var sent, dropped, late atomic.Int64
	start := time.Now()
	end := start.Add(dur)
	sum.Start = unix(start)
	interval := float64(time.Second) / rate
	for i := int64(0); ; i++ {
		next := start.Add(time.Duration(float64(i) * interval))
		if !next.Before(end) || ctx.Err() != nil {
			sum.Planned = i
			break
		}
		if d := time.Until(next); d > 0 {
			time.Sleep(d)
		}
		if time.Since(next) > lateAfter {
			late.Add(1)
		}
		select {
		case sem <- struct{}{}:
		default:
			dropped.Add(1)
			continue
		}
		sent.Add(1)
		ri := pick()
		r := routes[ri]
		var body []byte
		if len(bodies[ri]) > 0 {
			body = bodies[ri][rand.IntN(len(bodies[ri]))]
		} else if r.Body != "" {
			body = []byte(r.Body)
		}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			samples <- do(ctx, hc, l.BaseURL, r, body)
		}()
	}
	wg.Wait()
	close(samples)
	<-done
	sum.End = unix(time.Now())
	sum.Sent, sum.Dropped, sum.Late = sent.Load(), dropped.Load(), late.Load()
	sum.CPUSeconds = rusage.CPUSeconds() - cpu0
	return sum
}

func do(ctx context.Context, hc *http.Client, base string, r config.Route, body []byte) Sample {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	t0 := time.Now()
	s := Sample{T: unix(t0), Route: r.Name}
	req, err := http.NewRequestWithContext(ctx, r.Method, strings.TrimRight(base, "/")+r.Path, rd)
	if err != nil {
		return s
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		s.Status = resp.StatusCode
		s.OK = resp.StatusCode < 400
	}
	s.LatencyMs = float64(time.Since(t0).Microseconds()) / 1000
	return s
}

func unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// Writer writes samples to a gzipped CSV (t, route, latency_ms, ok, status).
type Writer struct {
	f  *os.File
	gz *gzip.Writer
	w  *csv.Writer
}

func NewWriter(path string) (*Writer, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	gz := gzip.NewWriter(f)
	w := csv.NewWriter(gz)
	w.Write([]string{"t", "route", "latency_ms", "ok", "status"})
	return &Writer{f, gz, w}, nil
}

func (w *Writer) Write(s Sample) {
	ok := "0"
	if s.OK {
		ok = "1"
	}
	w.w.Write([]string{strconv.FormatFloat(s.T, 'f', 6, 64), s.Route, strconv.FormatFloat(s.LatencyMs, 'f', 3, 64), ok, strconv.Itoa(s.Status)})
}

func (w *Writer) Close() error {
	w.w.Flush()
	w.gz.Close()
	return w.f.Close()
}

// RunK6 runs a k6 script with BASE_URL, RATE and DURATION in its environment,
// then converts its CSV output: request rows go to sink, rows of metrics with
// counterPrefix go to counters (t, metric, value, tags).
func RunK6(ctx context.Context, cfg *config.Config, rate float64, dur time.Duration, dir string, sink func(Sample), counters func([]string)) (Summary, error) {
	k := cfg.Load.K6
	raw := dir + "/k6.csv.gz"
	args := append([]string{"run", "--quiet", "--out", "csv=" + raw}, k.Args...)
	args = append(args, cfg.Path(k.Script))
	c := exec.CommandContext(ctx, "k6", args...)
	c.Env = append(os.Environ(), "BASE_URL="+cfg.Load.BaseURL, fmt.Sprintf("RATE=%g", rate), fmt.Sprintf("DURATION=%ds", int(dur.Seconds())))
	lf, _ := os.Create(dir + "/k6.log")
	c.Stdout, c.Stderr = lf, lf
	sum := Summary{Tool: "k6", Rate: rate, Cores: runtime.NumCPU(), Start: unix(time.Now())}
	err := c.Run()
	lf.Close()
	sum.End = unix(time.Now())
	if err != nil {
		return sum, fmt.Errorf("k6: %w (see k6.log)", err)
	}
	f, err := os.Open(raw)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return sum, err
	}
	r := csv.NewReader(gz)
	r.FieldsPerRecord = -1
	head, err := r.Read()
	if err != nil {
		return sum, err
	}
	col := map[string]int{}
	for i, h := range head {
		col[h] = i
	}
	for {
		row, err := r.Read()
		if err != nil {
			break
		}
		get := func(k string) string {
			if i, ok := col[k]; ok && i < len(row) {
				return row[i]
			}
			return ""
		}
		tags := map[string]string{}
		for h, i := range col {
			if i < len(row) && row[i] != "" {
				tags[h] = row[i]
			}
		}
		extra := get("extra_tags")
		for _, kv := range strings.Split(extra, "&") {
			if a, b, ok := strings.Cut(kv, "="); ok {
				tags[a] = b
			}
		}
		name := get("metric_name")
		ts, _ := strconv.ParseFloat(get("timestamp"), 64)
		v, _ := strconv.ParseFloat(get("metric_value"), 64)
		switch {
		case name == k.LatencyMetric:
			sum.Sent++
			st, _ := strconv.Atoi(tags["status"])
			sink(Sample{T: ts, Route: tags[k.RouteTag], LatencyMs: v, OK: tags[k.OKTag] == "true", Status: st})
		case name == "dropped_iterations":
			sum.Dropped += int64(v)
		case k.CounterPrefix != "" && strings.HasPrefix(name, k.CounterPrefix):
			counters([]string{strconv.FormatFloat(ts, 'f', 3, 64), name, get("metric_value"), extra})
		}
	}
	sum.Planned = sum.Sent + sum.Dropped
	return sum, nil
}
