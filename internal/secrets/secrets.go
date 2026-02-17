package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Secrets holds the generated secrets for the deployment.
type Secrets struct {
	PostgresPassword string `json:"postgres_password"`
	ValkeyPassword   string `json:"valkey_password"`
	ClusterKey       string `json:"cluster_key"` // Base64 encoded 32-byte key
}

// LoadOrGenerate loads secrets from the given path.
// If the file does not exist, it generates new secrets and saves them.
func LoadOrGenerate(path string) (*Secrets, error) {
	s, err := load(path)
	if err == nil {
		// Check if any new fields are missing, if so generate them and save
		updated := false
		if s.ValkeyPassword == "" {
			s.ValkeyPassword = generateRandomString(32)
			updated = true
		}
		if s.ClusterKey == "" {
			s.ClusterKey = generateRandomBytesBase64(32)
			updated = true
		}
		if updated {
			if err := save(path, s); err != nil {
				return nil, fmt.Errorf("failed to save updated secrets: %w", err)
			}
		}
		return s, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to load secrets: %w", err)
	}

	// Generate new secrets
	s = &Secrets{
		PostgresPassword: generateRandomString(32),
		ValkeyPassword:   generateRandomString(32),
		ClusterKey:       generateRandomBytesBase64(32),
	}

	if err := save(path, s); err != nil {
		return nil, fmt.Errorf("failed to save generated secrets: %w", err)
	}

	return s, nil
}

func load(path string) (*Secrets, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var s Secrets
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("failed to unmarshal secrets: %w", err)
	}

	return &s, nil
}

func save(path string, s *Secrets) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal secrets: %w", err)
	}

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create secrets directory: %w", err)
	}

	// Save with restricted permissions (0600)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write secrets file: %w", err)
	}

	return nil
}

func generateRandomString(length int) string {
	bytes := make([]byte, length/2)
	if _, err := rand.Read(bytes); err != nil {
		panic(fmt.Sprintf("failed to read random bytes: %v", err))
	}
	return hex.EncodeToString(bytes)
}

func generateRandomBytesBase64(length int) string {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		panic(fmt.Sprintf("failed to read random bytes: %v", err))
	}
	return base64.StdEncoding.EncodeToString(bytes)
}
