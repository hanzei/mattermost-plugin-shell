package main

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"github.com/mattermost/mattermost/server/public/model"
)

// frameSocket is the part of a websocket the edge needs; *websocket.Conn
// satisfies it.
type frameSocket interface {
	WriteMessage(messageType int, data []byte) error
	Close() error
}

// edgeSession is a browser websocket on this node whose shell runs on another.
type edgeSession struct {
	sid    string
	self   string
	target string
	sock   frameSocket

	inSeq uint64 // next input sequence number; only the socket reader uses it

	opened   atomic.Bool  // the target confirmed the shell started
	lastSeen atomic.Int64 // unix nanos of the last message from the target

	mu      sync.Mutex // serializes output writes and guards the fields below
	out     *sequencer[[]byte]
	closing bool   // the target ended the session...
	closeAt uint64 // ...after this many output messages
}

// targetSession is a shell on this node driven from another node's websocket.
type targetSession struct {
	sid    string
	self   string
	origin string
	term   *terminalSession

	lastSeen atomic.Int64 // unix nanos of the last message from the edge

	mu sync.Mutex // serializes PTY input and guards in
	in *sequencer[*clusterMessage]
}

// relay runs both halves of remote terminals: edge sessions (socket here, shell
// elsewhere) and target sessions (shell here, socket elsewhere).
type relay struct {
	// publish sends msg to the node target. msg.From is already set.
	publish func(target string, msg *clusterMessage) error
	// allowed reports whether the user may use the terminal.
	allowed func(userID string) bool
	// spawn starts a shell for the user; requested may be empty.
	spawn func(userID, requested string) (*terminalSession, shellInfo, error)
	// shells lists the shells on this node and the user's selection.
	shells func(userID string) ([]shellInfo, string)
	// logWarn logs a warning.
	logWarn func(msg string, keyValuePairs ...any)

	// Each side of a session pings the other every pingInterval and ends the
	// session when it hears nothing for peerTimeout, so a node that dies or
	// drops out of the cluster doesn't leave a hung terminal or a shell nobody
	// can reach.
	pingInterval time.Duration
	peerTimeout  time.Duration
	stop         chan struct{}
	stopOnce     sync.Once

	mu      sync.Mutex
	pending map[string]chan *clusterMessage
	edges   map[string]*edgeSession
	targets map[string]*targetSession
}

func newRelay() *relay {
	return &relay{
		pingInterval: 10 * time.Second,
		peerTimeout:  30 * time.Second,
		stop:         make(chan struct{}),
		pending:      make(map[string]chan *clusterMessage),
		edges:        make(map[string]*edgeSession),
		targets:      make(map[string]*targetSession),
	}
}

// start runs the heartbeat until closeAll.
func (r *relay) start() {
	go func() {
		ticker := time.NewTicker(r.pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				return
			case now := <-ticker.C:
				r.beat(now)
			}
		}
	}()
}

// beat pings the peer of every session and ends sessions whose peer has gone
// quiet or can't be reached. Each check runs on its own goroutine because a
// send to a dead node can block for a long time.
func (r *relay) beat(now time.Time) {
	r.mu.Lock()
	edges := make([]*edgeSession, 0, len(r.edges))
	for _, es := range r.edges {
		if es.opened.Load() { // until then the open request has its own timeout
			edges = append(edges, es)
		}
	}
	targets := make([]*targetSession, 0, len(r.targets))
	for _, ts := range r.targets {
		targets = append(targets, ts)
	}
	r.mu.Unlock()

	for _, es := range edges {
		go func() {
			err := r.checkPeer(now, es.lastSeen.Load(), es.target, &clusterMessage{Type: msgPing, From: es.self, SID: es.sid})
			if err != nil && r.dropEdge(es.sid) {
				r.logWarn("Ending remote terminal", "node", es.target, "session_id", es.sid, "error", err.Error())
				es.mu.Lock()
				_ = es.sock.WriteMessage(websocket.TextMessage, []byte(err.Error()))
				es.mu.Unlock()
				_ = es.sock.Close()
			}
		}()
	}
	for _, ts := range targets {
		go func() {
			err := r.checkPeer(now, ts.lastSeen.Load(), ts.origin, &clusterMessage{Type: msgPing, From: ts.self, SID: ts.sid})
			if err != nil && r.dropTarget(ts.sid) {
				r.logWarn("Ending shell for remote terminal", "node", ts.origin, "session_id", ts.sid, "error", err.Error())
			}
		}()
	}
}

