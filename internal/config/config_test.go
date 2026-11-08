package config

import (
	"os"
	"strings"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"HTTP_ADDR",
		"DATABASE_URL",
		"MOCK_PROVIDER_URL",
		"PROVIDER_WEBHOOK_SECRET",
		"DATA_DIR",
		"API_PUBLIC_URL",
	} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unset %s: %v", k, err)
		}
	}
}

func TestLoad_mapsFullEnvWithDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/dbname")
	t.Setenv("MOCK_PROVIDER_URL", "http://127.0.0.1:9000")
	t.Setenv("PROVIDER_WEBHOOK_SECRET", "test-webhook-secret-value")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr: got %q, want %q", cfg.HTTPAddr, ":8080")
	}
	if cfg.DatabaseURL != "postgres://user:pass@localhost:5432/dbname" {
		t.Errorf("DatabaseURL: got %q, want %q", cfg.DatabaseURL, "postgres://user:pass@localhost:5432/dbname")
	}
	if cfg.MockProviderURL != "http://127.0.0.1:9000" {
		t.Errorf("MockProviderURL: got %q, want %q", cfg.MockProviderURL, "http://127.0.0.1:9000")
	}
	if cfg.ProviderWebhookSecret != "test-webhook-secret-value" {
		t.Errorf("ProviderWebhookSecret: got %q, want %q", cfg.ProviderWebhookSecret, "test-webhook-secret-value")
	}
	if cfg.DataDir != "data" {
		t.Errorf("DataDir: got %q, want %q", cfg.DataDir, "data")
	}
	if cfg.APIPublicURL != "http://localhost:8080" {
		t.Errorf("APIPublicURL: got %q, want %q", cfg.APIPublicURL, "http://localhost:8080")
	}
}

func TestLoad_explicitHTTPAddrAndDataDirOverrideDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/dbname")
	t.Setenv("MOCK_PROVIDER_URL", "http://127.0.0.1:9000")
	t.Setenv("PROVIDER_WEBHOOK_SECRET", "test-webhook-secret-value")
	t.Setenv("HTTP_ADDR", "  127.0.0.1:9090  ")
	t.Setenv("DATA_DIR", "  /var/lib/doverka  ")
	t.Setenv("API_PUBLIC_URL", "  http://api:8080  ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9090" {
		t.Errorf("HTTPAddr: got %q, want %q", cfg.HTTPAddr, "127.0.0.1:9090")
	}
	if cfg.DataDir != "/var/lib/doverka" {
		t.Errorf("DataDir: got %q, want %q", cfg.DataDir, "/var/lib/doverka")
	}
	if cfg.APIPublicURL != "http://api:8080" {
		t.Errorf("APIPublicURL: got %q, want %q", cfg.APIPublicURL, "http://api:8080")
	}
}

func TestLoad_requiresDatabaseURL(t *testing.T) {
	clearEnv(t)
	t.Setenv("MOCK_PROVIDER_URL", "http://127.0.0.1:9000")
	t.Setenv("PROVIDER_WEBHOOK_SECRET", "test-webhook-secret-value")

	_, err := Load()
	if err == nil {
		t.Fatal("Load: want error, got nil")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error %q must name DATABASE_URL", err.Error())
	}
}

func TestLoad_requiresMockProviderURL(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/dbname")
	t.Setenv("PROVIDER_WEBHOOK_SECRET", "test-webhook-secret-value")

	_, err := Load()
	if err == nil {
		t.Fatal("Load: want error, got nil")
	}
	if !strings.Contains(err.Error(), "MOCK_PROVIDER_URL") {
		t.Errorf("error %q must name MOCK_PROVIDER_URL", err.Error())
	}
}

func TestLoad_requiresProviderWebhookSecret(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/dbname")
	t.Setenv("MOCK_PROVIDER_URL", "http://127.0.0.1:9000")

	_, err := Load()
	if err == nil {
		t.Fatal("Load: want error, got nil")
	}
	if !strings.Contains(err.Error(), "PROVIDER_WEBHOOK_SECRET") {
		t.Errorf("error %q must name PROVIDER_WEBHOOK_SECRET", err.Error())
	}
}

func TestLoad_rejectsWhitespaceOnlyDatabaseURL(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "   \t  ")
	t.Setenv("MOCK_PROVIDER_URL", "http://127.0.0.1:9000")
	t.Setenv("PROVIDER_WEBHOOK_SECRET", "test-webhook-secret-value")

	_, err := Load()
	if err == nil {
		t.Fatal("Load: want error, got nil")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error %q must name DATABASE_URL", err.Error())
	}
}
