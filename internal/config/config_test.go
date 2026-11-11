package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	t.Setenv("CLIENT_A", strings.Repeat("a", 32))
	t.Setenv("WEBHOOK_A", strings.Repeat("w", 32))
	return &Config{Version: 1, StateDir: "state", DryRun: true, IPC: IPC{Socket: "/tmp/gbd/socket"}, Webhook: Webhook{Bind: "127.0.0.1:8787", Sources: map[string]Source{"main": {SecretEnv: "WEBHOOK_A"}}}, Repositories: map[string]Repository{"main": {FullName: "owner/repo"}}, Clients: map[string]Client{"agent": {TokenEnv: "CLIENT_A", Repositories: []string{"main"}, Capabilities: []string{"events.read"}}}, Routes: []Route{{ID: "mentions", Repositories: []string{"main"}, Events: []string{"issue_comment"}, Trigger: "mention", Names: []string{"bot"}, Actors: []string{"alice"}, Clients: []string{"agent"}}}}
}
func TestConfigurationDefaultsAndRejections(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
	}{{"nonloopback", func(c *Config) { c.IPC.Bind = "0.0.0.0:80" }}, {"hostname", func(c *Config) { c.IPC.Bind = "localhost:80" }}, {"noactor", func(c *Config) { c.Routes[0].Actors = nil }}, {"unknowncap", func(c *Config) {
		p := c.Clients["agent"]
		p.Capabilities = append(p.Capabilities, "merge")
		c.Clients["agent"] = p
	}}, {"duplicatetoken", func(c *Config) { c.Clients["other"] = c.Clients["agent"] }}, {"outsidegrant", func(c *Config) {
		c.Repositories["second"] = Repository{FullName: "owner/second"}
		c.Routes[0].Repositories = []string{"second"}
	}}, {"noghauth", func(c *Config) { c.DryRun = false }}, {"mixedauth", func(c *Config) {
		c.GitHub.TokenEnv = "GH"
		c.GitHub.Apps = []App{{AppID: 1, PrivateKeyFile: "x", InstallationIDs: []int64{1}}}
	}}, {"badversion", func(c *Config) { c.Version = 2 }}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig(t)
			tt.edit(c)
			if c.Validate() == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	c := testConfig(t)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Limits.LeaseSeconds != 120 || c.Clients["agent"].Token == "" {
		t.Fatal("defaults/secrets missing")
	}
}
func TestStrictLoadRelativePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{"version":1,"typo":true}`), 0600)
	if _, err := Load(path); err == nil {
		t.Fatal("unknown accepted")
	}
	os.WriteFile(path, []byte(`{} {}`), 0600)
	if _, err := Load(path); err == nil {
		t.Fatal("trailing accepted")
	}
}
func TestSecretFiles(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte(strings.Repeat("x", 32)), 0644)
	if _, err := Secret("", file); err == nil {
		t.Fatal("insecure file accepted")
	}
	os.Chmod(file, 0600)
	if _, err := Secret("", file); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(file, []byte("first\nsecond"), 0600)
	if _, err := Secret("", file); err == nil {
		t.Fatal("multiline accepted")
	}
}

func TestSampleConfigurations(t *testing.T) {
	for _, env := range []string{"GITHUB_BOTD_WEBHOOK_SECRET", "GITHUB_BOTD_REVIEWER_TOKEN", "GITHUB_BOTD_CI_TOKEN", "GITHUB_BOTD_AGENT_TOKEN"} {
		t.Setenv(env, strings.Repeat(env, 2))
	}
	for _, path := range []string{"../../config/github-botd.example.json", "../../config/github-app.example.json"} {
		c, err := Load(path)
		if err != nil {
			t.Fatal(path, err)
		}
		if !filepath.IsAbs(c.StateDir) || !filepath.IsAbs(c.IPC.Socket) {
			t.Fatal("relative paths not resolved")
		}
		if !c.DryRun {
			t.Fatal("sample enables writes")
		}
	}
}
