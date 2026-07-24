package config

import (
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
