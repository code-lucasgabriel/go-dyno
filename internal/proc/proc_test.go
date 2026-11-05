package proc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/code-lucasgabriel/go-dyno/internal/config"
)

// Reap removes containers whose go-dyno owner has exited and keeps the rest.
func TestReap(t *testing.T) {
	if testing.Short() {
		t.Skip("needs docker")
	}
	img := "alpine:3.23"
	if exec.Command("docker", "image", "inspect", img).Run() != nil {
		t.Skip("docker or the image unavailable")
	}
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	start := func(name, own string) {
		exec.Command("docker", "rm", "-f", name).Run()
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
		if out, err := exec.Command("docker", "run", "-d", "--name", name, "--label", ownerLabel+"="+own, img, "sleep", "60").CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	start("dyno-reap-orphan", fmt.Sprintf("%s:%d", host, dead.Process.Pid))
	start("dyno-reap-live", owner())
	cfg := &config.Config{Processes: []config.Process{{Name: "x", Docker: &config.Docker{Image: img}}}}
	Reap(context.Background(), cfg, t.Logf)
	out, _ := exec.Command("docker", "ps", "-a", "--format", "{{.Names}}").Output()
	if strings.Contains(string(out), "dyno-reap-orphan") {
		t.Error("orphan container not removed")
	}
	if !strings.Contains(string(out), "dyno-reap-live") {
		t.Error("container of a running go-dyno was removed")
	}
}
