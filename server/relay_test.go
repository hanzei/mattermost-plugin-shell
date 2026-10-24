package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCluster delivers envelopes between relays the way the server does: as a
// copy, on another goroutine, with no ordering guarantee between messages.
type fakeCluster struct {
	mu    sync.Mutex
	nodes map[string]*relay
}

func (c *fakeCluster) add(id string, r *relay) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.nodes == nil {
		c.nodes = make(map[string]*relay)
	}
	c.nodes[id] = r
	r.publish = func(target string, msg *clusterMessage) error {
		c.mu.Lock()
		dst, ok := c.nodes[target]
		c.mu.Unlock()
		if !ok {
			return errors.New("unknown node " + target)
		}
		data, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		go func() {
			var copied clusterMessage
			if err := json.Unmarshal(data, &copied); err == nil {
				dst.handle(&copied)
			}
		}()
		return nil
	}
}

// fakeSocket records the binary frames written to it.
type fakeSocket struct {
	mu     sync.Mutex
	out    bytes.Buffer
	closed bool
}

func (s *fakeSocket) WriteMessage(_ int, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("closed")
	}
	s.out.Write(data)
	return nil
}

func (s *fakeSocket) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *fakeSocket) state() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.String(), s.closed
}

// spawnCommand returns a spawn func that runs name with args on a PTY.
func spawnCommand(name string, args ...string) func(string, string) (*terminalSession, shellInfo, error) {
	return func(_, _ string) (*terminalSession, shellInfo, error) {
		cmd := exec.Command(name, args...)
		ptmx, err := pty.Start(cmd)
		if err != nil {
			return nil, shellInfo{}, err
		}
		return &terminalSession{id: model.NewId(), ptmx: ptmx, cmd: cmd}, shellInfo{Name: name, Path: name}, nil
	}
}

func newTestRelay(spawn func(string, string) (*terminalSession, shellInfo, error)) *relay {
	r := newRelay()
	r.allowed = func(userID string) bool { return userID == "admin" }
	r.spawn = spawn
	r.shells = func(string) ([]shellInfo, string) {
		return []shellInfo{{Name: "bash", Path: "/bin/bash"}, {Name: "zsh", Path: "/bin/zsh"}}, "zsh"
	}
	r.logWarn = func(string, ...any) {}
	return r
}

// newTestCluster returns an edge relay "a" and a target relay "b".
func newTestCluster(spawn func(string, string) (*terminalSession, shellInfo, error)) (edge, target *relay) {
	edge, target = newTestRelay(nil), newTestRelay(spawn)
	var c fakeCluster
	c.add("a", edge)
	c.add("b", target)
	return edge, target
}

func sessionCounts(r *relay) (edges, targets int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.edges), len(r.targets)
}

func TestRelayRoundTrip(t *testing.T) {
	edge, target := newTestCluster(spawnCommand("cat"))
	sock := &fakeSocket{}

	es, shell, err := edge.openEdge(sock, "a", "b", "admin", "")
	require.NoError(t, err)
	assert.Equal(t, "cat", shell)

	require.NoError(t, edge.sendInput(es, clientMessage{Type: "resize", Cols: 100, Rows: 30}))
	for _, part := range []string{"hel", "lo ", "wor", "ld\n"} {
		require.NoError(t, edge.sendInput(es, clientMessage{Type: "data", Data: part}))
	}

	// The PTY echoes the line and cat repeats it; the parts must arrive in order.
	require.Eventually(t, func() bool {
		out, _ := sock.state()
		return strings.Count(out, "hello world") == 2
	}, 5*time.Second, 10*time.Millisecond)

	edge.closeEdge(es)
	require.Eventually(t, func() bool {
		_, targets := sessionCounts(target)
		return targets == 0
	}, 5*time.Second, 10*time.Millisecond)
	edges, _ := sessionCounts(edge)
	assert.Zero(t, edges)
}

func TestRelayShellExitClosesSocketAfterOutput(t *testing.T) {
	edge, target := newTestCluster(spawnCommand("sh", "-c", "printf 'one two three'"))
	sock := &fakeSocket{}

	_, _, err := edge.openEdge(sock, "a", "b", "admin", "")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, closed := sock.state()
		return closed
	}, 5*time.Second, 10*time.Millisecond)

	out, _ := sock.state()
	assert.Contains(t, out, "one two three")
	edges, _ := sessionCounts(edge)
	_, targets := sessionCounts(target)
	assert.Zero(t, edges)
	assert.Zero(t, targets)
}

