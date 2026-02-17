package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"time"

	"github.com/3clabs/xavi/internal/cluster"
	"github.com/3clabs/xavi/internal/config"
	"github.com/3clabs/xavi/internal/container"
	"github.com/3clabs/xavi/internal/secrets"
)

const (
	ConfigPath  = "/etc/tripleclabs/xavi.json"
	SecretsPath = "/etc/tripleclabs/xavi.secrets"
	NetworkName = "xavi-net"
)

// Agent manages the local deployment.
type Agent struct {
	AuthBundle string // Optional initial bundle from CLI
	Config     *config.Config
	Secrets    *secrets.Secrets
	docker     *container.Client
	watcher    *config.Watcher
	node       *cluster.Node
}

// New creates a new Agent.
func New(authBundle string) (*Agent, error) {
	dockerCli, err := container.NewClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	// Load or generate secrets
	sec, err := secrets.LoadOrGenerate(SecretsPath)
	if err != nil {
		// Log error but continue? Or fail?
		// For now fail as we need secrets for DBs
		// check if directory permission issue, maybe try local dir if /etc fails?
		// For skeleton, just fail or log. Let's log and proceed with empty secrets to not crash if perm denied?
		// No, let's return error, clean failure is better.
		// Actually user might run this without sudo on dev machine.
		// Let's try to load from local directory if absolute path fails?
		// Stick to plan: return error.
		return nil, fmt.Errorf("failed to load secrets from %s: %w", SecretsPath, err)
	}

	return &Agent{
		AuthBundle: authBundle,
		Secrets:    sec,
		docker:     dockerCli,
		watcher:    config.NewWatcher(ConfigPath, 5*time.Second),
	}, nil
}

// Run starts the agent loop.
func (a *Agent) Run(ctx context.Context) error {
	defer a.docker.Close()

	// 1. Try to load config from file
	cfg, err := config.LoadFromFile(ConfigPath)
	if err == nil {
		log.Printf("Loaded configuration from %s", ConfigPath)
		a.applyConfig(ctx, cfg)
	} else {
		log.Printf("No config file at %s (or read error): %v", ConfigPath, err)
		// 2. If no file, check CLI bundle
		if a.AuthBundle != "" {
			log.Println("Parsing auth bundle from CLI...")
			cfg, err := config.ParseBundle(a.AuthBundle)
			if err != nil {
				return fmt.Errorf("failed to parse auth bundle: %w", err)
			}
			a.applyConfig(ctx, cfg)
		} else {
			log.Println("Booting in unauthenticated mode. Waiting for configuration...")
		}
	}

	// Start watcher
	go a.watcher.Run(ctx)

	// Polling loop
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case newCfg := <-a.watcher.Updates:
			log.Println("Configuration update received.")
			a.applyConfig(ctx, newCfg)
		case <-ticker.C:
			if a.Config != nil {
				// Poll API (Stub)
				log.Println("Polling API for release updates...")
			}
		}
	}
}

func (a *Agent) applyConfig(ctx context.Context, cfg *config.Config) {
	a.Config = cfg

	// Initialize cluster if config present and not already running
	if a.node == nil && (len(cfg.Cluster.Peers) > 0 || cfg.Cluster.BindPort > 0 || len(cfg.Services) > 0) {
		log.Println("Initializing cluster node...")

		var secretKey []byte
		if a.Secrets != nil && a.Secrets.ClusterKey != "" {
			key, err := base64.StdEncoding.DecodeString(a.Secrets.ClusterKey)
			if err != nil {
				log.Printf("Failed to decode cluster key: %v", err)
			} else {
				secretKey = key
			}
		}

		nodeCfg := cluster.Config{
			BindPort:  cfg.Cluster.BindPort,
			Peers:     cfg.Cluster.Peers,
			Services:  cfg.Services,
			SecretKey: secretKey,
		}
		node, err := cluster.New(nodeCfg)
		if err != nil {
			log.Printf("Failed to initialize cluster: %v", err)
		} else {
			a.node = node
		}
	} else if a.node != nil {
		// TODO: Update node services/peers dynamically?
		// For now simple static init
	}

	// Perform Docker Login if credentials are present
	if cfg.Docker.Username != "" && cfg.Docker.Password != "" && cfg.Docker.Registry != "" {
		log.Printf("Authenticating with registry %s...", cfg.Docker.Registry)
		if err := a.docker.Login(ctx, cfg.Docker.Username, cfg.Docker.Password, cfg.Docker.Registry); err != nil {
			log.Printf("Failed to login to registry: %v", err)
		} else {
			log.Println("Registry authentication successful.")
		}
	}

	log.Println("Agent configuration applied. Ready to manage node.")

	// Trigger immediate reconciliation (stub)
	if err := a.ensureInfrastructure(ctx); err != nil {
		log.Printf("Failed to ensure infrastructure: %v", err)
	}
}

