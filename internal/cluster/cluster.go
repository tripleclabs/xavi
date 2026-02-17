package cluster

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/hashicorp/memberlist"
)

// NodeMeta contains the metadata broadcast to other nodes.
type NodeMeta struct {
	Services []string `json:"services"`
	// We don't explicitly need IP here as memberlist provides it,
	// but can be useful if we want to advertise a different service IP.
}

// Node represents a member of the cluster.
type Node struct {
	list     *memberlist.Memberlist
	services []string
	events   chan *NodeMeta // Internal event channel (stub for now)
}

// Config for the cluster node.
type Config struct {
	BindPort  int
	Peers     []string
	Services  []string // Services running on this node
	SecretKey []byte   // Encryption key (must be 16, 24, or 32 bytes)
}

// New creates a new cluster node.
func New(cfg Config) (*Node, error) {
	// Use default LAN config as a base
	c := memberlist.DefaultLANConfig()
	c.BindPort = cfg.BindPort
	c.Name = hostname() // Helper to get hostname
	if len(cfg.SecretKey) > 0 {
		c.SecretKey = cfg.SecretKey
	}

	// Custom delegate to manage metadata
	meta := NodeMeta{Services: cfg.Services}
	data, _ := json.Marshal(meta)

	d := &delegate{
		meta: data,
	}
	c.Delegate = d
	c.Events = d // We can implement event delegate here too

	list, err := memberlist.Create(c)
	if err != nil {
		return nil, fmt.Errorf("failed to create memberlist: %w", err)
	}

	if len(cfg.Peers) > 0 {
		_, err := list.Join(cfg.Peers)
		if err != nil {
			log.Printf("Failed to join peers %v: %v", cfg.Peers, err)
		} else {
			log.Printf("Joined cluster via peers: %v", cfg.Peers)
		}
	}

	return &Node{
		list:     list,
		services: cfg.Services,
	}, nil
}

// FindServiceAddr returns the address (IP) of a node running the given service.
// Returns empty string if not found.
func (n *Node) FindServiceAddr(serviceName string) string {
	// 1. Check local first? (Caller should handle this, but harmless)
	for _, s := range n.services {
		if s == serviceName {
			return "127.0.0.1" // Localhost
		}
	}

	// 2. Check remote members
	for _, member := range n.list.Members() {
		if member.Name == n.list.LocalNode().Name {
			continue // Skip self
		}

		var meta NodeMeta
		if err := json.Unmarshal(member.Meta, &meta); err != nil {
			continue // Malformed meta
		}

		for _, s := range meta.Services {
			if s == serviceName {
				return member.Addr.String()
			}
		}
	}

	return ""
}

// Shutdown leaves the cluster.
func (n *Node) Shutdown() error {
	return n.list.Leave(time.Second * 5)
}

func hostname() string {
	h, _ := os.Hostname()
	if h == "" {
		return "unknown-" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return h
}

// Delegate implementation
type delegate struct {
	meta []byte
}

func (d *delegate) NodeMeta(limit int) []byte {
	return d.meta
}

func (d *delegate) NotifyMsg([]byte)                           {}
func (d *delegate) GetBroadcasts(overhead, limit int) [][]byte { return nil }
func (d *delegate) LocalState(join bool) []byte                { return nil }
func (d *delegate) MergeRemoteState(buf []byte, join bool)     {}

// Event delegate stubs (can be expanded)
func (d *delegate) NotifyJoin(node *memberlist.Node) {
	log.Printf("Node joined: %s (%s)", node.Name, node.Addr)
}
func (d *delegate) NotifyLeave(node *memberlist.Node) {
	log.Printf("Node left: %s", node.Name)
}
func (d *delegate) NotifyUpdate(node *memberlist.Node) {}
