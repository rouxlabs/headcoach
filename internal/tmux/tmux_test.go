package tmux

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAttachCommandShowsTheWayOutAndAttachQuietDoesNot(t *testing.T) {
	quiet := []string{"tmux", "-S", "/tmp/s", "attach-session", "-t", "%1"}
	if got := AttachQuiet("%1", "/tmp/s"); !reflect.DeepEqual(got, quiet) {
		t.Errorf("AttachQuiet = %v, want %v", got, quiet)
	}
	want := append(quiet, ";", "display-message", "-d", "3000", DetachHint)
	if got := AttachCommand("%1", "/tmp/s"); !reflect.DeepEqual(got, want) {
		t.Errorf("AttachCommand = %v, want %v", got, want)
	}
}

func TestQuietDetach(t *testing.T) {
	var out bytes.Buffer
	w := QuietDetach(&out)
	for _, chunk := range []string{"\x1b[?1049l[deta", "ched (from session 0123abcd-demo)]\n", "[lost tty]\n", "[detached]\n", "[exited]\n"} {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	if got, want := out.String(), "\x1b[?1049l[lost tty]\n[exited]\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTypeTextTypesWhatTmuxWouldReadAsFlagsOrSeparators(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	dir, err := os.MkdirTemp("/tmp", "hc") // a socket path has a short limit
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket, out := filepath.Join(dir, "s"), filepath.Join(dir, "out")
	if _, err := run(socket, "", "new-session", "-d", "-s", "t", "cat > "+out); err != nil {
		t.Fatal(err)
	}
	defer run(socket, "", "kill-server")
	c := &Client{}
	for _, text := range []string{"-1 is fine", " -H", "both;", ";", "a;b", `C:\tmp\;;`} {
		if err := c.TypeText("t", text, socket); err != nil {
			t.Fatalf("TypeText(%q): %v", text, err)
		}
	}
	if _, err := run(socket, "", "send-keys", "-t", "t", "Enter", "C-d"); err != nil {
		t.Fatal(err)
	}
	want := "-1 is fine -Hboth;;a;bC:\\tmp\\;;\n"
	for i := 0; i < 50; i++ {
		if got, _ := os.ReadFile(out); string(got) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := os.ReadFile(out)
	t.Errorf("typed %q, want %q", got, want)
}

func direnvDiffOf(t *testing.T, prev, next map[string]string) string {
	t.Helper()
	data, err := json.Marshal(map[string]map[string]string{"p": prev, "n": next})
	if err != nil {
		t.Fatal(err)
	}
	var packed bytes.Buffer
	w := zlib.NewWriter(&packed)
	w.Write(data)
	w.Close()
	return "DIRENV_DIFF=" + base64.URLEncoding.EncodeToString(packed.Bytes())
}

func TestCleanLeavesOutTheTerminalAndAgentItRunsIn(t *testing.T) {
	diff := direnvDiffOf(t, map[string]string{"PATH": "/usr/bin:/bin", "GONE": "back"},
		map[string]string{"PATH": "/w/repo/bin:/usr/bin:/bin", "CLOUDSDK_ACTIVE_CONFIG_NAME": "repo",
			"DIRENV_DIR": "-/w/repo"})
	env := []string{
		"PATH=/w/repo/bin:/usr/bin:/bin", "HOME=/Users/x", "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8",
		"SSH_AUTH_SOCK=/tmp/agent", "ANTHROPIC_API_KEY=k", "HC_URL=http://127.0.0.1:7337", "COLORTERM=truecolor",
		"CLOUDSDK_ACTIVE_CONFIG_NAME=repo", "TMUX=/tmp/tmux-1/default,1,0", "TMUX_PANE=%3",
		"HERDR_PANE_ID=wB:p3", "HERDR_SOCKET_PATH=/run/herdr.sock", "HERDR_ENV=1", "DIRENV_DIR=-/w/repo",
		"DIRENV_FILE=/w/repo/.envrc", diff, "CLAUDECODE=1", "CLAUDE_CODE_SSE_PORT=1234",
		"CLAUDE_CONFIG_DIR=/Users/x/.claude-work", "AI_AGENT=claude-code", "HC_IGNORE=1", "HC_TASK=1a2b3c4d",
		"TERM_SESSION_ID=w0t0p0", "VSCODE_GIT_IPC_HANDLE=/tmp/vscode", "GIT_ASKPASS=/vscode/askpass.sh",
		"PWD=/w/repo", "SHLVL=3",
	}
	want := []string{
		"PATH=/usr/bin:/bin", "HOME=/Users/x", "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8", "SSH_AUTH_SOCK=/tmp/agent",
		"ANTHROPIC_API_KEY=k", "HC_URL=http://127.0.0.1:7337", "COLORTERM=truecolor", "GONE=back",
	}
	if got := cleanEnv(env); !reflect.DeepEqual(got, want) {
		t.Errorf("cleanEnv =\n%v\nwant\n%v", got, want)
	}
	// A DIRENV_DIFF it cannot read undoes nothing.
	if got := cleanEnv([]string{"A=1", "DIRENV_DIFF=%%%"}); !reflect.DeepEqual(got, []string{"A=1"}) {
		t.Errorf("cleanEnv = %v", got)
	}
}

// privateSocket is a socket for a server of the test's own, killed when the test ends.
func privateSocket(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	dir, err := os.MkdirTemp("/tmp", "hc") // a socket path has a short limit
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "s")
	t.Cleanup(func() {
		run(socket, "", "kill-server")
		os.RemoveAll(dir)
	})
	return socket
}

// newSessionEnv opens a session with NewSession whose window writes its environment down, and returns it.
func newSessionEnv(t *testing.T, socket string) map[string]string {
	t.Helper()
	out := filepath.Join(filepath.Dir(socket), "env")
	c := &Client{}
	_, err := c.NewSession(Window{Name: "t", Command: "env > " + out + ".tmp && mv " + out + ".tmp " + out + "; sleep 5",
		Env: []string{"HC_TASK=1a2b3c4d"}, Socket: socket, Conf: filepath.Join(filepath.Dir(socket), "tmux.conf")})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if data, err := os.ReadFile(out); err == nil {
			env := map[string]string{}
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				name, value, _ := strings.Cut(line, "=")
				env[name] = value
			}
			return env
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the window never wrote its environment")
	return nil
}

func TestNewSessionStartsTheServerWithoutTheEnvironmentOfItsStarter(t *testing.T) {
	socket := privateSocket(t)
	t.Setenv("HERDR_PANE_ID", "wB:p3")
	t.Setenv("CLAUDE_CONFIG_DIR", "/Users/x/.claude-work")
	t.Setenv("HC_STARTER_KEPT", "yes")
	env := newSessionEnv(t, socket)
	for _, name := range []string{"HERDR_PANE_ID", "CLAUDE_CONFIG_DIR"} {
		if value, ok := env[name]; ok {
			t.Errorf("the window has %s=%s", name, value)
		}
	}
	if env["HC_STARTER_KEPT"] != "yes" || env["HC_TASK"] != "1a2b3c4d" {
		t.Errorf("the window lost what it should keep: %v", env)
	}
}

func TestNewSessionCleansAServerStartedWithTheEnvironmentOfItsStarter(t *testing.T) {
	socket := privateSocket(t)
	// The way an older hc started it: with the environment of the Herdr pane it ran in.
	start := exec.Command("tmux", "-S", socket, "-f", "/dev/null", "new-session", "-d", "-s", "old", "sleep 30")
	start.Env = append(os.Environ(), "HERDR_PANE_ID=wB:p3", "HERDR_SOCKET_PATH=/run/herdr.sock", "CLAUDECODE=1",
		"DIRENV_DIR=-/w/repo", "HC_STARTER_KEPT=yes", "HC_DIRENV_SET=repo", "HC_DIRENV_CHANGED=repo",
		direnvDiffOf(t, map[string]string{"HC_DIRENV_CHANGED": "before", "HC_DIRENV_UNSET": "before"},
			map[string]string{"HC_DIRENV_SET": "repo", "HC_DIRENV_CHANGED": "repo"}))
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	env := newSessionEnv(t, socket)
	for _, name := range []string{"HERDR_PANE_ID", "HERDR_SOCKET_PATH", "CLAUDECODE", "DIRENV_DIR", "DIRENV_DIFF",
		"HC_DIRENV_SET"} {
		if value, ok := env[name]; ok {
			t.Errorf("the window has %s=%s", name, value)
		}
	}
	kept := []string{env["HC_STARTER_KEPT"], env["HC_DIRENV_CHANGED"], env["HC_DIRENV_UNSET"]}
	if !reflect.DeepEqual(kept, []string{"yes", "before", "before"}) {
		t.Errorf("the window has %v, want what was there before direnv", kept)
	}
	global, err := run(socket, "", "show-environment", "-g")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(global, "HERDR_") || strings.Contains(global, "CLAUDECODE") {
		t.Errorf("the server still has them:\n%s", global)
	}
}
