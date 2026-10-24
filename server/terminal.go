package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"github.com/mattermost/mattermost/server/public/model"
)

// candidateShells are probed on the host (via PATH) to build the set of
// selectable shells. The detected set doubles as the allowlist: only a shell in
// this set can ever be launched, so no free-form path from the client reaches
// exec. Order matters — earlier entries win the default and survive dedup.
//
// MVP note: shells run as the Mattermost server process user. Hardening
// (dedicated user / container / nsjail) is a deliberate follow-up, see README.
var candidateShells = []string{"bash", "zsh", "fish", "sh", "dash", "ksh", "tcsh"}

// shellInfo is a detected, launchable shell.
type shellInfo struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// detectShells returns the candidate shells present on the host, deduped by their
// resolved binary (so e.g. /bin/sh -> dash doesn't appear twice).
func detectShells() []shellInfo {
	var out []shellInfo
	seen := make(map[string]bool)
	for _, name := range candidateShells {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		resolved := path
		if r, err := filepath.EvalSymlinks(path); err == nil {
			resolved = r
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		out = append(out, shellInfo{Name: name, Path: path})
	}
	return out
}

// resolveShell decides which detected shell to launch. A valid requested name
// wins (the caller persists it); otherwise the saved per-user preference, if
// still present on the host; otherwise a default (bash, then sh, then the first
// detected). Returns false only when no shells were detected at all. This is the
// security-relevant gate — an unknown/invalid name never selects a shell, so it
// can never be executed.
func resolveShell(requested, savedPref string, detected []shellInfo) (shellInfo, bool) {
	if len(detected) == 0 {
		return shellInfo{}, false
	}
	byName := func(name string) (shellInfo, bool) {
		for _, s := range detected {
			if s.Name == name {
				return s, true
			}
		}
		return shellInfo{}, false
	}

	if requested != "" {
		if s, ok := byName(requested); ok {
			return s, true
		}
	}
	if savedPref != "" {
		if s, ok := byName(savedPref); ok {
			return s, true
		}
	}
	for _, def := range []string{"bash", "sh"} {
		if s, ok := byName(def); ok {
			return s, true
		}
	}
	return detected[0], true
}

// shellFamily groups shells by how their rc / color setup is injected.
func shellFamily(name string) string {
	switch name {
	case "bash":
		return "bash"
	case "sh", "dash", "ash", "ksh":
		return "posix" // honor $ENV for interactive shells
	default:
		return "other" // zsh, fish, tcsh — rely on the user's own dotfiles
	}
}

// terminalRC is sourced by interactive shells started for the terminal. It turns
// on color (prompt, ls/grep aliases, dircolors) and, under bash, programmable
// tab completion. It is written POSIX-first with bash-only bits guarded so it is
// safe for both `bash --rcfile` and sh's $ENV.
const terminalRC = `
# Sourced by the Mattermost Shell plugin for interactive shells.
[ -r /etc/profile ] && . /etc/profile
if [ -n "$BASH_VERSION" ] && [ -r "$HOME/.bashrc" ]; then . "$HOME/.bashrc"; fi

# Color setup (after the user's rc so these win).
eval "$(dircolors -b 2>/dev/null)" 2>/dev/null
alias ls='ls --color=auto'
alias ll='ls -alF --color=auto'
alias grep='grep --color=auto'
alias fgrep='fgrep --color=auto'
alias egrep='egrep --color=auto'

if [ -n "$BASH_VERSION" ]; then
    PS1='\[\e[1;32m\]\u@\h\[\e[0m\]:\[\e[1;34m\]\w\[\e[0m\]\$ '
    if ! shopt -oq posix 2>/dev/null; then
        for _bc in /usr/share/bash-completion/bash_completion /etc/bash_completion; do
            if [ -r "$_bc" ]; then . "$_bc"; break; fi
        done
    fi
fi
`

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

	closeOnce sync.Once
}

