package agent

import (
	"context"
	"fmt"
	"log"
	"time"

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

	// Example: Ensure Caddy is running if configured
	// if a.Config.Images.Caddy != "" {
	// 	 return a.docker.RunContainer(ctx, a.Config.Images.Caddy, "xavi-caddy")
	// }

	if err := a.ensureValkey(ctx); err != nil {
		log.Printf("Failed to ensure Valkey: %v", err)
	}

	if err := a.ensurePostgres(ctx); err != nil {
		log.Printf("Failed to ensure Postgres: %v", err)
	}

	return nil
}

func (a *Agent) ensureValkey(ctx context.Context) error {
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
	}

	return a.docker.RunContainer(ctx, opts)
}

func (a *Agent) ensurePostgres(ctx context.Context) error {
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
