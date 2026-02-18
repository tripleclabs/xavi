package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

// UIDSpec holds a uid or uid:gid pair and accepts either a JSON string ("101" or
// "101:103") or a plain JSON integer (101) for backwards compatibility.
type UIDSpec string

func (u *UIDSpec) UnmarshalJSON(data []byte) error {
	// Try string first ("101" or "101:103")
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*u = UIDSpec(s)
		return nil
	}
	// Fall back to integer (101)
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("uid spec must be a string or integer, got %s", data)
	}
	*u = UIDSpec(strconv.Itoa(n))
	return nil
}

// Config represents the Xavi configuration.
type Config struct {
	Control       Control        `json:"control"`
	Docker        Docker         `json:"docker"`
	Images        Images         `json:"images,omitempty"`
	ContainerUIDs ContainerUIDs  `json:"container_uids,omitempty"`
	App           App            `json:"app,omitempty"`
	Valkey        ValkeyConfig   `json:"valkey,omitempty"`
	Postgres      PostgresConfig `json:"postgres,omitempty"`
	Cluster       ClusterConfig  `json:"cluster,omitempty"`
	Caddy         CaddyConfig    `json:"caddy,omitempty"`
	Services      []string       `json:"services,omitempty"` // Explicit list of services to run locally
}

type ClusterConfig struct {
	BindPort int      `json:"bind_port"`
	Peers    []string `json:"peers"`
}

type Control struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

type Docker struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Registry string `json:"registry"`
}

type Images struct {
	App       string `json:"app,omitempty"`
	Valkey    string `json:"valkey,omitempty"`
	Postgres  string `json:"postgres,omitempty"`
	BackupBot string `json:"backupbot,omitempty"`
	Caddy     string `json:"caddy,omitempty"`
}

type ContainerUIDs struct {
	App       UIDSpec `json:"app,omitempty"`
	Valkey    UIDSpec `json:"valkey,omitempty"`
	Postgres  UIDSpec `json:"postgres,omitempty"`
	BackupBot UIDSpec `json:"backupbot,omitempty"`
	Caddy     UIDSpec `json:"caddy,omitempty"`
}

type App struct {
	License                    string `json:"license,omitempty"`
	MaxConnectionsPerEventLoop int    `json:"max_connections_per_event_loop,omitempty"` // Default: 2
	MaxRAM                     string `json:"max_ram,omitempty"`
	MaxCPU                     string `json:"max_cpu,omitempty"`
}

type CaddyConfig struct {
	Domain string `json:"domain"`
}

type ValkeyConfig struct {
	MaxRAM string `json:"max_ram"` // e.g. "512m", "1g"
	MaxCPU string `json:"max_cpu"` // e.g. "0.5", "2"
}

type PostgresConfig struct {
	MaxRAM         string              `json:"max_ram"`
	MaxCPU         string              `json:"max_cpu"`
	MaxConnections int                 `json:"max_connections,omitempty"` // Default: 100
	StoragePath    string              `json:"storage_path"`
	Replication    PostgresReplication `json:"replication,omitempty"`
}

type PostgresReplication struct {
	Mode string `json:"mode"` // "single" (default) or "cluster"
	Role string `json:"role"` // "primary" or "secondary"
}

// ParseBundle parses a base64 encoded JSON configuration bundle.
func ParseBundle(base64String string) (*Config, error) {
	decoded, err := base64.StdEncoding.DecodeString(base64String)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64 bundle: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(decoded, &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config json: %w", err)
	}

	return &cfg, nil
}

// LoadFromFile loads configuration from a JSON file.
func LoadFromFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config json from file: %w", err)
	}

	return &cfg, nil
}
