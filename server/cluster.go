package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
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
//	browser <-ws-> edge --open/input/close--> target (PTY)
//	               edge <--output/close------ target
//
// Input and output are one-way streams; only open and shell listing wait for a
// reply. Each stream carries a sequence number so the receiver can restore order
// whatever the cluster transport guarantees.

// clusterEventID is the PluginClusterEvent.Id used for every message; the
// envelope's Type says what it is.
const clusterEventID = "shell"

const (
	msgOpen        = "open"   // edge -> target: start a shell (request)
	msgInput       = "input"  // edge -> target: keystrokes / resize
	msgOutput      = "output" // target -> edge: PTY bytes
	msgClose       = "close"  // either way: session is over
	msgShells      = "shells" // edge -> target: list shells (request)
	msgReply       = "reply"  // response to a request
	requestTimeout = 10 * time.Second

	// maxReorder bounds how many out-of-order messages a session buffers before
	// giving up on it; a gap this large means a message was lost.
	maxReorder = 1024
)

// clusterMessage is the envelope carried in PluginClusterEvent.Data.
type clusterMessage struct {
	Type  string `json:"type"`
	From  string `json:"from"`             // sender's cluster node ID, so the receiver can answer
	To    string `json:"to,omitempty"`     // addressee's node ID, on requests; the target learns its own ID from it
	ReqID string `json:"req_id,omitempty"` // set on requests and their replies
	SID   string `json:"sid,omitempty"`    // terminal session
	Seq   uint64 `json:"seq,omitempty"`

	UserID string `json:"user_id,omitempty"`
	Shell  string `json:"shell,omitempty"`
	Data   []byte `json:"data,omitempty"`
	Cols   uint16 `json:"cols,omitempty"`
	Rows   uint16 `json:"rows,omitempty"`
	Input  string `json:"input,omitempty"` // "data" | "resize", for msgInput

	Error    string      `json:"error,omitempty"`
	Shells   []shellInfo `json:"shells,omitempty"`
	Selected string      `json:"selected,omitempty"`
}

// clusterNode is one entry of /api/v4/cluster/status, which the webapp passes
// along as the "nodes" query parameter.
type clusterNode struct {
	ID        string `json:"id"`
	Hostname  string `json:"hostname"`
	IPAddress string `json:"ipaddress"` // gossip address, ip:port
}

// localIdentity is what this host knows about itself, to find itself in the
// node list.
type localIdentity struct {
	names      map[string]bool // host name, configured override/advertise names
	ips        map[string]bool // local interface addresses
	gossipPort string
}

// splitHost strips an optional port from addr.
func splitHost(addr string) (host, port string) {
	if h, p, err := net.SplitHostPort(addr); err == nil {
		return h, p
	}
	return addr, ""
}

// selfNodeID finds this node in the cluster's node list. The plugin API can't
// report the node ID yet, so it matches what the cluster advertises for each
// node: first the gossip address against local interfaces and the gossip port
// (which also tells apart nodes sharing a host), then the advertised name,
// which is the host name, an IP or ClusterSettings.OverrideHostname.
func selfNodeID(nodes []clusterNode, local localIdentity) (string, error) {
	var byAddr, byName []string
	for _, n := range nodes {
		if ip, port := splitHost(n.IPAddress); local.ips[ip] && (local.gossipPort == "" || port == local.gossipPort) {
			byAddr = append(byAddr, n.ID)
		}
		if name, _ := splitHost(n.Hostname); local.names[name] || local.ips[name] {
			byName = append(byName, n.ID)
		}
	}
	for _, matches := range [][]string{byAddr, byName} {
		switch len(matches) {
		case 0:
			continue
		case 1:
			return matches[0], nil
		default:
			return "", errors.New("several cluster nodes match this host")
		}
	}
	return "", errors.New("this host does not match any cluster node")
}

// resolveRoute decides whether a request for target runs locally. It returns
// the local node ID when the session must be relayed.
func resolveRoute(target string, nodes []clusterNode, local localIdentity) (self string, remote bool, err error) {
	if target == "" {
		return "", false, nil
	}
	self, err = selfNodeID(nodes, local)
	if err != nil {
		return "", false, fmt.Errorf("could not identify this node in the cluster node list: %w", err)
	}
	return self, self != target, nil
}

// clusterRoute reads the node and nodes query parameters for a request.
func (p *Plugin) clusterRoute(target, rawNodes string) (self string, remote bool, err error) {
	if target == "" {
		return "", false, nil
	}
	var nodes []clusterNode
	if err := json.Unmarshal([]byte(rawNodes), &nodes); err != nil {
		return "", false, fmt.Errorf("invalid nodes parameter: %w", err)
	}
	return resolveRoute(target, nodes, p.localIdentity())
}

// localIdentity collects this host's names and addresses.
func (p *Plugin) localIdentity() localIdentity {
	local := localIdentity{names: make(map[string]bool), ips: make(map[string]bool)}
	if hostname, err := os.Hostname(); err == nil {
		local.names[hostname] = true
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok {
				local.ips[ipNet.IP.String()] = true
			}
		}
	}
	if cfg := p.client.Configuration.GetConfig(); cfg != nil {
		cs := cfg.ClusterSettings
		for _, name := range []*string{cs.OverrideHostname, cs.AdvertiseAddress} {
			if name != nil && *name != "" {
				host, _ := splitHost(*name)
				local.names[host] = true
			}
		}
		if cs.GossipPort != nil {
			local.gossipPort = strconv.Itoa(*cs.GossipPort)
		}
	}
	return local
}

// sequencer releases items in sequence order, holding back any that arrive
// early. It is not safe for concurrent use; callers lock around it.
type sequencer[T any] struct {
	next    uint64
	pending map[uint64]T
}

func newSequencer[T any]() *sequencer[T] {
	return &sequencer[T]{pending: make(map[uint64]T)}
}

// push accepts the item with sequence number seq and returns every item that is
// now ready, in order. It errors when too many items are waiting on a gap.
func (s *sequencer[T]) push(seq uint64, item T) ([]T, error) {
	if seq < s.next {
		return nil, nil // duplicate
	}
	s.pending[seq] = item
	var ready []T
	for {
		it, ok := s.pending[s.next]
		if !ok {
			break
		}
		delete(s.pending, s.next)
		ready = append(ready, it)
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
	return p.API.PublishPluginClusterEvent(
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
	var msg clusterMessage
	if err := json.Unmarshal(ev.Data, &msg); err != nil {
		p.API.LogWarn("Dropping malformed cluster message", "error", err.Error())
		return
	}
	p.relay.handle(&msg)
}
