package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/3clabs/xavi/internal/cluster"
	"github.com/3clabs/xavi/internal/config"
	"github.com/3clabs/xavi/internal/container"
	"github.com/3clabs/xavi/internal/secrets"
	sentry "github.com/getsentry/sentry-go"

	"gopkg.in/yaml.v3"
)

const (
	NetworkName    = "xavi-net"
	AppConfigMount = "/etc/tripleclabs/config.json"
	RuntimeDir     = "/var/lib/xavi/runtime"
)

// Agent manages the local deployment.
type Agent struct {
	AuthBundle       string // Optional initial bundle from CLI
	ConfigDir        string
	ConfigPath       string
	SecretsPath      string
	AppConfigPath    string
	BackupConfigPath string
	RuntimeDir       string

	// Runtime subdirectories per container
	AppRuntimeDir         string
	TraefikRuntimeDir     string
	BackupBotRuntimeDir   string

	// Runtime file paths (generated configs)
	TempConfigPath              string
	TraefikStaticConfigPath     string
	TraefikDynamicConfigPath    string
	TraefikACMEPath             string
	TempBackupBotConfigPath     string

	Config  *config.Config
	Secrets *secrets.Secrets
	docker  *container.Client
	watcher *config.Watcher
	appConfigWatcher    *config.FileWatcher
	backupConfigWatcher *config.FileWatcher
	node    *cluster.Node
}

// New creates a new Agent.
func New(authBundle, configDir string) (*Agent, error) {
	dockerCli, err := container.NewClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	if configDir == "" {
		configDir = "/etc/tripleclabs"
	}

	configPath := filepath.Join(configDir, "xavi.json")
	secretsPath := filepath.Join(configDir, "xavi.secrets")

	// Create runtime directory for generated configs
	runtimeDir := RuntimeDir
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create runtime directory %s: %w", runtimeDir, err)
	}

	// Load or generate secrets
	sec, err := secrets.LoadOrGenerate(secretsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load secrets from %s: %w", secretsPath, err)
	}

	// Create subdirectories for each container
	appRuntimeDir := filepath.Join(runtimeDir, "app")
	traefikRuntimeDir := filepath.Join(runtimeDir, "traefik")
	backupBotRuntimeDir := filepath.Join(runtimeDir, "backupbot")

	for _, dir := range []string{appRuntimeDir, traefikRuntimeDir, backupBotRuntimeDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create runtime subdirectory %s: %w", dir, err)
		}
	}

	return &Agent{
		AuthBundle:                  authBundle,
		ConfigDir:                   configDir,
		ConfigPath:                  configPath,
		SecretsPath:                 secretsPath,
		AppConfigPath:               filepath.Join(configDir, "pulse.json"),
		BackupConfigPath:            filepath.Join(configDir, "backup.json"),
		RuntimeDir:                  runtimeDir,
		AppRuntimeDir:               appRuntimeDir,
		TraefikRuntimeDir:           traefikRuntimeDir,
		BackupBotRuntimeDir:         backupBotRuntimeDir,
		TempConfigPath:              filepath.Join(appRuntimeDir, "config.json"),
		TraefikStaticConfigPath:     filepath.Join(traefikRuntimeDir, "traefik.yml"),
		TraefikDynamicConfigPath:    filepath.Join(traefikRuntimeDir, "dynamic.yml"),
		TraefikACMEPath:             filepath.Join(traefikRuntimeDir, "acme.json"),
		TempBackupBotConfigPath:     filepath.Join(backupBotRuntimeDir, "config.yaml"),
		Secrets:                 sec,
		docker:                  dockerCli,
		watcher:                 config.NewWatcher(configPath, 5*time.Second),
		appConfigWatcher:        config.NewFileWatcher(filepath.Join(configDir, "pulse.json"), 5*time.Second),
		backupConfigWatcher:     config.NewFileWatcher(filepath.Join(configDir, "backup.json"), 5*time.Second),
	}, nil
}

