package main

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mattermost/mattermost/server/public/model"
)

// frameSocket is the part of a websocket the edge needs; *websocket.Conn
// satisfies it.
type frameSocket interface {
	WriteMessage(messageType int, data []byte) error
	Close() error
}

// link is what both halves of a relayed session share: the session, the node
// on the other end, and when that node was last heard from.
type link struct {
	sid      string
	peer     string
	lastSeen atomic.Int64 // unix nanos
}

// msg returns an envelope of the given type for this session.
func (l *link) msg(typ string) *clusterMessage {
	return &clusterMessage{Type: typ, SID: l.sid}
}

func (l *link) touch() { l.lastSeen.Store(time.Now().UnixNano()) }

// edgeSession is a browser websocket on this node whose shell runs on the peer.
type edgeSession struct {
	link
	sock frameSocket

	inSeq  uint64      // next input sequence number; only the socket reader uses it
	opened atomic.Bool // the target confirmed the shell started

	mu  sync.Mutex // serializes socket writes and guards out
	out *sequencer
}

// targetSession is a shell on this node driven from the peer's websocket.
type targetSession struct {
	link
	term *terminalSession

	mu sync.Mutex // serializes PTY input and guards in
	in *sequencer
}

// relay runs both halves of remote terminals: edge sessions (socket here, shell
// elsewhere) and target sessions (shell here, socket elsewhere).
type relay struct {
	// publish sends msg to the node target.
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
	closed  bool // set by closeAll; nothing registers after it
	pending map[string]chan *clusterMessage
	edges   map[string]*edgeSession
	targets map[string]*targetSession
}

var errForbidden = errors.New("forbidden: system administrator required")

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
	type peerCheck struct {
		link *link
		end  func(error)
	}

	r.mu.Lock()
	checks := make([]peerCheck, 0, len(r.edges)+len(r.targets))
	for _, es := range r.edges {
		if es.opened.Load() { // until then the open request has its own timeout
			checks = append(checks, peerCheck{&es.link, func(err error) {
				if r.endEdge(es, err.Error(), false) {
					r.logWarn("Ending remote terminal", "node", es.peer, "session_id", es.sid, "error", err.Error())
				}
			}})
		}
	}
	for _, ts := range r.targets {
		checks = append(checks, peerCheck{&ts.link, func(err error) {
			if r.endTarget(ts, false) {
				r.logWarn("Ending shell for remote terminal", "node", ts.peer, "session_id", ts.sid, "error", err.Error())
			}
		}})
	}
	r.mu.Unlock()

	for _, c := range checks {
		go func() {
			if err := r.checkPeer(now, c.link); err != nil {
				c.end(err)
			}
		}()
	}
}

