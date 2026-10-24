package main

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

// Cluster-aware terminals.
//
// The browser's terminal websocket lands on whatever node the load balancer
// picks (the "edge"). When the user asked for a different node (the "target"),
// the edge keeps the websocket and the target runs the PTY. The two talk only
// through cluster events addressed to one node (PublishPluginClusterEvent with a
// TargetId), so nothing is broadcast and no node-to-node HTTP is needed:
//
//	browser <-ws-> edge --open/input/close-----> target (PTY)
//	               edge <--output/end/close----- target
//
// Each node knows its own cluster ID (GetNodeID), and the server tells the
// receiver which node sent each event (PluginClusterEvent.SourceId), so the
// envelope carries no addresses.
//
// Input and output are one-way streams; only open and shell listing wait for a
// reply. The reliable cluster channel doesn't keep order: Mattermost Enterprise
// sends each message with its own memberlist SendReliable call (retrying once
// on failure) and handles each received message on its own goroutine. So each
// stream carries a sequence number, and the receiver restores order and drops
// duplicates.

// clusterEventID is the PluginClusterEvent.Id used for every message; the
// envelope's Type says what it is.
const clusterEventID = "shell"

const (
	msgOpen   = "open"   // edge -> target: start a shell (request)
	msgInput  = "input"  // edge -> target: keystrokes / resize (sequenced)
	msgOutput = "output" // target -> edge: PTY bytes (sequenced)
	msgEnd    = "end"    // target -> edge: the shell exited, after the last output (sequenced)
	msgClose  = "close"  // either way: end the session now
	msgShells = "shells" // edge -> target: list shells (request)
	msgReply  = "reply"  // response to a request
	msgPing   = "ping"   // either way: heartbeat for a session
)

const (
	requestTimeout = 10 * time.Second

	// maxReorder bounds how many out-of-order messages a session buffers before
	// giving up on it; a gap this large means a message was lost.
	maxReorder = 1024

	// maxChunk caps how much queued PTY output is merged into one message.
	maxChunk = 32 * 1024
)

// clusterMessage is the envelope carried in PluginClusterEvent.Data.
type clusterMessage struct {
	Type  string `json:"type"`
	ReqID string `json:"req_id,omitempty"` // set on requests and their replies
	SID   string `json:"sid,omitempty"`    // terminal session
	Seq   uint64 `json:"seq,omitempty"`

	UserID string         `json:"user_id,omitempty"`
	Shell  string         `json:"shell,omitempty"`
	Data   []byte         `json:"data,omitempty"`  // PTY output, for msgOutput
	Input  *clientMessage `json:"input,omitempty"` // browser control message, for msgInput

	Error    string      `json:"error,omitempty"`
	Shells   []shellInfo `json:"shells,omitempty"`
	Selected string      `json:"selected,omitempty"`
}

// routeFor decides whether a terminal for target runs on another node, given
// this node's cluster ID. An empty target means this node.
func routeFor(target, self string) (remote bool, err error) {
	switch {
	case target == "":
		return false, nil
	case self == "":
		return false, errors.New("this server is not part of a cluster")
	default:
		return target != self, nil
	}
}

// route decides whether a terminal for target runs on another node.
func (p *Plugin) route(target string) (remote bool, err error) {
	if target == "" {
		return false, nil
	}
	return routeFor(target, p.client.Cluster.GetNodeID())
}

// sequencer releases messages in sequence order, holding back any that arrive
// early and dropping duplicates. It is not safe for concurrent use; callers
// lock around it.
type sequencer struct {
	next    uint64
	pending map[uint64]*clusterMessage
}

func newSequencer() *sequencer {
	return &sequencer{pending: make(map[uint64]*clusterMessage)}
}

// push accepts msg and returns every message that is now ready, in order. It
// errors when too many messages are waiting on a gap.
func (s *sequencer) push(msg *clusterMessage) ([]*clusterMessage, error) {
	if msg.Seq < s.next {
		return nil, nil // duplicate
	}
	s.pending[msg.Seq] = msg
	var ready []*clusterMessage
	for {
		m, ok := s.pending[s.next]
		if !ok {
			break
		}
		delete(s.pending, s.next)
		ready = append(ready, m)
		s.next++
	}
	if len(s.pending) > maxReorder {
		return ready, errors.New("too many out-of-order messages")
	}
	return ready, nil
}

// publishCluster sends one envelope to one node over the reliable cluster
// channel.
func (p *Plugin) publishCluster(target string, msg *clusterMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return p.client.Cluster.PublishPluginEvent(
		model.PluginClusterEvent{Id: clusterEventID, Data: data},
		model.PluginClusterEventSendOptions{
			SendType: model.PluginClusterEventSendTypeReliable,
			TargetId: target,
		},
	)
}

// OnPluginClusterEvent receives envelopes from other nodes.
func (p *Plugin) OnPluginClusterEvent(_ *plugin.Context, ev model.PluginClusterEvent) {
	if ev.Id != clusterEventID || p.relay == nil {
		return
	}
	if ev.SourceId == "" {
		p.API.LogWarn("Dropping cluster message with no sender")
		return
	}
	var msg clusterMessage
	if err := json.Unmarshal(ev.Data, &msg); err != nil {
		p.API.LogWarn("Dropping malformed cluster message", "error", err.Error())
		return
	}
	p.relay.handle(ev.SourceId, &msg)
}