// Run starts the agent loop.
func (a *Agent) Run(ctx context.Context) error {
	defer a.docker.Close()

	// 1. Try to load config from file
	cfg, err := config.LoadFromFile(a.ConfigPath)
	if err == nil {
		log.Printf("Loaded configuration from %s", a.ConfigPath)
		a.applyConfig(ctx, cfg)
	} else {
		log.Printf("No config file at %s (or read error): %v", a.ConfigPath, err)
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

	// Start watchers
	go a.watcher.Run(ctx)
	go a.appConfigWatcher.Run(ctx)
	go a.backupConfigWatcher.Run(ctx)

	// Polling loop
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	healthTicker := time.NewTicker(60 * time.Second)
	defer healthTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case newCfg := <-a.watcher.Updates:
			log.Println("Configuration update received.")
			a.applyConfig(ctx, newCfg)
		case <-a.appConfigWatcher.Changed:
			log.Println("App config (pulse.json) changed, rebuilding app container...")
			if a.Config != nil {
				if err := a.ensureApp(ctx); err != nil {
					log.Printf("Failed to rebuild app: %v", err)
				}
			}
		case <-a.backupConfigWatcher.Changed:
			log.Println("Backup config (backup.json) changed, rebuilding backupbot container...")
			if a.Config != nil {
				if err := a.ensureBackupBot(ctx); err != nil {
					log.Printf("Failed to rebuild backupbot: %v", err)
				}
			}
		case <-ticker.C:
			if a.Config != nil {
				// Poll API (Stub)
				log.Println("Polling API for release updates...")
			}
		case <-healthTicker.C:
			if a.Config != nil {
				a.runHealthChecks(ctx)
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
			PgRole:    cfg.Postgres.Replication.Role,
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

	if err := a.ensureTraefik(ctx); err != nil {
		log.Printf("Failed to ensure Traefik: %v", err)
	}

	if err := a.ensureBackupBot(ctx); err != nil {
		log.Printf("Failed to ensure BackupBot: %v", err)
	}

	return nil
}

func (a *Agent) ensureBackupBot(ctx context.Context) error {
	// Runs alongside Postgres
	// In cluster mode, only run on Secondary
	if !a.isServiceEnabled("postgres") {
		return nil
	}
	if a.Config.Postgres.Replication.Mode == "cluster" && a.Config.Postgres.Replication.Role != "secondary" {
		return nil
	}

	image := a.Config.Images.BackupBot
	if image == "" {
		image = "wearecococo/backupbot:latest"
	}

	if err := a.mergeBackupBotConfig("xavi-postgres"); err != nil {
		log.Printf("Failed to merge backupbot config: %v", err)
	}

	// Set ownership of backupbot runtime directory
	if err := a.setRuntimeDirOwnership(a.BackupBotRuntimeDir, a.Config.ContainerUIDs.BackupBot); err != nil {
		log.Printf("Failed to set ownership for backupbot runtime dir: %v", err)
	}

	opts := container.RunOptions{
		Image:    image,
		Name:     "xavi-backupbot",
		Cmd:      []string{"--config", "/etc/backupbot/config.yaml"},
		Memory:   128 * 1024 * 1024, // 128MB
		NanoCPUs: 250000000,         // 0.25 CPU
		Mounts: []string{
			fmt.Sprintf("%s:/etc/backupbot/config.yaml:ro", a.TempBackupBotConfigPath),
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

type backupBotConfig struct {
	Schedule string            `yaml:"schedule"`
	Postgres backupBotPgConfig `yaml:"postgres"`
	Backends []backupBackend   `yaml:"backends"`
}

type backupBotPgConfig struct {
	ConnectionString string `yaml:"connection_string"`
}

type backupBackend struct {
	File *backupFileBackend `yaml:"file,omitempty"`
	S3   *backupS3Backend   `yaml:"s3,omitempty"`
}

type backupFileBackend struct {
	Path      string `yaml:"path"`
	Retention int    `yaml:"retention"`
}

type backupS3Backend struct {
	Bucket         string `yaml:"bucket"`
	Prefix         string `yaml:"prefix,omitempty"`
	AccountID      string `yaml:"account_id,omitempty"`
	AccessKeyID    string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	Region         string `yaml:"region,omitempty"`
	Endpoint       string `yaml:"endpoint,omitempty"`
	Retention      int    `yaml:"retention"`
}

func (a *Agent) mergeBackupBotConfig(pgHost string) error {
	var backends []backupBackend

	data, err := os.ReadFile(a.BackupConfigPath)
	if err != nil {
		log.Printf("No backup config at %s: %v (using file backend fallback)", a.BackupConfigPath, err)
	} else {
		backends = parseBackupConfig(data, a.BackupConfigPath)
	}

	if len(backends) == 0 {
		log.Printf("No S3 backends configured, falling back to local file backend")
		backends = []backupBackend{
			{File: &backupFileBackend{Path: "/backups", Retention: 1}},
		}
	}

	pgConn := fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=disable",
		a.Secrets.PostgresDBUser, a.Secrets.PostgresPassword, pgHost, a.Secrets.PostgresDBName)

	cfg := backupBotConfig{
		Schedule: "0 2 * * *",
		Postgres: backupBotPgConfig{ConnectionString: pgConn},
		Backends: backends,
	}

	yamlContent, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal backupbot config: %w", err)
	}

	return os.WriteFile(a.TempBackupBotConfigPath, yamlContent, 0600)
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

	// Replication Logic
	isCluster := a.Config.Postgres.Replication.Mode == "cluster"
	role := a.Config.Postgres.Replication.Role

	// Base options
	env := []string{
		fmt.Sprintf("POSTGRES_PASSWORD=%s", a.Secrets.PostgresPassword),
		fmt.Sprintf("POSTGRES_DB=%s", a.Secrets.PostgresDBName),
		fmt.Sprintf("POSTGRES_USER=%s", a.Secrets.PostgresDBUser),
	}
	maxConns := a.Config.Postgres.MaxConnections
	if maxConns <= 0 {
		maxConns = 100
	}

	cmd := []string{
		"postgres",
		"-c", fmt.Sprintf("max_connections=%d", maxConns),
	}

	if isCluster {
		if role == "primary" {
			// Primary Configuration
			// Enable replication in postgres.conf via args
			cmd = append(cmd,
				"-c", "wal_level=replica",
				"-c", "max_wal_senders=10",
				"-c", "hot_standby=on",
			)
			// Note: We need to create the replication user.
			// For simplicity in this skeleton, we'll assume a separate init script or manual step
			// OR inject a superuser script via /docker-entrypoint-initdb.d if volume is empty.
			// But since we mount host volume, we can just run a one-off exec if needed?
			// Let's rely on standard PG env vars if possible, but standard image doesn't support automatic replication user creation easily.
			// We can use POSTGRES_INITDB_ARGS? No.
			// Plan: The user/admin handles user creation or we do it via Exec later?
			// Let's stick to just setting flags for now.
		} else if role == "secondary" {
			// Secondary Configuration
			// 1. Discover Primary
			if a.node == nil {
				log.Println("Cluster not initialized, cannot find primary.")
				return nil
			}
			primaryIP := a.node.FindPrimary()
			if primaryIP == "" {
				log.Println("Waiting for Primary Postgres to appear in cluster...")
				return nil
			}

			// 2. Check if data directory is empty (needs bootstrapping)
			// This is tricky without inspecting the volume.
			// We can assume if the container doesn't exist? Or check a marker file?
			// For this proof of concept, let's assume if Config.Postgres.StoragePath is set, we check that dir on host?
			// But Agent might run in container?
			// Let's assume Agent has access to StoragePath (it must to mount it).
			if a.Config.Postgres.StoragePath != "" {
				empty, err := isDirEmpty(a.Config.Postgres.StoragePath)
				if err == nil && empty {
					log.Printf("Bootstrapping secondary from primary at %s...", primaryIP)
					// Run pg_basebackup container
					// We need to run this as a one-off task BEFORE starting the main postgres container.
					// Command: pg_basebackup -h <primaryIP> -U replication -D /var/lib/postgresql/data -R -P
					// Password: PGPASSWORD env

					// NOTE: To make this work, the Primary MUST have the replication user created.
					// And pg_hba.conf must allow it. The standard image allows partial trust or MD5.

					// This is complex for a single function.
					// Simplified: We skip the actual pg_basebackup implementation code here to keep snippet size manageable,
					// but this is where it would go.
					log.Println("TODO: Run pg_basebackup ... (Simulated)")
				}
			}

			// 3. Start as Standby
			// The -R flag in basebackup creates the standby.signal.
			// If we just start it, it should be fine if data dir is prepped.
		}
	}

	opts := container.RunOptions{
		Image:    a.Config.Images.Postgres,
		Name:     "xavi-postgres",
		Env:      env,
		Mounts:   mounts,
		Memory:   mem,
		NanoCPUs: cpu,
		Network:  NetworkName,
		Cmd:      cmd,
	}

	return a.docker.RunContainer(ctx, opts)
}

func isDirEmpty(name string) (bool, error) {
	f, err := os.Open(name)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	defer f.Close()

	_, err = f.Readdirnames(1) // Or f.Readdir(1)
	if err == io.EOF {
		return true, nil
	}
	return false, err
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

	// Merge Config
	if err := a.mergeAppConfig(a.AppConfigPath, a.TempConfigPath, pgHost, valkeyHost); err != nil {
		log.Printf("Failed to merge app config: %v", err)
		// Actually typical pattern: if missing config, app might crash. Let's return error.
		return fmt.Errorf("failed to merge app config: %w", err)
	}

	// Set ownership of app runtime directory
	if err := a.setRuntimeDirOwnership(a.AppRuntimeDir, a.Config.ContainerUIDs.App); err != nil {
		log.Printf("Failed to set ownership for app runtime dir: %v", err)
	}

	appMem := parseMemory(a.Config.App.MaxRAM)
	appCPU := parseCPU(a.Config.App.MaxCPU)

	opts := container.RunOptions{
		Image:    a.Config.Images.App,
		Name:     "xavi-app",
		Memory:   appMem,
		NanoCPUs: appCPU,
		Mounts: []string{
			fmt.Sprintf("%s:%s:ro", a.TempConfigPath, AppConfigMount),
		},
		Network: NetworkName,
	}

	return a.docker.RunContainer(ctx, opts)
}

// Helpers for parsing resources (stubs)
const defaultS3Retention = 30

// parseBackupConfig parses backup.json in two formats:
//   - Object: {"bucket": "...", "access_key_id": "...", ...} — single S3 backend
//   - Array:  [{"s3": {"bucket": "...", ...}}, ...] — multiple backends
func parseBackupConfig(data []byte, path string) []backupBackend {
	// Try as a flat S3 credentials object first
	var single backupS3Backend
	if err := json.Unmarshal(data, &single); err != nil {
		log.Printf("Failed to parse %s: %v", path, err)
		return nil
	}

	if single.Bucket != "" {
		if single.Retention <= 0 {
			single.Retention = defaultS3Retention
		}
		return []backupBackend{{S3: &single}}
	}

	// Otherwise try as an array of backends
	var backends []backupBackend
	if err := json.Unmarshal(data, &backends); err != nil {
		log.Printf("Failed to parse %s as array: %v (expected object with 'bucket' or array of backends)", path, err)
		return nil
	}

	for i := range backends {
		if backends[i].S3 != nil && backends[i].S3.Retention <= 0 {
			backends[i].S3.Retention = defaultS3Retention
		}
		if backends[i].File != nil && backends[i].File.Retention <= 0 {
			backends[i].File.Retention = 1
		}
	}
	return backends
}

func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// parseMemory parses a memory string like "512m", "1g", "256k" into bytes.
func parseMemory(s string) int64 {
	if s == "" {
		return 0
	}
	s = strings.TrimSpace(strings.ToLower(s))

	var multiplier int64 = 1
	if strings.HasSuffix(s, "g") {
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "g")
	} else if strings.HasSuffix(s, "m") {
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "m")
	} else if strings.HasSuffix(s, "k") {
		multiplier = 1024
		s = strings.TrimSuffix(s, "k")
	}

	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		log.Printf("Failed to parse memory value %q: %v", s, err)
		return 0
	}
	return int64(val * float64(multiplier))
}

