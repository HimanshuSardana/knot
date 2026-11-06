// Package config loads and persists knot's node list.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// NodeConfig is one device knot can browse.
type NodeConfig struct {
	Name string `json:"name"`
	// Kind is "local" or "ssh".
	Kind string `json:"kind"`
	// Addr is a hostname or IP. When empty, knot resolves the node name through
	// the tailnet, so devices can move addresses without editing this file.
	Addr string `json:"addr,omitempty"`
	User string `json:"user,omitempty"`
	Port int    `json:"port,omitempty"`
	Home string `json:"home,omitempty"`
}

// WebConfig controls the HTTP frontend.
type WebConfig struct {
	// Listen is the address the web UI binds to. Defaults to the tailnet only.
	Listen string `json:"listen"`
	// Token is the shared secret required by the web UI. Empty means one is
	// generated at startup and printed once.
	Token string `json:"token,omitempty"`
}

type Config struct {
	Nodes []NodeConfig `json:"nodes"`
	Web   WebConfig    `json:"web"`
}

// Dir returns knot's config directory, honouring XDG_CONFIG_HOME.
func Dir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "knot")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".config", "knot")
}

// Path returns the config file location.
func Path() string { return filepath.Join(Dir(), "config.json") }

// Load reads the config, writing a starter file the first time knot runs.
func Load() (*Config, error) {
	data, err := os.ReadFile(Path())
	if os.IsNotExist(err) {
		cfg := Default()
		if werr := cfg.Save(); werr != nil {
			return nil, werr
		}
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", Path(), err)
	}
	cfg.normalise()
	return &cfg, nil
}

func (c *Config) Save() error {
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path(), append(data, '\n'), 0o600)
}

func (c *Config) normalise() {
	for i := range c.Nodes {
		n := &c.Nodes[i]
		if n.Kind == "" {
			if n.Addr == "" && n.User == "" {
				n.Kind = "local"
			} else {
				n.Kind = "ssh"
			}
		}
		if n.Kind == "ssh" && n.Port == 0 {
			n.Port = 22
		}
	}
	if c.Web.Listen == "" {
		c.Web.Listen = ":8787"
	}
}

// Default returns a config pre-filled with this machine plus the devices found
// on a typical tailnet. Addresses are left empty so they resolve dynamically.
func Default() *Config {
	host, _ := os.Hostname()
	cfg := &Config{
		Web: WebConfig{Listen: ":8787"},
		Nodes: []NodeConfig{
			{Name: host, Kind: "local"},
			{Name: "thinkpad", Kind: "ssh", User: "himanshu", Port: 22},
			{Name: "vps", Kind: "ssh", User: "himanshu", Port: 22},
			{Name: "phone", Kind: "ssh", User: "u0_a430", Port: 8022, Home: "/data/data/com.termux/files/home"},
		},
	}
	if host == "" {
		cfg.Nodes[0].Name = "local"
	}
	return cfg
}
