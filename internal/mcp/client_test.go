package mcp

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStdioMCPRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A tiny JSON-RPC MCP test server embedded in sh/python keeps this package standalone.
	server := `import sys,json
for line in sys.stdin:
 r=json.loads(line); m=r.get("method"); p=r.get("params",{})
 if m=="initialize": out={"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"test"}}
 elif m=="tools/list": out={"tools":[{"name":"echo","description":"Echo text","inputSchema":{"type":"object","properties":{"text":{"type":"string"}}}}]}
 elif m=="tools/call": out={"content":[{"type":"text","text":p.get("arguments",{}).get("text","")}]}
 else: out={}
 print(json.dumps({"jsonrpc":"2.0","id":r.get("id"),"result":out}),flush=True)`
	c, err := StartStdio(ctx, "python3", []string{"-c", server}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	tools, err := c.ListTools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools=%v err=%v", tools, err)
	}
	got, err := c.CallTool(ctx, "echo", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content[0]["text"] != "hello" {
		t.Fatalf("result=%v", got)
	}
}

func pythonTestCommand() string {
	if runtime.GOOS == "windows" {
		return "python"
	}
	return "python3"
}

func TestStdioMCPReaderExitFailsPendingRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	server := `import sys
sys.stdin.readline()
sys.exit(0)`
	client, err := StartStdio(ctx, pythonTestCommand(), []string{"-c", server}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	started := time.Now()
	err = client.Initialize(ctx)
	if err == nil {
		t.Fatal("Initialize succeeded after the MCP subprocess exited")
	}
	if !strings.Contains(err.Error(), "MCP stdio reader stopped") {
		t.Fatalf("Initialize error=%q, want MCP stdio reader stopped", err)
	}
	if elapsed := time.Since(started); elapsed >= 4*time.Second {
		t.Fatalf("pending request was not failed promptly; elapsed=%v", elapsed)
	}
}

func TestStdioMCPRejectsOversizedResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server := `import json,sys
r=json.loads(sys.stdin.readline())
out={"jsonrpc":"2.0","id":r.get("id"),"result":{"padding":"x"*(9*1024*1024)}}
print(json.dumps(out),flush=True)`
	client, err := StartStdio(ctx, pythonTestCommand(), []string{"-c", server}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	err = client.Initialize(ctx)
	if err == nil {
		t.Fatal("Initialize accepted an MCP response larger than the configured limit")
	}
	if !strings.Contains(err.Error(), "MCP stdio reader stopped") {
		t.Fatalf("Initialize error=%q, want MCP stdio reader stopped", err)
	}
}

var _ = exec.Command