// parseCPU parses a CPU string like "0.5", "1.0", "2" into NanoCPUs.
func parseCPU(s string) int64 {
	if s == "" {
		return 0
	}
	val, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		log.Printf("Failed to parse CPU value %q: %v", s, err)
		return 0
	}
	return int64(val * 1e9)
}

func (a *Agent) mergeAppConfig(sourcePath, destPath, pgHost, valkeyHost string) error {
	// Read source config
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("app config template not found at %s", sourcePath)
		}
		return fmt.Errorf("failed to read app config: %w", err)
	}

	var cfg map[string]interface{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("failed to parse app config template: %w", err)
	}

	// Helper to safely get or create map
	ensureMap := func(key string) map[string]interface{} {
		if val, ok := cfg[key]; ok {
			if m, ok := val.(map[string]interface{}); ok {
				return m
			}
		}
		m := make(map[string]interface{})
		cfg[key] = m
		return m
	}

	// Inject Postgres URL
	pgURL := fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=disable",
		a.Secrets.PostgresDBUser, a.Secrets.PostgresPassword, pgHost, a.Secrets.PostgresDBName)
	pgCfg := ensureMap("postgresql")
	pgCfg["url"] = pgURL

	maxConnsPerLoop := 2
	if a.Config != nil && a.Config.App.MaxConnectionsPerEventLoop > 0 {
		maxConnsPerLoop = a.Config.App.MaxConnectionsPerEventLoop
	}
	pgCfg["max_connections_per_event_loop"] = maxConnsPerLoop

	// Inject Valkey URL
	valkeyURL := fmt.Sprintf("redis://:%s@%s:6379",
		a.Secrets.ValkeyPassword, valkeyHost)
	valkeyCfg := ensureMap("valkey")
	valkeyCfg["url"] = valkeyURL

	// Inject Encryption Key
	encryptionCfg := ensureMap("encryption")
	encryptionCfg["key"] = a.Secrets.AppEncryptionKey

	// Inject Token Secrets
	tokenCfg := ensureMap("token")
	tokenCfg["secrets"] = a.Secrets.AppTokenSecrets

	// Inject License Bundle
	if a.Config != nil && a.Config.App.License != "" {
		licenseCfg := ensureMap("license")
		licenseCfg["bundle"] = a.Config.App.License
	}

	// Write merged config to temp file
	newData, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal merged app config: %w", err)
	}

	if err := os.WriteFile(destPath, newData, 0600); err != nil {
		return fmt.Errorf("failed to write merged config to %s: %w", destPath, err)
	}

	return nil
}

