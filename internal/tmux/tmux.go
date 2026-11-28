// Package tmux is everything that touches tmux. The board talks to live sessions through their pane.
package tmux

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	detachKey = "C-]"
	// DetachHint is shown when a terminal enters a session of the board's own tmux server.
	DetachHint = "Ctrl-] returns to where you came from, Ctrl-\\ lists the recent tasks, Ctrl-^ goes to the last one"
	// MainSessionHint replaces DetachHint when a sub-agent's row was entered: it has no terminal of its own.
	MainSessionHint = "Sub-agents run inside this main session. " + DetachHint
	// privateBase is the start of the board's own tmux config: no status bar, no prefix key, one key to
	// leave, and a mouse wheel that scrolls from its first click, where tmux's own only enters copy mode
	// with it. It should feel like plain Claude.
	privateBase = `set -g status off
set -g prefix None
unbind-key -a
bind-key -n ` + detachKey + ` detach-client
set -g mouse on
bind-key -n WheelUpPane if-shell -F '#{||:#{alternate_on},#{pane_in_mode},#{mouse_any_flag}}' { send-keys -M } { copy-mode -e; send-keys -X -N 5 scroll-up }
set -g history-limit 50000
set -g default-size 200x50
set -g focus-events on
set -s escape-time 0
set -s extended-keys on
`
	// switcher opens the switcher of recent tasks in a box over the session. It notes in HC_CLIENT the
	// terminal the key came from, the one to hand over. /bin/sh runs it whatever your shell, and finds hc
	// in HC_BIN, so that no path goes through tmux's quoting. A failure stays on screen until a key.
	switcher = ` set-environment -F HC_CLIENT '#{client_name}' \; display-popup -EE -w 80% -h 60% /bin/sh -c '"${HC_BIN:-hc}" switch --popup'` + "\n"
	// jump goes straight back to the task you were in before, without the switcher: run-shell runs it
	// with /bin/sh whatever your shell, and with the session's environment, as the popup does.
	jump = ` set-environment -F HC_CLIENT '#{client_name}' \; run-shell -b '"${HC_BIN:-hc}" switch --last'` + "\n"
)

// privateConf configures the board's own tmux server: privateBase, then, when popup says this tmux
// has popups enough for it, Ctrl-\ to switch to a recent task, and Ctrl-^ to go straight back to the
// last one, Ctrl-Tab too where the terminal tells it from Tab. bin is the hc the switcher runs; empty, or a path tmux cannot quote, leaves it to
// the PATH.
func privateConf(bin string, popup bool) string {
	conf := privateBase
	if !popup {
		return conf
	}
	if bin != "" && !strings.ContainsAny(bin, "'\n") {
		conf += "set-environment -g HC_BIN '" + bin + "'\n"
	} else {
		conf += "set-environment -gu HC_BIN\n" // not the one an earlier config left on a running server
	}
	return conf + `bind-key -n 'C-\'` + switcher + "bind-key -n 'C-^'" + jump + "bind-key -n C-Tab" + jump
}

var tmuxVersion = regexp.MustCompile(`(\d+)\.(\d+)`)

