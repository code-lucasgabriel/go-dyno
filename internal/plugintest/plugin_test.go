// Package plugintest guards the Claude Code plugin: manifests, the MCP
// launcher, and the skill's references to commands, config keys and files.
// The skill is prose an agent follows; these tests catch it drifting from the
// tool it drives.
package plugintest

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/code-lucasgabriel/go-dyno/internal/config"
	"github.com/code-lucasgabriel/go-dyno/internal/mcp"
)

const root = "../.."

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, p))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readJSON(t *testing.T, p string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(read(t, p)), v); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
}

type manifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func TestManifestsAgree(t *testing.T) {
	var p manifest
	readJSON(t, ".claude-plugin/plugin.json", &p)
	var m struct {
		Name    string `json:"name"`
		Plugins []struct {
			manifest
			Source string `json:"source"`
		} `json:"plugins"`
	}
	readJSON(t, ".claude-plugin/marketplace.json", &m)
	if p.Name != "go-dyno" || p.Version == "" {
		t.Errorf("plugin.json: name %q version %q", p.Name, p.Version)
	}
	if len(m.Plugins) != 1 {
		t.Fatalf("marketplace lists %d plugins, want 1", len(m.Plugins))
	}
	e := m.Plugins[0]
	if e.Name != p.Name || e.Version != p.Version {
		t.Errorf("marketplace entry %s@%s, plugin.json %s@%s: bump both together", e.Name, e.Version, p.Name, p.Version)
	}
	if e.Source != "./" {
		t.Errorf("marketplace source %q: the repo root is the plugin", e.Source)
	}
	if !strings.Contains(read(t, "README.md"), "/plugin install "+p.Name+" --marketplace code-lucasgabriel/go-dyno") {
		t.Error("README lost the one-line plugin install")
	}
}

func TestMCPConfigPointsAtLauncher(t *testing.T) {
	var c struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	readJSON(t, ".mcp.json", &c)
	s, ok := c.MCPServers["go-dyno"]
	if !ok {
		t.Fatal(".mcp.json has no go-dyno server")
	}
	if s.Command != "${CLAUDE_PLUGIN_ROOT}/scripts/go-dyno" || !reflect.DeepEqual(s.Args, []string{"mcp"}) {
		t.Errorf("server = %s %v", s.Command, s.Args)
	}
	st, err := os.Stat(filepath.Join(root, "scripts/go-dyno"))
	if err != nil || st.Mode()&0o111 == 0 {
		t.Errorf("launcher missing or not executable: %v", err)
	}
}

// TestLauncher builds through the launcher into an empty cache and checks
// that stdout carries only the command's output, for a plain command and for
// the MCP handshake. Build chatter on stdout would corrupt the MCP stream.
func TestLauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell launcher")
	}
	if testing.Short() {
		t.Skip("builds the binary")
	}
	cache := t.TempDir()
	run := func(stdin string, args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(filepath.Join(root, "scripts/go-dyno"), args...)
		cmd.Env = append(os.Environ(), "XDG_CACHE_HOME="+cache)
		cmd.Stdin = strings.NewReader(stdin)
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, errb.String())
		}
		return out.String(), errb.String()
	}
	out, errOut := run("", "version")
	if !regexp.MustCompile(`^go-dyno \S+\n$`).MatchString(out) {
		t.Errorf("first run stdout = %q", out)
	}
	if !strings.Contains(errOut, "building") {
		t.Errorf("first run should build, stderr = %q", errOut)
	}
	if _, errOut = run("", "version"); errOut != "" {
		t.Errorf("second run should reuse the cached build, stderr = %q", errOut)
	}
	out, _ = run(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`+"\n", "mcp")
	var resp struct {
		Result struct {
			ServerInfo struct{ Name string } `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(out, "\n", 2)[0]), &resp); err != nil || resp.Result.ServerInfo.Name != "go-dyno" {
		t.Errorf("mcp stdout is not a clean JSON-RPC response: %q (%v)", out, err)
	}
}

func skill(t *testing.T) (front map[string]string, body string) {
	s := read(t, "skills/audit/SKILL.md")
	parts := strings.SplitN(s, "\n---\n", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "---\n") {
		t.Fatal("SKILL.md: missing frontmatter")
	}
	front = map[string]string{}
	for _, l := range strings.Split(strings.TrimPrefix(parts[0], "---\n"), "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok {
			front[k] = strings.Trim(strings.TrimSpace(v), "'\"")
		}
	}
	return front, parts[1]
}