// checkPeer reports whether the peer has been silent too long, and otherwise
// pings it.
func (r *relay) checkPeer(now time.Time, lastSeen int64, peer string, ping *clusterMessage) error {
	if now.Sub(time.Unix(0, lastSeen)) > r.peerTimeout {
		return fmt.Errorf("node %s stopped responding", peer)
	}
	if err := r.publish(peer, ping); err != nil {
		return fmt.Errorf("node %s is unreachable: %w", peer, err)
	}
	return nil
}

// request sends msg to target and waits for the reply. A reply that carries
// Error is returned as an error.
func (r *relay) request(target string, msg *clusterMessage) (*clusterMessage, error) {
	msg.ReqID = model.NewId()
	ch := make(chan *clusterMessage, 1)

	r.mu.Lock()
	r.pending[msg.ReqID] = ch
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, msg.ReqID)
		r.mu.Unlock()
	}()

	if err := r.publish(target, msg); err != nil {
		return nil, err
	}

	select {
	case reply := <-ch:
		if reply.Error != "" {
			return nil, errors.New(reply.Error)
		}
		return reply, nil
	case <-time.After(requestTimeout):
		return nil, fmt.Errorf("no reply from node %s", target)
	}
}

// reply answers req, sending from the node req was addressed to.
func (r *relay) reply(req, resp *clusterMessage) {
	resp.Type = msgReply
	resp.ReqID = req.ReqID
	resp.From = req.To
	if err := r.publish(req.From, resp); err != nil {
		r.logWarn("Failed to send cluster reply", "node", req.From, "error", err.Error())
	}
}

// remoteShells asks target for its shells and the user's selection.
func (r *relay) remoteShells(self, target, userID string) ([]shellInfo, string, error) {
	resp, err := r.request(target, &clusterMessage{Type: msgShells, From: self, To: target, UserID: userID})
	if err != nil {
		return nil, "", err
	}
	return resp.Shells, resp.Selected, nil
}

// openEdge starts a shell on target for a websocket on this node. The returned
// session receives output as soon as it exists, so the caller must already have
// upgraded the socket.
func (r *relay) openEdge(sock frameSocket, self, target, userID, shell string) (*edgeSession, string, error) {
	es := &edgeSession{
		sid:    model.NewId(),
		self:   self,
		target: target,
		sock:   sock,
		out:    newSequencer[[]byte](),
	}

	r.mu.Lock()
	r.edges[es.sid] = es
	r.mu.Unlock()

	resp, err := r.request(target, &clusterMessage{
		Type: msgOpen, From: self, To: target, SID: es.sid, UserID: userID, Shell: shell,
	})
	if err != nil {
		r.dropEdge(es.sid)
		return nil, "", err
	}
	es.lastSeen.Store(time.Now().UnixNano())
	es.opened.Store(true)
	return es, resp.Shell, nil
}

// sendInput forwards one browser control message to the target. Only the
// socket reader calls it, so inSeq needs no lock.
func (r *relay) sendInput(es *edgeSession, cm clientMessage) error {
	msg := &clusterMessage{Type: msgInput, From: es.self, SID: es.sid, Seq: es.inSeq, Input: cm.Type}
	switch cm.Type {
	case "data":
		msg.Data = []byte(cm.Data)
	case "resize":
		msg.Cols, msg.Rows = cm.Cols, cm.Rows
	default:
		return nil
	}
	es.inSeq++
	return r.publish(es.target, msg)
}

