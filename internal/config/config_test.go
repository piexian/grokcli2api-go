package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadValidatesStreamCompression(t *testing.T) {
	t.Setenv("GROK_STREAM_COMPRESSION", "brotli")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "GROK_STREAM_COMPRESSION") {
		t.Fatalf("Load() error = %v, want stream compression validation error", err)
	}
}

func TestLoadAdminKey(t *testing.T) {
	t.Setenv("GROK_ADMIN_KEY", "  admin-secret  ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminKey != "admin-secret" {
		t.Fatalf("AdminKey = %q", cfg.AdminKey)
	}
}

func TestLoadBillingRefreshInterval(t *testing.T) {
	t.Setenv("GROK_BILLING_REFRESH_INTERVAL", "7m")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BillingRefreshInterval != 7*time.Minute {
		t.Fatalf("BillingRefreshInterval = %s", cfg.BillingRefreshInterval)
	}
}

func TestLoadQuotaRetryMaxAccounts(t *testing.T) {
	t.Setenv("GROK_QUOTA_RETRY_MAX_ACCOUNTS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuotaRetryMaxAccounts != 3 {
		t.Fatalf("QuotaRetryMaxAccounts default = %d", cfg.QuotaRetryMaxAccounts)
	}

	t.Setenv("GROK_QUOTA_RETRY_MAX_ACCOUNTS", "5")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuotaRetryMaxAccounts != 5 {
		t.Fatalf("QuotaRetryMaxAccounts override = %d", cfg.QuotaRetryMaxAccounts)
	}
}

func TestLoadRejectsInvalidQuotaRetryMaxAccounts(t *testing.T) {
	t.Setenv("GROK_QUOTA_RETRY_MAX_ACCOUNTS", "0")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "GROK_QUOTA_RETRY_MAX_ACCOUNTS") {
		t.Fatalf("Load() error = %v, want quota retry validation error", err)
	}
}

func TestLoadModernClientTransportConfig(t *testing.T) {
	t.Setenv("GROK_CLIENT_VERSION", "0.2.102")
	t.Setenv("GROK_CLIENT_MODE", "HEADLESS")
	t.Setenv("GROK_XAI_API_BASE_URL", "https://api.example.test/")
	t.Setenv("GROK_DEPLOYMENT_ID", "  deployment-1  ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientVersion != "0.2.102" || cfg.ClientMode != "headless" {
		t.Fatalf("client version/mode = %q/%q", cfg.ClientVersion, cfg.ClientMode)
	}
	if cfg.XAIAPIBaseURL != "https://api.example.test" || cfg.DeploymentID != "deployment-1" {
		t.Fatalf("xAI transport/deployment = %q/%q", cfg.XAIAPIBaseURL, cfg.DeploymentID)
	}
}

func TestLoadRejectsInvalidClientMode(t *testing.T) {
	t.Setenv("GROK_CLIENT_MODE", "batch")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "GROK_CLIENT_MODE") {
		t.Fatalf("Load() error = %v, want client mode validation error", err)
	}
}

func TestLoadAuditDefaultsAndOverrides(t *testing.T) {
	authsDir := t.TempDir()
	t.Setenv("GROK_AUTHS_DIR", authsDir)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuditDB != filepath.Join(authsDir, "audit.db") {
		t.Fatalf("AuditDB = %q", cfg.AuditDB)
	}
	if cfg.AuditRetentionDays != 30 {
		t.Fatalf("AuditRetentionDays = %d", cfg.AuditRetentionDays)
	}
	if cfg.AuditQueueSize != 4096 {
		t.Fatalf("AuditQueueSize = %d", cfg.AuditQueueSize)
	}

	t.Setenv("GROK_AUDIT_DB", " off ")
	t.Setenv("GROK_AUDIT_RETENTION_DAYS", "45")
	t.Setenv("GROK_AUDIT_QUEUE_SIZE", "8192")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuditDB != "off" || cfg.AuditRetentionDays != 45 || cfg.AuditQueueSize != 8192 {
		t.Fatalf("audit config = %q/%d/%d", cfg.AuditDB, cfg.AuditRetentionDays, cfg.AuditQueueSize)
	}
}

func TestLoadRejectsInvalidAuditRetention(t *testing.T) {
	t.Setenv("GROK_AUDIT_RETENTION_DAYS", "0")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "GROK_AUDIT_RETENTION_DAYS") {
		t.Fatalf("Load() error = %v, want retention validation error", err)
	}
}

func TestLoadRejectsInvalidAuditQueueSize(t *testing.T) {
	t.Setenv("GROK_AUDIT_QUEUE_SIZE", "0")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "GROK_AUDIT_QUEUE_SIZE") {
		t.Fatalf("Load() error = %v, want queue size validation error", err)
	}
}

func TestLoadWebConfigDefaultsAndOverrides(t *testing.T) {
	t.Setenv("GROK_WEB_DIST", "")
	t.Setenv("GROK_PUBLIC_BASE_URL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebDist != "frontend/dist" || cfg.PublicBaseURL != "" {
		t.Fatalf("web config defaults = %q/%q", cfg.WebDist, cfg.PublicBaseURL)
	}

	t.Setenv("GROK_WEB_DIST", "  /srv/grok/web  ")
	t.Setenv("GROK_PUBLIC_BASE_URL", "  https://grok.example.test/api  ")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebDist != "/srv/grok/web" || cfg.PublicBaseURL != "https://grok.example.test/api" {
		t.Fatalf("web config overrides = %q/%q", cfg.WebDist, cfg.PublicBaseURL)
	}
}
