// Package cluster assembles the configured devices into ready-to-use nodes.
package cluster

import (
	"errors"
	"fmt"
	"sync"

	"github.com/HimanshuSardana/knot/internal/config"
	"github.com/HimanshuSardana/knot/internal/node"
	"github.com/HimanshuSardana/knot/internal/tailnet"
)

// ErrNoPassword means a device wanted a password and nothing supplied one.
var ErrNoPassword = errors.New("password required")

// PasswordStore keeps passwords in memory only. Nothing is ever written to
// disk: a tailnet file manager has no business persisting credentials.
type PasswordStore struct {
	mu     sync.Mutex
	cached map[string]string
	// Prompt is consulted when a password is not cached. Frontends install it
	// (terminal prompt, web form). Nil means passwords are unavailable.
	Prompt func(node string) (string, error)
}

func NewPasswordStore() *PasswordStore {
	return &PasswordStore{cached: map[string]string{}}
}

func (p *PasswordStore) Set(name, password string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cached[name] = password
}

func (p *PasswordStore) Has(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.cached[name]
	return ok
}

func (p *PasswordStore) Clear(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cached, name)
}

// Get returns a cached password, asking the prompt once if needed.
func (p *PasswordStore) Get(name string) (string, error) {
	p.mu.Lock()
	if pw, ok := p.cached[name]; ok {
		p.mu.Unlock()
		return pw, nil
	}
	prompt := p.Prompt
	p.mu.Unlock()

	if prompt == nil {
		return "", ErrNoPassword
	}
	pw, err := prompt(name)
	if err != nil {
		return "", err
	}
	if pw == "" {
		return "", ErrNoPassword
	}
	p.Set(name, pw)
	return pw, nil
}

// Cluster is the set of browsable devices.
type Cluster struct {
	cfg     *config.Config
	devices []tailnet.Device
	nodes   map[string]node.Node
	order   []string

	Passwords *PasswordStore
}

// Open builds nodes for every configured device. Devices are contacted lazily,
// so an offline machine does not prevent knot from starting.
func Open(cfg *config.Config) (*Cluster, error) {
	devices, err := tailnet.Status()
	if err != nil {
		// A missing tailscale binary should not stop a purely local session.
		devices = nil
	}

	c := &Cluster{
		cfg:       cfg,
		devices:   devices,
		nodes:     map[string]node.Node{},
		Passwords: NewPasswordStore(),
	}

	for _, nc := range cfg.Nodes {
		if nc.Name == "" {
			continue
		}
		if _, dup := c.nodes[nc.Name]; dup {
			continue
		}

		if nc.Kind == "local" {
			local, err := node.NewLocal(nc.Name)
			if err != nil {
				return nil, fmt.Errorf("local node %q: %w", nc.Name, err)
			}
			c.nodes[nc.Name] = local
			c.order = append(c.order, nc.Name)
			continue
		}

		addr := nc.Addr
		if addr == "" {
			dev, ok := tailnet.Resolve(devices, nc.Name)
			if !ok || dev.IP == "" {
				// Keep the node anyway: the tailnet may come up later, and an
				// unreachable node with a clear error beats a missing one.
				addr = nc.Name
			} else {
				addr = dev.IP
			}
		}

		name := nc.Name
		c.nodes[name] = node.NewSSH(node.SSHSpec{
			Name: name,
			Addr: addr,
			Port: nc.Port,
			User: nc.User,
			Home: nc.Home,
			Password: func() (string, error) {
				return c.Passwords.Get(name)
			},
		})
		c.order = append(c.order, name)
	}

	return c, nil
}

// Names returns node names in config order.
func (c *Cluster) Names() []string {
	out := make([]string, len(c.order))
	copy(out, c.order)
	return out
}

// Node looks up a node by name.
func (c *Cluster) Node(name string) (node.Node, error) {
	n, ok := c.nodes[name]
	if !ok {
		return nil, fmt.Errorf("unknown node %q", name)
	}
	return n, nil
}

// Nodes returns every node in config order.
func (c *Cluster) Nodes() []node.Node {
	out := make([]node.Node, 0, len(c.order))
	for _, name := range c.order {
		out = append(out, c.nodes[name])
	}
	return out
}

// Devices returns the raw tailnet inventory, for display.
func (c *Cluster) Devices() []tailnet.Device { return c.devices }

// DeviceInfo describes one node for the frontends.
type DeviceInfo struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Home   string `json:"home"`
	Addr   string `json:"addr,omitempty"`
	OS     string `json:"os,omitempty"`
	Online bool   `json:"online"`
	// NeedsPassword is true when the node has no cached password and a prompt
	// is possible, so the UI can offer a login affordance.
	NeedsPassword bool `json:"needsPassword"`
}

// Info summarises the nodes for JSON consumers.
func (c *Cluster) Info() []DeviceInfo {
	out := make([]DeviceInfo, 0, len(c.order))
	for _, name := range c.order {
		n := c.nodes[name]
		info := DeviceInfo{Name: name, Kind: n.Kind(), Home: n.Home(), Online: true}
		if dev, ok := tailnet.Resolve(c.devices, name); ok {
			info.OS = dev.OS
			info.Online = dev.Online
			info.Addr = dev.IP
		} else if n.Kind() == "local" {
			info.Online = true
		} else {
			info.Online = false
		}
		// Only claim a password is needed once the server actually said so.
		if needs, ok := n.(interface{ NeedsAuth() bool }); ok {
			info.NeedsPassword = needs.NeedsAuth()
		}
		out = append(out, info)
	}
	return out
}

// SelfIP returns this machine's tailnet IPv4 address, or "" when unknown.
func (c *Cluster) SelfIP() string {
	for _, d := range c.devices {
		if d.Self {
			return d.IP
		}
	}
	return ""
}

// Close releases every connection.
func (c *Cluster) Close() error {
	var first error
	for _, n := range c.nodes {
		if err := n.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
