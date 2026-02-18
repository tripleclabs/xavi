package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/3clabs/xavi/internal/secrets"
)

func TestMergeAppConfig(t *testing.T) {
	// Setup temporary directory
	tmpDir, err := os.MkdirTemp("", "agent_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create source config file
	sourcePath := filepath.Join(tmpDir, "pulse.json")
	destPath := filepath.Join(tmpDir, "config.json")

	sourceJSON := `{
		"postgresql": {
			"url": "postgres://old:secret@old-db:5432/pulse",
			"max_connections_per_event_loop": 4
		},
		"valkey": {
			"url": "redis://old-valkey:6379"
		},
		"mqtt": {
			"enabled": true,
			"port": 1883
		}
	}`

	if err := os.WriteFile(sourcePath, []byte(sourceJSON), 0644); err != nil {
		t.Fatalf("Failed to write source config: %v", err)
	}

	// Initialize Agent with secrets
	agent := &Agent{
		Secrets: &secrets.Secrets{
			PostgresPassword: "new-pg-secret",
			PostgresDBName:   "pulse",
			PostgresDBUser:   "pulse",
			ValkeyPassword:   "new-valkey-secret",
			AppEncryptionKey: "new-encryption-key",
			AppTokenSecrets:  []string{"new-token-secret-1"},
		},
	}

	// Run merge logic
	pgHost := "10.0.0.1"
	valkeyHost := "10.0.0.2"
	if err := agent.mergeAppConfig(sourcePath, destPath, pgHost, valkeyHost); err != nil {
		t.Fatalf("mergeAppConfig failed: %v", err)
	}

	// Verify output
	data, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("Failed to read output config: %v", err)
	}

	var output map[string]interface{}
	if err := json.Unmarshal(data, &output); err != nil {
		t.Fatalf("Failed to parse output config: %v", err)
	}

	// Check Postgres URL
	pg, ok := output["postgresql"].(map[string]interface{})
	if !ok {
		t.Fatal("postgresql key missing or invalid type")
	}
	expectedPG := "postgres://pulse:new-pg-secret@10.0.0.1:5432/pulse?sslmode=disable"
	if pg["url"] != expectedPG {
		t.Errorf("postgresql.url = %v, want %v", pg["url"], expectedPG)
	}

	// Check max_connections_per_event_loop is set to default (2) when Config is nil
	if val, ok := pg["max_connections_per_event_loop"].(float64); !ok || val != 2 {
		t.Errorf("postgresql.max_connections_per_event_loop = %v, want 2", pg["max_connections_per_event_loop"])
	}

	// Check Valkey URL
	valkey, ok := output["valkey"].(map[string]interface{})
	if !ok {
		t.Fatal("valkey key missing or invalid type")
	}
	expectedValkey := "redis://:new-valkey-secret@10.0.0.2:6379"
	if valkey["url"] != expectedValkey {
		t.Errorf("valkey.url = %v, want %v", valkey["url"], expectedValkey)
	}

	// Check Encryption Key
	enc, ok := output["encryption"].(map[string]interface{})
	if !ok {
		t.Fatal("encryption key missing or invalid type")
	}
	if enc["key"] != "new-encryption-key" {
		t.Errorf("encryption.key = %v, want new-encryption-key", enc["key"])
	}

	// Check Token Secrets
	token, ok := output["token"].(map[string]interface{})
	if !ok {
		t.Fatal("token key missing or invalid type")
	}
	secretsList, ok := token["secrets"].([]interface{})
	if !ok || len(secretsList) != 1 || secretsList[0] != "new-token-secret-1" {
		t.Errorf("token.secrets = %v, want [new-token-secret-1]", token["secrets"])
	}

	// Check MQTT preserved
	mqtt, ok := output["mqtt"].(map[string]interface{})
	if !ok {
		t.Fatal("mqtt key missing or invalid type")
	}
	if val, ok := mqtt["port"].(float64); !ok || val != 1883 {
		t.Errorf("mqtt.port = %v, want 1883", mqtt["port"])
	}
}
