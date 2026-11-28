package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// terminal is a fake tmux client: it prints its size, echoes lines, and leaves on "quit" or fails on "fail".
const terminal = `echo $$ > "%s"; stty size
while read -r line; do
  case "$line" in
    size) stty size ;;
    quit) exit 0 ;;
    fail) exit 3 ;;
    *) echo "got:$line" ;;
  esac
done`

type attached struct {
	t        *testing.T
	f        *fixture
	id       string
	srv      *httptest.Server
	conn     *websocket.Conn
	seen     strings.Builder
	commands chan []string
	pidfile  string
	want     []string // the attach command, the board's tmux one when nil
}

// session starts a session in the board's own tmux, and serves the board on a real socket. Attaching
// runs script, a fake tmux client.
func session(t *testing.T, script string) *attached {
	t.Helper()
	f := newPrivate(t)
	a := &attached{t: t, f: f, id: f.newTask(obj{"start": true}).str("id"), pidfile: filepath.Join(f.tmp, "pid"),
		commands: make(chan []string, 1)}
	f.srv.AttachProcess = func(command []string) *exec.Cmd {
		a.commands <- command
		// The daemon sets the environment of what it starts: the pid file is named in the script.
		return exec.Command("/bin/sh", "-c", fmt.Sprintf(script, a.pidfile))
	}
	a.srv = httptest.NewServer(f.h)
	t.Cleanup(a.srv.Close)
	return a
}

// dial attaches from this computer, or from a phone when cookie is set: through a tailnet name, as tailscale serve does.
func (a *attached) dial(query, cookie string, header ...string) error {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws://" + a.srv.Listener.Addr().String()
	opts := &websocket.DialOptions{HTTPHeader: http.Header{}}
	if cookie != "" {
		_, port, _ := net.SplitHostPort(a.srv.Listener.Addr().String())
		url = "ws://board.example.ts.net:" + port
		opts.HTTPHeader.Set("Cookie", cookie)
		opts.HTTPHeader.Set("X-Forwarded-For", "100.64.0.9")
		opts.HTTPClient = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, a.srv.Listener.Addr().String())
			},
		}}
	}
	for i := 0; i+1 < len(header); i += 2 {
		opts.HTTPHeader.Set(header[i], header[i+1])
	}
	conn, _, err := websocket.Dial(ctx, url+"/api/tasks/"+a.id+"/attach"+query, opts)
	if err != nil {
		return err
	}
	a.t.Cleanup(func() { conn.CloseNow() })
	a.conn = conn
	select {
	case command := <-a.commands:
		// The plain attach: no display-message with the Ctrl-] hint, which hc attach and the board show.
		want := a.want
		if want == nil {
			want = []string{"tmux", "-S", filepath.Join(a.f.tmp, "state", "tmux.sock"), "attach-session", "-t", "%0"}
		}
		eq(a.t, command, want)
	case <-time.After(5 * time.Second):
		a.t.Fatal("the daemon never started tmux attach")
	}
	return nil
}

// attach starts a session and attaches to it from this computer.
func attach(t *testing.T, query string) *attached {
	t.Helper()
	a := session(t, terminal)
	if err := a.dial(query, ""); err != nil {
		t.Fatal(err)
	}
	return a
}

// pid reads the pid of the fake tmux client.
func (a *attached) pid() int {
	a.t.Helper()
	raw, err := os.ReadFile(a.pidfile)
	if err != nil {
		a.t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		a.t.Fatal(err)
	}
	return pid
}

// gone waits for a process to exit.
func gone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the fake tmux client still runs after the socket closed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (a *attached) send(text string) {
	a.t.Helper()
	if err := a.conn.Write(context.Background(), websocket.MessageBinary, []byte(text)); err != nil {
		a.t.Fatal(err)
	}
}

// waitFor reads the screen until it shows want, and returns the close error if the socket closes first.
func (a *attached) waitFor(want string) error {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for !strings.Contains(a.seen.String(), want) {
		_, data, err := a.conn.Read(ctx)
		if err != nil {
			return err
		}
		a.seen.Write(data)
	}
	return nil
}

func (a *attached) mustSee(want string) {
	a.t.Helper()
	if err := a.waitFor(want); err != nil {
		a.t.Fatalf("waiting for %q: %v, screen so far: %q", want, err, a.seen.String())
	}
}

func TestAttachStreamsTheTerminalBothWays(t *testing.T) {
	a := attach(t, "?cols=100&rows=30")
	a.mustSee("30 100")
	a.send("hello\n")
	a.mustSee("got:hello")

	if err := a.conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
		t.Fatal(err)
	}
	a.send("size\n")
	a.mustSee("40 120")

	a.send("quit\n")
	err := a.waitFor("never shown")
	eq(t, websocket.CloseStatus(err), websocket.StatusNormalClosure)
	var closed websocket.CloseError
	if errors.As(err, &closed) {
		eq(t, closed.Reason, "Detached")
	}
}

func TestAttachSeesTheTask(t *testing.T) {
	a := attach(t, "")
	a.mustSee("24 80")
	if a.f.task(a.id).SeenAt == 0 {
		t.Error("a task entered from a phone is not seen")
	}
}

func TestAttachStartsAt80x24WithoutASize(t *testing.T) {
	a := attach(t, "?cols=0&rows=nope")
	a.mustSee("24 80")
}

