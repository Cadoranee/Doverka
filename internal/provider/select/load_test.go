package selection

import (
	"os"
	"path/filepath"
	"testing"
)

func findProvidersJSON(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "data", "providers.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("data/providers.json not found")
		}
		dir = parent
	}
}

func TestLoadProviders_repoFile(t *testing.T) {
	path := findProvidersJSON(t)
	providers, err := LoadProviders(path)
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	if len(providers) == 0 {
		t.Fatal("expected at least one provider")
	}
	var alpha *Provider
	for i := range providers {
		if providers[i].ID == "prov_alpha" {
			alpha = &providers[i]
			break
		}
	}
	if alpha == nil {
		t.Fatal("prov_alpha not found")
	}
	if !alpha.Enabled {
		t.Fatal("prov_alpha: want enabled")
	}
	if len(alpha.Maintenance) == 0 {
		t.Fatal("prov_alpha: want at least one maintenance window")
	}
}
