package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadYAMLUsesSQLiteDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("telegram:\n  token: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", path)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DATABASE_DRIVER", "")
	t.Setenv("DATABASE_DSN", "")
	t.Setenv("EPG_SERVICE_API_KEY", "")
	t.Setenv("EPG_SERVICE_BASE_URL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Driver != "sqlite" || cfg.Database.DSN != "./data/remind.db" {
		t.Fatalf("unexpected database config: %+v", cfg.Database)
	}
	if cfg.Providers.TV.BaseURL != "https://api.epgservice.ru" || cfg.Providers.TV.Timeout == 0 {
		t.Fatalf("unexpected TV config: %+v", cfg.Providers.TV)
	}
}

func TestDatabaseURLHasHighestPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", path)
	t.Setenv("DATABASE_DRIVER", "sqlite")
	t.Setenv("DATABASE_DSN", "ignored.db")
	t.Setenv("DATABASE_URL", "postgres://localhost/remind")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Driver != "postgres" || cfg.Database.DSN != "postgres://localhost/remind" {
		t.Fatalf("unexpected database config: %+v", cfg.Database)
	}
}