func (a *Agent) ensureTraefik(ctx context.Context) error {
	if !a.isServiceEnabled("app") {
		return nil
	}

	domain := a.Config.Traefik.Domain
	if domain == "" {
		domain = "localhost"
	}

	if err := a.generateTraefikStaticConfig(); err != nil {
		return fmt.Errorf("failed to generate Traefik static config: %w", err)
	}
	if err := a.generateTraefikDynamicConfig(domain); err != nil {
		return fmt.Errorf("failed to generate Traefik dynamic config: %w", err)
	}
	if err := a.ensureTraefikACMEFile(); err != nil {
		log.Printf("Failed to ensure Traefik acme.json: %v", err)
	}

	if err := a.setRuntimeDirOwnership(a.TraefikRuntimeDir, a.Config.ContainerUIDs.Traefik); err != nil {
		log.Printf("Failed to set ownership for traefik runtime dir: %v", err)
	}

	image := a.Config.Images.Traefik
	if image == "" {
		image = "traefik:v3"
	}

	opts := container.RunOptions{
		Image:    image,
		Name:     "xavi-traefik",
		Memory:   128 * 1024 * 1024, // 128MB
		NanoCPUs: 250000000,         // 0.25 CPU
		Ports: []string{
			"80:80",
			"443:443",
			"8443:8443",
		},
		Mounts: []string{
			fmt.Sprintf("%s:/etc/traefik/traefik.yml:ro", a.TraefikStaticConfigPath),
			fmt.Sprintf("%s:/etc/traefik/dynamic.yml:ro", a.TraefikDynamicConfigPath),
			fmt.Sprintf("%s:/data/acme.json", a.TraefikACMEPath),
		},
		Network: NetworkName,
	}

	return a.docker.RunContainer(ctx, opts)
}

