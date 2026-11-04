// Command go-dyno measures Go services under load with pprof and tells you whether to trust the numbers.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	"github.com/code-lucasgabriel/go-dyno/internal/compare"
	"github.com/code-lucasgabriel/go-dyno/internal/config"
	"github.com/code-lucasgabriel/go-dyno/internal/extract"
	"github.com/code-lucasgabriel/go-dyno/internal/mcp"
	"github.com/code-lucasgabriel/go-dyno/internal/redline"
	"github.com/code-lucasgabriel/go-dyno/internal/report"
	"github.com/code-lucasgabriel/go-dyno/internal/runner"
	"github.com/code-lucasgabriel/go-dyno/internal/trust"
)

const usage = `go-dyno: load-driven pprof audits for Go services

Usage:
  go-dyno init                          print a starter config
  go-dyno run -c dyno.yaml [flags]      measure: fresh processes, load, profiles, checks
  go-dyno redline -c dyno.yaml [flags]  ramp load to the ceiling and explain the limit
  go-dyno compare BASE HEAD [flags]     regression gate (exit 1 on regression)
  go-dyno report RESULTS [flags]        self-contained HTML report
  go-dyno summary RESULTS               JSON digest of a results folder
  go-dyno extract RUN_DIR...            rebuild tidy tables and checks of run folders
  go-dyno mcp                           MCP server over stdio
  go-dyno version

Run "go-dyno <command> -h" for flags.
`

func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	runner.Version = version()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logf := func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	code := 0
	switch cmd {
	case "init":
		fmt.Print(config.Example)
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		c := fs.String("c", "dyno.yaml", "config file")
		out := fs.String("o", "", "results folder (default: results next to the config)")
		label := fs.String("label", "", "build label stored with each run, e.g. base or head")
		sha := fs.String("sha", "", "commit to record (default: git rev-parse in the config folder)")
		reps := fs.Int("reps", 0, "repetitions (overrides the config)")
		parse(fs, args)
		var cfg *config.Config
		if cfg, err = config.Read(*c); err == nil {
			o := pick(*out, filepath.Join(cfg.Dir, "results"))
			var res []runner.Result
			res, err = runner.Run(ctx, cfg, runner.Options{Out: o, Label: *label, SHA: *sha, Reps: *reps, Log: logf})
			failed := 0
			for _, r := range res {
				if r.Trust.Failed {
					failed++
				}
			}
			logf("%d runs in %s (%d failed checks). Next: go-dyno report %s", len(res), o, failed, o)
			if err == nil && failed == len(res) && failed > 0 {
				code = 1
			}
		}
	case "redline":
		fs := flag.NewFlagSet("redline", flag.ExitOnError)
		c := fs.String("c", "dyno.yaml", "config file")
		out := fs.String("o", "", "results folder (default: results next to the config)")
		parse(fs, args)
		var cfg *config.Config
		if cfg, err = config.Read(*c); err == nil {
			var r redline.Result
			if r, err = redline.Run(ctx, cfg, pick(*out, filepath.Join(cfg.Dir, "results")), logf); err == nil {
				fmt.Printf("knee: %.0f req/s (%s)\n", r.Knee, r.Limits)
				for _, f := range r.Diagnosis {
					fmt.Printf("- %s: %s\n", f.Kind, f.Detail)
					if f.Evidence != "" {
						fmt.Printf("  %s\n", f.Evidence)
					}
				}
			}
		}
	case "compare":
		fs := flag.NewFlagSet("compare", flag.ExitOnError)
		cpu := fs.Float64("cpu", compare.Default.CPU, "allowed relative change in CPU per request")
		alloc := fs.Float64("alloc", compare.Default.Alloc, "allowed relative change in bytes per request")
		p99 := fs.Float64("p99", compare.Default.P99, "allowed relative change in p99 latency")
		p50 := fs.Float64("p50", compare.Default.P50, "allowed relative change in p50 latency")
		md := fs.String("markdown", "", "also write the markdown table to this file (for a PR comment)")
		asJSON := fs.Bool("json", false, "print JSON instead of markdown")
		parse(fs, args)
		if fs.NArg() != 2 {
			err = fmt.Errorf("usage: go-dyno compare BASE HEAD")
			break
		}
		var r compare.Report
		if r, err = compare.Run(fs.Arg(0), fs.Arg(1), compare.Thresholds{CPU: *cpu, Alloc: *alloc, P99: *p99, P50: *p50}); err == nil {
			if *asJSON {
				printJSON(r)
			} else {
				fmt.Print(r.Markdown())
			}
			if *md != "" {
				err = os.WriteFile(*md, []byte(r.Markdown()), 0o644)
			}
			if r.Regressed {
				code = 1
			}
		}
	case "report":
		fs := flag.NewFlagSet("report", flag.ExitOnError)
		base := fs.String("base", "", "base results folder: adds a compare section and flame graph diffs")
		out := fs.String("o", "", "output file (default <results>/report.html)")
		parse(fs, args)
		if fs.NArg() != 1 {
			err = fmt.Errorf("usage: go-dyno report RESULTS [-base BASE] [-o report.html]")
			break
		}
		var d *report.Data
		if d, err = report.Build(fs.Arg(0), *base); err == nil {
			o := pick(*out, filepath.Join(fs.Arg(0), "report.html"))
			if err = report.Write(d, o); err == nil {
				fmt.Println(o)
			}
		}
	case "summary":
		if len(args) != 1 {
			err = fmt.Errorf("usage: go-dyno summary RESULTS")
			break
		}
		var s *report.Summary
		if s, err = report.Summarize(args[0]); err == nil {
			printJSON(s)
		}
	case "extract":
		for _, d := range args {
			if err = extract.Run(d); err != nil {
				break
			}
			tr, e := trust.Evaluate(d)
			if e != nil {
				err = e
				break
			}
			fmt.Printf("%s: grade %s (%d)\n", d, tr.Grade, tr.Score)
		}
	case "mcp":
		err = mcp.Serve(ctx, os.Stdin, os.Stdout, version())
	case "version":
		fmt.Println("go-dyno", version())
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "go-dyno:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// parse accepts flags before and after positional arguments.
func parse(fs *flag.FlagSet, args []string) {
	var pos []string
	for {
		fs.Parse(args)
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	fs.Parse(pos)
}

func pick(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func printJSON(v any) {
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	e.Encode(v)
}
