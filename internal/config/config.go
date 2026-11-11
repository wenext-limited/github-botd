// Package config loads strict, fail-closed gateway configuration.
package config

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	gh "github.com/wenext-limited/github-botd/internal/github"
)

var aliasRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
var repositoryRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var Capabilities = map[string]bool{"events.read": true, "events.replay": true, "context.read": true, "issue.comment.create": true, "issue.comment.update": true, "review.reply": true, "review.comment": true, "review.submit": true, "check.create": true, "check.update": true, "actions.proactive": true}

type Config struct {
	Version      int                   `json:"version"`
	StateDir     string                `json:"state_dir"`
	DryRun       bool                  `json:"dry_run"`
	IPC          IPC                   `json:"ipc"`
	Webhook      Webhook               `json:"webhook"`
	GitHub       GitHub                `json:"github"`
	Repositories map[string]Repository `json:"repositories"`
	Clients      map[string]Client     `json:"clients"`
	Routes       []Route               `json:"routes"`
	Poll         Poll                  `json:"poll"`
	Limits       Limits                `json:"limits"`
}
type IPC struct {
	Socket string `json:"socket"`
	Bind   string `json:"bind,omitempty"`
}
type Webhook struct {
	Bind             string            `json:"bind,omitempty"`
	AllowNonLoopback bool              `json:"allow_non_loopback,omitempty"`
	Sources          map[string]Source `json:"sources"`
}
type Source struct {
	SecretEnv       string  `json:"secret_env,omitempty"`
	SecretFile      string  `json:"secret_file,omitempty"`
	InstallationIDs []int64 `json:"installation_ids,omitempty"`
	Secret          string  `json:"-"`
}
type GitHub struct {
	BaseURL   string `json:"base_url,omitempty"`
	TokenEnv  string `json:"token_env,omitempty"`
	TokenFile string `json:"token_file,omitempty"`
	Username  string `json:"username,omitempty"`
	Apps      []App  `json:"apps,omitempty"`
}
type App struct {
	BotLogin        string  `json:"bot_login,omitempty"`
	AppID           int64   `json:"app_id"`
	PrivateKeyFile  string  `json:"private_key_file"`
	InstallationIDs []int64 `json:"installation_ids"`
}
type Repository struct {
	FullName       string `json:"full_name"`
	InstallationID int64  `json:"installation_id,omitempty"`
}
type Client struct {
	TokenEnv     string   `json:"token_env,omitempty"`
	TokenFile    string   `json:"token_file,omitempty"`
	Repositories []string `json:"repositories"`
	Capabilities []string `json:"capabilities"`
	Token        string   `json:"-"`
}
type Route struct {
	ID           string   `json:"id"`
	Repositories []string `json:"repositories"`
	Events       []string `json:"events"`
	Actions      []string `json:"actions,omitempty"`
	Trigger      string   `json:"trigger"`
	Names        []string `json:"names,omitempty"`
	Actors       []string `json:"actors"`
	IncludeBots  bool     `json:"include_bots,omitempty"`
	Clients      []string `json:"clients"`
}
type Poll struct {
	Enabled         bool     `json:"enabled"`
	IntervalSeconds int      `json:"interval_seconds,omitempty"`
	Repositories    []string `json:"repositories,omitempty"`
}
type Limits struct {
	MaxBodyBytes int64 `json:"max_body_bytes,omitempty"`
	LeaseSeconds int   `json:"lease_seconds,omitempty"`
	MaxAttempts  int   `json:"max_attempts,omitempty"`
	MaxPending   int   `json:"max_pending,omitempty"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) > 1024*1024 {
		return nil, errors.New("config exceeds 1 MiB")
	}
	var c Config
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("config has trailing content")
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	c.StateDir = resolve(c.StateDir)
	c.IPC.Socket = resolve(c.IPC.Socket)
	c.GitHub.TokenFile = resolve(c.GitHub.TokenFile)
	for i := range c.GitHub.Apps {
		c.GitHub.Apps[i].PrivateKeyFile = resolve(c.GitHub.Apps[i].PrivateKeyFile)
	}
	for k, v := range c.Clients {
		v.TokenFile = resolve(v.TokenFile)
		c.Clients[k] = v
	}
	for k, v := range c.Webhook.Sources {
		v.SecretFile = resolve(v.SecretFile)
		c.Webhook.Sources[k] = v
	}
	if err = c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) Validate() error {
	if c.Version != 1 {
		return errors.New("config version must be 1")
	}
	if c.StateDir == "" {
		return errors.New("state_dir is required")
	}
	if c.IPC.Socket == "" && c.IPC.Bind == "" {
		return errors.New("an IPC socket or loopback bind is required")
	}
	if c.IPC.Socket != "" && len(c.IPC.Socket) > 100 {
		return errors.New("Unix socket path must be at most 100 bytes")
	}
	if c.IPC.Bind != "" {
		if err := Loopback(c.IPC.Bind); err != nil {
			return fmt.Errorf("IPC: %w", err)
		}
	}
	if c.Webhook.Bind != "" && !c.Webhook.AllowNonLoopback {
		if err := Loopback(c.Webhook.Bind); err != nil {
			return fmt.Errorf("webhook: %w", err)
		}
	}
	if len(c.Repositories) == 0 || len(c.Clients) == 0 {
		return errors.New("repositories and clients are required")
	}
	if c.Limits.MaxBodyBytes == 0 {
		c.Limits.MaxBodyBytes = 2 << 20
	}
	if c.Limits.MaxBodyBytes < 1024 || c.Limits.MaxBodyBytes > 25<<20 {
		return errors.New("max_body_bytes must be 1024..26214400")
	}
	if c.Limits.LeaseSeconds == 0 {
		c.Limits.LeaseSeconds = 120
	}
	if c.Limits.LeaseSeconds < 1 || c.Limits.LeaseSeconds > 3600 {
		return errors.New("lease_seconds must be 1..3600")
	}
	if c.Limits.MaxAttempts == 0 {
		c.Limits.MaxAttempts = 8
	}
	if c.Limits.MaxAttempts < 1 || c.Limits.MaxAttempts > 30 {
		return errors.New("max_attempts must be 1..30")
	}
	if c.Limits.MaxPending == 0 {
		c.Limits.MaxPending = 100000
	}
	if c.Limits.MaxPending < 1 {
		return errors.New("max_pending must be positive")
	}
	seenRepo := map[string]bool{}
	for a, r := range c.Repositories {
		if !aliasRE.MatchString(a) || !repositoryRE.MatchString(r.FullName) || r.InstallationID < 0 {
			return fmt.Errorf("invalid repository %q", a)
		}
		key := strings.ToLower(r.FullName)
		if seenRepo[key] {
			return errors.New("repository full names must be unique")
		}
		seenRepo[key] = true
	}
	tokens := []string{}
	for id, p := range c.Clients {
		if !aliasRE.MatchString(id) {
			return fmt.Errorf("invalid client ID %q", id)
		}
		token, err := Secret(p.TokenEnv, p.TokenFile)
		if err != nil {
			return fmt.Errorf("client %s: %w", id, err)
		}
		if len(token) < 32 {
			return fmt.Errorf("client %s token must be at least 32 bytes", id)
		}
		for _, old := range tokens {
			if subtle.ConstantTimeCompare([]byte(token), []byte(old)) == 1 {
				return errors.New("client tokens must be unique")
			}
		}
		tokens = append(tokens, token)
		p.Token = token
		if len(p.Repositories) == 0 {
			return fmt.Errorf("client %s must have repositories", id)
		}
		for _, a := range p.Repositories {
			if _, ok := c.Repositories[a]; !ok {
				return fmt.Errorf("client %s references unknown repository %s", id, a)
			}
		}
		for _, cap := range p.Capabilities {
			if !Capabilities[cap] {
				return fmt.Errorf("client %s has unknown capability %s", id, cap)
			}
		}
		c.Clients[id] = p
	}
	for id, s := range c.Webhook.Sources {
		if !aliasRE.MatchString(id) {
			return errors.New("invalid webhook source alias")
		}
		secret, err := Secret(s.SecretEnv, s.SecretFile)
		if err != nil || len(secret) < 32 {
			return fmt.Errorf("webhook %s requires a secret of at least 32 bytes", id)
		}
		s.Secret = secret
		c.Webhook.Sources[id] = s
		if len(c.GitHub.Apps) > 0 && len(s.InstallationIDs) == 0 {
			return fmt.Errorf("webhook %s must restrict installation_ids for App auth", id)
		}
		for _, token := range tokens {
			if subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1 {
				return errors.New("webhook and client secrets must be distinct")
			}
		}
	}
	if c.Webhook.Bind != "" && len(c.Webhook.Sources) == 0 {
		return errors.New("webhook bind requires at least one source")
	}
	routes := map[string]bool{}
	for _, r := range c.Routes {
		if !aliasRE.MatchString(r.ID) || routes[r.ID] {
			return errors.New("route IDs must be valid and unique")
		}
		routes[r.ID] = true
		if len(r.Repositories) == 0 || len(r.Events) == 0 || len(r.Actors) == 0 || len(r.Clients) == 0 {
			return fmt.Errorf("route %s requires repositories, events, actors, clients", r.ID)
		}
		if r.Trigger != "any" && r.Trigger != "mention" && r.Trigger != "review_requested" {
			return fmt.Errorf("route %s invalid trigger", r.ID)
		}
		if r.Trigger != "any" && len(r.Names) == 0 {
			return fmt.Errorf("route %s trigger requires names", r.ID)
		}
		for _, repo := range r.Repositories {
			if _, ok := c.Repositories[repo]; !ok {
				return fmt.Errorf("route %s unknown repository", r.ID)
			}
		}
		for _, id := range r.Clients {
			p, ok := c.Clients[id]
			if !ok {
				return fmt.Errorf("route %s unknown client", r.ID)
			}
			if !Contains(p.Capabilities, "events.read") {
				return fmt.Errorf("route %s client lacks events.read", r.ID)
			}
			for _, repo := range r.Repositories {
				if !Contains(p.Repositories, repo) {
					return fmt.Errorf("route %s exceeds client repository grant", r.ID)
				}
			}
		}
	}
	if c.Poll.Enabled {
		if c.Poll.IntervalSeconds == 0 {
			c.Poll.IntervalSeconds = 60
		}
		if c.Poll.IntervalSeconds < 30 {
			return errors.New("poll interval must be at least 30 seconds")
		}
		if len(c.Poll.Repositories) == 0 {
			return errors.New("poll repositories required")
		}
		for _, a := range c.Poll.Repositories {
			if _, ok := c.Repositories[a]; !ok {
				return errors.New("unknown polling repository")
			}
		}
	}
	if c.GitHub.TokenEnv != "" && c.GitHub.TokenFile != "" {
		return errors.New("choose token_env or token_file")
	}
	if len(c.GitHub.Apps) > 0 && (c.GitHub.TokenEnv != "" || c.GitHub.TokenFile != "") {
		return errors.New("choose App auth or token auth, not both")
	}
	installations := map[int64]bool{}
	for _, app := range c.GitHub.Apps {
		if app.AppID <= 0 || app.PrivateKeyFile == "" || len(app.InstallationIDs) == 0 {
			return errors.New("apps require app_id, private_key_file, installation_ids")
		}
		for _, id := range app.InstallationIDs {
			if id <= 0 || installations[id] {
				return errors.New("installation IDs must be positive and unique")
			}
			installations[id] = true
		}
	}
	for _, r := range c.Repositories {
		if len(c.GitHub.Apps) > 0 && !installations[r.InstallationID] {
			return errors.New("repository installation is not configured for App auth")
		}
		if len(c.GitHub.Apps) == 0 && r.InstallationID != 0 {
			return errors.New("machine-user repositories must use installation_id 0")
		}
	}
	if (!c.DryRun || c.Poll.Enabled) && len(c.GitHub.Apps) == 0 && c.GitHub.TokenEnv == "" && c.GitHub.TokenFile == "" {
		return errors.New("GitHub authentication is required for writes or polling")
	}
	return nil
}

func (c *Config) Auth() gh.AuthConfig {
	a := gh.AuthConfig{TokenEnv: c.GitHub.TokenEnv, TokenFile: c.GitHub.TokenFile, Username: c.GitHub.Username}
	for _, v := range c.GitHub.Apps {
		a.Apps = append(a.Apps, gh.AppConfig{BotLogin: v.BotLogin, AppID: v.AppID, PrivateKeyFile: v.PrivateKeyFile, InstallationIDs: v.InstallationIDs})
	}
	return a
}
func Contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
func Loopback(bind string) error {
	h, _, err := net.SplitHostPort(bind)
	if err != nil {
		return errors.New("listener must be literal loopback IP:port")
	}
	ip := net.ParseIP(h)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("listener must use a literal loopback IP")
	}
	return nil
}
func Secret(env, file string) (string, error) {
	if (env == "") == (file == "") {
		return "", errors.New("set exactly one secret env or file")
	}
	var s string
	if env != "" {
		s = os.Getenv(env)
	} else {
		fi, err := os.Lstat(file)
		if err != nil {
			return "", errors.New("secret file unavailable")
		}
		if !fi.Mode().IsRegular() || fi.Mode().Perm()&0077 != 0 || fi.Size() > 8192 {
			return "", errors.New("secret file must be regular and mode 0600 or stricter")
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
			return "", errors.New("secret file must belong to current user")
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return "", errors.New("cannot read secret file")
		}
		s = string(b)
	}
	s = strings.TrimSpace(s)
	if len(s) > 8192 || strings.ContainsAny(s, "\r\n\t ") {
		return "", errors.New("secret must be bounded and contain no whitespace")
	}
	return s, nil
}
