// Package config loads and validates a go-dyno YAML file.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Name      string            `yaml:"name"`
	Scenario  string            `yaml:"scenario"`
	Reps      int               `yaml:"reps"`
	CPUs      float64           `yaml:"cpus"`
	Params    map[string]string `yaml:"params"`
	Processes []Process         `yaml:"processes"`
	Load      Load              `yaml:"load"`
	Capture   Capture           `yaml:"capture"`
	Checks    Checks            `yaml:"checks"`
	Focus     []Focus           `yaml:"focus"`
	Hooks     Hooks             `yaml:"hooks"`
	Redline   Redline           `yaml:"redline"`

	Dir string `yaml:"-"` // directory of the config file; relative paths resolve against it
}

type Process struct {
	Name       string            `yaml:"name"`
	Entry      bool              `yaml:"entry"`   // receives the load; default: first process
	Debug      string            `yaml:"debug"`   // host:port of the pprof/dyno server, as seen from go-dyno
	Ready      string            `yaml:"ready"`   // optional URL that must answer 2xx before load starts
	Metrics    string            `yaml:"metrics"` // "dyno" (default) or "prometheus"
	Prometheus Prometheus        `yaml:"prometheus"`
	Command    []string          `yaml:"command"`
	Dir        string            `yaml:"dir"`
	Env        map[string]string `yaml:"env"`
	Docker     *Docker           `yaml:"docker"`
	CPUs       float64           `yaml:"cpus"`
}

type Docker struct {
	Image      string   `yaml:"image"`
	Build      string   `yaml:"build"` // build context; builds Image once before the first run
	Dockerfile string   `yaml:"dockerfile"`
	Ports      []string `yaml:"ports"`
	Network    string   `yaml:"network"`
	Args       []string `yaml:"args"` // arguments after the image
	RunArgs    []string `yaml:"run_args"`
}

type Prometheus struct {
	URL      string            `yaml:"url"`
	Requests string            `yaml:"requests"` // counter with the server-side request count
	Labels   map[string]string `yaml:"labels"`
}

type Load struct {
	BaseURL     string        `yaml:"base_url"`
	Rate        float64       `yaml:"rate"` // requests per second, open loop
	Duration    time.Duration `yaml:"duration"`
	Warmup      time.Duration `yaml:"warmup"`
	Timeout     time.Duration `yaml:"timeout"`
	MaxInFlight int           `yaml:"max_in_flight"`
	Routes      []Route       `yaml:"routes"`
	K6          *K6           `yaml:"k6"`
}

type Route struct {
	Name            string            `yaml:"name"`
	Method          string            `yaml:"method"`
	Path            string            `yaml:"path"`
	Weight          int               `yaml:"weight"`
	Body            string            `yaml:"body"`
	BodyRandomBytes int               `yaml:"body_random_bytes"` // incompressible body of this size
	Headers         map[string]string `yaml:"headers"`
}

type K6 struct {
	Script        string   `yaml:"script"`
	LatencyMetric string   `yaml:"latency_metric"`
	RouteTag      string   `yaml:"route_tag"`
	OKTag         string   `yaml:"ok_tag"`
	CounterPrefix string   `yaml:"counter_prefix"`
	Args          []string `yaml:"args"`
}

type Capture struct {
	CPU              time.Duration `yaml:"cpu"`
	Trace            time.Duration `yaml:"trace"`
	Contention       bool          `yaml:"contention"`
	SampleInterval   time.Duration `yaml:"sample_interval"`
	SnapshotInterval time.Duration `yaml:"snapshot_interval"`
}

type Checks struct {
	MaxErrorRate     float64 `yaml:"max_error_rate" json:"max_error_rate"`
	RequestTolerance float64 `yaml:"request_tolerance" json:"request_tolerance"`
	CPUAgreement     float64 `yaml:"cpu_agreement" json:"cpu_agreement"`
	MaxCV            float64 `yaml:"max_cv" json:"max_cv"`
	MaxLateRatio     float64 `yaml:"max_late_ratio" json:"max_late_ratio"`
	AllowThrottling  bool    `yaml:"allow_throttling" json:"allow_throttling"`
}