func TestAttachIgnoresAResizeOutOfBounds(t *testing.T) {
	a := attach(t, "")
	a.mustSee("24 80")
	for _, bad := range []string{`{"type":"resize","cols":0,"rows":10}`, `{"type":"resize","cols":5000,"rows":10}`, `not json`} {
		if err := a.conn.Write(context.Background(), websocket.MessageText, []byte(bad)); err != nil {
			t.Fatal(err)
		}
	}
	a.send("size\n")
	a.mustSee("size\r\n24 80")
}

func TestAttachSaysWhyTmuxLeft(t *testing.T) {
	a := attach(t, "")
	a.mustSee("24 80")
	a.send("fail\n")
	var closed websocket.CloseError
	if err := a.waitFor("never shown"); !errors.As(err, &closed) {
		t.Fatalf("got %v, want a close frame", err)
	}
	eq(t, closed.Code, websocket.StatusNormalClosure)
	eq(t, closed.Reason, "tmux left: exit status 3")
}

func TestLeavingTheSocketHangsUpOnTmux(t *testing.T) {
	a := attach(t, "")
	a.mustSee("24 80")
	pid := a.pid()
	a.conn.CloseNow()
	gone(t, pid)
}

func TestATmuxThatIgnoresTheHangupIsKilled(t *testing.T) {
	a := session(t, `trap '' HUP; `+terminal)
	if err := a.dial("", ""); err != nil {
		t.Fatal(err)
	}
	a.mustSee("24 80")
	pid := a.pid()
	a.conn.CloseNow()
	gone(t, pid)
}

func TestAPairedPhoneAttachesUntilItIsUnpaired(t *testing.T) {
	a := session(t, terminal)
	a.f.srv.attachCheck = 50 * time.Millisecond
	cookie := a.f.pairPhone("phone")
	if err := a.dial("", cookie); err != nil {
		t.Fatal(err)
	}
	a.mustSee("24 80")
	a.send("hello\n")
	a.mustSee("got:hello")
	pid := a.pid()

	a.f.ok(a.f.do("DELETE", "/api/devices/phone", nil))
	var closed websocket.CloseError
	if err := a.waitFor("never shown"); !errors.As(err, &closed) {
		t.Fatalf("got %v, want a close frame", err)
	}
	eq(t, closed.Code, websocket.StatusPolicyViolation)
	eq(t, closed.Reason, "This device was unpaired")
	gone(t, pid)
}

func TestAPhoneAttachIsPingedAndSurvivesIt(t *testing.T) {
	a := session(t, terminal)
	a.f.srv.attachCheck = 50 * time.Millisecond
	if err := a.dial("", a.f.pairPhone("phone")); err != nil {
		t.Fatal(err)
	}
	// A client always reads, as a real one does: that is what answers the pings.
	var mu sync.Mutex
	var screen strings.Builder
	failed := make(chan error, 1)
	go func() {
		for {
			_, data, err := a.conn.Read(context.Background())
			if err != nil {
				failed <- err
				return
			}
			mu.Lock()
			screen.Write(data)
			mu.Unlock()
		}
	}()
	time.Sleep(400 * time.Millisecond) // eight pings
	a.send("still\n")
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		done := strings.Contains(screen.String(), "got:still")
		mu.Unlock()
		select {
		case err := <-failed:
			t.Fatalf("the attach ended while pinged: %v", err)
		default:
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the session stopped answering")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A phone that sleeps behind tailscale serve never closes its socket: the ping finds out.
func TestAClientThatStopsAnsweringIsHungUp(t *testing.T) {
	a := session(t, terminal)
	a.f.srv.attachCheck = 50 * time.Millisecond
	if err := a.dial("", a.f.pairPhone("phone")); err != nil {
		t.Fatal(err)
	}
	a.mustSee("24 80")
	pid := a.pid() // from here the client reads nothing, so it answers no ping
	gone(t, pid)
}

func TestAttachRefusesAnUnpairedPhoneAndAnotherOrigin(t *testing.T) {
	a := session(t, terminal)
	if err := a.dial("", "hc_device=forged"); err == nil {
		t.Fatal("a forged cookie attached")
	}
	if err := a.dial("", "", "Origin", "https://evil.example"); err == nil {
		t.Fatal("a page on another origin attached")
	}
	select {
	case <-a.commands:
		t.Fatal("tmux attach ran for a refused client")
	default:
	}
}

func TestAPlainRequestIsToldToUseAWebSocket(t *testing.T) {
	f := newPrivate(t)
	id := f.newTask(obj{"start": true}).str("id")
	r := f.wantError(f.get("/api/tasks/"+id+"/attach"), 426)
	eq(t, r.str("error"), "Open this address as a WebSocket")
}

func TestAttachNeedsALiveSessionInTheBoardsTmux(t *testing.T) {
	f := newPrivate(t)
	f.wantError(f.get("/api/tasks/nope/attach"), 404)
	id := f.newTask(nil).str("id")
	r := f.wantError(f.get("/api/tasks/"+id+"/attach"), 409)
	eq(t, r.str("error"), "The session is not running: start it first")

	own := newFixture(t)
	id = own.live()
	r = own.wantError(own.get("/api/tasks/"+id+"/attach"), 409)
	eq(t, r.str("error"), "The session runs in your own tmux: enter it from the computer")
}

func TestAttachFromAnotherMachineNeedsAPairedDevice(t *testing.T) {
	f := newPrivate(t)
	id := f.newTask(obj{"start": true}).str("id")
	f.wantError(f.do("GET", "/api/tasks/"+id+"/attach", nil, "Host", "box.example.ts.net", "RemoteAddr", "100.64.0.9:50000"), 401)
}