// close kills the shell and reaps it, so it doesn't linger as a zombie. It is
// safe to call more than once.
func (s *terminalSession) close() {
	s.closeOnce.Do(func() {
		if s.ptmx != nil {
			_ = s.ptmx.Close()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
			go func() { _ = s.cmd.Wait() }()
		}
	})
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
//
// With ?node=<cluster node ID> the shell runs on that node: when it is not this
// one, the socket stays here and the session is relayed (see cluster.go).
func (p *Plugin) handleTerminal(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if !p.client.User.HasPermissionTo(userID, model.PermissionManageSystem) {
		http.Error(w, "Forbidden: system administrator required", http.StatusForbidden)
		return
	}

	query := r.URL.Query()
	self, remote, err := p.clusterRoute(query.Get("node"), query.Get("nodes"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if remote {
		p.relayTerminal(w, r, userID, self, query.Get("node"), query.Get("shell"))
		return
	}

	// Upgrade (and so check the Origin) before starting anything, so a
	// cross-origin request never spawns a shell or saves a preference.
	conn, err := p.upgrader().Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response on failure.
		p.API.LogError("Terminal websocket upgrade failed", "error", err.Error())
		return
	}
	defer func() { _ = conn.Close() }()

	session, shell, err := p.startShell(userID, query.Get("shell"))
	if err != nil {
		// Text frames are shown to the user as errors.
		_ = conn.WriteMessage(websocket.TextMessage, []byte(err.Error()))
		return
	}
	p.terminals.add(session)
	defer p.terminals.remove(session.id)

	ptmx := session.ptmx

	p.API.LogInfo("Terminal session started", "user_id", userID, "session_id", session.id, "shell", shell.Name)
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
		msg, ok := readClientMessage(conn)
		if !ok {
			break
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

// relayTerminal serves a terminal whose shell runs on another cluster node. The
// browser socket stays here; input goes to the target and its output comes back
// through the relay.
func (p *Plugin) relayTerminal(w http.ResponseWriter, r *http.Request, userID, self, target, shell string) {
	conn, err := p.upgrader().Upgrade(w, r, nil)
	if err != nil {
		p.API.LogError("Terminal websocket upgrade failed", "error", err.Error())
		return
	}
	defer func() { _ = conn.Close() }()

	es, shellName, err := p.relay.openEdge(conn, self, target, userID, shell)
	if err != nil {
		// openEdge already told the browser; output handlers may share the socket.
		p.API.LogError("Failed to open remote terminal", "node", target, "error", err.Error())
		return
	}
	defer p.relay.closeEdge(es)

	p.API.LogInfo("Remote terminal session started", "user_id", userID, "session_id", es.sid, "node", target, "shell", shellName)
	defer p.API.LogInfo("Remote terminal session ended", "user_id", userID, "session_id", es.sid, "node", target)

	for {
		msg, ok := readClientMessage(conn)
		if !ok {
			return
		}
		if err := p.relay.sendInput(es, msg); err != nil {
			p.API.LogWarn("Failed to send terminal input", "node", target, "session_id", es.sid, "error", err.Error())
			return
		}
	}
}

// readClientMessage reads the next control envelope, skipping malformed frames.
// It returns false once the socket is closed.
func readClientMessage(conn *websocket.Conn) (clientMessage, bool) {
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return clientMessage{}, false
		}
		var msg clientMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue // ignore malformed control frames
		}
		return msg, true
	}
}

func (p *Plugin) upgrader() *websocket.Upgrader {
	return &websocket.Upgrader{CheckOrigin: p.checkTerminalOrigin}
}

// startShell resolves the user's shell on this node and starts it on a PTY. A
// valid explicit request is saved as the user's preference.
func (p *Plugin) startShell(userID, requested string) (*terminalSession, shellInfo, error) {
	shell, ok := resolveShell(requested, p.getShellPref(userID), detectShells())
	if !ok {
		return nil, shellInfo{}, errors.New("no supported shell found on the host")
	}
	// Persist only an explicit, valid selection (so the dropdown sticks).
	if requested != "" && requested == shell.Name {
		p.setShellPref(userID, shell.Name)
	}

	cmd := p.terminalCmd(shell)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		p.API.LogError("Failed to start PTY", "error", err.Error())
		return nil, shellInfo{}, fmt.Errorf("failed to start shell: %w", err)
	}
	return &terminalSession{id: model.NewId(), ptmx: ptmx, cmd: cmd}, shell, nil
}

