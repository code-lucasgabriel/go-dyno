// Package mcp serves go-dyno as a Model Context Protocol server over stdio,
// so any agent can run measurements and read results.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/code-lucasgabriel/go-dyno/internal/compare"
	"github.com/code-lucasgabriel/go-dyno/internal/config"
	"github.com/code-lucasgabriel/go-dyno/internal/redline"
	"github.com/code-lucasgabriel/go-dyno/internal/report"
	"github.com/code-lucasgabriel/go-dyno/internal/results"
	"github.com/code-lucasgabriel/go-dyno/internal/runner"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	run         func(ctx context.Context, args map[string]any) (string, error)
}

func obj(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func numb(desc string) map[string]any {
	return map[string]any{"type": "number", "description": desc}
}

func arg(a map[string]any, k string) string { s, _ := a[k].(string); return s }
func argf(a map[string]any, k string, def float64) float64 {
	if v, ok := a[k].(float64); ok {
		return v
	}
	return def
}

func tools() []tool {
	return []tool{
		{
			Name:        "dyno_run",
			Description: "Run a measured load test from a go-dyno YAML config: fresh processes, load, CPU/heap/allocs/goroutine/trace capture per repetition, tidy tables and trust checks. Takes minutes (warm-up + capture per repetition). Returns each run's grade and a summary.",
			InputSchema: obj(map[string]any{"config": str("path to the YAML config"), "out": str("results folder (default: results next to the config)"), "label": str("build label, e.g. base or head"), "reps": numb("repetitions, overrides the config")}, "config"),
			run: func(ctx context.Context, a map[string]any) (string, error) {
				cfg, err := config.Read(arg(a, "config"))
				if err != nil {
					return "", err
				}
				out := outDir(cfg, arg(a, "out"))
				var log strings.Builder
				res, err := runner.Run(ctx, cfg, runner.Options{Out: out, Label: arg(a, "label"), Reps: int(argf(a, "reps", 0)), Log: func(f string, x ...any) { fmt.Fprintf(&log, f+"\n", x...) }})
				if err != nil {
					return log.String(), err
				}
				s, _ := report.Summarize(out)
				return fmt.Sprintf("%d runs in %s\n%s\n%s", len(res), out, log.String(), js(s)), nil
			},
		},
		{
			Name:        "dyno_redline",
			Description: "Ramp load until the service stops meeting its latency/error/throughput limits, then diagnose the limit: CPU-bound process, CPU throttling, load generator, concurrency cap (Little's law, with the goroutine group that waits), or a single hot goroutine.",
			InputSchema: obj(map[string]any{"config": str("path to the YAML config"), "out": str("results folder")}, "config"),
			run: func(ctx context.Context, a map[string]any) (string, error) {
				cfg, err := config.Read(arg(a, "config"))
				if err != nil {
					return "", err
				}
				r, err := redline.Run(ctx, cfg, outDir(cfg, arg(a, "out")), func(string, ...any) {})
				if err != nil {
					return "", err
				}
				return js(r), nil
			},
		},
		{
			Name:        "dyno_compare",
			Description: "Compare two results folders (base vs head) on CPU per request, allocations per request and latency, using run-to-run spread as the noise floor. Returns a markdown verdict table.",
			InputSchema: obj(map[string]any{"base": str("base results folder"), "head": str("head results folder"), "cpu": numb("allowed CPU change, default 0.05"), "alloc": numb("allowed allocation change, default 0.05"), "p99": numb("allowed p99 change, default 0.10")}, "base", "head"),
			run: func(ctx context.Context, a map[string]any) (string, error) {
				th := compare.Default
				th.CPU, th.Alloc, th.P99 = argf(a, "cpu", th.CPU), argf(a, "alloc", th.Alloc), argf(a, "p99", th.P99)
				r, err := compare.Run(arg(a, "base"), arg(a, "head"), th)
				if err != nil {
					return "", err
				}
				return r.Markdown(), nil
			},
		},
		{
			Name:        "dyno_report",
			Description: "Render a results folder as one self-contained HTML report (trust grades, CPU per request, flame graphs, leak trends, redline, optional diff against a base).",
			InputSchema: obj(map[string]any{"results": str("results folder"), "base": str("optional base results folder for a diff"), "out": str("output HTML path (default <results>/report.html)")}, "results"),
			run: func(ctx context.Context, a map[string]any) (string, error) {
				d, err := report.Build(arg(a, "results"), arg(a, "base"))
				if err != nil {
					return "", err
				}
				out := arg(a, "out")
				if out == "" {
					out = filepath.Join(arg(a, "results"), "report.html")
				}
				return out, report.Write(d, out)
			},
		},
		{
			Name:        "dyno_summary",
			Description: "Digest of a results folder as JSON: run grades and issues, per scenario CPU µs/request, bytes/request, GC share, goroutine and heap slopes, top CPU and allocation functions, heap growth sites, latency percentiles, redline diagnosis.",
			InputSchema: obj(map[string]any{"results": str("results folder")}, "results"),
			run: func(ctx context.Context, a map[string]any) (string, error) {
				s, err := report.Summarize(arg(a, "results"))
				if err != nil {
					return "", err
				}
				return js(s), nil
			},
		},
		{
			Name:        "dyno_top",
			Description: "Top functions of one process in one scenario, merged over passing runs: own CPU (flat) and cumulative share, or allocation share.",
			InputSchema: obj(map[string]any{"results": str("results folder"), "scenario": str("scenario name"), "process": str("process name"), "kind": str("cpu (default) or alloc"), "n": numb("rows, default 20")}, "results", "scenario", "process"),
			run: func(ctx context.Context, a map[string]any) (string, error) {
				runs, err := results.LoadAll(arg(a, "results"))
				if err != nil {
					return "", err
				}
				_, by := results.ByScenario(results.Passing(runs))
				fs := results.Functions(by[arg(a, "scenario")], arg(a, "process"), arg(a, "kind") == "alloc")
				if n := int(argf(a, "n", 20)); len(fs) > n {
					fs = fs[:n]
				}
				return js(fs), nil
			},
		},
	}
}

func outDir(cfg *config.Config, out string) string {
	if out == "" {
		return filepath.Join(cfg.Dir, "results")
	}
	return out
}

func js(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) }