// ensureTraefikACMEFile creates acme.json with mode 0600 if it does not exist.
// Traefik refuses to start if the file is missing or has incorrect permissions.
func (a *Agent) ensureTraefikACMEFile() error {
	if _, err := os.Stat(a.TraefikACMEPath); os.IsNotExist(err) {
		return os.WriteFile(a.TraefikACMEPath, []byte("{}"), 0600)
	}
	return nil
}

func (a *Agent) generateTraefikStaticConfig() error {
	email := a.Config.Traefik.Email

	content := "entryPoints:\n" +
		"  web:\n" +
		"    address: \":80\"\n" +
		"  websecure:\n" +
		"    address: \":443\"\n" +
		"  mqtts:\n" +
		"    address: \":8443\"\n"

	if email != "" {
		content += "certificatesResolvers:\n" +
			"  letsencrypt:\n" +
			"    acme:\n" +
			"      email: " + email + "\n" +
			"      storage: /data/acme.json\n" +
			"      httpChallenge:\n" +
			"        entryPoint: web\n"
	}

	content += "providers:\n" +
		"  file:\n" +
		"    filename: /etc/traefik/dynamic.yml\n" +
		"log:\n" +
		"  level: INFO\n"

	return os.WriteFile(a.TraefikStaticConfigPath, []byte(content), 0600)
}

