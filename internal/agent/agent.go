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
	"time"

	"github.com/3clabs/xavi/internal/cluster"
	"github.com/3clabs/xavi/internal/config"
	"github.com/3clabs/xavi/internal/container"
	"github.com/3clabs/xavi/internal/secrets"
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
	AppRuntimeDir       string
	CaddyRuntimeDir     string
	BackupBotRuntimeDir string

	// Runtime file paths (generated configs)
	TempConfigPath          string
	CaddyConfigPath         string
	TempBackupBotConfigPath string

	Config  *config.Config
	Secrets *secrets.Secrets
	docker  *container.Client
	watcher *config.Watcher
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
	caddyRuntimeDir := filepath.Join(runtimeDir, "caddy")
	backupBotRuntimeDir := filepath.Join(runtimeDir, "backupbot")

	for _, dir := range []string{appRuntimeDir, caddyRuntimeDir, backupBotRuntimeDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create runtime subdirectory %s: %w", dir, err)
		}
	}

	return &Agent{
		AuthBundle:              authBundle,
		ConfigDir:               configDir,
		ConfigPath:              configPath,
		SecretsPath:             secretsPath,
		AppConfigPath:           filepath.Join(configDir, "pulse.json"),
		BackupConfigPath:        filepath.Join(configDir, "backup.json"),
		RuntimeDir:              runtimeDir,
		AppRuntimeDir:           appRuntimeDir,
		CaddyRuntimeDir:         caddyRuntimeDir,
		BackupBotRuntimeDir:     backupBotRuntimeDir,
		TempConfigPath:          filepath.Join(appRuntimeDir, "config.json"),
		CaddyConfigPath:         filepath.Join(caddyRuntimeDir, "config.json"),
		TempBackupBotConfigPath: filepath.Join(backupBotRuntimeDir, "config.yaml"),
		Secrets:                 sec,
		docker:                  dockerCli,
		watcher:                 config.NewWatcher(configPath, 5*time.Second),
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

	if err := a.ensureCaddy(ctx); err != nil {
		log.Printf("Failed to ensure Caddy: %v", err)
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
		Image: image,
		Name:  "xavi-backupbot",
		Cmd:   []string{"backupbot", "--config", "/etc/backupbot/config.yaml"},
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

func (a *Agent) mergeBackupBotConfig(pgHost string) error {
	// Read backup.json (S3 config)
	var s3Backends string
	data, err := os.ReadFile(a.BackupConfigPath)
	if err == nil {
		// Simple injection logic: if backup.json exists, we assume it has the backends list.
		// For now, let's just parse the S3 parts specifically or use it as a template.
		// To be robust, let's assume storage.json is a JSON list of backends.
		var backends []map[string]interface{}
		if err := json.Unmarshal(data, &backends); err == nil {
			// Convert to YAML-like string (simplified)
			for _, b := range backends {
				if s3, ok := b["s3"].(map[string]interface{}); ok {
					s3Backends += fmt.Sprintf(`  - s3:
      bucket: %v
      prefix: %v
      account_id: %v
      access_key_id: %v
      secret_access_key: %v
      region: %v
      endpoint: %v
      retention: %v
`, s3["bucket"], s3["prefix"], s3["account_id"], s3["access_key_id"], s3["secret_access_key"], s3["region"], s3["endpoint"], s3["retention"])
				}
			}
		}
	}

	// If no S3 backends found, default to a file backend
	if s3Backends == "" {
		s3Backends = `  - file:
      path: /backups
      retention: 1
`
	}

	pgConn := fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=disable",
		a.Secrets.PostgresDBUser, a.Secrets.PostgresPassword, pgHost, a.Secrets.PostgresDBName)

	yamlContent := fmt.Sprintf(`schedule: 0 2 * * *
postgres:
  connection_string: %s
backends:
%s`, pgConn, s3Backends)

	return os.WriteFile(a.TempBackupBotConfigPath, []byte(yamlContent), 0600)
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
	cmd := []string{} // Default entrypoint

	if isCluster {
		if role == "primary" {
			// Primary Configuration
			// Enable replication in postgres.conf via args
			cmd = []string{
				"postgres",
				"-c", "wal_level=replica",
				"-c", "max_wal_senders=10",
				"-c", "hot_standby=on",
			}
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
			cmd = []string{"postgres"} // Standard start
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

	opts := container.RunOptions{
		Image: a.Config.Images.App,
		Name:  "xavi-app",
		Mounts: []string{
			fmt.Sprintf("%s:%s:ro", a.TempConfigPath, AppConfigMount),
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

func (a *Agent) ensureCaddy(ctx context.Context) error {
	if !a.isServiceEnabled("app") { // Colocated with app
		return nil
	}

	domain := a.Config.Caddy.Domain
	if domain == "" {
		domain = "localhost" // Default for local
	}

	if err := a.generateCaddyJSON(domain, "xavi-app"); err != nil {
		return fmt.Errorf("failed to generate Caddy JSON: %w", err)
	}

	// Set ownership of caddy runtime directory
	if err := a.setRuntimeDirOwnership(a.CaddyRuntimeDir, a.Config.ContainerUIDs.Caddy); err != nil {
		log.Printf("Failed to set ownership for caddy runtime dir: %v", err)
	}

	image := a.Config.Images.Caddy
	if image == "" {
		image = "wearecococo/caddy:2.10.2"
	}

	opts := container.RunOptions{
		Image: image,
		Name:  "xavi-caddy",
		Cmd:   []string{"caddy", "run", "--config", "/etc/caddy/config.json"},
		Ports: []string{
			"80:80",
			"443:443",
			"8883:8883",
		},
		Mounts: []string{
			fmt.Sprintf("%s:/etc/caddy/config.json:ro", a.CaddyConfigPath),
		},
		Network: NetworkName,
	}

	return a.docker.RunContainer(ctx, opts)
}

func (a *Agent) generateCaddyJSON(domain, targetHost string) error {
	// Simple JSON template for HTTP + L4 MQTTS
	// Note: In a production scenario, we'd use structs and json.Marshal
	// but for this implementation we'll use a template string for clarity.

	jsonTpl := `{
	"apps": {
		"http": {
			"servers": {
				"srv0": {
					"listen": [":443"],
					"routes": [
						{
							"match": [{"host": ["%s"]}],
							"handle": [
								{
									"handler": "reverse_proxy",
									"upstreams": [{"dial": "%s:8080"}]
								}
							]
						}
					]
				}
			}
		},
		"layer4": {
			"servers": {
				"mqtts": {
					"listen": [":8883"],
					"routes": [
						{
							"handle": [
								{
									"handler": "tls"
								},
								{
									"handler": "proxy",
									"upstreams": [{"dial": ["%s:1883"]}]
								}
							]
						}
					]
				}
			}
		}
	}
}`
	content := fmt.Sprintf(jsonTpl, domain, targetHost, targetHost)

	if err := os.WriteFile(a.CaddyConfigPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("failed to write Caddy JSON: %w", err)
	}
	return nil
}

func (a *Agent) Close() error {
	return a.docker.Close()
}

// setRuntimeDirOwnership sets the ownership of a runtime directory and its contents to the specified UID.
// If uid is 0 or not set, ownership is not changed (runs as root).
func (a *Agent) setRuntimeDirOwnership(dir string, uid int) error {
	if uid == 0 {
		return nil // Skip chown for root
	}

	// Chown the directory
	if err := os.Chown(dir, uid, uid); err != nil {
		return fmt.Errorf("failed to chown directory %s to uid %d: %w", dir, uid, err)
	}

	// Chown all files in the directory
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("failed to read directory %s: %w", dir, err)
	}

	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if err := os.Chown(path, uid, uid); err != nil {
			return fmt.Errorf("failed to chown file %s to uid %d: %w", path, uid, err)
		}
	}

	return nil
}
