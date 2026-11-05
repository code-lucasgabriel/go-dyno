<p align="center">
  <img src=".github/assets/go-dyno.jpg" alt="The Go gopher pressing a button wired to a pressure gauge whose needle drives a jagged latency trace" width="720">
</p>

<h1 align="center">go-dyno</h1>

<p align="center">
  <strong>Load-driven performance audits for Go services.</strong><br>
  Put your service under controlled load, profile it with pprof, and find out whether the measurement can be trusted before you read a single number.
</p>

<p align="center">
  <a href="https://github.com/code-lucasgabriel/go-dyno/actions/workflows/ci.yml"><img src="https://github.com/code-lucasgabriel/go-dyno/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://pkg.go.dev/github.com/code-lucasgabriel/go-dyno/dyno"><img src="https://pkg.go.dev/badge/github.com/code-lucasgabriel/go-dyno/dyno.svg" alt="Go Reference"></a>
  <a href="https://goreportcard.com/report/github.com/code-lucasgabriel/go-dyno"><img src="https://goreportcard.com/badge/github.com/code-lucasgabriel/go-dyno" alt="Go Report Card"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="MIT License"></a>
</p>

<p align="center">
  <a href="#quick-start">Quick start</a> ·
  <a href="#use-it-on-your-service">Your service</a> ·
  <a href="#agents">Agents</a> ·
  <a href="#commands">Commands</a> ·
  <a href="#trust-checks">Trust checks</a> ·
  <a href="#regression-gate-in-ci">CI gate</a>
</p>

---

A profile is only as good as the load and the environment it was taken under. go-dyno runs the service in fresh processes, drives it at a fixed rate, captures every profile inside the same window, and grades each run before any number reaches you.

| | |
|---|---|
| **Measure** | CPU µs per request, bytes allocated per request, GC share, latency percentiles, top functions, flame graphs. |
| **Trust** | Every run is graded A–F by validity checks: errors, request count on target, throttling, pprof vs OS CPU, Linux host, load generator health. Failed runs are kept but never averaged in. |
| **Redline** | Ramps load to the throughput ceiling and names the limit: a CPU-bound process, throttling, a single hot goroutine, the load generator, or a concurrency cap. |
| **Leaks** | Goroutine and heap slopes under constant load, plus the allocation sites holding the growth. |
| **Compare** | Base vs head with run-to-run noise as the floor. Exits 1 on regression, for CI. |
| **Report** | One self-contained HTML file. Also a JSON summary, tidy CSV tables, and an MCP server for agents. |

## Quick start

```sh
go install github.com/code-lucasgabriel/go-dyno/cmd/go-dyno@latest
```

Requires Go 1.26+. Docker is recommended: profiles taken on a macOS host are dominated by the kernel.

### Try it on the demo (5 minutes)

The demo service has three planted problems: a hotspot, a leak, and a concurrency ceiling.

```sh
git clone https://github.com/code-lucasgabriel/go-dyno && cd go-dyno
go install ./cmd/go-dyno

go-dyno run     -c examples/demo/hotspot.yaml   # regexp compiled per request
go-dyno run     -c examples/demo/leak.yaml      # subscribers never removed
go-dyno redline -c examples/demo/redline.yaml   # 4 workers × 20 ms
go-dyno report  examples/demo/results           # → examples/demo/results/report.html
```

What go-dyno finds (Linux container, 2 CPUs):

