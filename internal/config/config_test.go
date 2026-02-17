package config

import (
	"encoding/base64"
	"testing"
)

func TestParseBundle(t *testing.T) {
	// {"control":{"url":"https://example.com","token":"abc"},"docker":{"registry":"docker.io"}}
	validJSON := `{"control":{"url":"https://example.com","token":"abc"},"docker":{"registry":"docker.io"}}`
	encoded := base64.StdEncoding.EncodeToString([]byte(validJSON))

	cfg, err := ParseBundle(encoded)
	if err != nil {
		t.Fatalf("ParseBundle failed: %v", err)
	}

	if cfg.Control.URL != "https://example.com" {
		t.Errorf("Expected URL https://example.com, got %s", cfg.Control.URL)
	}
	if cfg.Docker.Registry != "docker.io" {
		t.Errorf("Expected Registry docker.io, got %s", cfg.Docker.Registry)
	}
}

func TestParseBundle_Invalid(t *testing.T) {
	_, err := ParseBundle("invalid-base64")
	if err == nil {
		t.Error("Expected error for invalid base64, got nil")
	}

	encoded := base64.StdEncoding.EncodeToString([]byte("{invalid-json"))
	_, err = ParseBundle(encoded)
	if err == nil {
		t.Error("Expected error for invalid json, got nil")
	}
}
