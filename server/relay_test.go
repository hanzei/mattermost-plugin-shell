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
	down  map[string]bool // nodes that silently lose everything sent to or from them
}

// partition cuts id off: messages to and from it are silently lost, as when a
// node hangs or the network between nodes fails.
func (c *fakeCluster) partition(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down == nil {
		c.down = make(map[string]bool)
	}
	c.down[id] = true
}

// remove takes id out of the cluster: sends to it fail, as when a node has
// left.
func (c *fakeCluster) remove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.nodes, id)
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
		lost := c.down[id] || c.down[target]
		c.mu.Unlock()
		if !ok {
			return errors.New("unknown node " + target)
		}
		if lost {
			return nil
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

func newTestRelay(t *testing.T, spawn func(string, string) (*terminalSession, shellInfo, error)) *relay {
	r := newRelay()
	r.pingInterval = 20 * time.Millisecond
	r.peerTimeout = 200 * time.Millisecond
	r.allowed = func(userID string) bool { return userID == "admin" }
	r.spawn = spawn
	r.shells = func(string) ([]shellInfo, string) {
		return []shellInfo{{Name: "bash", Path: "/bin/bash"}, {Name: "zsh", Path: "/bin/zsh"}}, "zsh"
	}
	r.logWarn = func(string, ...any) {}
	r.start()
	t.Cleanup(r.closeAll)
	return r
}

// newTestCluster returns an edge relay "a" and a target relay "b".
func newTestCluster(t *testing.T, spawn func(string, string) (*terminalSession, shellInfo, error)) (edge, target *relay, c *fakeCluster) {
	edge, target = newTestRelay(t, nil), newTestRelay(t, spawn)
	c = &fakeCluster{}
	c.add("a", edge)
	c.add("b", target)
	return edge, target, c
}

func sessionCounts(r *relay) (edges, targets int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.edges), len(r.targets)
}