// popups tells whether the tmux of tmux -V runs the switcher: 3.2 or later. An older one has no popup,
// and its key would only add an error to the config. A build that names no version is taken as recent.
func popups(v string) bool {
	m := tmuxVersion.FindStringSubmatch(v)
	if m == nil {
		return true
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major > 3 || major == 3 && minor >= 2
}

// keys are the only keys SendKeys presses: enough to answer a dialog, not enough to type a command.
var keys = map[string]bool{
	"Enter": true, "Escape": true, "Up": true, "Down": true, "Left": true, "Right": true, "Tab": true,
	"BTab": true, "Space": true, "y": true, "n": true,
	"0": true, "1": true, "2": true, "3": true, "4": true, "5": true, "6": true, "7": true, "8": true, "9": true,
}

// CheckKeys tells whether every key may be pressed by SendKeys.
func CheckKeys(pressed []string) error {
	ok := len(pressed) > 0
	for _, key := range pressed {
		ok = ok && keys[key]
	}
	if ok {
		return nil
	}
	names := make([]string, 0, len(keys))
	for key := range keys {
		names = append(names, key)
	}
	sort.Strings(names)
	return fmt.Errorf("Keys must be chosen from: %s", strings.Join(names, ", "))
}

// Window describes a session to open. Socket and Conf only matter to NewSession, Session to NewWindow.
type Window struct {
	Cwd     string
	Command string
	Name    string
	Env     []string // KEY=value
	Session string   // the tmux session to open the window in; empty means the most recent one
	Socket  string   // the socket of the board's own server
	Conf    string   // the config file of the board's own server
	Bin     string   // the hc binary the switcher key of the board's own server runs
}

type paneCache struct {
	at    time.Time
	known map[string]bool
}

// Client runs tmux commands. The zero value is ready to use.
type Client struct {
	mu    sync.Mutex
	cache map[string]paneCache // by socket
}

// dropped are the variables of the terminal, multiplexer or agent the process that starts the board's
// server runs in. Every task would inherit them from the server, though none of them runs there.
var dropped = map[string]bool{
	"TMUX": true, "TMUX_PANE": true, "STY": true, "WINDOWID": true, "WT_SESSION": true, "WEZTERM_PANE": true,
	"KITTY_WINDOW_ID": true, "TERM_PROGRAM": true, "TERM_PROGRAM_VERSION": true, "TERM_SESSION_ID": true,
	"LC_TERMINAL": true, "LC_TERMINAL_VERSION": true, "GIT_ASKPASS": true, "CLAUDECODE": true, "AI_AGENT": true,
	"CODEX_THREAD_ID": true, "HC_IGNORE": true, "HC_TASK": true, "HC_HANDOFF": true,
	"PWD": true, "OLDPWD": true, "SHLVL": true, "_": true,
}

var droppedPrefixes = []string{"HERDR_", "DIRENV_", "CLAUDE_", "ZELLIJ", "ITERM_", "VSCODE_"}

func drop(name string) bool {
	if dropped[name] {
		return true
	}
	for _, prefix := range droppedPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// cleanEnv is the environment the board's own tmux server starts with: env without the variables of the
// terminal or agent it was started from, and without what direnv exported there.
func cleanEnv(env []string) []string {
	prev, next := direnvDiff(env)
	var out []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if value, ok := prev[name]; ok {
			kv = name + "=" + value
			delete(prev, name)
		} else if _, ok := next[name]; ok {
			continue
		}
		if !drop(name) {
			out = append(out, kv)
		}
	}
	restored := make([]string, 0, len(prev))
	for name, value := range prev {
		if !drop(name) {
			restored = append(restored, name+"="+value)
		}
	}
	sort.Strings(restored)
	return append(out, restored...)
}

// direnvDiff reads what direnv changed, from the DIRENV_DIFF it keeps: the values before and after.
// A shell would undo it on leaving the directory, but tmux starts a task's shell with no direnv hook.
func direnvDiff(env []string) (prev, next map[string]string) {
	for _, kv := range env {
		encoded, ok := strings.CutPrefix(kv, "DIRENV_DIFF=")
		if !ok {
			continue
		}
		raw, err := base64.URLEncoding.DecodeString(encoded)
		if err != nil {
			return nil, nil
		}
		r, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, nil
		}
		var diff struct {
			Prev map[string]string `json:"p"`
			Next map[string]string `json:"n"`
		}
		if json.NewDecoder(r).Decode(&diff) != nil {
			return nil, nil
		}
		return diff.Prev, diff.Next
	}
	return nil, nil
}

// cleanServer takes out of a running server's global environment what cleanEnv leaves out, for a server
// an older hc started with them. A new window gets the global environment.
func cleanServer(socket string) {
	out, err := run(socket, "", "show-environment", "-g")
	if err != nil {
		return
	}
	var global []string
	for _, line := range strings.Split(out, "\n") {
		if line != "" && !strings.HasPrefix(line, "-") && strings.Contains(line, "=") {
			global = append(global, line)
		}
	}
	kept := map[string]string{}
	for _, kv := range cleanEnv(global) {
		name, value, _ := strings.Cut(kv, "=")
		kept[name] = value
	}
	// One command each: tmux reads a trailing ; as the end of a command, so it spoils only its own.
	for _, kv := range global {
		name, value, _ := strings.Cut(kv, "=")
		if now, ok := kept[name]; !ok {
			_, _ = run(socket, "", "set-environment", "-g", "-u", "--", name)
		} else if now != value {
			_, _ = run(socket, "", "set-environment", "-g", "--", name, now)
		}
		delete(kept, name)
	}
	for name, value := range kept {
		_, _ = run(socket, "", "set-environment", "-g", "--", name, value)
	}
}