// checkPeer reports whether the peer has been silent too long, and otherwise
// pings it.
func (r *relay) checkPeer(now time.Time, l *link) error {
	if now.Sub(time.Unix(0, l.lastSeen.Load())) > r.peerTimeout {
		return fmt.Errorf("node %s stopped responding", l.peer)
	}
	if err := r.publish(l.peer, l.msg(msgPing)); err != nil {
		return fmt.Errorf("node %s is unreachable: %w", l.peer, err)
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

// reply answers req, which the node from sent.
func (r *relay) reply(from string, req, resp *clusterMessage) {
	resp.Type = msgReply
	resp.ReqID = req.ReqID
	if err := r.publish(from, resp); err != nil {
		r.logWarn("Failed to send cluster reply", "node", from, "error", err.Error())
	}
}

// remoteShells asks target for its shells and the user's selection.
func (r *relay) remoteShells(target, userID string) ([]shellInfo, string, error) {
	resp, err := r.request(target, &clusterMessage{Type: msgShells, UserID: userID})
	if err != nil {
		return nil, "", err
	}
	return resp.Shells, resp.Selected, nil
}

// openEdge starts a shell on target for a websocket on this node. The session
// receives output as soon as it is registered, so the caller must already have
// upgraded the socket. On failure the browser has been told why.
func (r *relay) openEdge(sock frameSocket, target, userID, shell string) (*edgeSession, string, error) {
	es := &edgeSession{
		link: link{sid: model.NewId(), peer: target},
		sock: sock,
		out:  newSequencer(),
	}

	r.mu.Lock()
	closed := r.closed
	if !closed {
		r.edges[es.sid] = es
	}
	r.mu.Unlock()
	if closed {
		return nil, "", errors.New("the plugin is shutting down")
	}

	resp, err := r.request(target, &clusterMessage{Type: msgOpen, SID: es.sid, UserID: userID, Shell: shell})
	if err != nil {
		r.endEdge(es, "failed to start shell on node "+target+": "+err.Error(), false)
		return nil, "", err
	}
	es.touch()
	es.opened.Store(true)
	return es, resp.Shell, nil
}

// sendInput forwards one browser control message to the target. Only the
// socket reader calls it, so inSeq needs no lock.
func (r *relay) sendInput(es *edgeSession, cm clientMessage) error {
	msg := es.msg(msgInput)
	msg.Seq = es.inSeq
	msg.Input = &cm
	es.inSeq++
	return r.publish(es.peer, msg)
}

// closeEdge ends an edge session after its socket closed and tells the target.
func (r *relay) closeEdge(es *edgeSession) {
	r.endEdge(es, "", true)
}

// endEdge ends an edge session: it tells the browser why (if reason is set),
// closes the socket and, if notify, tells the target. It reports whether the
// session was still open.
func (r *relay) endEdge(es *edgeSession, reason string, notify bool) bool {
	if !r.dropEdge(es.sid) {
		return false
	}
	if notify {
		if err := r.publish(es.peer, es.msg(msgClose)); err != nil {
			r.logWarn("Failed to tell node the terminal closed", "node", es.peer, "session_id", es.sid, "error", err.Error())
		}
	}
	if reason != "" {
		es.mu.Lock() // output may be writing to the socket too
		_ = es.sock.WriteMessage(websocket.TextMessage, []byte(reason))
		es.mu.Unlock()
	}
	_ = es.sock.Close()
	return true
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

// endTarget ends a target session, killing its shell and, if notify, telling
// the edge. It reports whether the session was still open.
func (r *relay) endTarget(ts *targetSession, notify bool) bool {
	if !r.dropTarget(ts.sid) {
		return false
	}
	if notify {
		if err := r.publish(ts.peer, ts.msg(msgClose)); err != nil {
			r.logWarn("Failed to tell node the shell ended", "node", ts.peer, "session_id", ts.sid, "error", err.Error())
		}
	}
	return true
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

// handle dispatches one envelope from the node from.
func (r *relay) handle(from string, msg *clusterMessage) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return
	}

	switch msg.Type {
	case msgReply:
		r.mu.Lock()
		ch, ok := r.pending[msg.ReqID]
		r.mu.Unlock()
		if ok {
			select {
			case ch <- msg:
			default: // duplicate reply; the first one wins
			}
		}
	case msgShells:
		r.handleShells(from, msg)
	case msgOpen:
		r.handleOpen(from, msg)
	case msgClose:
		r.handleClose(from, msg)
	case msgInput, msgOutput, msgEnd, msgPing:
		r.handleSession(from, msg)
	}
}

// handleSession routes a message for an existing session and notes that its
// peer is alive. Messages from any other node are ignored. A message for a
// session this node doesn't have means the sender outlived its partner here,
// e.g. an open that timed out on the edge after the target started the shell,
// or a plugin restart, so the sender is told to end it.
func (r *relay) handleSession(from string, msg *clusterMessage) {
	r.mu.Lock()
	es := r.edges[msg.SID]
	ts := r.targets[msg.SID]
	r.mu.Unlock()

	switch {
	case es != nil && es.peer != from, ts != nil && ts.peer != from:
		return
	case es != nil:
		es.touch()
		if msg.Type == msgOutput || msg.Type == msgEnd {
			r.handleStream(es, msg)
		}
	case ts != nil:
		ts.touch()
		if msg.Type == msgInput {
			r.handleInput(ts, msg)
		}
	case msg.Type != msgEnd:
		_ = r.publish(from, &clusterMessage{Type: msgClose, SID: msg.SID})
	}
}

func (r *relay) handleShells(from string, msg *clusterMessage) {
	if !r.allowed(msg.UserID) {
		r.reply(from, msg, &clusterMessage{Error: errForbidden.Error()})
		return
	}
	shells, selected := r.shells(msg.UserID)
	r.reply(from, msg, &clusterMessage{Shells: shells, Selected: selected})
}

// handleOpen starts a shell here for a websocket on the node from.
func (r *relay) handleOpen(from string, msg *clusterMessage) {
	if msg.SID == "" {
		r.reply(from, msg, &clusterMessage{Error: "invalid open request"})
		return
	}
	// The edge already checked, but this node decides what runs on it.
	if !r.allowed(msg.UserID) {
		r.reply(from, msg, &clusterMessage{Error: errForbidden.Error()})
		return
	}
	if !r.canRegisterTarget(msg.SID) {
		return // a duplicate open, or shutting down
	}

	term, shell, err := r.spawn(msg.UserID, msg.Shell)
	if err != nil {
		r.reply(from, msg, &clusterMessage{Error: "failed to start shell: " + err.Error()})
		return
	}

	ts := &targetSession{
		link: link{sid: msg.SID, peer: from},
		term: term,
		in:   newSequencer(),
	}
	ts.touch()

	r.mu.Lock()
	ok := !r.closed && r.targets[ts.sid] == nil // re-check: a duplicate may have raced us
	if ok {
		r.targets[ts.sid] = ts
	}
	r.mu.Unlock()
	if !ok {
		term.close()
		return
	}

	r.reply(from, msg, &clusterMessage{SID: ts.sid, Shell: shell.Name})
	go r.pumpOutput(ts)
}

func (r *relay) canRegisterTarget(sid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.closed && r.targets[sid] == nil
}

var errStopped = errors.New("stopped")

// pumpOutput streams PTY output to the edge until the shell exits, then sends
// end after the last output. A reader goroutine queues output while a send is
// in flight, and whatever queued up goes out as one message, so a burst
// doesn't cost one cluster message per read.
func (r *relay) pumpOutput(ts *targetSession) {
	chunks := make(chan []byte, 16)
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer close(chunks)
		pumpPTY(ts.term.ptmx, func(b []byte) error {
			select {
			case chunks <- bytes.Clone(b):
				return nil
			case <-done:
				return errStopped
			}
		})
	}()

	var seq uint64
	for data := range chunks {
		msg := ts.msg(msgOutput)
		msg.Seq = seq
		msg.Data = coalesce(data, chunks)
		seq++
		if err := r.publish(ts.peer, msg); err != nil {
			r.logWarn("Failed to send terminal output", "node", ts.peer, "session_id", ts.sid, "error", err.Error())
			r.endTarget(ts, false)
			return
		}
	}

	// The shell exited; tell the edge unless the session already ended.
	if r.dropTarget(ts.sid) {
		end := ts.msg(msgEnd)
		end.Seq = seq
		if err := r.publish(ts.peer, end); err != nil {
			r.logWarn("Failed to tell node the shell exited", "node", ts.peer, "session_id", ts.sid, "error", err.Error())
		}
	}
}

// coalesce appends output that is already queued to data, up to maxChunk.
func coalesce(data []byte, chunks <-chan []byte) []byte {
	for len(data) < maxChunk {
		select {
		case more, ok := <-chunks:
			if !ok {
				return data
			}
			data = append(data, more...)
		default:
			return data
		}
	}
	return data
}

// handleInput applies input to the shell in sequence order.
func (r *relay) handleInput(ts *targetSession, msg *clusterMessage) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ready, err := ts.in.push(msg)
	for _, in := range ready {
		if in.Input == nil {
			continue
		}
		if werr := applyInput(ts.term.ptmx, *in.Input); werr != nil {
			err = werr
		}
	}
	if err != nil {
		r.logWarn("Ending remote terminal", "session_id", ts.sid, "error", err.Error())
		ts.term.close() // pumpOutput sees the PTY close and sends end
	}
}