// terminalCmd builds the interactive command for the chosen shell. The shell
// path comes from detectShells (never from client input), so it is trusted. rc
// injection is dispatched by shell family: bash via --rcfile, POSIX shells via
// $ENV; other shells (zsh, fish, tcsh) run interactively and rely on the host
// user's own dotfiles. Color-forcing env vars help tools that auto-detect a TTY.
func (p *Plugin) terminalCmd(shell shellInfo) *exec.Cmd {
	env := append(os.Environ(),
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"CLICOLOR=1",
		"CLICOLOR_FORCE=1",
	)

	var cmd *exec.Cmd
	switch shellFamily(shell.Name) {
	case "bash":
		args := []string{"-i"}
		if rc := p.writeTerminalRC(); rc != "" {
			args = []string{"--rcfile", rc, "-i"}
		}
		// #nosec G204 -- shell.Path is a host-detected shell from the allowlist,
		// not client input; an interactive host shell is this endpoint's purpose
		// and access is gated to system admins before the upgrade.
		cmd = exec.Command(shell.Path, args...)
	case "posix":
		if rc := p.writeTerminalRC(); rc != "" {
			env = append(env, "ENV="+rc)
		}
		// #nosec G204 -- see above.
		cmd = exec.Command(shell.Path, "-i")
	default:
		// #nosec G204 -- see above.
		cmd = exec.Command(shell.Path, "-i")
	}

	cmd.Env = env
	return cmd
}

// getShellPref returns the user's saved shell name, or "" if none/unreadable.
func (p *Plugin) getShellPref(userID string) string {
	var name string
	if err := p.client.KV.Get("shell_pref_"+userID, &name); err != nil {
		p.API.LogWarn("Failed to read shell preference", "user_id", userID, "error", err.Error())
		return ""
	}
	return name
}

// setShellPref persists the user's shell selection (best-effort).
func (p *Plugin) setShellPref(userID, name string) {
	if _, err := p.client.KV.Set("shell_pref_"+userID, name); err != nil {
		p.API.LogWarn("Failed to save shell preference", "user_id", userID, "error", err.Error())
	}
}

// handleShells lists the shells available on the host and the caller's current
// selection, for populating the picker. System-admin only, like the terminal.
// With ?node= it asks that cluster node instead.
func (p *Plugin) handleShells(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if !p.client.User.HasPermissionTo(userID, model.PermissionManageSystem) {
		http.Error(w, "Forbidden: system administrator required", http.StatusForbidden)
		return
	}

	query := r.URL.Query()
	self, remote, err := p.clusterRoute(query.Get("node"), query.Get("nodes"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var resp struct {
		Shells   []shellInfo `json:"shells"`
		Selected string      `json:"selected"`
	}
	if remote {
		resp.Shells, resp.Selected, err = p.relay.remoteShells(self, query.Get("node"), userID)
		if err != nil {
			http.Error(w, "Failed to list shells on node: "+err.Error(), http.StatusBadGateway)
			return
		}
	} else {
		resp.Shells, resp.Selected = p.localShells(userID)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		p.API.LogError("Failed to encode shells response", "error", err.Error())
	}
}

// localShells returns this node's shells and the user's resolved selection.
func (p *Plugin) localShells(userID string) ([]shellInfo, string) {
	detected := detectShells()
	selected, _ := resolveShell("", p.getShellPref(userID), detected)
	return detected, selected.Name
}

// writeTerminalRC writes the shared rc file and returns its path, or "" if it
// could not be written (in which case the shell still runs, just without the
// color/completion niceties).
func (p *Plugin) writeTerminalRC() string {
	path := filepath.Join(os.TempDir(), "mattermost-shell-terminal.rc")
	if err := os.WriteFile(path, []byte(terminalRC), 0o600); err != nil {
		p.API.LogWarn("Failed to write terminal rc file; color/completion may be limited", "error", err.Error())
		return ""
	}
	return path
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
