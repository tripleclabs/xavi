package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
)

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
	App       int `json:"app,omitempty"`
	Valkey    int `json:"valkey,omitempty"`
	Postgres  int `json:"postgres,omitempty"`
	BackupBot int `json:"backupbot,omitempty"`
	Caddy     int `json:"caddy,omitempty"`
}

type App struct {
	License string `json:"license,omitempty"`
}

type CaddyConfig struct {
	Domain string `json:"domain"`
}

type ValkeyConfig struct {
	MaxRAM string `json:"max_ram"` // e.g. "512m", "1g"
	MaxCPU string `json:"max_cpu"` // e.g. "0.5", "2"
}

type PostgresConfig struct {
	MaxRAM      string              `json:"max_ram"`
	MaxCPU      string              `json:"max_cpu"`
	StoragePath string              `json:"storage_path"`
	Replication PostgresReplication `json:"replication,omitempty"`
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
