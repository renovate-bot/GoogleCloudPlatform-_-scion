package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAgentSettings(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "scion-config-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	path := filepath.Join(tmpDir, "settings.json")
	content := `{
		"apiKey": "test-key",
		"security": {
			"auth": {
				"selectedType": "api-key"
			}
		},
		"tools": {
			"sandbox": "docker"
		}
	}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadAgentSettings(path)
	if err != nil {
		t.Fatalf("LoadAgentSettings failed: %v", err)
	}

	if s.ApiKey != "test-key" {
		t.Errorf("Expected apiKey test-key, got %s", s.ApiKey)
	}
	if s.Security.Auth.SelectedType != "api-key" {
		t.Errorf("Expected selectedType api-key, got %s", s.Security.Auth.SelectedType)
	}
	if s.Tools.Sandbox != "docker" {
		t.Errorf("Expected sandbox docker, got %v", s.Tools.Sandbox)
	}

	// Test invalid JSON
	if err := os.WriteFile(path, []byte(`{invalid}`), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadAgentSettings(path)
	if err == nil {
		t.Error("Expected error for invalid JSON")
	}

	// Test nonexistent file
	_, err = LoadAgentSettings(filepath.Join(tmpDir, "nonexistent.json"))
	if err == nil {
		t.Error("Expected error for nonexistent file")
	}
}

func TestGetAgentSettings(t *testing.T) {
	// This test depends on the environment, so we just check if it returns without crashing
	// or if it returns an error that is not "user home directory not found"
	s, err := GetAgentSettings()
	if err != nil {
		// It's okay if it fails if the file doesn't exist on the host
		return
	}
	if s == nil {
		t.Fatal("GetAgentSettings returned nil without error")
	}
}
