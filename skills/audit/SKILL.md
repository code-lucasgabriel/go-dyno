---
name: audit
description: 'Run a load-driven CPU, memory and latency audit of a Go service end to end with the go-dyno tool: bootstrap an unfamiliar repo (instrument, config, dependencies, smoke test), measure under load in Linux containers, and deliver a graded HTML report. Use when asked to profile, find hotspots, measure CPU per request, check a performance fix end to end, reproduce a stall/freeze/leak under load, find a throughput ceiling, or compare builds (A/B, version vs version). Triggers: pprof, flamegraph, CPU per request, allocation rate, GC share, goroutine dump, execution trace, mutex/block profile, saturation sweep, redline, load test with k6, "why is this service slow". NOT for distributed tracing, production telemetry pipelines, or non-Go services.'
argument-hint: '[what to investigate, e.g. "why is checkout slow" or "compare main vs this branch"]'
---

# go-dyno: Go pprof audit

A profile is only as good as the load and the environment it was taken under. Most of the effort in an audit goes into making the measurement trustworthy; reading the profile is the easy part. The `go-dyno` binary does the mechanics (processes, load, capture window, tidy tables, validity checks, report). This skill is the judgment around it: what to set up, what to distrust, how to read the result.

**The user's request:** $ARGUMENTS. If it is empty, run a baseline audit of the service in the current repo: hotspots, allocation, leaks and the throughput ceiling.

**Drive the whole audit yourself.** Decide everything the repo can answer (entry point, routes, dependencies, settings, config). Ask the user only for what it cannot: credentials, a production rate they know, which of several services to audit. Ask once, with your best guess, then continue.

Tool: run `go-dyno version`. If it is missing, install it with `go install github.com/code-lucasgabriel/go-dyno/cmd/go-dyno@latest` (needs Go 1.26+). When this skill comes from the go-dyno plugin, the same commands are also available as `dyno_*` MCP tools; long runs started that way continue in the background.

Deliverable: the HTML report (`go-dyno report`) plus a short written summary. Every number comes from the tidy tables; none is typed by hand. Add a notebook (`references/notebook-report.md`) only when the user wants to re-cut the data.

## Hard rules