func (a *Agent) ensureInfrastructure(ctx context.Context) error {
	if a.Config == nil {
		return nil
	}
	log.Println("Ensuring core infrastructure is running...")

	if err := a.docker.EnsureNetwork(ctx, NetworkName); err != nil {
		return fmt.Errorf("failed to ensure network: %w", err)
	}

	if err := a.ensureValkey(ctx); err != nil {
		log.Printf("Failed to ensure Valkey: %v", err)
	}

	if err := a.ensurePostgres(ctx); err != nil {
		log.Printf("Failed to ensure Postgres: %v", err)
	}

	if err := a.ensureApp(ctx); err != nil {
		log.Printf("Failed to ensure App: %v", err)
	}

	if err := a.ensureBackupBot(ctx); err != nil {
		log.Printf("Failed to ensure BackupBot: %v", err)
	}

	return nil
}

func (a *Agent) ensureBackupBot(ctx context.Context) error {
	// Runs alongside Postgres
	if !a.isServiceEnabled("postgres") {
		return nil
	}

	image := a.Config.Images.BackupBot
	if image == "" {
		image = "wearecococo/backupbot:latest"
	}

	opts := container.RunOptions{
		Image: image,
		Name:  "xavi-backupbot",
		Env: []string{
			"POSTGRES_HOST=xavi-postgres",
			fmt.Sprintf("POSTGRES_PASSWORD=%s", a.Secrets.PostgresPassword),
		},
		Network: NetworkName,
	}

	return a.docker.RunContainer(ctx, opts)
}

func (a *Agent) isServiceEnabled(name string) bool {
	if len(a.Config.Services) == 0 {
		return true // Default to all if not specified
	}
	for _, s := range a.Config.Services {
		if s == name {
			return true
		}
	}
	return false
}

func (a *Agent) ensureValkey(ctx context.Context) error {
	if !a.isServiceEnabled("valkey") {
		return nil
	}
	if a.Config.Images.Valkey == "" {
		return nil
	}

	// Parse limits (simplistic for now)
	mem := parseMemory(a.Config.Valkey.MaxRAM)
	cpu := parseCPU(a.Config.Valkey.MaxCPU)

	opts := container.RunOptions{
		Image:    a.Config.Images.Valkey,
		Name:     "xavi-valkey",
		Memory:   mem,
		NanoCPUs: cpu,
		Network:  NetworkName,
		Cmd:      []string{"valkey-server", "--requirepass", a.Secrets.ValkeyPassword},
	}

	return a.docker.RunContainer(ctx, opts)
}

func (a *Agent) ensurePostgres(ctx context.Context) error {
	if !a.isServiceEnabled("postgres") {
		return nil
	}
	if a.Config.Images.Postgres == "" {
		return nil
	}

	mem := parseMemory(a.Config.Postgres.MaxRAM)
	cpu := parseCPU(a.Config.Postgres.MaxCPU)

	var mounts []string
	if a.Config.Postgres.StoragePath != "" {
		mounts = append(mounts, fmt.Sprintf("%s:/var/lib/postgresql/data", a.Config.Postgres.StoragePath))
	}

	opts := container.RunOptions{
		Image:    a.Config.Images.Postgres,
		Name:     "xavi-postgres",
		Env:      []string{fmt.Sprintf("POSTGRES_PASSWORD=%s", a.Secrets.PostgresPassword)},
		Mounts:   mounts,
		Memory:   mem,
		NanoCPUs: cpu,
		Network:  NetworkName,
	}

	return a.docker.RunContainer(ctx, opts)
}

func (a *Agent) ensureApp(ctx context.Context) error {
	if !a.isServiceEnabled("app") {
		return nil
	}
	if a.Config.Images.App == "" {
		return nil
	}

	// Determine Postgres Host
	pgHost := "localhost"
	if a.isServiceEnabled("postgres") {
		pgHost = "xavi-postgres" // Local (on same network)
	} else if a.node != nil {
		// Remote discovery
		addr := a.node.FindServiceAddr("postgres")
		if addr != "" {
			pgHost = addr
			log.Printf("Discovered Postgres at %s", addr)
		} else {
			log.Println("Waiting for Postgres to appear in cluster...")
			return nil // Retry later
		}
	}

	// Determine Valkey Host
	valkeyHost := "localhost"
	if a.isServiceEnabled("valkey") {
		valkeyHost = "xavi-valkey"
	} else if a.node != nil {
		addr := a.node.FindServiceAddr("valkey")
		if addr != "" {
			valkeyHost = addr
			log.Printf("Discovered Valkey at %s", addr)
		}
	}

	opts := container.RunOptions{
		Image: a.Config.Images.App,
		Name:  "xavi-app",
		Env: []string{
			fmt.Sprintf("POSTGRES_HOST=%s", pgHost),
			fmt.Sprintf("VALKEY_HOST=%s", valkeyHost),
			fmt.Sprintf("POSTGRES_PASSWORD=%s", a.Secrets.PostgresPassword),
			fmt.Sprintf("VALKEY_PASSWORD=%s", a.Secrets.ValkeyPassword),
		},
		Network: NetworkName,
	}

	return a.docker.RunContainer(ctx, opts)
}

// Helpers for parsing resources (stubs)
func parseMemory(s string) int64 {
	// TODO: fully implement unit parsing (m, g, etc.)
	// For now, return 0 if empty
	return 0
}

func parseCPU(s string) int64 {
	// TODO: fully implement float parsing * 1e9
	return 0
}

func (a *Agent) Close() error {
	return a.docker.Close()
}
