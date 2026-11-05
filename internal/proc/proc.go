// Package proc starts and stops the processes under test, in Docker or as local commands.
package proc

import (
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/code-lucasgabriel/go-dyno/internal/collect"
	"github.com/code-lucasgabriel/go-dyno/internal/config"
)

// Running is a started process.
type Running struct {
	Spec    config.Process
	Mode    string // docker or command
	Started time.Time
	cmd     *exec.Cmd
	exited  chan error
	cname   string
	log     *os.File
}

// Build builds every docker image that has a build context. Call once per invocation.
func Build(ctx context.Context, cfg *config.Config, logf func(string, ...any)) error {
	for _, p := range cfg.Processes {
		if p.Docker == nil || p.Docker.Build == "" {
			continue
		}
		args := []string{"build", "-t", p.Docker.Image}
		if p.Docker.Dockerfile != "" {
			args = append(args, "-f", cfg.Path(p.Docker.Dockerfile))
		}
		args = append(args, cfg.Path(p.Docker.Build))
		logf("building %s", p.Docker.Image)
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker build %s: %v\n%s", p.Docker.Image, err, tail(string(out), 30))
		}
	}
	return nil
}

// Env returns the environment go-dyno injects into a process.
func Env(cfg *config.Config, p config.Process, inDocker bool) map[string]string {
	env := map[string]string{}
	for k, v := range p.Env {
		env[k] = v
	}
	gd := "gctrace=1"
	if v := env["GODEBUG"]; v != "" {
		gd = v + "," + gd
	}
	env["GODEBUG"] = gd
	if p.CPUs > 0 {
		if _, ok := env["GOMAXPROCS"]; !ok {
			env["GOMAXPROCS"] = fmt.Sprint(int(math.Max(1, math.Ceil(p.CPUs))))
		}
	}
	addr := p.Debug
	if inDocker {
		if _, port, err := net.SplitHostPort(p.Debug); err == nil {
			addr = "0.0.0.0:" + port
		}
	}
	env["DYNO_ADDR"] = addr
	if cfg.Capture.Contention {
		env["DYNO_CONTENTION"] = "1"
	}
	return env
}

var unsafe = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

// ownerLabel marks every container go-dyno starts with "<host>:<pid>" of the
// go-dyno process that started it.
const ownerLabel = "go-dyno.owner"

func owner() string {
	h, _ := os.Hostname()
	return fmt.Sprintf("%s:%d", h, os.Getpid())
}

// Reap removes containers started by a go-dyno process on this host that is no
// longer running (it crashed or was killed before it could stop them), so
// they don't hold ports or CPU in the next run. Containers of go-dyno
// processes that are still running are left alone.
func Reap(ctx context.Context, cfg *config.Config, logf func(string, ...any)) {
	docker := false
	for _, p := range cfg.Processes {
		docker = docker || p.Docker != nil
	}
	if !docker {
		return
	}
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "label="+ownerLabel,
		"--format", `{{.Names}} {{.Label "`+ownerLabel+`"}}`).Output()
	if err != nil {
		return
	}
	host, _ := os.Hostname()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, own, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		i := strings.LastIndex(own, ":")
		if i < 0 {
			continue
		}
		pid, err := strconv.Atoi(own[i+1:])
		if err != nil || own[:i] != host || alive(pid) {
			continue
		}
		if exec.CommandContext(ctx, "docker", "rm", "-f", name).Run() == nil {
			logf("removed container %s left by an earlier go-dyno (pid %d) that did not stop it", name, pid)
		}
	}
}

// Start launches p and waits until its debug endpoint (and Ready URL) answer.
func Start(ctx context.Context, cfg *config.Config, p config.Process, runID, logPath string) (*Running, error) {
	lf, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	r := &Running{Spec: p, log: lf, exited: make(chan error, 1)}
	if p.Docker != nil {
		r.Mode = "docker"
		r.cname = unsafe.ReplaceAllString("dyno-"+runID+"-"+p.Name, "-")
		exec.Command("docker", "rm", "-f", r.cname).Run()
		args := []string{"run", "-d", "--name", r.cname, "--label", ownerLabel + "=" + owner()}
		if p.CPUs > 0 {
			args = append(args, "--cpus", fmt.Sprint(p.CPUs))
		}
		for _, port := range p.Docker.Ports {
			args = append(args, "-p", port)
		}
		if p.Docker.Network != "" {
			args = append(args, "--network", p.Docker.Network)
		}
		env := Env(cfg, p, true)
		for _, k := range sortedKeys(env) {
			args = append(args, "-e", k+"="+env[k])
		}
		args = append(args, p.Docker.RunArgs...)
		args = append(args, p.Docker.Image)
		args = append(args, p.Docker.Args...)
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			lf.Close()
			return nil, fmt.Errorf("docker run %s: %v: %s", p.Name, err, strings.TrimSpace(string(out)))
		}
		r.Started = time.Now()
	} else {
		r.Mode = "command"
		cmd := exec.Command(p.Command[0], p.Command[1:]...)
		cmd.Dir = cfg.Path(p.Dir)
		if cmd.Dir == "" {
			cmd.Dir = cfg.Dir
		}
		cmd.Env = os.Environ()
		env := Env(cfg, p, false)
		for _, k := range sortedKeys(env) {
			cmd.Env = append(cmd.Env, k+"="+env[k])
		}
		cmd.Stdout, cmd.Stderr = lf, lf
		setGroup(cmd)
		if err := cmd.Start(); err != nil {
			lf.Close()
			return nil, fmt.Errorf("start %s: %w", p.Name, err)
		}
		r.Started = time.Now()
		r.cmd = cmd
		go func() { r.exited <- cmd.Wait() }()
	}
	if err := r.waitReady(ctx, 3*time.Minute); err != nil {
		r.Stop()
		b, _ := os.ReadFile(logPath)
		return nil, fmt.Errorf("%s not ready: %v\n--- log tail ---\n%s", p.Name, err, tail(string(b), 20))
	}
	return r, nil
}

func (r *Running) waitReady(ctx context.Context, max time.Duration) error {
	deadline := time.Now().Add(max)
	var last error
	for time.Now().Before(deadline) {
		select {
		case err := <-r.exited:
			r.exited <- err
			return fmt.Errorf("process exited: %v", err)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if r.Mode == "docker" {
			out, _ := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", r.cname).Output()
			if strings.TrimSpace(string(out)) == "false" {
				return fmt.Errorf("container exited")
			}
		}
		if last = collect.Ready(ctx, r.Spec); last == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return last
}

// Stop stops the process and finishes its log file.
func (r *Running) Stop() error {
	defer r.log.Close()
	if r.Mode == "docker" {
		logs := exec.Command("docker", "logs", r.cname)
		logs.Stdout, logs.Stderr = r.log, r.log
		logs.Run()
		return exec.Command("docker", "rm", "-f", r.cname).Run()
	}
	if r.cmd == nil || r.cmd.Process == nil {
		return nil
	}
	terminate(r.cmd)
	select {
	case <-r.exited:
	case <-time.After(5 * time.Second):
		kill(r.cmd)
		<-r.exited
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Shell runs a hook command in dir. Empty commands are a no-op.
func Shell(ctx context.Context, dir, command string) error {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	c := exec.CommandContext(ctx, "sh", "-c", command)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("hook %q: %v\n%s", command, err, tail(string(out), 20))
	}
	return nil
}
