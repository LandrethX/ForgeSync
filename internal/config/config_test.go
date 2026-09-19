package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
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
	if cfg.Inventory.Interval != 5*time.Minute || cfg.Inventory.BranchConcurrency != 4 {
		t.Errorf("inventory = %+v", cfg.Inventory)
	}
	if cfg.Replication.Enabled || cfg.Replication.WorkDir != "/var/lib/forgesync/git" || cfg.Replication.Concurrency != 2 {
		t.Errorf("replication defaults = %+v", cfg.Replication)
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

// SceneID signs people in to the nodes, not to ForgeSync: an `oidc`
// block is now an unknown key, and unknown keys are errors, so an old
// config says so rather than quietly doing nothing.
func TestOIDCIsNoLongerConfigurable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "se.token", "tok")
	path := writeFile(t, dir, "c.yaml", `
database: {url: postgres://x}
oidc:
  issuer: "https://id.example"
  client_id: forgesync
nodes: [{name: se, url: "http://se", token_file: se.token}]
`)
	t.Setenv(EnvDatabaseURL, "")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "oidc") {
		t.Fatalf("loading a config with oidc: %v", err)
	}
}

func TestReplicationWorkDirIsRelativeToConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "se.token", "tok")
	path := writeFile(t, dir, "c.yaml", `
database: {url: postgres://x}
replication: {enabled: true, work_dir: .work/git}
nodes: [{name: se, url: "http://se", token_file: se.token}]
`)
	t.Setenv(EnvDatabaseURL, "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Replication.WorkDir != filepath.Join(dir, ".work/git") {
		t.Errorf("work dir = %q", cfg.Replication.WorkDir)
	}
}

func TestWebhooks(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "se.token", "tok")
	writeFile(t, dir, "hook.secret", strings.Repeat("s", 40))
	writeFile(t, dir, "short.secret", "short")
	load := func(hooks string) (*Config, error) {
		t.Helper()
		t.Setenv(EnvDatabaseURL, "")
		return Load(writeFile(t, dir, "c.yaml", `
database: {url: postgres://x}
`+hooks+`
nodes: [{name: se, url: "http://se", token_file: se.token}]
`))
	}
	cfg, err := load(`webhooks: {url: "http://forgesync.test:8090/api/v1/hooks/forgejo", secret_file: hook.secret}`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Webhooks.Secret != strings.Repeat("s", 40) || cfg.Webhooks.CheckInterval != 10*time.Minute {
		t.Errorf("webhooks = %+v", cfg.Webhooks)
	}
	if cfg, err := load(``); err != nil || cfg.Webhooks.URL != "" {
		t.Errorf("off by default: %+v, %v", cfg.Webhooks, err)
	}
	for _, bad := range []string{
		`webhooks: {url: "http://x/hooks", secret_file: short.secret}`,
		`webhooks: {url: "http://x/hooks"}`,
		`webhooks: {url: "ftp://x/hooks", secret_file: hook.secret}`,
		`webhooks: {url: "http://x/hooks", secret_file: hook.secret, check_interval: 10s}`,
	} {
		if _, err := load(bad); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
}

// TLS: both files or neither, and they have to be a keypair. Finding out
// at startup beats finding out on the first request.
func TestTLSConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "se.token", "tok")
	cert, key := writeSelfSigned(t, dir)

	base := "database: {url: postgres://x}\nnodes: [{name: se, url: \"http://se\", token_file: se.token}]\n"
	t.Setenv(EnvDatabaseURL, "")

	path := writeFile(t, dir, "ok.yaml", base+"http: {tls_cert_file: cert.pem, tls_key_file: key.pem}\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a good pair: %v", err)
	}
	if !cfg.HTTP.TLS() || cfg.HTTP.TLSCertFile != cert || cfg.HTTP.TLSKeyFile != key {
		t.Errorf("tls = %v %q %q", cfg.HTTP.TLS(), cfg.HTTP.TLSCertFile, cfg.HTTP.TLSKeyFile)
	}

	path = writeFile(t, dir, "half.yaml", base+"http: {tls_cert_file: cert.pem}\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "set both or neither") {
		t.Errorf("only the certificate: %v", err)
	}

	writeFile(t, dir, "other.pem", "not a key")
	path = writeFile(t, dir, "bad.yaml", base+"http: {tls_cert_file: cert.pem, tls_key_file: other.pem}\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "tls_cert_file") {
		t.Errorf("a key that isn't the certificate's: %v", err)
	}

	// Without it, the controller serves plain HTTP as before.
	path = writeFile(t, dir, "plain.yaml", base)
	cfg, err = Load(path)
	if err != nil || cfg.HTTP.TLS() {
		t.Errorf("plain = %v, %v", cfg.HTTP.TLS(), err)
	}
}

// writeSelfSigned puts a throwaway certificate and key in dir.
func writeSelfSigned(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "forgesync.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"forgesync.test"},
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = writeFile(t, dir, "cert.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	keyPath = writeFile(t, dir, "key.pem", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	return certPath, keyPath
}
