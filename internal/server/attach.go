package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
	"github.com/rouxlabs/headcoach/internal/herdr"
	"github.com/rouxlabs/headcoach/internal/tmux"
)

const (
	attachReadLimit = 1 << 20          // a paste of a few hundred lines
	attachHangup    = 2 * time.Second  // how long tmux gets to leave once the client is gone
	attachCheck     = 15 * time.Second // how often a client is pinged, and its pairing checked again
)

// resize is the one control message of an attached client, sent as a text frame. Keys are binary frames.
type resize struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// attachSize reads the terminal size the client starts with, 80x24 when it says nothing sensible.
func attachSize(r *http.Request) *pty.Winsize {
	size := &pty.Winsize{Cols: 80, Rows: 24}
	if cols, err := strconv.Atoi(r.URL.Query().Get("cols")); err == nil && cols > 0 && cols <= 1000 {
		size.Cols = uint16(cols)
	}
	if rows, err := strconv.Atoi(r.URL.Query().Get("rows")); err == nil && rows > 0 && rows <= 1000 {
		size.Rows = uint16(rows)
	}
	return size
}

// attachTask takes a WebSocket into a task's session in the board's own tmux, or in Herdr: tmux attach, or
// herdr agent attach, runs in a pseudo-terminal on this computer, and its screen goes to the client as it
// would to a terminal. Leaving, with Ctrl-], closes the socket; closing the socket detaches.
func (s *Server) attachTask(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	command, inHerdr, err := s.attachCommand(r)
	s.mu.Unlock()
	if err != nil {
		respond(w, nil, err)
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		respond(w, nil, fail(426, "Open this address as a WebSocket"))
		return
	}
	// A device paired from another machine stays paired only as long as hc devices keeps it.
	token := ""
	if c, err := r.Cookie(deviceCookie); err == nil && remote(r) {
		token = c.Value
	}

	size := attachSize(r)
	conn, err := websocket.Accept(w, r, nil) // checks the Origin, and answers the client itself on failure
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(attachReadLimit)

	cmd := s.AttachProcess(command)
	cmd.Env = append(tmux.AttachEnv(), "TERM=xterm-256color") // launchd starts the daemon without a TERM
	if inHerdr {
		cmd.Env = append(herdr.AttachEnv(), "TERM=xterm-256color")
	}
	term, err := pty.StartWithSize(cmd, size)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "Cannot start tmux: "+err.Error())
		return
	}
	defer term.Close()
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	// A client that leaves hangs tmux up, as a closed terminal would; tmux detaches and the session runs on.
	// Closing the terminal is not enough: it waits for the read in progress, which waits for tmux.
	var once sync.Once
	var detached atomic.Bool // Ctrl-] hung a Herdr attach up
	hangup := func() {
		once.Do(func() {
			_ = cmd.Process.Signal(syscall.SIGHUP)
			select {
			case <-exited:
			case <-time.After(attachHangup):
				_ = cmd.Process.Kill()
			}
		})
	}

	// The request's context ends with the handler; the socket lives on its own after the upgrade.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A phone that sleeps or changes network behind tailscale serve leaves no trace on this side: ping it,
	// and hang up on it when it no longer answers or no longer is paired.
	go func() {
		tick := time.NewTicker(s.attachCheck)
		defer tick.Stop()
		for {
			select {
			case <-exited:
				return
			case <-tick.C:
			}
			if token != "" {
				if name, err := s.st.DeviceByToken(token); err == nil && name == "" {
					conn.Close(websocket.StatusPolicyViolation, "This device was unpaired")
					hangup()
					return
				}
			}
			pctx, pcancel := context.WithTimeout(ctx, s.attachCheck)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				hangup()
				return
			}
		}
	}()

	// What the client sends: keys to the terminal, and its new size.
	go func() {
		defer hangup()
		for {
			kind, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if kind == websocket.MessageBinary {
				// tmux leaves on Ctrl-] by itself; Herdr is hung up, as its own detach is a prefix key.
				typed, detach := data, false
				if inHerdr {
					typed, detach = herdr.BeforeDetach(data)
				}
				if _, err := term.Write(typed); err != nil {
					return
				}
				if detach {
					detached.Store(true)
					return
				}
				continue
			}
			var msg resize
			if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" && msg.Cols > 0 && msg.Rows > 0 &&
				msg.Cols <= 1000 && msg.Rows <= 1000 {
				_ = pty.Setsize(term, &pty.Winsize{Cols: uint16(msg.Cols), Rows: uint16(msg.Rows)})
			}
		}
	}()

	// What tmux draws, to the client, until tmux leaves: the terminal reads an error once it has exited.
	buf := make([]byte, 32<<10)
	for {
		n, err := term.Read(buf)
		if n > 0 {
			if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
				go hangup() // the client is gone: tmux exits, and the next read ends the loop
			}
		}
		if err != nil {
			break
		}
	}
	hangup()
	<-exited
	reason := attachReason(cmd.ProcessState)
	if detached.Load() {
		reason = "Detached"
	}
	conn.Close(websocket.StatusNormalClosure, reason)
}

// attachReason says why the session ended, in the 123 bytes a close frame has room for.
func attachReason(state *os.ProcessState) string {
	if state == nil || state.Success() {
		return "Detached"
	}
	return "tmux left: " + strings.TrimSpace(state.String())
}

// attachCommand is the command that enters the task's session, when it runs in the board's own tmux or in
// Herdr, and whether it is Herdr's.
func (s *Server) attachCommand(r *http.Request) ([]string, bool, error) {
	task, err := s.find(r)
	if err != nil {
		return nil, false, err
	}
	view, err := s.view(task, false)
	switch {
	case err != nil:
		return nil, false, err
	case !view.Live:
		return nil, false, fail(409, "The session is not running: start it first")
	case view.Tmux == "herdr":
		return view.Attach, true, s.st.SeeTask(task.ID)
	case len(view.Attach) == 0:
		return nil, false, fail(409, "The session runs in your own tmux: enter it from the computer")
	}
	if err := s.st.SeeTask(task.ID); err != nil {
		return nil, false, err
	}
	// Without the Ctrl-] hint a terminal gets: the client has its own way out, a key of the app.
	return tmux.AttachQuiet(view.Pane, s.cfg.TmuxSocket()), false, nil
}

// attachProcess is the process an attached client talks to: the tmux attach command. Tests replace it.
func attachProcess(command []string) *exec.Cmd {
	return exec.Command(command[0], command[1:]...)
}