type Focus struct {
	Process string `yaml:"process" json:"process"`
	Label   string `yaml:"label" json:"label"`
	Regex   string `yaml:"regex" json:"regex"`
}

// Hooks are shell commands (run with sh -c in the config directory).
type Hooks struct {
	BeforeAll string `yaml:"before_all"`
	BeforeRep string `yaml:"before_rep"` // e.g. clear stale service-discovery entries
	AfterRep  string `yaml:"after_rep"`
	AfterAll  string `yaml:"after_all"`
}

type Redline struct {
	Start        float64       `yaml:"start"`
	Max          float64       `yaml:"max"`
	Factor       float64       `yaml:"factor"`
	Step         time.Duration `yaml:"step"`
	Warmup       time.Duration `yaml:"warmup"`
	P99          time.Duration `yaml:"p99"`
	MaxErrorRate float64       `yaml:"max_error_rate"`
	MinAchieved  float64       `yaml:"min_achieved"`
	Refine       int           `yaml:"refine"`
}

// Read reads, defaults and validates a config file.
func Read(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, _ := filepath.Abs(path)
	c.Dir = filepath.Dir(abs)
	c.Defaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) Defaults() {
	def := func(p *time.Duration, v time.Duration) {
		if *p == 0 {
			*p = v
		}
	}
	deff := func(p *float64, v float64) {
		if *p == 0 {
			*p = v
		}
	}
	if c.Name == "" {
		c.Name = "dyno"
	}
	if c.Scenario == "" {
		c.Scenario = c.Name
	}
	if c.Reps == 0 {
		c.Reps = 5
	}
	entry := false
	for i := range c.Processes {
		p := &c.Processes[i]
		if p.Metrics == "" {
			p.Metrics = "dyno"
		}
		if p.CPUs == 0 {
			p.CPUs = c.CPUs
		}
		entry = entry || p.Entry
	}
	if !entry && len(c.Processes) > 0 {
		c.Processes[0].Entry = true
	}
	l := &c.Load
	def(&l.Warmup, 60*time.Second)
	def(&l.Timeout, 5*time.Second)
	if l.MaxInFlight == 0 {
		l.MaxInFlight = 2000
	}
	for i := range l.Routes {
		r := &l.Routes[i]
		if r.Method == "" {
			r.Method = "GET"
		}
		if r.Weight == 0 {
			r.Weight = 1
		}
		if r.Name == "" {
			r.Name = r.Path
		}
	}
	if l.K6 != nil {
		if l.K6.LatencyMetric == "" {
			l.K6.LatencyMetric = "http_req_duration"
		}
		if l.K6.RouteTag == "" {
			l.K6.RouteTag = "name"
		}
		if l.K6.OKTag == "" {
			l.K6.OKTag = "expected_response"
		}
	}
	k := &c.Capture
	def(&k.CPU, 30*time.Second)
	def(&k.Trace, 5*time.Second)
	def(&k.SampleInterval, 5*time.Second)
	def(&k.SnapshotInterval, 60*time.Second)
	if min := l.Warmup + k.CPU + k.Trace + 10*time.Second; l.Duration < min {
		l.Duration = min
	}
	ch := &c.Checks
	deff(&ch.MaxErrorRate, 0.01)
	deff(&ch.RequestTolerance, 0.05)
	deff(&ch.CPUAgreement, 0.10)
	deff(&ch.MaxCV, 0.05)
	deff(&ch.MaxLateRatio, 0.01)
	r := &c.Redline
	deff(&r.Start, 50)
	deff(&r.Max, 1e6)
	deff(&r.Factor, 2)
	def(&r.Step, 30*time.Second)
	def(&r.Warmup, 10*time.Second)
	def(&r.P99, 500*time.Millisecond)
	deff(&r.MaxErrorRate, 0.01)
	deff(&r.MinAchieved, 0.95)
	if r.Refine == 0 {
		r.Refine = 2
	}
}

