package conctest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// mcpClient drives one `skald mcp` process over its pipes. Requests are sent
// one at a time: the server answers concurrently, so pipelining would reorder
// the responses.
type mcpClient struct {
	h     *harness
	p     *proc
	in    io.Writer
	out   *bufio.Scanner
	next  int
	mu    sync.Mutex
	lines int
}

func (h *harness) newMCP(label string) (*mcpClient, error) {
	p := h.start(label, "", true, "mcp")
	c := &mcpClient{h: h, p: p, in: p.stdin, out: bufio.NewScanner(p.stdout)}
	c.out.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	_, err := c.rpc("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": label, "version": "1"},
	})
	if err != nil {
		return nil, err
	}
	if err := c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *mcpClient) send(msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = c.in.Write(append(b, '\n'))
	return err
}

// rpc sends a request and waits for the response with the same id. Every line
// the server prints must be JSON, which also checks that stdout carries only
// protocol messages.
func (c *mcpClient) rpc(method string, params any) (json.RawMessage, error) {
	c.next++
	id := c.next
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	type result struct {
		raw json.RawMessage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		for c.out.Scan() {
			var m struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(c.out.Bytes(), &m); err != nil {
				ch <- result{err: fmt.Errorf("non-JSON on stdout: %q", c.out.Text())}
				return
			}
			c.mu.Lock()
			c.lines++
			c.mu.Unlock()
			if m.ID == nil || *m.ID != id {
				continue
			}
			if m.Error != nil {
				ch <- result{err: fmt.Errorf("%s: %s", method, m.Error.Message)}
				return
			}
			ch <- result{raw: m.Result}
			return
		}
		ch <- result{err: fmt.Errorf("%s: stdout closed (%v)", method, c.out.Err())}
	}()
	select {
	case r := <-ch:
		return r.raw, r.err
	case <-time.After(45 * time.Second):
		return nil, fmt.Errorf("%s: no response within 45s\nstderr: %s", method, c.p.stderr.String())
	}
}

// call invokes a tool and returns its text. A tool error is an error here.
func (c *mcpClient) call(tool string, args map[string]any) (string, error) {
	raw, err := c.rpc("tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return "", err
	}
	var res struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("%s: bad result %s: %v", tool, raw, err)
	}
	var text strings.Builder
	for _, ct := range res.Content {
		text.WriteString(ct.Text)
	}
	if res.IsError {
		return "", fmt.Errorf("%s failed: %s", tool, text.String())
	}
	return text.String(), nil
}

// close ends the session: closing stdin makes the server exit cleanly.
func (c *mcpClient) close() {
	if closer, ok := c.in.(io.Closer); ok {
		_ = closer.Close()
	}
	c.h.finish(c.p, 15*time.Second)
}
