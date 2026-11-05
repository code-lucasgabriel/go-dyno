# Tidy data and the report notebook

The HTML report covers most audits. Write a notebook when an engineer wants to re-cut the data (another percentile, another route, another function) without re-running anything. It reads the tidy tables go-dyno writes; raw profiles stay where they are for `go tool pprof`.

## Run folder layout (written by `go-dyno run`)

```
results/<run-id>/
  meta.json                    run metadata: config snapshot, times, processes, checks config, status
  meta.md                      human summary and checks
  checks.json                  trust grade and every check
  loadgen.json                 load generator schedule: planned, sent, late, dropped, its own CPU
  stats-<proc>-0.json stats-<proc>-1.json   dyno stats at CPU window start/end
  <proc>-cpu.pb.gz <proc>-heap.pb.gz <proc>-allocs.pb.gz <proc>-goroutine.txt <proc>-trace.out
  <proc>-mutex.pb.gz <proc>-block.pb.gz      contention runs only
  <proc>.log                   process stdout/stderr, including gctrace lines
  requests.csv.gz              one row per request: t (unix), route, latency_ms, ok, status
  k6.csv.gz counters-raw.csv   k6 runs only
  runtime-<proc>.csv           goroutines and heap every sample_interval
  heapsnap-<proc>-<t>.pb.gz    heap snapshots every snapshot_interval
  tidy/                        the tables below
results/redline-<scenario>/    redline.json, one step-<rate>/ folder per step
```

`<proc>` is a process role from the config (for example `api`, `worker`). `go-dyno extract <run-dir>` rebuilds `tidy/` and `checks.json`.

## Tidy tables (`<run-id>/tidy/`)

| file | grain | columns |
|---|---|---|
| `run.csv` | one row per run × process | run_id, scenario, rep, label, sha, process, entry, cpus, rate, rate_source (measured / estimate / redline / empty), every param, window_s, requests_server, requests_loadgen, cpu_s_os, cpu_source (cgroup / rusage / prometheus), cpu_s_pprof, throttled_periods, alloc_bytes, gc_cycles, gc_cpu_s, warmup_s, load_s, goos, status |
| `requests.csv.gz` | one row per request | t (s since load start), route, latency_ms, ok, status, in_window (after warm-up), in_cpu_window |
| `counters.csv` | one row per custom counter sample (k6) | t, metric, value, tags |
| `cpu_functions.csv` | one row per process × function | process, function, flat_s, cum_s, flat_share, cum_share |
| `alloc_functions.csv` | one row per process × function (alloc_space, since process start) | process, function, flat_bytes, cum_bytes, flat_share, cum_share |
| `focus.csv` | one row per process × code path | process, label, regex, share |
| `flame.csv` | one row per process × unique CPU stack | process, stack (root;...;leaf), seconds |
| `runtime.csv` | one row per process × sample | process, t (s since load start), goroutines, heap_inuse, heap_alloc, heap_sys, num_gc |
| `heap_growth.csv` | one row per process × function with non-zero change | process, t_first, t_last, function, flat_bytes_delta, cum_bytes_delta (inuse_space, last − first snapshot after warm-up) |
| `gc.csv` | one row per GC cycle in the log | process, t (s since process start), cpu_ms (assist + background + STW), idle_ms, heap_before_mb, heap_after_mb |
| `goroutines.csv` | one row per process × (state, top frames) | process, state, top (leaf first, " < "), count |

Rules:

- Raw units (seconds, bytes); derive ratios in the notebook.
- Run ids are `<scenario>-r<N>`.
- Runs with failing checks have `"status": "FAILED"` in `meta.json`; leave them out of every aggregate.

## Notebook structure (`REPORT.ipynb`)

1. **Header (markdown):** goal, load calibration, deviations from the plan, measurement caveats. Short.
2. **Load (code):** glob all `*/tidy/run.csv` into one DataFrame, joined with `checks.json` grades; same for the other tables. Load `requests.csv.gz` lazily per scenario.
3. **Run validity (code + markdown):** every run with its grade and failing checks. Conclusions only use passing runs.
4. **One section per question**, each in the same shape:
   - markdown: the question, in one sentence;
   - code: the computation (median and min–max over reps) and the chart;
   - markdown: the conclusion, with **MEASURED / CODE / OPINION** labels and file:line for CODE.
5. **Findings (markdown + code):** a table ranked by estimated saving, each row pointing to the section that measures it. Effort S/M/L, risk.
6. **What could not be measured.**
7. **Explore (code):** helpers for the reader, for example `top(process, scenario, n)`, `share(regex, process, scenario)`, `latency(route, scenario, q)`, and how to open any raw profile with `go tool pprof -http=:`.

Register every conclusion as a check next to the number it rests on, for example `claim('MEASURED', 'buffer pooling cuts api CPU/request ≥20%', cut >= .20, f'{cut:.0%}')`, which prints PASS or FAIL. A notebook re-run on new data then flags a conclusion that no longer holds. End with the table of all claims.

Execute it top to bottom (`jupyter nbconvert --to notebook --execute --inplace REPORT.ipynb`) in an isolated environment (venv with pandas, matplotlib, nbclient, ipykernel) before handing it over.

Chart conventions: one measure per axis (no dual axes), each process role always the same colour, medians as bars or ticks with min–max whiskers, log scale for latency when ranges span decades.