func TestSkillFrontmatter(t *testing.T) {
	f, body := skill(t)
	if f["name"] != "audit" {
		t.Errorf("name = %q: /go-dyno:audit is documented in the README", f["name"])
	}
	if len(f["description"]) < 100 || f["argument-hint"] == "" {
		t.Errorf("description or argument-hint missing: %v", f)
	}
	if !strings.Contains(body, "$ARGUMENTS") {
		t.Error("the skill no longer reads the user's request")
	}
}

// cliCommands reads the subcommands from the CLI's switch.
func cliCommands(t *testing.T) map[string]bool {
	src := read(t, "cmd/go-dyno/main.go")
	cmds := map[string]bool{}
	for _, m := range regexp.MustCompile(`case "([a-z]+)"`).FindAllStringSubmatch(src, -1) {
		cmds[m[1]] = true
	}
	return cmds
}

func TestSkillCommandsExist(t *testing.T) {
	cmds := cliCommands(t)
	for _, doc := range []string{"skills/audit/SKILL.md", "README.md"} {
		for _, m := range regexp.MustCompile("`go-dyno ([a-z]+)").FindAllStringSubmatch(read(t, doc), -1) {
			if !cmds[m[1]] {
				t.Errorf("%s mentions `go-dyno %s`, which is not a command", doc, m[1])
			}
		}
	}
}

func TestSkillToolsExist(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
	pr, pw := io.Pipe()
	go func() { mcp.Serve(context.Background(), strings.NewReader(in), pw, "test"); pw.Close() }()
	var resp struct {
		Result struct {
			Tools []struct{ Name string } `json:"tools"`
		} `json:"result"`
	}
	sc := bufio.NewScanner(pr)
	if !sc.Scan() || json.Unmarshal(sc.Bytes(), &resp) != nil {
		t.Fatal("no tools/list response")
	}
	go io.Copy(io.Discard, pr)
	have := map[string]bool{}
	for _, x := range resp.Result.Tools {
		have[x.Name] = true
	}
	for _, doc := range []string{"skills/audit/SKILL.md", "README.md"} {
		for _, m := range regexp.MustCompile("`(dyno_[a-z]+)`").FindAllStringSubmatch(read(t, doc), -1) {
			if !have[m[1]] {
				t.Errorf("%s mentions tool %s, which the MCP server does not offer", doc, m[1])
			}
		}
	}
}

// yamlKeys lists every dotted config key, e.g. load.rate_source.kind.
func yamlKeys(t reflect.Type, prefix string, out map[string]bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t.PkgPath() != reflect.TypeOf(config.Config{}).PkgPath() {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		tag := strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		k := prefix + tag
		out[k] = true
		yamlKeys(t.Field(i).Type, k+".", out)
	}
}

func TestSkillConfigKeysExist(t *testing.T) {
	keys := map[string]bool{}
	yamlKeys(reflect.TypeOf(config.Config{}), "", keys)
	// a process key appears without its list prefix, e.g. `docker.network`
	for k := range keys {
		if rest, ok := strings.CutPrefix(k, "processes."); ok {
			keys[rest] = true
		}
	}
	top := []string{}
	for k := range keys {
		if !strings.Contains(k, ".") {
			top = append(top, k)
		}
	}
	sort.Strings(top)
	re := regexp.MustCompile("`((?:" + strings.Join(top, "|") + `)\.[a-z0-9_.]+)` + "[`:= ]")
	_, body := skill(t)
	n := 0
	for _, doc := range []string{body, read(t, "README.md")} {
		for _, m := range re.FindAllStringSubmatch(doc, -1) {
			if regexp.MustCompile(`\.(json|md|csv|yaml|gz|html|out)$`).MatchString(m[1]) {
				continue // a file name, e.g. checks.json
			}
			n++
			if !keys[m[1]] {
				t.Errorf("docs mention config key %s, which does not exist", m[1])
			}
		}
	}
	if n < 5 {
		t.Errorf("found only %d config keys in the docs; the pattern is broken", n)
	}
}

func TestSkillLinksExist(t *testing.T) {
	_, body := skill(t)
	for _, m := range regexp.MustCompile("`(references/[a-z0-9-]+\\.md)`").FindAllStringSubmatch(body, -1) {
		if _, err := os.Stat(filepath.Join(root, "skills/audit", m[1])); err != nil {
			t.Errorf("skill links %s: %v", m[1], err)
		}
	}
	for _, m := range regexp.MustCompile(`\]\(([^)#:]+)\)`).FindAllStringSubmatch(read(t, "README.md"), -1) {
		if _, err := os.Stat(filepath.Join(root, m[1])); err != nil {
			t.Errorf("README links %s: %v", m[1], err)
		}
	}
}
