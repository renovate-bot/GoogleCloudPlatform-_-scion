package harness

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGeminiDiscoverAuth(t *testing.T) {
	// Setup a temporary home directory
	tmpHome, err := os.MkdirTemp("", "scion-home-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpHome)

	// Mock HOME environment variable
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	geminiDir := filepath.Join(tmpHome, ".gemini")
	if err := os.MkdirAll(geminiDir, 0755); err != nil {
		t.Fatal(err)
	}

	// 1. Test OAuth discovery via host settings
	settingsPath := filepath.Join(geminiDir, "settings.json")
	settingsData := `{
		"security": {
			"auth": {
				"selectedType": "oauth-personal"
			}
		}
	}`
	if err := os.WriteFile(settingsPath, []byte(settingsData), 0644); err != nil {
		t.Fatal(err)
	}

	oauthCredsPath := filepath.Join(geminiDir, "oauth_creds.json")
	if err := os.WriteFile(oauthCredsPath, []byte(`{"dummy":"creds"}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Setup agent home
	agentHome, err := os.MkdirTemp("", "agent-home-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(agentHome)

	g := &GeminiCLI{}
	auth := g.DiscoverAuth(agentHome)
	if auth.OAuthCreds != oauthCredsPath {
		t.Errorf("expected OAuthCreds to be %s, got %s", oauthCredsPath, auth.OAuthCreds)
	}

	// 2. Test OAuth discovery via agent settings (overriding host)
	// Create agent-specific settings.json
	agentGeminiDir := filepath.Join(agentHome, ".gemini")
	os.MkdirAll(agentGeminiDir, 0755)
	agentSettingsPath := filepath.Join(agentGeminiDir, "settings.json")
	os.WriteFile(agentSettingsPath, []byte(`{"security":{"auth":{"selectedType":"gemini-api-key"}}}`), 0644)
	
	auth = g.DiscoverAuth(agentHome)
	// wait, if agent settings says gemini-api-key, and we have oauth-personal on host,
	// DiscoverAuth currently prefers agent setting if present.
	// But it only checks agent settings for "SelectedType".
	// If agent settings has SelectedType="gemini-api-key", it will NOT return OAuthCreds.
	if auth.OAuthCreds != "" {
		t.Errorf("expected OAuthCreds to be empty when requested by agent settings, got %s", auth.OAuthCreds)
	}

	// 3. Test API Key fallback from host settings
	os.Remove(settingsPath)
	os.Remove(agentSettingsPath)
	settingsData = `{
		"apiKey": "test-api-key"
	}`
	if err := os.WriteFile(settingsPath, []byte(settingsData), 0644); err != nil {
		t.Fatal(err)
	}

	// Clear env vars that might interfere
	origApiKey := os.Getenv("GEMINI_API_KEY")
	origGoogleApiKey := os.Getenv("GOOGLE_API_KEY")
	os.Unsetenv("GEMINI_API_KEY")
	os.Unsetenv("GOOGLE_API_KEY")
	defer func() {
		os.Setenv("GEMINI_API_KEY", origApiKey)
		os.Setenv("GOOGLE_API_KEY", origGoogleApiKey)
	}()

	auth = g.DiscoverAuth(agentHome)
	if auth.GeminiAPIKey != "test-api-key" {
		t.Errorf("expected GeminiAPIKey to be 'test-api-key', got '%s'", auth.GeminiAPIKey)
	}
}
