package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

const maxStdioMessageBytes = 8 << 20

type stdioResponse struct {
	raw json.RawMessage
	err error
}

// StdioClient 通过子进程 stdin/stdout 交换 JSON-RPC 消息，用于测试与轻量桥接。
type StdioClient struct {
	cmd         *exec.Cmd
	stdin       interface{ Write([]byte) (int, error) }
	stdout      *bufio.Scanner
	mu          sync.Mutex
	writeMu     sync.Mutex
	pending     map[int64]chan stdioResponse
	nextID      int64
	done        chan struct{}
	terminalErr error
}

// StartStdio 启动一个子进程作为 MCP JSON-RPC 服务器并返回客户端。
// opts 保留用于未来的启动选项，当前忽略。
func StartStdio(ctx context.Context, command string, args []string, opts any) (*StdioClient, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &StdioClient{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewScanner(stdout),
		pending: map[int64]chan stdioResponse{},
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *StdioClient) readLoop() {
	c.stdout.Buffer(make([]byte, 64*1024), maxStdioMessageBytes)
	for c.stdout.Scan() {
		line := c.stdout.Bytes()
		if len(line) == 0 {
			continue
		}
		var meta struct {
			ID *int64 `json:"id"`
		}
		if err := json.Unmarshal(line, &meta); err != nil || meta.ID == nil {
			continue
		}
		c.mu.Lock()
		ch := c.pending[*meta.ID]
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- stdioResponse{raw: append(json.RawMessage(nil), line...)}:
			default:
			}
		}
	}

	err := c.stdout.Err()
	if err == nil {
		err = io.EOF
	}
	err = fmt.Errorf("MCP stdio reader stopped: %w", err)

	c.mu.Lock()
	c.terminalErr = err
	pending := make([]chan stdioResponse, 0, len(c.pending))
	for _, ch := range c.pending {
		pending = append(pending, ch)
	}
	c.mu.Unlock()

	for _, ch := range pending {
		select {
		case ch <- stdioResponse{err: err}:
		default:
		}
	}
	close(c.done)
}

func (c *StdioClient) call(ctx context.Context, method string, id int64, params any) (json.RawMessage, error) {
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')

	c.mu.Lock()
	if c.terminalErr != nil {
		err := c.terminalErr
		c.mu.Unlock()
		return nil, err
	}
	ch := make(chan stdioResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	c.writeMu.Lock()
	written := 0
	for written < len(b) {
		n, writeErr := c.stdin.Write(b[written:])
		if writeErr != nil {
			c.writeMu.Unlock()
			return nil, writeErr
		}
		if n <= 0 {
			c.writeMu.Unlock()
			return nil, io.ErrShortWrite
		}
		written += n
	}
	c.writeMu.Unlock()

	select {
	case response := <-ch:
		if response.err != nil {
			return nil, response.err
		}
		return response.raw, nil
	case <-c.done:
		c.mu.Lock()
		err := c.terminalErr
		c.mu.Unlock()
		if err == nil {
			err = errors.New("MCP stdio reader stopped")
		}
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Initialize 发送初始化握手。
func (c *StdioClient) Initialize(ctx context.Context) error {
	_, err := c.sendRequest(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "m365-copilot2api-mcp-stdio", "version": "0.1.0"},
	})
	return err
}

func (c *StdioClient) sendRequest(ctx context.Context, method string, params any) (map[string]any, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	raw, err := c.call(ctx, method, id, params)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if e, ok := obj["error"]; ok && e != nil {
		return nil, fmt.Errorf("rpc error: %v", e)
	}
	return obj, nil
}

// ListTools 列出子进程提供的工具。
func (c *StdioClient) ListTools(ctx context.Context) ([]Tool, error) {
	obj, err := c.sendRequest(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	result, _ := json.Marshal(obj["result"])
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(result, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// CallTool 调用子进程提供的工具。
func (c *StdioClient) CallTool(ctx context.Context, name string, arguments map[string]any) (CallResult, error) {
	var result CallResult
	obj, err := c.sendRequest(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		return result, err
	}
	res, _ := json.Marshal(obj["result"])
	if err := json.Unmarshal(res, &result); err != nil {
		return result, err
	}
	return result, nil
}

// Close 终止子进程。
func (c *StdioClient) Close() error {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return c.cmd.Wait()
}