// Serve runs the server on r/w until r closes.
func Serve(ctx context.Context, r io.Reader, w io.Writer, version string) error {
	ts := tools()
	byName := map[string]tool{}
	for _, t := range ts {
		byName[t.Name] = t
	}
	var mu sync.Mutex
	send := func(resp response) {
		b, _ := json.Marshal(resp)
		mu.Lock()
		w.Write(append(b, '\n'))
		mu.Unlock()
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var wg sync.WaitGroup
	for sc.Scan() {
		var req request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			send(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			continue
		}
		if len(req.ID) == 0 { // notification
			continue
		}
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			json.Unmarshal(req.Params, &p)
			pv := p.ProtocolVersion
			if pv == "" {
				pv = "2025-06-18"
			}
			send(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"protocolVersion": pv,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "go-dyno", "version": version},
				"instructions":    "go-dyno measures Go services under load with pprof. Use dyno_run with a YAML config to measure, dyno_summary to read results, dyno_redline to find the throughput ceiling, dyno_compare for base vs head. Only trust numbers from runs graded A or B; report failed checks.",
			}})
		case "ping":
			send(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})
		case "tools/list":
			send(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": ts}})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p)
			t, ok := byName[p.Name]
			if !ok {
				send(response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{-32602, "unknown tool " + p.Name}})
				continue
			}
			wg.Add(1)
			go func(id json.RawMessage) { // long runs must not block pings
				defer wg.Done()
				text, err := t.run(ctx, p.Arguments)
				res := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": false}
				if err != nil {
					res["content"] = []map[string]any{{"type": "text", "text": strings.TrimSpace(text + "\nerror: " + err.Error())}}
					res["isError"] = true
				}
				send(response{JSONRPC: "2.0", ID: id, Result: res})
			}(req.ID)
		default:
			send(response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{-32601, "method not found: " + req.Method}})
		}
	}
	wg.Wait()
	return sc.Err()
}