func TestRelayOpenForbidden(t *testing.T) {
	spawned := false
	edge, _ := newTestCluster(func(string, string) (*terminalSession, shellInfo, error) {
		spawned = true
		return nil, shellInfo{}, errors.New("unreachable")
	})

	_, _, err := edge.openEdge(&fakeSocket{}, "a", "b", "someone", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden")
	assert.False(t, spawned)
	edges, _ := sessionCounts(edge)
	assert.Zero(t, edges)
}

func TestRelayOpenSpawnError(t *testing.T) {
	edge, _ := newTestCluster(func(string, string) (*terminalSession, shellInfo, error) {
		return nil, shellInfo{}, errors.New("no supported shell found on the host")
	})

	_, _, err := edge.openEdge(&fakeSocket{}, "a", "b", "admin", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no supported shell")
}

func TestRelayOpenUnknownNode(t *testing.T) {
	edge, _ := newTestCluster(spawnCommand("cat"))

	_, _, err := edge.openEdge(&fakeSocket{}, "a", "nope", "admin", "")
	require.Error(t, err)
	edges, _ := sessionCounts(edge)
	assert.Zero(t, edges)
}

func TestRelayRemoteShells(t *testing.T) {
	edge, _ := newTestCluster(nil)

	shells, selected, err := edge.remoteShells("a", "b", "admin")
	require.NoError(t, err)
	assert.Len(t, shells, 2)
	assert.Equal(t, "zsh", selected)

	_, _, err = edge.remoteShells("a", "b", "someone")
	require.Error(t, err)
}

func TestRelayCloseAll(t *testing.T) {
	edge, target := newTestCluster(spawnCommand("cat"))
	sock := &fakeSocket{}

	_, _, err := edge.openEdge(sock, "a", "b", "admin", "")
	require.NoError(t, err)

	// Deactivating the target node ends the shell and closes the browser socket.
	target.closeAll()
	require.Eventually(t, func() bool {
		_, closed := sock.state()
		return closed
	}, 5*time.Second, 10*time.Millisecond)
	edges, _ := sessionCounts(edge)
	assert.Zero(t, edges)
}

func TestRelayOrphanedShellIsClosed(t *testing.T) {
	edge, target := newTestCluster(spawnCommand("sh", "-c", "echo ready; exec cat"))

	// An open arriving at the target with no edge session waiting for it, as
	// when the edge's request timed out first.
	target.handle(&clusterMessage{Type: msgOpen, From: "a", To: "b", SID: model.NewId(), UserID: "admin"})

	// Its first output reaches the edge, which knows no such session and tells
	// the target to end it.
	require.Eventually(t, func() bool {
		_, targets := sessionCounts(target)
		return targets == 0
	}, 5*time.Second, 10*time.Millisecond)
	edges, _ := sessionCounts(edge)
	assert.Zero(t, edges)
}

func TestSequencer(t *testing.T) {
	s := newSequencer[string]()

	ready, err := s.push(1, "b")
	require.NoError(t, err)
	assert.Empty(t, ready)

	ready, err = s.push(0, "a")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, ready)

	ready, err = s.push(0, "dup")
	require.NoError(t, err)
	assert.Empty(t, ready)

	ready, err = s.push(2, "c")
	require.NoError(t, err)
	assert.Equal(t, []string{"c"}, ready)
}

func TestSequencerGapOverflow(t *testing.T) {
	s := newSequencer[uint64]()
	var err error
	for i := uint64(1); i <= maxReorder+1 && err == nil; i++ {
		_, err = s.push(i, i)
	}
	assert.Error(t, err)
}

func TestParseNodes(t *testing.T) {
	nodes := parseNodes("id1:host-a, id2:host-b,broken,:nohost,noid:,id3:host-c")
	assert.Equal(t, []clusterNode{
		{ID: "id1", Hostname: "host-a"},
		{ID: "id2", Hostname: "host-b"},
		{ID: "id3", Hostname: "host-c"},
	}, nodes)
	assert.Empty(t, parseNodes(""))
}

func TestResolveRoute(t *testing.T) {
	nodes := []clusterNode{{ID: "n1", Hostname: "host-a"}, {ID: "n2", Hostname: "host-b"}}

	tests := []struct {
		name       string
		target     string
		nodes      []clusterNode
		hostname   string
		wantSelf   string
		wantRemote bool
		wantErr    bool
	}{
		{name: "no node requested runs locally", hostname: "host-a"},
		{name: "this node runs locally", target: "n1", nodes: nodes, hostname: "host-a", wantSelf: "n1"},
		{name: "other node is relayed", target: "n2", nodes: nodes, hostname: "host-a", wantSelf: "n1", wantRemote: true},
		{name: "unknown local host is an error", target: "n2", nodes: nodes, hostname: "host-z", wantErr: true},
		{name: "missing node list is an error", target: "n2", hostname: "host-a", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			self, remote, err := resolveRoute(tt.target, tt.nodes, tt.hostname)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantSelf, self)
			assert.Equal(t, tt.wantRemote, remote)
		})
	}
}
