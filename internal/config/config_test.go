package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadResolvesSecretsAndDefaults(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "secrets/se.token", "tok-se\n")
	writeFile(t, dir, "secrets/admin.token", "admin-secret\n")
	writeFile(t, dir, "secrets/db.url", "postgres://u:p@db/forgesync\n")
	path := writeFile(t, dir, "forgesync.yaml", `
http:
  admin_token_file: secrets/admin.token
database:
  url_file: secrets/db.url
health:
  interval: 30s
nodes:
  - name: se
    url: http://forgejo-se.test:3001
    site: SE
    token_file: secrets/se.token
`)
	t.Setenv(EnvDatabaseURL, "")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Nodes[0].Token != "tok-se" || cfg.HTTP.AdminToken != "admin-secret" {
		t.Errorf("secrets not resolved: node token %q, admin token %q", cfg.Nodes[0].Token, cfg.HTTP.AdminToken)
	}
	if cfg.Database.URL != "postgres://u:p@db/forgesync" {
		t.Errorf("database url = %q", cfg.Database.URL)
	}
	if cfg.Health.Interval != 30*time.Second || cfg.Health.Timeout != 5*time.Second || cfg.Health.FailureThreshold != 3 {
		t.Errorf("health = %+v", cfg.Health)
	}
	if !*cfg.HTTP.SecureCookies {
		t.Error("secure_cookies should default to true")
	}
	if cfg.HTTP.Listen != "127.0.0.1:8090" || cfg.Nodes[0].ServiceUser != "forgesync" || cfg.Log.Level != "info" {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestLoadDatabaseURLFromEnv(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "se.token", "tok")
	path := writeFile(t, dir, "c.yaml", `
database:
  url: postgres://from-file
nodes:
  - {name: se, url: "http://se", token_file: se.token}
`)
	t.Setenv(EnvDatabaseURL, "postgres://from-env")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.URL != "postgres://from-env" {
		t.Errorf("database url = %q, want the env override", cfg.Database.URL)
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	cases := map[string]struct {
		yaml string
		want string
	}{
		"unknown field": {`
database: {url: postgres://x}
nodes: [{name: se, url: "http://se", token_file: se.token, tokn: x}]
`, "field tokn not found"},
		"missing token file": {`
database: {url: postgres://x}
nodes: [{name: se, url: "http://se"}]
`, "token_file is required"},
		"empty token file": {`
database: {url: postgres://x}
nodes: [{name: se, url: "http://se", token_file: empty.token}]
`, "is empty"},
		"duplicate names and bad url": {`
database: {url: postgres://x}
nodes:
  - {name: se, url: "http://se", token_file: se.token}
  - {name: se, url: "forgejo-dk.test:3002", token_file: se.token}
`, "used twice"},
		"bad url": {`
database: {url: postgres://x}
nodes: [{name: se, url: "forgejo-se.test:3001", token_file: se.token}]
`, "absolute http(s) URL"},
		"no database": {`
nodes: [{name: se, url: "http://se", token_file: se.token}]
`, "database: set url"},
		"timeout not below interval": {`
database: {url: postgres://x}
health: {interval: 5s, timeout: 5s}
nodes: [{name: se, url: "http://se", token_file: se.token}]
`, "health.timeout"},
		"bad node name": {`
database: {url: postgres://x}
nodes: [{name: SE, url: "http://se", token_file: se.token}]
`, "lowercase"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "se.token", "tok")
			writeFile(t, dir, "empty.token", "\n")
			path := writeFile(t, dir, "c.yaml", tc.yaml)
			t.Setenv(EnvDatabaseURL, "")
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
