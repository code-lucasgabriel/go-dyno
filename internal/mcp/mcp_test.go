package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestHandshake(t *testing.T) {
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"dyno_summary","arguments":{"results":"/does/not/exist"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"nope"}`,
	}, "\n") + "\n"
	pr, pw := io.Pipe()
	go func() { Serve(context.Background(), strings.NewReader(in), pw, "test"); pw.Close() }()
	got := map[string]map[string]any{}
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(m["id"])
		got[string(b)] = m
	}
	if len(got) != 4 {
		t.Fatalf("want 4 responses (notification gets none), got %d", len(got))
	}
	if v := got["1"]["result"].(map[string]any)["protocolVersion"]; v != "2025-06-18" {
		t.Errorf("protocolVersion = %v", v)
	}
	tools := got["2"]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 6 {
		t.Errorf("tools = %d", len(tools))
	}
	if got["3"]["result"].(map[string]any)["isError"] != true {
		t.Error("summary of a missing folder should be a tool error")
	}
	if got["4"]["error"] == nil {
		t.Error("unknown method should be a protocol error")
	}
}