func TestRelayRoundTrip(t *testing.T) {
	edge, target, _ := newTestCluster(t, spawnCommand("cat"))
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
	edge, target, _ := newTestCluster(t, spawnCommand("sh", "-c", "printf 'one two three'"))
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
	edge, _, _ := newTestCluster(t, func(string, string) (*terminalSession, shellInfo, error) {
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
	edge, _, _ := newTestCluster(t, func(string, string) (*terminalSession, shellInfo, error) {
		return nil, shellInfo{}, errors.New("no supported shell found on the host")
	})

	_, _, err := edge.openEdge(&fakeSocket{}, "a", "b", "admin", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no supported shell")
}

func TestRelayOpenUnknownNode(t *testing.T) {
	edge, _, _ := newTestCluster(t, spawnCommand("cat"))

	_, _, err := edge.openEdge(&fakeSocket{}, "a", "nope", "admin", "")
	require.Error(t, err)
	edges, _ := sessionCounts(edge)
	assert.Zero(t, edges)
}

func TestRelayRemoteShells(t *testing.T) {
	edge, _, _ := newTestCluster(t, nil)

	shells, selected, err := edge.remoteShells("a", "b", "admin")
	require.NoError(t, err)
	assert.Len(t, shells, 2)
	assert.Equal(t, "zsh", selected)

	_, _, err = edge.remoteShells("a", "b", "someone")
	require.Error(t, err)
}

func TestRelayCloseAll(t *testing.T) {
	edge, target, _ := newTestCluster(t, spawnCommand("cat"))
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
	edge, target, _ := newTestCluster(t, spawnCommand("sh", "-c", "echo ready; exec cat"))

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

func TestRelayIdleSessionStaysOpen(t *testing.T) {
	edge, target, _ := newTestCluster(t, spawnCommand("cat"))
	sock := &fakeSocket{}

	_, _, err := edge.openEdge(sock, "a", "b", "admin", "")
	require.NoError(t, err)

	// Several timeouts pass with no input or output; the pings keep it alive.
	time.Sleep(5 * edge.peerTimeout)
	_, closed := sock.state()
	assert.False(t, closed)
	edges, _ := sessionCounts(edge)
	_, targets := sessionCounts(target)
	assert.Equal(t, 1, edges)
	assert.Equal(t, 1, targets)
}

func TestRelayPeerFailure(t *testing.T) {
	tests := []struct {
		name    string
		fail    func(c *fakeCluster)
		wantMsg string
	}{
		{name: "target goes silent", fail: func(c *fakeCluster) { c.partition("b") }, wantMsg: "stopped responding"},
		{name: "target leaves the cluster", fail: func(c *fakeCluster) { c.remove("b") }, wantMsg: "unreachable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			edge, target, c := newTestCluster(t, spawnCommand("cat"))
			sock := &fakeSocket{}

			_, _, err := edge.openEdge(sock, "a", "b", "admin", "")
			require.NoError(t, err)

			tt.fail(c)

			// The edge tells the browser and closes the socket.
			require.Eventually(t, func() bool {
				_, closed := sock.state()
				return closed
			}, 5*time.Second, 10*time.Millisecond)
			out, _ := sock.state()
			assert.Contains(t, out, tt.wantMsg)
			edges, _ := sessionCounts(edge)
			assert.Zero(t, edges)

			// The target, cut off from the edge, ends the shell.
			require.Eventually(t, func() bool {
				_, targets := sessionCounts(target)
				return targets == 0
			}, 5*time.Second, 10*time.Millisecond)
		})
	}
}

func TestRelayEdgeFailureEndsShell(t *testing.T) {
	edge, target, c := newTestCluster(t, spawnCommand("cat"))

	_, _, err := edge.openEdge(&fakeSocket{}, "a", "b", "admin", "")
	require.NoError(t, err)

	c.partition("a")

	require.Eventually(t, func() bool {
		_, targets := sessionCounts(target)
		return targets == 0
	}, 5*time.Second, 10*time.Millisecond)
}

func TestRelayPingForUnknownSessionCloses(t *testing.T) {
	edge, target, _ := newTestCluster(t, spawnCommand("cat"))
	sock := &fakeSocket{}

	es, _, err := edge.openEdge(sock, "a", "b", "admin", "")
	require.NoError(t, err)

	// The edge forgets the session without telling the target, as after a
	// restart. The target's next ping gets a close back.
	edge.dropEdge(es.sid)

	require.Eventually(t, func() bool {
		_, targets := sessionCounts(target)
		return targets == 0
	}, 5*time.Second, 10*time.Millisecond)
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

func TestSelfNodeID(t *testing.T) {
	local := localIdentity{
		names:      map[string]bool{"app-1": true},
		ips:        map[string]bool{"10.0.0.1": true, "127.0.0.1": true},
		gossipPort: "8074",
	}

	tests := []struct {
		name    string
		nodes   []clusterNode
		want    string
		wantErr bool
	}{
		{
			name: "default setup advertises the IP as host name",
			nodes: []clusterNode{
				{ID: "n1", Hostname: "10.0.0.1", IPAddress: "10.0.0.1:8074"},
				{ID: "n2", Hostname: "10.0.0.2", IPAddress: "10.0.0.2:8074"},
			},
			want: "n1",
		},
		{
			name: "gossip port tells apart nodes on one host",
			nodes: []clusterNode{
				{ID: "n1", Hostname: "127.0.0.1", IPAddress: "127.0.0.1:8075"},
				{ID: "n2", Hostname: "127.0.0.1", IPAddress: "127.0.0.1:8074"},
			},
			want: "n2",
		},
		{
			name: "falls back to the host name",
			nodes: []clusterNode{
				{ID: "n1", Hostname: "app-1"},
				{ID: "n2", Hostname: "app-2"},
			},
			want: "n1",
		},
		{
			name: "host name with port",
			nodes: []clusterNode{
				{ID: "n1", Hostname: "app-2:8074"},
				{ID: "n2", Hostname: "app-1:8074"},
			},
			want: "n2",
		},
		{
			name: "no match",
			nodes: []clusterNode{
				{ID: "n1", Hostname: "app-2", IPAddress: "10.0.0.2:8074"},
			},
			wantErr: true,
		},
		{
			name: "ambiguous match",
			nodes: []clusterNode{
				{ID: "n1", Hostname: "app-1"},
				{ID: "n2", Hostname: "app-1"},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selfNodeID(tt.nodes, local)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveRoute(t *testing.T) {
	nodes := []clusterNode{{ID: "n1", Hostname: "host-a"}, {ID: "n2", Hostname: "host-b"}}
	local := localIdentity{names: map[string]bool{"host-a": true}}

	self, remote, err := resolveRoute("", nil, local)
	require.NoError(t, err)
	assert.Empty(t, self)
	assert.False(t, remote, "no node requested runs locally")

	self, remote, err = resolveRoute("n1", nodes, local)
	require.NoError(t, err)
	assert.Equal(t, "n1", self)
	assert.False(t, remote, "this node runs locally")

	self, remote, err = resolveRoute("n2", nodes, local)
	require.NoError(t, err)
	assert.Equal(t, "n1", self)
	assert.True(t, remote, "other node is relayed")

	_, _, err = resolveRoute("n2", nil, local)
	assert.Error(t, err, "missing node list")
}
