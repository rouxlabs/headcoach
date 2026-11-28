// Package client talks to the daemon over its JSON API.
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rouxlabs/headcoach/internal/api"
	"github.com/rouxlabs/headcoach/internal/limits"
)

// Client is a daemon address.
type Client struct {
	URL string
}

// NoAnswer is the error of a request the daemon did not answer in time. Unlike a daemon that is not
// there, it may have done what was asked.
type NoAnswer struct {
	URL   string
	After time.Duration
}

func (e NoAnswer) Error() string {
	return fmt.Sprintf("The daemon at %s did not answer within %s", e.URL, e.After)
}

// Caller is what tells the daemon who runs this command: the Claude Code session and the task an
// agent runs it from. A command you type outside a session sends none, and counts as yours.
func Caller() map[string]string {
	headers := map[string]string{}
	for name, env := range map[string]string{"X-HC-Session": "CLAUDE_CODE_SESSION_ID", "X-HC-Task": "HC_TASK"} {
		if value := os.Getenv(env); value != "" {
			headers[name] = value
		}
	}
	if os.Getenv("CLAUDECODE") != "" {
		headers["X-HC-Agent"] = "1"
	}
	return headers
}

// New returns a client for the daemon at url.
func New(url string) *Client { return &Client{URL: strings.TrimRight(url, "/")} }

// CallFor sends one request and decodes the answer into out, which may be nil. in is sent as JSON
// unless it is nil. The error of a refused request is the sentence the daemon gave.
func (c *Client) CallFor(timeout time.Duration, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.URL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range Caller() {
		req.Header.Set(name, value)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		if ne := net.Error(nil); errors.As(err, &ne) && ne.Timeout() {
			return NoAnswer{c.URL, timeout}
		}
		return fmt.Errorf("No daemon at %s. Start it with: hc service install, or hc serve", c.URL)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var refused struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &refused) == nil && refused.Error != "" {
			return fmt.Errorf("%s", refused.Error)
		}
		return fmt.Errorf("The daemon answered %d", resp.StatusCode)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Call is CallFor with the usual timeout.
func (c *Client) Call(method, path string, in, out any) error {
	return c.CallFor(10*time.Second, method, path, in, out)
}

// Board returns every task and the usage limits.
func (c *Client) Board() (api.Tasks, error) {
	var out api.Tasks
	err := c.Call("GET", "/api/tasks", nil, &out)
	return out, err
}

// Limits returns the usage limits.
func (c *Client) Limits() ([]limits.Entry, error) {
	var out struct {
		Limits []limits.Entry `json:"limits"`
	}
	err := c.Call("GET", "/api/limits", nil, &out)
	return out.Limits, err
}

// Task returns one task with its detail. id may be a unique prefix.
func (c *Client) Task(id string) (api.Task, error) {
	var out api.Task
	err := c.Call("GET", "/api/tasks/"+id, nil, &out)
	return out, err
}

// Meta tells what the daemon is and what it can do.
func (c *Client) Meta() (api.Meta, error) {
	var out api.Meta
	err := c.Call("GET", "/api/meta", nil, &out)
	return out, err
}

// Do runs an action on a task and returns the task as it is afterwards.
func (c *Client) Do(method, path string, in any) (api.Task, error) {
	var out api.Task
	err := c.Call(method, path, in, &out)
	return out, err
}

// Pin pins a task to the top of the board, or unpins it. A daemon too old to know pins ignores the
// request: that is an error, not a pin that never happens.
func (c *Client) Pin(id string, pin bool) (api.Task, error) {
	t, err := c.Do("PATCH", "/api/tasks/"+id, map[string]bool{"pinned": pin})
	if err == nil && t.Pinned != pin {
		err = errors.New("The daemon runs an older hc that cannot pin: restart it with hc restart")
	}
	return t, err
}

// Enter returns the command that takes this terminal into a task's session in the board's tmux,
// starting the session if needed. It notes the task as seen, for the switcher of recent tasks.
//
// It is empty when the session lives somewhere else: in your own tmux, or nowhere yet with
// private_tmux off.
func (c *Client) Enter(t api.Task) ([]string, error) {
	if !t.Live {
		meta, err := c.Meta()
		if err != nil {
			return nil, err
		}
		if meta.PrivateTmux {
			if t, err = c.Do("POST", "/api/tasks/"+t.ID+"/start", nil); err != nil {
				return nil, err
			}
		}
	}
	_, _ = c.Do("PATCH", "/api/tasks/"+t.ID, map[string]bool{"seen": true}) // not worth keeping you out for
	return t.Attach, nil
}
