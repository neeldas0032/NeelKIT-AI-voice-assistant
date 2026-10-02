package config

import (
	"strings"
	"testing"
)

func TestDefaultsAreValid(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("shipped defaults do not validate: %v", err)
	}
}

func TestValidateRejectsBrokenConfigurations(t *testing.T) {
	cases := map[string]func(*Config){
		"empty addr":       func(c *Config) { c.HTTPAddr = "" },
		"unknown device":   func(c *Config) { c.ASRDevice = "tpu" },
		"zero workers":     func(c *Config) { c.ASRWorkers = 0 },
		"bad log level":    func(c *Config) { c.LogLevel = "verbose" },
		"unknown llm":      func(c *Config) { c.LLMProvider = "openai-ish" },
		"no workspace":     func(c *Config) { c.WorkspaceRoot = "" },
		"absurd hangover":  func(c *Config) { c.HangoverMS = 50 },
		"overlap >= chunk": func(c *Config) { c.OverlapSecond = 400 },
	}
	for name, mutate := range cases {
		c := Default()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
}

// A wildcard origin on a bearer-authenticated API that can run commands is not
// a preference, it is a vulnerability, so the server refuses to start with it.
func TestWildcardCORSIsRefusedWithAuthEnabled(t *testing.T) {
	c := Default()
	c.CORSOrigins = []string{"*"}
	c.AuthEnable = true
	if err := c.Validate(); err == nil {
		t.Fatal("wildcard CORS accepted with authentication enabled")
	}
}

func TestEnvironmentOverridesDefaults(t *testing.T) {
	t.Setenv("NEELKIT_HTTP_ADDR", "0.0.0.0:9000")
	t.Setenv("NEELKIT_ASR_WORKERS", "7")
	t.Setenv("NEELKIT_WORKSPACE_READ_ONLY", "true")
	t.Setenv("NEELKIT_CORS_ORIGINS", "https://a.example, https://b.example")

	c, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != "0.0.0.0:9000" || c.ASRWorkers != 7 || !c.WorkspaceRO {
		t.Fatalf("env not applied: %+v", c)
	}
	if len(c.CORSOrigins) != 2 || c.CORSOrigins[1] != "https://b.example" {
		t.Fatalf("list parsing: %v", c.CORSOrigins)
	}
}

func TestFlagsBeatEnvironment(t *testing.T) {
	t.Setenv("NEELKIT_ASR_WORKERS", "7")
	c, err := Load([]string{"-workers", "3"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ASRWorkers != 3 {
		t.Fatalf("workers = %d, want the flag value 3", c.ASRWorkers)
	}
}

func TestInvalidEnvValueFallsBackInsteadOfZeroing(t *testing.T) {
	t.Setenv("NEELKIT_ASR_WORKERS", "not-a-number")
	c, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.ASRWorkers < 1 {
		t.Fatalf("a malformed value zeroed the setting: %d", c.ASRWorkers)
	}
}

// A config dump reaches logs and error messages. It must never carry a secret.
func TestStringRedactsSecrets(t *testing.T) {
	c := Default()
	c.APIKey = "nk_abc_supersecret"
	c.LLMAPIKey = "sk-also-secret"

	s := c.String()
	for _, secret := range []string{"supersecret", "sk-also-secret"} {
		if strings.Contains(s, secret) {
			t.Fatalf("String() leaked %q: %s", secret, s)
		}
	}
	if !strings.Contains(s, "***") {
		t.Fatal("String() does not mark redacted values")
	}
}

func TestPublicBindDetection(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8420": false,
		"localhost:8420": false,
		"[::1]:8420":     false,
		"0.0.0.0:8420":   true,
		"192.168.1.5:80": true,
	}
	for addr, want := range cases {
		c := Default()
		c.HTTPAddr = addr
		if got := c.PublicBind(); got != want {
			t.Errorf("PublicBind(%q) = %v, want %v", addr, got, want)
		}
	}
}