- Work in a separate git worktree; never touch the user's checkout. Don't commit or push unless asked.
- Only add instrumentation, test handlers, load scripts and go-dyno configs. Don't fix the code under test during the audit; record findings.
- Report every failed check or suspicious number as it happens. Never smooth a bad run into the averages; go-dyno already excludes runs with failing checks, so say how many were excluded and why.
- Only draw conclusions from runs graded A or B. Quote the grade next to the number.
- Label every claim **MEASURED** (from a run in the results folder), **CODE** (file:line) or **OPINION**.
- Report relative numbers (percent of a process's CPU, µs per request, bytes per request) under a documented load. No money figures.

## Procedure

### 0. Bootstrap an unfamiliar repo

Do this before any measurement; it is most of the work on a repo you have not seen.

1. **Worktree:** `git worktree add ../<repo>-dyno -b dyno-audit` and work there.
2. **Find the service.** Look for `package main` under `cmd/` or the root, and for `http.ListenAndServe`, `http.Server`, `gin`, `echo`, `chi`, `fiber`, gRPC servers. If there are several binaries, pick the one the request is about; if unclear, ask with your best guess.
3. **Find how it runs.** A `Dockerfile`, `docker-compose.yml`, `Makefile`, Helm chart or deployment manifest shows the start command, port, environment variables, CPU limit and dependencies. Production-like values come from the deployment manifest, not from local defaults.
4. **Find the routes** from the router setup. Prefer the routes the request is about; otherwise the high-traffic read and write paths, weighted roughly as production traffic would be (ask if a production mix exists). Note which need auth, IDs that must exist, or request bodies.
5. **Dependencies.** Start databases, caches and queues with the repo's compose file, or minimal containers on one Docker network (`docker.network` in the config). Load seed data or migrations if the routes need data; put the reset in `hooks.before_rep`.
6. **Instrument** (step 1 below), then `go-dyno init > dyno.yaml` and fill it in. Set `load.rate_source` honestly (step 3).
7. **Smoke test** before real runs: a copy of the config with `reps: 1`, `warmup: 5s`, `capture.cpu: 5s`, a low rate. Read the run's `checks.json` and `<proc>.log`. Fix startup failures (missing env, unreachable dependency, wrong port) and failing routes (401/404/400: add headers, real IDs, valid bodies) until the error rate is 0 and `requests_on_target` passes. Never start the real runs with errors in the smoke test.
8. **Rate:** if production rate is unknown, run `go-dyno redline` now and set `rate_source: {kind: redline, redline: results/redline-<scenario>, fraction: 0.5}`.

Common traps: the service reads config from a file or flag, not env; it needs an auth token generated by a script in the repo; a background worker is a second process (add it to `processes`); the Dockerfile builds for production with stripped symbols (profiles still work; do not change the build).

### 1. Instrument the service

- Add the drop-in package: `stop, _ := dyno.Start(); defer stop()` in `main`, and wrap the HTTP handler with `dyno.Middleware`. For other protocols call `dyno.CountRequest()` per request. `dyno.Start` is a no-op unless `DYNO_ADDR` is set, so it is safe to merge.
- If the service can't be changed, use `metrics: prometheus` in the config with the service's request counter. CPU then comes from `process_cpu_seconds_total` and there are no cgroup or GC-CPU numbers; say so.
- Mutex/block profiles only with `capture.contention: true` in a separate run: they add overhead, so CPU baselines run without them.

### 2. Environment that resembles production

- **Profile on Linux, not on a macOS host.** On macOS the kernel dominates the profile (in one audit: write syscalls 37%, thread wake-ups 24%, event polling 23%; the service's own code barely visible). Use `docker:` processes with `cpus: <pod limit>`; go-dyno sets `--cpus` and `GOMAXPROCS`. The `linux` check warns otherwise.
- **Match production settings that change the profile** in `env:`: log level (a framework default of debug can add several log lines per request), tracing sampler (an SDK that traces every request with no collector wastes CPU), compression, serializer, metrics on/off. Check the service reads them: look at the effective config in `<proc>.log` once.
- **Clean shared state between runs** with `hooks.before_rep`. Service-discovery registrations from killed processes (etcd leases, Consul, etc.) send traffic to dead instances.
- Record anything not in the config under `params:`; it lands in `meta.json` and `run.csv`.

### 3. Load

- Use the production calibration you have (clients per pod, req/s per pod, payload mix), put it in `load:`, and record where the rate came from in `load.rate_source` (`measured` or `estimate`, with a note). The report prints it next to every scenario.
- **No production numbers** (no metrics access, new service): don't invent a rate. Run `go-dyno redline` first, then measure at fixed fractions of the knee with `rate_source: {kind: redline, redline: <redline folder>, fraction: 0.3 | 0.5 | 0.7}`. Hotspot shares, leaks and A/B comparisons hold at any steady rate; absolute latency and headroom do not, so say so.
- The built-in generator is open loop: it sends at `rate` whether or not the service keeps up, so slowness shows as latency and drops instead of hiding. Use `k6:` when you need sessions, persistent connections or a protocol other than HTTP; the script reads `BASE_URL`, `RATE` and `DURATION` from the environment.
- **Synthetic payloads must not be trivially compressible** if compression is on: use `body_random_bytes`.
- **Validate inside k6 scripts:** retry and count failed client setup (login, handshake, session creation), count timeouts and errors per route, and for servers that send messages on their own, count messages sent vs received (export them with `counter_prefix`). Know your load client's quirks (example: a client that buffers one late message and hands it to the next wait counts late messages as delivered).
- Warm-up (default 60 s) is excluded from every statistic.

### 4. Measure

`go-dyno run -c dyno.yaml [--label base] [-o results/base]`

Per repetition, go-dyno starts fresh processes, starts load, waits for warm-up, scrapes stats, takes a CPU profile of every process in parallel, scrapes again (the two scrapes bracket the CPU window), then takes heap, allocs, a goroutine dump and an execution trace. A sampler records goroutines and heap every `sample_interval` and a heap snapshot every `snapshot_interval` for leak detection.

Then it runs the per-run checks: captures complete, error rate, requests in the CPU window vs target, server count vs load-generator count, throttling, pprof CPU vs OS CPU, Linux host, load generator on schedule, no requests dropped, load generator CPU. Fail → grade F, run excluded. Repeat each scenario ≥5 times (`reps`); the scenario checks flag fewer reps and run-to-run spread above `max_cv`.

**Leak hunting:** a long `duration` (10 min or more) at constant load. Under constant load after warm-up, goroutines and heap in use should be flat. The report gives the slope (goroutines/min, MiB/min) and the allocation sites holding the growth. Always run a steady control scenario next to the suspect one, so "flat" has a reference.

### 5. Numbers go-dyno computes

- **CPU µs per request** = OS CPU (cgroup, else rusage) in the window ÷ the entry process's server-side request count in the same window (load-generator count if the service doesn't count). Every process is divided by the same count, so a backend's figure is CPU per user request.
- **Utilisation** = CPU seconds ÷ window ÷ CPU limit.
- **Bytes allocated per request** = allocated-bytes delta over the window ÷ requests.
- **GC share** = GC CPU (assist + background + pauses, idle mark excluded) in the window ÷ process CPU.
- Latency p50/p95/p99 from the load generator, after warm-up, successful requests only, per route and overall.

### 6. Reading profiles

- Rank findings by **share of the process's CPU** (report top functions, `focus:` code paths). These shares are stable; absolute µs are not (see Pitfalls).
- Shares of different code paths overlap (a function can sit under several). Never sum them.
- go-dyno computes shares against the profile's real total (no hidden nodes). When using `go tool pprof` by hand, pass `-nodefraction=0 -edgefraction=0` or the shares come out inflated (in one audit 35.6% read as 48.9%), and with `-focus` use a large `-nodecount`.
- The flame graph in the report is merged over passing repetitions; search highlights frames and gives the share under them. For digging deeper: `go tool pprof -http=: results/<run>/<proc>-cpu.pb.gz`.
- Attribute syscalls to callers with `go tool pprof -focus=Syscall -top -cum`.
- Compare builds with `go-dyno compare` and `go-dyno report --base`. If a difference is spread evenly over every function of unchanged code, suspect the environment, not the code.
- Escape analysis (`go build -gcflags=-m`) only for the hot paths the profile pointed to.

### 7. Stalls, freezes and ceilings

- The **goroutine groups** (state + top frames) are the primary evidence for "something is stuck": a leak is many goroutines in the same state growing over time; a freeze is a chain: one goroutine blocked on a send, many blocked behind it. A goroutine blocked for the whole trace window does **not** appear in `go tool trace -pprof=sync` (it records unblock events); use the dump for it.
- `go tool trace -pprof=sync|syscall|sched results/<run>/<proc>-trace.out` turns a trace into a blocking profile: good for "many goroutines wait a little" (queueing).
- **Ceiling:** `go-dyno redline -c dyno.yaml`. It multiplies the rate until p99, errors or throughput cross the limits, bisects the knee, and diagnoses the limit: throttling, a CPU-bound process, a single hot goroutine (one core used of several), the load generator, or a concurrency cap (Little's law: max throughput ≈ concurrent slots ÷ time each holds a request), with the largest group of waiting goroutines as evidence. **Then test the hypothesis by changing one parameter** (pool size, buffer) and re-running.

### 8. Method checks before trusting anything

- **Known-fix A/B:** measure a change whose effect is known (for example, from a microbenchmark). If `go-dyno compare` doesn't show a clear move in the expected direction, the setup is not measuring what you think: stop and report.
- **Environment control:** repeat the baseline with unrelated CPU load on the machine. If CPU per request moves by more than the run-to-run spread, absolute µs on this machine are not reliable; say so and rank by shares.
- **Microbenchmarks** for isolated hotspots: `-count=10`, `benchstat`. Set the framework's log level inside the benchmark file; debug logging inside the measured path corrupts both timing and `benchstat` parsing.

### 9. Deliverable

- `go-dyno report results [--base results/base]` writes `results/report.html`: trust grades, CPU per request, latency, flame graphs, allocation, leak trends, goroutine groups, redline, compare. Hand over the file path.
- `go-dyno summary results` (or `dyno_summary`) gives the same numbers as JSON for the written summary. Each conclusion cites its number, its run grade and its label.
- For a CI gate: `go-dyno compare results/base results/head --markdown comment.md` exits 1 on regression.
- Notebook only on request: follow `references/notebook-report.md`.

## Pitfalls seen in real audits

| Symptom | Cause | What to do |
|---|---|---|
| Profile is mostly syscalls, `pthread_cond_signal`, `kevent` | Profiling on a macOS host | Run in Linux containers (`linux` check) |
| Same load, CPU per request varies 20–40% between otherwise identical runs | Apple Silicon moves threads between efficiency and performance cores with total load | Rank by shares; run the environment control; compare builds at the same load; treat absolute µs as indicative |
| Unchanged code is uniformly cheaper next to a heavier build | Same core-placement effect | Don't attribute it to code |
| Requests fail for the first seconds of a run (no instance responds, connection refused) | Stale service-discovery entries from killed processes | `hooks.before_rep` clears them |
| Server-sent messages silently missing in a baseline | Client setup never completed (failed first call), so the server has no session to send to | Retry setup in the load script and count failures |
| A slow-reader test never fills socket buffers | Synthetic payload compressed to a few bytes | `body_random_bytes` |
| Sweep stops at a low rate with fine latency | Pass rule too strict | Use an error *rate* threshold (`redline.max_error_rate`) |
| Throughput plateaus with no process CPU-bound | Worker pool or single goroutine bound, or the load generator | Read the redline diagnosis; change the pool size and re-run |
| `cpu_agreement` warns on every run | macOS host, or a process spends CPU outside Go (cgo, children) | Linux containers; profile the other process too |
| `benchstat` "parsing iteration count" errors | Log lines interleaved with benchmark output | Set log level in the benchmark |

## Not worth reporting as findings

A cost under ~1% of a process's CPU, unless it is a correctness or reliability issue, or it is cheap to remove and on every request.

## NOT for

Distributed tracing (use OpenTelemetry tooling), production telemetry pipelines, non-Go services.