func (c *Config) Validate() error {
	if len(c.Processes) == 0 {
		return fmt.Errorf("no processes")
	}
	seen := map[string]bool{}
	for _, p := range c.Processes {
		if p.Name == "" || !regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`).MatchString(p.Name) {
			return fmt.Errorf("process name %q: use letters, digits, '-', '_' or '.'", p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate process %q", p.Name)
		}
		seen[p.Name] = true
		if p.Debug == "" {
			return fmt.Errorf("process %s: debug address is required (host:port of its pprof server)", p.Name)
		}
		if (p.Docker == nil) == (len(p.Command) == 0) {
			return fmt.Errorf("process %s: set exactly one of docker or command", p.Name)
		}
		if p.Docker != nil && p.Docker.Image == "" {
			return fmt.Errorf("process %s: docker.image is required", p.Name)
		}
		if p.Metrics != "dyno" && p.Metrics != "prometheus" {
			return fmt.Errorf("process %s: metrics must be dyno or prometheus", p.Name)
		}
		if p.Metrics == "prometheus" && p.Prometheus.URL == "" {
			return fmt.Errorf("process %s: prometheus.url is required", p.Name)
		}
	}
	if c.Load.Rate <= 0 {
		return fmt.Errorf("load.rate must be > 0")
	}
	if c.Load.K6 == nil && len(c.Load.Routes) == 0 {
		return fmt.Errorf("load: set routes or k6")
	}
	if c.Load.K6 == nil && c.Load.BaseURL == "" {
		return fmt.Errorf("load.base_url is required")
	}
	for _, f := range c.Focus {
		if _, err := regexp.Compile(f.Regex); err != nil {
			return fmt.Errorf("focus %q: %w", f.Label, err)
		}
	}
	return nil
}

// Path resolves p against the config directory.
func (c *Config) Path(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

// Entry returns the process that receives the load.
func (c *Config) Entry() Process {
	for _, p := range c.Processes {
		if p.Entry {
			return p
		}
	}
	return c.Processes[0]
}

// Example is a commented starter config, printed by `go-dyno init`.
const Example = `# go-dyno config. Paths are relative to this file.
name: my-service
scenario: baseline        # run ids become <scenario>-r<N>
reps: 5                   # repeat each scenario at least 5 times
cpus: 2                   # pod CPU limit: docker --cpus and GOMAXPROCS

processes:
  - name: api
    debug: localhost:6060          # pprof + dyno stats (DYNO_ADDR inside the process)
    ready: http://localhost:8080/healthz
    docker:
      build: .                     # optional: build the image before the first run
      image: my-service:dyno
      ports: ["8080:8080", "6060:6060"]
    env:
      LOG_LEVEL: info              # match production settings that change the profile
    # command: ["./bin/my-service"]  # alternative to docker (Linux hosts)
    # metrics: prometheus            # if the service doesn't import the dyno package
    # prometheus: {url: http://localhost:9090/metrics, requests: http_requests_total}

load:
  base_url: http://localhost:8080
  rate: 200                        # requests per second, open loop
  warmup: 60s                      # excluded from every statistic
  # duration: 10m                  # longer runs for leak hunting; default is just enough
  routes:
    - {name: list, path: /items, weight: 3}
    - {name: create, method: POST, path: /items, body_random_bytes: 1024,
       headers: {Content-Type: application/octet-stream}}

capture:
  cpu: 30s
  trace: 5s
  contention: false                # mutex/block profiles; off for CPU baselines

checks:
  max_error_rate: 0.01
  request_tolerance: 0.05          # requests in the CPU window vs target
  cpu_agreement: 0.10              # pprof CPU vs OS CPU
  max_cv: 0.05                     # run-to-run spread of CPU per request

focus:                             # code paths to measure as a share of CPU
  - {process: api, label: json, regex: "encoding/json"}

hooks:
  before_rep: ""                   # e.g. clear stale service-discovery entries

redline:                           # go-dyno redline
  start: 50
  factor: 2
  step: 30s
  p99: 250ms
`