// closeEdge ends an edge session after its socket closed and tells the target.
func (r *relay) closeEdge(es *edgeSession) {
	if !r.dropEdge(es.sid) {
		return // the target already ended it
	}
	if err := r.publish(es.target, &clusterMessage{Type: msgClose, From: es.self, SID: es.sid}); err != nil {
		r.logWarn("Failed to tell node the terminal closed", "node", es.target, "session_id", es.sid, "error", err.Error())
	}
}

func (r *relay) dropEdge(sid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.edges[sid]; !ok {
		return false
	}
	delete(r.edges, sid)
	return true
}

// handle dispatches one envelope from another node.
func (r *relay) handle(msg *clusterMessage) {
	switch msg.Type {
	case msgReply:
		r.mu.Lock()
		ch, ok := r.pending[msg.ReqID]
		r.mu.Unlock()
		if ok {
			ch <- msg
		}
	case msgShells:
		r.handleShells(msg)
	case msgOpen:
		r.handleOpen(msg)
	case msgInput:
		r.handleInput(msg)
	case msgOutput:
		r.handleOutput(msg)
	case msgClose:
		r.handleClose(msg)
	case msgPing:
		r.handlePing(msg)
	}
}

// handlePing records that the peer of a session is alive. A ping for a
// session this node doesn't have means the peer outlived its partner here, so
// it is told to end the session.
func (r *relay) handlePing(msg *clusterMessage) {
	now := time.Now().UnixNano()
	r.mu.Lock()
	es, isEdge := r.edges[msg.SID]
	ts, isTarget := r.targets[msg.SID]
	r.mu.Unlock()

	switch {
	case isEdge:
		es.lastSeen.Store(now)
	case isTarget:
		ts.lastSeen.Store(now)
	default:
		_ = r.publish(msg.From, &clusterMessage{Type: msgClose, SID: msg.SID})
	}
}

func (r *relay) handleShells(msg *clusterMessage) {
	if !r.allowed(msg.UserID) {
		r.reply(msg, &clusterMessage{Error: "forbidden: system administrator required"})
		return
	}
	shells, selected := r.shells(msg.UserID)
	r.reply(msg, &clusterMessage{Shells: shells, Selected: selected})
}

// handleOpen starts a shell here for a websocket on msg.From.
func (r *relay) handleOpen(msg *clusterMessage) {
	if msg.SID == "" || msg.To == "" {
		r.reply(msg, &clusterMessage{Error: "invalid open request"})
		return
	}
	// The edge already checked, but this node decides what runs on it.
	if !r.allowed(msg.UserID) {
		r.reply(msg, &clusterMessage{Error: "forbidden: system administrator required"})
		return
	}

	term, shell, err := r.spawn(msg.UserID, msg.Shell)
	if err != nil {
		r.reply(msg, &clusterMessage{Error: "failed to start shell: " + err.Error()})
		return
	}

	ts := &targetSession{
		sid:    msg.SID,
		self:   msg.To,
		origin: msg.From,
		term:   term,
		in:     newSequencer[*clusterMessage](),
	}
	ts.lastSeen.Store(time.Now().UnixNano())
	r.mu.Lock()
	r.targets[ts.sid] = ts
	r.mu.Unlock()

	r.reply(msg, &clusterMessage{SID: ts.sid, Shell: shell.Name})
	go r.pumpOutput(ts)
}

// pumpOutput streams PTY output to the edge until the shell exits.
func (r *relay) pumpOutput(ts *targetSession) {
	buf := make([]byte, 32*1024)
	var seq uint64
	for {
		n, readErr := ts.term.ptmx.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			msg := &clusterMessage{Type: msgOutput, From: ts.self, SID: ts.sid, Seq: seq, Data: data}
			seq++
			if err := r.publish(ts.origin, msg); err != nil {
				r.logWarn("Failed to send terminal output", "node", ts.origin, "session_id", ts.sid, "error", err.Error())
				break
			}
		}
		if readErr != nil {
			break
		}
	}

	// The shell is gone; tell the edge unless it ended the session first.
	if r.dropTarget(ts.sid) {
		if err := r.publish(ts.origin, &clusterMessage{Type: msgClose, From: ts.self, SID: ts.sid, Seq: seq}); err != nil {
			r.logWarn("Failed to tell node the shell exited", "node", ts.origin, "session_id", ts.sid, "error", err.Error())
		}
	}
}