// handleStream writes output to the browser in sequence order and closes the
// socket once the target's end arrives.
func (r *relay) handleStream(es *edgeSession, msg *clusterMessage) {
	es.mu.Lock()
	defer es.mu.Unlock()
	ready, err := es.out.push(msg)
	for _, m := range ready {
		if m.Type == msgEnd {
			r.dropEdge(es.sid)
			_ = es.sock.Close()
			return
		}
		if werr := es.sock.WriteMessage(websocket.BinaryMessage, m.Data); werr != nil {
			err = werr
			break
		}
	}
	if err != nil {
		_ = es.sock.Close() // the socket reader sees this and calls closeEdge
	}
}

// handleClose ends whichever half of the session lives here, right away, if
// the close came from its peer.
func (r *relay) handleClose(from string, msg *clusterMessage) {
	r.mu.Lock()
	es := r.edges[msg.SID]
	ts := r.targets[msg.SID]
	r.mu.Unlock()
	if es != nil && es.peer == from {
		r.endEdge(es, "", false)
	}
	if ts != nil && ts.peer == from {
		r.endTarget(ts, false)
	}
}

// closeAll ends every session on deactivation, tells the other nodes and
// stops the heartbeat. Nothing registers afterwards.
func (r *relay) closeAll() {
	r.stopOnce.Do(func() { close(r.stop) })

	r.mu.Lock()
	r.closed = true
	edges, targets := r.edges, r.targets
	r.edges = make(map[string]*edgeSession)
	r.targets = make(map[string]*targetSession)
	r.mu.Unlock()

	for _, es := range edges {
		_ = r.publish(es.peer, es.msg(msgClose))
		_ = es.sock.Close()
	}
	for _, ts := range targets {
		_ = r.publish(ts.peer, ts.msg(msgClose))
		ts.term.close()
	}
}