// run runs one tmux command. socket picks the server; empty means the default one.
func run(socket, stdin string, args ...string) (string, error) {
	if socket != "" {
		args = append([]string{"-S", socket}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", args...)
	if socket != "" {
		// A command that starts the server hands it this environment for good. Without -S, TMUX picks the server.
		cmd.Env = cleanEnv(os.Environ())
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	name := args[0]
	if socket != "" {
		name = args[2]
	}
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return "", errors.New("tmux is not installed")
	case ctx.Err() != nil:
		return "", fmt.Errorf("tmux %s timed out", name)
	case err != nil:
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return "", errors.New(detail)
		}
		return "", fmt.Errorf("tmux %s failed", name)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Installed tells whether the tmux program exists.
func (c *Client) Installed() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// Available tells whether your own tmux server is running.
func (c *Client) Available() bool {
	if !c.Installed() {
		return false
	}
	_, err := run("", "", "list-sessions")
	return err == nil
}

// Panes returns the ids of every pane on one tmux server. Cached for two seconds: the board asks on
// each refresh.
func (c *Client) Panes(socket string) map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.cache[socket]; ok && time.Since(cached.at) < 2*time.Second {
		return cached.known
	}
	known := map[string]bool{}
	if out, err := run(socket, "", "list-panes", "-a", "-F", "#{pane_id}"); err == nil {
		for _, pane := range strings.Fields(out) {
			known[pane] = true
		}
	}
	if c.cache == nil {
		c.cache = map[string]paneCache{}
	}
	c.cache[socket] = paneCache{time.Now(), known}
	return known
}

func (c *Client) forget() {
	c.mu.Lock()
	c.cache = nil
	c.mu.Unlock()
}

// SendText types a message into a pane and submits it. Bracketed paste keeps multi-line text in one message.
func (c *Client) SendText(pane, text, socket string) error {
	buffer := fmt.Sprintf("hc-%d", os.Getpid())
	if _, err := run(socket, text, "load-buffer", "-b", buffer, "-"); err != nil {
		return err
	}
	if _, err := run(socket, "", "paste-buffer", "-d", "-p", "-b", buffer, "-t", pane); err != nil {
		return err
	}
	time.Sleep(250 * time.Millisecond)
	_, err := run(socket, "", "send-keys", "-t", pane, "Enter")
	return err
}

// RenameWindow names the window that holds a pane. A missing pane is not worth failing a hook for.
func (c *Client) RenameWindow(pane, name, socket string) {
	_, _ = run(socket, "", "rename-window", "-t", pane, name)
}

// Capture returns the text a pane is showing right now. Empty when the pane cannot be read.
func (c *Client) Capture(pane, socket string) string {
	out, _ := run(socket, "", "capture-pane", "-p", "-t", pane)
	return out
}

// CaptureStyled is Capture with the colors and styles of the text, as escape sequences.
func (c *Client) CaptureStyled(pane, socket string) string {
	out, _ := run(socket, "", "capture-pane", "-e", "-p", "-t", pane)
	return out
}

// KillPane closes a pane, and its window when it was the only pane. A pane already gone is fine.
func (c *Client) KillPane(pane, socket string) {
	_, _ = run(socket, "", "kill-pane", "-t", pane)
	c.forget()
}

// SendKeys presses navigation keys in a pane, for answering a dialog.
func (c *Client) SendKeys(pane string, pressed []string, socket string) error {
	if err := CheckKeys(pressed); err != nil {
		return err
	}
	for _, key := range pressed {
		if _, err := run(socket, "", "send-keys", "-t", pane, key); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// TypeText types text in a pane as if on the keyboard, without submitting it: tmux reads no key names in it.
// A trailing ; would end the tmux command, and escaping it eats backslashes: it is sent as its byte instead.
func (c *Client) TypeText(pane, text, socket string) error {
	words := strings.TrimRight(text, ";")
	if words != "" {
		if _, err := run(socket, "", "send-keys", "-l", "-t", pane, "--", words); err != nil {
			return err
		}
	}
	for range len(text) - len(words) {
		if _, err := run(socket, "", "send-keys", "-H", "-t", pane, "3b"); err != nil {
			return err
		}
	}
	return nil
}

// Focus brings a pane to the front of whichever tmux client is attached.
func (c *Client) Focus(pane, socket string) error {
	session, err := run(socket, "", "display-message", "-p", "-t", pane, "#{session_name}")
	if err != nil {
		return err
	}
	if _, err := run(socket, "", "select-window", "-t", pane); err != nil {
		return err
	}
	if _, err := run(socket, "", "select-pane", "-t", pane); err != nil {
		return err
	}
	_, _ = run(socket, "", "switch-client", "-t", session) // fails when no client is attached, which is fine
	return nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func (w Window) args() []string {
	var args []string
	if w.Cwd != "" && isDir(w.Cwd) {
		args = append(args, "-c", w.Cwd)
	}
	if w.Session != "" {
		args = append(args, "-t", w.Session+":")
	}
	for _, kv := range w.Env {
		args = append(args, "-e", kv)
	}
	return append(args, w.Command)
}

// NewWindow opens a detached window in your own tmux. It returns the new pane id.
func (c *Client) NewWindow(w Window) (string, error) {
	args := append([]string{"new-window", "-d", "-P", "-F", "#{pane_id}", "-n", w.Name}, w.args()...)
	pane, err := run("", "", args...)
	c.forget()
	return pane, err
}

// Configure writes the config of the board's own server, and has the server read it again when it
// runs and the config changed: an update brings its keys to the sessions already open. bin is the hc
// binary the switcher key runs.
func (c *Client) Configure(socket, conf, bin string) error {
	if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
		return err
	}
	version, _ := run("", "", "-V")
	want := privateConf(bin, popups(version))
	if current, err := os.ReadFile(conf); err == nil && string(current) == want {
		return nil
	}
	if err := os.WriteFile(conf, []byte(want), 0o644); err != nil {
		return err
	}
	if _, err := run(socket, "", "list-sessions"); err == nil {
		_, _ = run(socket, "", "source-file", conf) // the sessions run on anyway, with the old keys
	}
	return nil
}

// NewSession opens a detached session of its own on the board's private server. It returns the new pane id.
//
// One tmux session per task: two terminals can then look at two tasks without following each other.
func (c *Client) NewSession(w Window) (string, error) {
	if err := c.Configure(w.Socket, w.Conf, w.Bin); err != nil {
		return "", err
	}
	taken := map[string]bool{}
	if out, err := run(w.Socket, "", "list-sessions", "-F", "#{session_name}"); err == nil {
		for _, name := range strings.Fields(out) {
			taken[name] = true
		}
	}
	unique := w.Name
	for n := 2; taken[unique]; n++ {
		unique = fmt.Sprintf("%s-%d", w.Name, n)
	}
	cleanServer(w.Socket)
	w.Session = "" // a session of its own, not a window in another
	args := append([]string{"-f", w.Conf, "new-session", "-d", "-P", "-F", "#{pane_id}", "-s", unique, "-n", w.Name},
		w.args()...)
	pane, err := run(w.Socket, "", args...)
	c.forget()
	return pane, err
}

// ReplaceClient runs command in place of a client of the tmux server named in TMUX, from a popup of
// that server: the client's terminal leaves tmux and runs command instead. client is its name, as
// #{client_name} gives it.
func ReplaceClient(client, command string) error {
	_, err := run("", "", "detach-client", "-t", client, "-E", command)
	return err
}

// SwitchClient moves a client of the tmux server named in TMUX to the session of pane, with that pane
// in front: no new client, unlike ReplaceClient.
func SwitchClient(client, pane string) error {
	_, err := run("", "", "switch-client", "-c", client, "-t", pane)
	return err
}

// Tell shows text for a few seconds at the bottom of a client of the tmux server named in TMUX.
func Tell(client, text string) error {
	_, err := run("", "", "display-message", "-c", client, "-d", "3000", strings.ReplaceAll(text, "#", "##"))
	return err
}

// AttachCommand is the command that puts a terminal inside a session of the private server, with the
// way out shown.
func AttachCommand(pane, socket string) []string {
	return append(AttachQuiet(pane, socket), ";", "display-message", "-d", "3000", DetachHint)
}

// AttachQuiet is AttachCommand without the way out shown, for a client that has its own.
func AttachQuiet(pane, socket string) []string {
	return []string{"tmux", "-S", socket, "attach-session", "-t", pane}
}

// AttachEnv is the environment to attach with: without TMUX set, tmux agrees to attach from inside
// another tmux.
func AttachEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TMUX=") {
			env = append(env, kv)
		}
	}
	return env
}

// QuietDetach passes what tmux prints on stdout to w, without the "[detached ...]" line it prints
// when a client leaves. Other exit reasons, such as "[lost tty]", still show.
func QuietDetach(w io.Writer) io.Writer { return &quietDetach{w: w} }

type quietDetach struct {
	w    io.Writer
	line []byte
}

func (q *quietDetach) Write(p []byte) (int, error) {
	q.line = append(q.line, p...)
	for {
		i := bytes.IndexByte(q.line, '\n')
		if i < 0 {
			break
		}
		line := q.line[:i+1]
		// The message follows the codes that restore the terminal, on the same line: keep those.
		if at := bytes.Index(line, []byte("[detached")); at >= 0 {
			line = line[:at]
		}
		if _, err := q.w.Write(line); err != nil {
			return len(p), err
		}
		q.line = q.line[i+1:]
	}
	return len(p), nil
}
