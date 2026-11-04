// Package collect scrapes stats and downloads profiles from a running process.
package collect

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/code-lucasgabriel/go-dyno/dyno"
	"github.com/code-lucasgabriel/go-dyno/internal/config"
)

var client = &http.Client{}

func get(ctx context.Context, url string, timeout time.Duration) (io.ReadCloser, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(b)))
	}
	return &cancelBody{resp.Body, cancel}, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel func()
}

func (b *cancelBody) Close() error { err := b.ReadCloser.Close(); b.cancel(); return err }

// Stats reads a snapshot from the process, from dyno or from Prometheus metrics.
func Stats(ctx context.Context, p config.Process) (dyno.Stats, error) {
	if p.Metrics == "prometheus" {
		return prometheus(ctx, p)
	}
	body, err := get(ctx, "http://"+p.Debug+dyno.StatsPath, 10*time.Second)
	if err != nil {
		return dyno.Stats{}, err
	}
	defer body.Close()
	var s dyno.Stats
	err = json.NewDecoder(body).Decode(&s)
	return s, err
}

// Ready reports whether the pprof endpoint answers.
func Ready(ctx context.Context, p config.Process) error {
	body, err := get(ctx, "http://"+p.Debug+"/debug/pprof/", 2*time.Second)
	if err != nil {
		return err
	}
	body.Close()
	if p.Ready != "" {
		b, err := get(ctx, p.Ready, 2*time.Second)
		if err != nil {
			return err
		}
		b.Close()
	}
	return nil
}

// Fetch downloads a pprof endpoint (path like "/debug/pprof/heap") into file.
func Fetch(ctx context.Context, p config.Process, path, file string, timeout time.Duration) error {
	body, err := get(ctx, "http://"+p.Debug+path, timeout)
	if err != nil {
		return err
	}
	defer body.Close()
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ParseProm reads Prometheus text format into series name{labels} -> value.
func ParseProm(r io.Reader) map[string]float64 {
	d := map[string]float64{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		// value is the first field after the series (labels may contain spaces)
		end := strings.LastIndexByte(line, '}')
		rest := line
		key := ""
		if end >= 0 {
			key, rest = line[:end+1], strings.TrimSpace(line[end+1:])
		} else {
			k, v, ok := strings.Cut(line, " ")
			if !ok {
				continue
			}
			key, rest = k, strings.TrimSpace(v)
		}
		val, _, _ := strings.Cut(rest, " ")
		if v, err := strconv.ParseFloat(val, 64); err == nil {
			d[key] = v
		}
	}
	return d
}

// PromSum sums every series of metric whose labels include all of labels.
func PromSum(d map[string]float64, metric string, labels map[string]string) float64 {
	t := 0.0
	for k, v := range d {
		name, _, _ := strings.Cut(k, "{")
		if name != metric {
			continue
		}
		ok := true
		for a, b := range labels {
			if !strings.Contains(k, a+`="`+b+`"`) {
				ok = false
				break
			}
		}
		if ok {
			t += v
		}
	}
	return t
}

func prometheus(ctx context.Context, p config.Process) (dyno.Stats, error) {
	body, err := get(ctx, p.Prometheus.URL, 10*time.Second)
	if err != nil {
		return dyno.Stats{}, err
	}
	defer body.Close()
	d := ParseProm(body)
	s := func(m string) float64 { return PromSum(d, m, nil) }
	return dyno.Stats{
		Source:     "prometheus",
		Time:       float64(time.Now().UnixNano()) / 1e9,
		CPUSeconds: s("process_cpu_seconds_total"),
		Requests:   PromSum(d, p.Prometheus.Requests, p.Prometheus.Labels),
		Goroutines: int(s("go_goroutines")),
		HeapInuse:  s("go_memstats_heap_inuse_bytes"),
		HeapAlloc:  s("go_memstats_heap_alloc_bytes"),
		HeapSys:    s("go_memstats_heap_sys_bytes"),
		TotalAlloc: s("go_memstats_alloc_bytes_total"),
		NumGC:      s("go_gc_duration_seconds_count"),
	}, nil
}