| Problem | Finding |
|---|---|
| Hotspot | `regexp` compilation is ~70% of the process's CPU, ~900 µs CPU per request. Grade A. |
| Leak | Goroutines grow ~6,000/min at 100 req/s (one per request); heap growth points at `main.leak`. |
| Ceiling | Requests queue at 200 req/s; the knee under a 200 ms p99 limit is ~170 req/s. Diagnosis: a concurrency cap of ~4 requests (Little's law: 200 req/s × 22 ms), with the goroutines waiting in `main.work` as evidence. |

## Use it on your service

### 1. Instrument (optional, recommended)

One call, a no-op unless `DYNO_ADDR` is set, so it is safe to merge:

```go
import "github.com/code-lucasgabriel/go-dyno/dyno"

func main() {
	stop, err := dyno.Start() // pprof + stats on DYNO_ADDR, a private port
	if err != nil {
		log.Fatal(err)
	}
	defer stop()
	http.ListenAndServe(":8080", dyno.Middleware(mux)) // counts requests
}
```

It serves the standard pprof endpoints and `/debug/dyno/stats`: process and cgroup CPU, throttling, request count, goroutines, heap and GC CPU. The middleware keeps `http.Flusher` and `http.Hijacker` working for the handlers behind it. For non-HTTP servers call `dyno.CountRequest()`. Without the package, set `metrics: prometheus` and point go-dyno at your request counter.

### 2. Configure

`go-dyno init > dyno.yaml` prints a commented starter. The essentials:

```yaml
name: my-service
scenario: baseline
reps: 5
cpus: 2                          # pod limit: docker --cpus and GOMAXPROCS
processes:
  - name: api
    debug: localhost:6060        # where DYNO_ADDR listens
    ready: http://localhost:8080/healthz
    docker:
      build: .
      image: my-service:dyno
      ports: ["8080:8080", "6060:6060"]
    env: {LOG_LEVEL: info}       # match production settings
load:
  base_url: http://localhost:8080
  rate: 200                      # req/s, open loop
  rate_source: {kind: measured, note: "p95 req/s per pod, gateway metrics"}
  warmup: 60s
  routes:
    - {name: list, path: /items, weight: 3}
    - {name: create, method: POST, path: /items, body_random_bytes: 1024}
```

**Where the rate comes from.** `rate_source` is shown on every report next to the numbers, so an estimate is never read as production traffic. `kind` is `measured`, `estimate` or `redline`. Without production numbers, run `go-dyno redline` first and measure at a fraction of the knee:

```yaml
load:
  rate_source: {kind: redline, redline: results/redline-baseline, fraction: 0.5}   # sets rate
```

Processes run in Docker (`docker:`) or as local commands (`command:`). Load comes from the built-in open-loop HTTP generator or from a k6 script (`load.k6.script`), which receives `BASE_URL`, `RATE` and `DURATION`. `run` and `redline` both use k6 when it is configured.

### 3. Run and read

```sh
go-dyno run -c dyno.yaml
go-dyno report results
```

## Agents

go-dyno is built to be driven by an agent. This repo is its own Claude Code plugin marketplace: Claude Code installs the plugin straight from GitHub, with no catalog in between. Inside Claude Code, run this on its own line:

```
/plugin install go-dyno --marketplace code-lucasgabriel/go-dyno
```

On older Claude Code versions, use the two-step form instead:

```
/plugin marketplace add code-lucasgabriel/go-dyno
/plugin install go-dyno@go-dyno
```

To pick up a new release later, run `/plugin update go-dyno`. Then open Claude Code in any Go repo and run:

```
/go-dyno:audit why is checkout slow
```

The agent works in a separate git worktree. It finds the service, its routes and dependencies, adds the `dyno` package, writes `dyno.yaml`, smoke-tests until requests succeed, finds a rate (redline if there is no production number), measures, and hands back the report with graded findings. It asks only for what the repo can't tell it, such as credentials, and removes the containers, networks and images it created when it is done. Without arguments it runs a baseline audit.

The plugin also registers the MCP server, built from the plugin's own source on first use (needs Go 1.26+). Tools: `dyno_run`, `dyno_redline`, `dyno_compare`, `dyno_report`, `dyno_summary`, `dyno_top`.

**Other agents:** the skill is plain Markdown in [`skills/audit/`](skills/audit/SKILL.md); copy it into the agent's skills folder. Any MCP client can run `go-dyno mcp` over stdio, for example `claude mcp add go-dyno -- go-dyno mcp`.

## Commands

| Command | What it does |
|---|---|
| `go-dyno init` | Print a starter config. |
| `go-dyno run -c dyno.yaml` | Run `reps` repetitions: fresh processes, warm-up, stats, parallel CPU profiles, stats again, then heap, allocs, goroutines, trace. Writes `results/<scenario>-r<N>/`. Flags: `-o`, `--label`, `--sha`, `--reps`. |
| `go-dyno redline -c dyno.yaml` | Multiply the rate (`redline.start`, `factor`) until p99, errors or throughput cross the limits, bisect the knee, diagnose. Uses the k6 script when `load.k6` is set. |
| `go-dyno compare BASE HEAD` | Compare two results folders. Exits 1 on regression. Flags: `--cpu`, `--alloc`, `--p99`, `--p50`, `--markdown`, `--json`. |
| `go-dyno report RESULTS` | Write `RESULTS/report.html`. `--base` adds a comparison and flame graph diffs. |
| `go-dyno summary RESULTS` | Print a JSON digest. |
| `go-dyno extract RUN_DIR...` | Rebuild tidy tables and checks for run folders. |
| `go-dyno mcp` | Serve the tools over MCP (stdio). |

Containers that go-dyno starts are labelled with the go-dyno process that owns them. If a run crashes or is killed, the next `run` or `redline` on the same host removes its leftover containers before starting.

## How the numbers are made

- **CPU per request** = OS CPU in the capture window (cgroup if available, else rusage) ÷ requests the entry process served in the same window. Every process is divided by the same count, so a backend's figure is CPU per user request.
- **Shares** are computed against the profile's full total, with no hidden nodes.
- **Latency** comes from the load generator: successful requests after warm-up, per route and overall. Each request is timed from when it was sent, for the built-in generator and k6 alike.
- **GC share** = GC CPU (assist, background and pauses, idle mark excluded) ÷ process CPU.
- **Aggregates** are the median and min–max over passing repetitions.

## Trust checks

| Check | Fails or warns when |
|---|---|
| `captures` | a profile or stats scrape is missing (fail) |
| `error_rate` | errors after warm-up exceed `max_error_rate` (fail) |
| `requests_on_target` | requests in the CPU window are off target by more than `request_tolerance` (fail) |
| `loadgen_dropped` | requests weren't sent because `max_in_flight` was reached (fail) |
| `server_count` | the server's count disagrees with the load generator's (warn) |
| `throttling` | the cgroup throttled the process (warn) |
| `cpu_agreement` | pprof CPU and OS CPU differ by more than `cpu_agreement` (warn) |
| `linux` | the process wasn't profiled on Linux (warn) |
| `loadgen_schedule`, `loadgen_cpu` | the load generator fell behind or used >80% of the host (warn) |
| `repetitions`, `noise` | a scenario has fewer than 5 passing runs, or CPU per request varies more than `max_cv` (warn) |

The score starts at 100, minus 40 per failure and 10 per warning. A ≥ 90, B ≥ 75, C ≥ 50, and any failure is F.

## Redline diagnosis

At the first failing rate, go-dyno checks, in order:

- **Throttling:** the cgroup throttled a process.
- **CPU-bound:** a process used ≥ 90% of its CPU limit.
- **Single core:** a process used about one core while more were available, which points to one goroutine doing the work.
- **Load generator:** sends were late, or the generator used most of the host.
- **Concurrency cap:** requests queue while nothing is CPU-bound. The cap is estimated with Little's law (peak throughput × latency at low load), and the largest group of waiting goroutines is shown.

## Regression gate in CI

```yaml
- run: go-dyno run -c dyno.yaml -o results/base --label base   # on the base commit
- run: go-dyno run -c dyno.yaml -o results/head --label head   # on the PR commit
- run: go-dyno compare results/base results/head --markdown comment.md
```

A change counts only if it exceeds both its threshold (5% for CPU and allocations, 10% for latency by default) and half the run-to-run spread. Run base and head on the same machine.

## Output

Each run folder holds the raw profiles (open with `go tool pprof -http=: <file>`), `meta.json`, `checks.json`, `meta.md`, and `tidy/` CSV tables for notebooks or scripts. The layout and columns are in [`skills/audit/references/notebook-report.md`](skills/audit/references/notebook-report.md). `report` and `summary` also read run folders that are symlinks, so a results folder can be assembled from links.

## Limitations

- The built-in load generator speaks HTTP only. Use k6 for other protocols, sessions or WebSockets.
- Throttling and cgroup CPU need the `dyno` package and a Linux container.
- Run the load generator on the same machine only when it has spare CPU. The `loadgen_cpu` check tells you when it doesn't.

## License

[MIT](LICENSE)