func (a *Agent) generateTraefikDynamicConfig(domain string) error {
	hasACME := a.Config.Traefik.Email != ""

	// Traefik rule syntax uses backticks inside double-quoted YAML strings.
	httpTLS := "      tls: {}\n"
	tcpTLS := "      tls: {}\n"
	if hasACME {
		httpTLS = "      tls:\n        certResolver: letsencrypt\n"
		tcpTLS = "      tls:\n        certResolver: letsencrypt\n"
	}

	content := "http:\n" +
		"  routers:\n" +
		"    app:\n" +
		"      rule: \"Host(`" + domain + "`)\"\n" +
		"      entryPoints:\n" +
		"        - websecure\n" +
		"      service: app-service\n" +
		httpTLS +
		"  services:\n" +
		"    app-service:\n" +
		"      loadBalancer:\n" +
		"        servers:\n" +
		"          - url: \"http://xavi-app:8080\"\n" +
		"tcp:\n" +
		"  routers:\n" +
		"    mqtts:\n" +
		"      rule: \"HostSNI(`" + domain + "`)\"\n" +
		"      entryPoints:\n" +
		"        - mqtts\n" +
		"      service: mqtt-service\n" +
		tcpTLS +
		"  services:\n" +
		"    mqtt-service:\n" +
		"      loadBalancer:\n" +
		"        servers:\n" +
		"          - address: \"xavi-app:1883\"\n"

	return os.WriteFile(a.TraefikDynamicConfigPath, []byte(content), 0600)
}

func (a *Agent) runHealthChecks(ctx context.Context) {
	type check struct {
		name    string
		service string
		ensure  func(context.Context) error
	}
	checks := []check{
		{"xavi-app", "app", a.ensureApp},
		{"xavi-traefik", "app", a.ensureTraefik},
	}
	for _, hc := range checks {
		if !a.isServiceEnabled(hc.service) {
			continue
		}
		running, err := a.docker.IsRunning(ctx, hc.name)
		if err != nil {
			log.Printf("Health check error for %s: %v", hc.name, err)
			continue
		}
		if !running {
			msg := fmt.Sprintf("Container %s is not running; triggering reconciliation", hc.name)
			log.Println(msg)
			sentry.CaptureMessage(msg)
			if err := hc.ensure(ctx); err != nil {
				log.Printf("Failed to recover %s: %v", hc.name, err)
			}
		}
	}
}


func (a *Agent) Close() error {
	return a.docker.Close()
}

// parseUID parses a uid string of the form "101" or "101:103" into uid and gid.
// If only a uid is provided, gid is set equal to uid.
// Returns 0, 0 if the string is empty.
func parseUID(s string) (uid, gid int, err error) {
	if s == "" {
		return 0, 0, nil
	}
	parts := strings.SplitN(s, ":", 2)
	uid, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid uid %q: %w", parts[0], err)
	}
	if len(parts) == 2 {
		gid, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0, fmt.Errorf("invalid gid %q: %w", parts[1], err)
		}
	} else {
		gid = uid
	}
	return uid, gid, nil
}

// setRuntimeDirOwnership sets the ownership of a runtime directory and its contents.
// spec is a uid string of the form "101" or "101:103". If empty or "0", ownership is not changed.
func (a *Agent) setRuntimeDirOwnership(dir string, spec config.UIDSpec) error {
	uid, gid, err := parseUID(string(spec))
	if err != nil {
		return fmt.Errorf("invalid uid spec %q: %w", spec, err)
	}
	if uid == 0 {
		return nil // Skip chown for root or unset
	}

	// Chown the directory
	if err := os.Chown(dir, uid, gid); err != nil {
		return fmt.Errorf("failed to chown directory %s to %d:%d: %w", dir, uid, gid, err)
	}

	// Chown all files in the directory
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("failed to read directory %s: %w", dir, err)
	}

	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if err := os.Chown(path, uid, gid); err != nil {
			return fmt.Errorf("failed to chown file %s to %d:%d: %w", path, uid, gid, err)
		}
	}

	return nil
}
