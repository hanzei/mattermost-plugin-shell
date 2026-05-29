package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"github.com/mattermost/mattermost/server/public/model"
)

// terminalShell is the shell spawned for each session. MVP: plain /bin/sh as the
// Mattermost server process user. Hardening (dedicated user / container / nsjail)
// is a deliberate follow-up, see README.
const terminalShell = "/bin/sh"

// clientMessage is the control envelope sent browser -> backend over the socket.
// Output flows the other way as raw binary frames written straight from the PTY,
// so the browser never has to parse anything.
type clientMessage struct {
	Type string `json:"type"`           // "data" | "resize"
	Data string `json:"data,omitempty"` // keystrokes, for Type == "data"
	Cols uint16 `json:"cols,omitempty"` // for Type == "resize"
	Rows uint16 `json:"rows,omitempty"`
}

// terminalSession is one live shell attached to one websocket.
type terminalSession struct {
	id   string
	ptmx *os.File
	cmd  *exec.Cmd
}

func (s *terminalSession) close() {
	if s.ptmx != nil {
		_ = s.ptmx.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

// sessionManager tracks active sessions so they can all be torn down on plugin
// deactivation (otherwise every redeploy orphans live shells).
type sessionManager struct {
	mu       sync.Mutex
	sessions map[string]*terminalSession
}

func newSessionManager() *sessionManager {
	return &sessionManager{sessions: make(map[string]*terminalSession)}
}

func (m *sessionManager) add(s *terminalSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.id] = s
}

func (m *sessionManager) remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		s.close()
		delete(m.sessions, id)
	}
}

func (m *sessionManager) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		s.close()
		delete(m.sessions, id)
	}
}

// handleTerminal upgrades the request to a websocket and bridges it to a PTY.
// The Mattermost-User-ID header is guaranteed non-empty by the router's auth
// middleware; here we additionally require system-admin before upgrading.
func (p *Plugin) handleTerminal(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if !p.client.User.HasPermissionTo(userID, model.PermissionManageSystem) {
		http.Error(w, "Forbidden: system administrator required", http.StatusForbidden)
		return
	}

	upgrader := websocket.Upgrader{
		CheckOrigin: p.checkTerminalOrigin,
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response on failure.
		p.API.LogError("Terminal websocket upgrade failed", "error", err.Error())
		return
	}
	defer func() { _ = conn.Close() }()

	// #nosec G204 -- running an interactive shell on the host is the entire
	// purpose of this endpoint; access is gated to system admins above.
	cmd := exec.Command(terminalShell)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		p.API.LogError("Failed to start PTY", "error", err.Error())
		_ = conn.WriteMessage(websocket.TextMessage, []byte("failed to start shell: "+err.Error()))
		return
	}

	session := &terminalSession{id: model.NewId(), ptmx: ptmx, cmd: cmd}
	p.terminals.add(session)
	defer p.terminals.remove(session.id)

	p.API.LogInfo("Terminal session started", "user_id", userID, "session_id", session.id)
	defer p.API.LogInfo("Terminal session ended", "user_id", userID, "session_id", session.id)

	// PTY output -> websocket. When the shell exits, the read errors and we close
	// the socket, which unblocks the reader goroutine below.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := ptmx.Read(buf)
			if n > 0 {
				if writeErr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					break
				}
			}
			if readErr != nil {
				break
			}
		}
		_ = conn.Close()
	}()

	// websocket -> PTY (runs on the handler goroutine; returning tears everything down).
	for {
		_, raw, readErr := conn.ReadMessage()
		if readErr != nil {
			break
		}

		var msg clientMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue // ignore malformed control frames
		}

		switch msg.Type {
		case "data":
			if _, err := ptmx.WriteString(msg.Data); err != nil {
				return
			}
		case "resize":
			_ = pty.Setsize(ptmx, &pty.Winsize{Rows: msg.Rows, Cols: msg.Cols})
		}
	}
}

// checkTerminalOrigin validates the websocket Origin against the configured site
// URL host (falling back to the request Host). This is a real same-origin check,
// not a blanket allow.
func (p *Plugin) checkTerminalOrigin(r *http.Request) bool {
	siteURL := ""
	if cfg := p.client.Configuration.GetConfig(); cfg != nil && cfg.ServiceSettings.SiteURL != nil {
		siteURL = *cfg.ServiceSettings.SiteURL
	}
	return originAllowed(r.Header.Get("Origin"), siteURL, r.Host)
}

// originAllowed is the pure same-origin decision behind checkTerminalOrigin,
// split out so it can be unit-tested without an HTTP request or plugin client.
// The origin host must match the site URL host; if no site URL is configured it
// falls back to the request Host.
func originAllowed(origin, siteURL, reqHost string) bool {
	if origin == "" {
		return false
	}
	originURL, err := url.Parse(origin)
	if err != nil || originURL.Host == "" {
		return false
	}

	if siteURL != "" {
		if parsed, err := url.Parse(siteURL); err == nil && parsed.Host != "" {
			return originURL.Host == parsed.Host
		}
	}

	return originURL.Host == reqHost
}