func (r *relay) handleInput(msg *clusterMessage) {
	r.mu.Lock()
	ts, ok := r.targets[msg.SID]
	r.mu.Unlock()
	if !ok {
		return
	}
	ts.lastSeen.Store(time.Now().UnixNano())

	ts.mu.Lock()
	defer ts.mu.Unlock()
	ready, err := ts.in.push(msg.Seq, msg)
	for _, in := range ready {
		switch in.Input {
		case "data":
			if _, werr := ts.term.ptmx.Write(in.Data); werr != nil {
				err = werr
			}
		case "resize":
			_ = pty.Setsize(ts.term.ptmx, &pty.Winsize{Rows: in.Rows, Cols: in.Cols})
		}
	}
	if err != nil {
		r.logWarn("Ending remote terminal", "session_id", ts.sid, "error", err.Error())
		ts.term.close() // pumpOutput sees the PTY close and notifies the edge
	}
}

func (r *relay) handleOutput(msg *clusterMessage) {
	r.mu.Lock()
	es, ok := r.edges[msg.SID]
	r.mu.Unlock()
	if !ok {
		// Nobody is watching this shell, e.g. the open request timed out here
		// after the target had started it. Tell the target to end it.
		_ = r.publish(msg.From, &clusterMessage{Type: msgClose, SID: msg.SID})
		return
	}
	es.lastSeen.Store(time.Now().UnixNano())

	es.mu.Lock()
	defer es.mu.Unlock()
	ready, err := es.out.push(msg.Seq, msg.Data)
	for _, data := range ready {
		if werr := es.sock.WriteMessage(websocket.BinaryMessage, data); werr != nil {
			err = werr
			break
		}
	}
	if err != nil {
		_ = es.sock.Close() // the socket reader sees this and calls closeEdge
		return
	}
	if es.closing && es.out.next >= es.closeAt {
		r.dropEdge(es.sid)
		_ = es.sock.Close()
	}
}

// handleClose ends whichever half of the session lives here. A close from the
// target carries the number of output messages it sent, so the edge closes the
// socket only once all of them have been written.
func (r *relay) handleClose(msg *clusterMessage) {
	r.mu.Lock()
	es, isEdge := r.edges[msg.SID]
	r.mu.Unlock()
	if isEdge {
		es.mu.Lock()
		es.closing, es.closeAt = true, msg.Seq
		done := es.out.next >= msg.Seq
		es.mu.Unlock()
		if done {
			r.dropEdge(es.sid)
			_ = es.sock.Close()
		}
	}

	r.dropTarget(msg.SID)
}

// dropTarget removes a target session and kills its shell. It reports whether
// the session was still registered.
func (r *relay) dropTarget(sid string) bool {
	r.mu.Lock()
	ts, ok := r.targets[sid]
	delete(r.targets, sid)
	r.mu.Unlock()
	if ok {
		ts.term.close()
	}
	return ok
}

// closeAll ends every session on deactivation, tells the other nodes and
// stops the heartbeat.
func (r *relay) closeAll() {
	r.stopOnce.Do(func() { close(r.stop) })

	r.mu.Lock()
	edges := r.edges
	targets := r.targets
	r.edges = make(map[string]*edgeSession)
	r.targets = make(map[string]*targetSession)
	r.mu.Unlock()

	for _, es := range edges {
		_ = r.publish(es.target, &clusterMessage{Type: msgClose, From: es.self, SID: es.sid})
		_ = es.sock.Close()
	}
	for _, ts := range targets {
		_ = r.publish(ts.origin, &clusterMessage{Type: msgClose, From: ts.self, SID: ts.sid})
		ts.term.close()
	}
}
