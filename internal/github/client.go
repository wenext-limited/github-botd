// Package github implements a deliberately narrow, non-autonomous GitHub REST adapter.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const maxBody = 4 << 20

type AppConfig struct {
	AppID           int64
	PrivateKeyFile  string
	InstallationIDs []int64
	BotLogin        string
}
type AuthConfig struct {
	Apps                          []AppConfig
	TokenEnv, TokenFile, Username string
}
type Options struct {
	BaseURL    string
	HTTPClient *http.Client
}
type app struct {
	id    int64
	key   *rsa.PrivateKey
	login string
}
type cachedToken struct {
	value   string
	expires time.Time
}
type Client struct {
	base            string
	http            *http.Client
	token, username string
	apps            map[int64]*app
	mu              sync.Mutex
	tokens          map[int64]cachedToken
}

// APIError never includes upstream bodies, request URLs, or credentials.
// Retryable means a known rejection (rate limit), not an ambiguous write.
type APIError struct {
	State      string
	StatusCode int
	RetryAfter time.Duration
	Message    string
}

func (e *APIError) Error() string  { return e.Message }
func failure(message string) error { return &APIError{State: "failed", Message: message} }

func New(auth AuthConfig, opts Options) (*Client, error) {
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"))) {
		return nil, failure("invalid trusted GitHub API base URL")
	}
	h := http.Client{Timeout: 30 * time.Second}
	if opts.HTTPClient != nil {
		h = *opts.HTTPClient
		if h.Timeout == 0 {
			h.Timeout = 30 * time.Second
		}
	}
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c := &Client{base: base, http: &h, username: auth.Username, apps: map[int64]*app{}, tokens: map[int64]cachedToken{}}
	if auth.TokenEnv != "" && auth.TokenFile != "" {
		return nil, failure("choose one machine token source")
	}
	if auth.TokenEnv != "" {
		c.token = strings.TrimSpace(os.Getenv(auth.TokenEnv))
	}
	if auth.TokenFile != "" {
		b, e := readSecretFile(auth.TokenFile, 64<<10)
		if e != nil {
			return nil, failure("cannot read machine token file")
		}
		c.token = strings.TrimSpace(string(b))
	}
	if (auth.TokenEnv != "" || auth.TokenFile != "") && c.token == "" {
		return nil, failure("machine token is empty")
	}
	if len(c.token) > 64<<10 || strings.IndexFunc(c.token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return nil, failure("invalid machine token format")
	}
	for _, a := range auth.Apps {
		if a.AppID <= 0 || len(a.InstallationIDs) == 0 {
			return nil, failure("app ID and owned installation IDs required")
		}
		b, e := readSecretFile(a.PrivateKeyFile, 1<<20)
		if e != nil {
			return nil, failure("cannot read app private key")
		}
		p, _ := pem.Decode(b)
		if p == nil {
			return nil, failure("invalid app RSA PEM")
		}
		k, e := x509.ParsePKCS1PrivateKey(p.Bytes)
		if e != nil {
			v, e2 := x509.ParsePKCS8PrivateKey(p.Bytes)
			if e2 != nil {
				return nil, failure("invalid app private key")
			}
			var ok bool
			k, ok = v.(*rsa.PrivateKey)
			if !ok {
				return nil, failure("app key must be RSA")
			}
		}
		if k.N.BitLen() < 2048 {
			return nil, failure("RSA key must be at least 2048 bits")
		}
		for _, id := range a.InstallationIDs {
			if id <= 0 || c.apps[id] != nil {
				return nil, failure("invalid or duplicate installation routing")
			}
			c.apps[id] = &app{a.AppID, k, a.BotLogin}
		}
	}
	if c.token == "" && len(c.apps) == 0 {
		return nil, failure("GitHub authentication is not configured")
	}
	return c, nil
}
func jwt(a *app) (string, error) {
	enc := base64.RawURLEncoding.EncodeToString
	now := time.Now()
	b, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": strconv.FormatInt(a.id, 10)})
	s := enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc(b)
	sum := sha256.Sum256([]byte(s))
	sig, e := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	return s + "." + enc(sig), e
}
func (c *Client) credential(ctx context.Context, id int64) (string, error) {
	if id == 0 && c.token != "" {
		return c.token, nil
	}
	a := c.apps[id]
	if a == nil {
		return "", failure("installation is not owned by a configured app")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := c.tokens[id]; t.value != "" && time.Until(t.expires) > time.Minute {
		return t.value, nil
	}
	j, e := jwt(a)
	if e != nil {
		return "", failure("cannot sign app JWT")
	}
	b, _, e := c.request(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", id), j, map[string]any{}, false)
	if e != nil {
		return "", e
	}
	var t struct {
		Token   string    `json:"token"`
		Expires time.Time `json:"expires_at"`
	}
	if json.Unmarshal(b, &t) != nil || t.Token == "" || time.Until(t.Expires) <= time.Minute {
		return "", failure("invalid installation token response")
	}
	c.tokens[id] = cachedToken{t.Token, t.Expires}
	return t.Token, nil
}
func (c *Client) request(ctx context.Context, method, path, token string, payload any, mutation bool) (json.RawMessage, http.Header, error) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return nil, nil, failure("invalid API path")
	}
	var body io.Reader
	if payload != nil {
		b, e := json.Marshal(payload)
		if e != nil {
			return nil, nil, failure("invalid JSON payload")
		}
		body = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if e != nil {
		return nil, nil, failure("invalid API request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	resp, e := c.http.Do(req)
	if e != nil {
		state := "failed"
		if mutation {
			state = "uncertain"
		}
		return nil, nil, &APIError{State: state, Message: "GitHub request transport failure"}
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if e != nil || len(b) > maxBody {
		state := "failed"
		if mutation {
			state = "uncertain"
		}
		return nil, resp.Header, &APIError{State: state, StatusCode: resp.StatusCode, Message: "GitHub response incomplete or too large"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		state := "failed"
		wait := time.Duration(0)
		if resp.StatusCode == 429 || (resp.StatusCode == 403 && (resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "")) {
			state = "retryable"
			wait = time.Minute
			if n, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && n >= 0 {
				wait = time.Duration(n) * time.Second
			} else if t, e := http.ParseTime(resp.Header.Get("Retry-After")); e == nil {
				wait = time.Until(t)
			} else if n, e := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); e == nil {
				wait = time.Until(time.Unix(n, 0))
			}
			if wait < time.Second {
				wait = time.Second
			}
		}
		if mutation && resp.StatusCode >= 500 {
			state = "uncertain"
		}
		return nil, resp.Header, &APIError{State: state, StatusCode: resp.StatusCode, RetryAfter: wait, Message: fmt.Sprintf("GitHub API returned HTTP %d", resp.StatusCode)}
	}
	if len(b) > 0 && !json.Valid(b) {
		state := "failed"
		if mutation {
			state = "uncertain"
		}
		return nil, resp.Header, &APIError{State: state, Message: "invalid GitHub JSON response"}
	}
	// GitHub repository objects can contain temporary clone tokens. Never expose these.
	if strings.HasPrefix(path, "/app/installations/") && strings.HasSuffix(path, "/access_tokens") {
		return b, resp.Header, nil
	}
	return sanitize(b, c.token, token), resp.Header, nil
}
func sanitize(b []byte, secrets ...string) json.RawMessage {
	var v any
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if decoder.Decode(&v) != nil {
		return b
	}
	var clean func(any) any
	clean = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, z := range x {
				l := strings.ToLower(k)
				if l == "token" || l == "temp_clone_token" || l == "access_token" || l == "refresh_token" || l == "private_key" {
					delete(x, k)
				} else {
					x[k] = clean(z)
				}
			}
		case []any:
			for i := range x {
				x[i] = clean(x[i])
			}
		case string:
			for _, s := range secrets {
				if s != "" {
					x = strings.ReplaceAll(x, s, "[REDACTED]")
				}
			}
			return x
		}
		return v
	}
	out, _ := json.Marshal(clean(v))
	return out
}

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func repoPath(repo string) (string, error) {
	if !repositoryPattern.MatchString(repo) {
		return "", failure("invalid repository")
	}
	for _, s := range strings.Split(repo, "/") {
		if s == "." || s == ".." {
			return "", failure("invalid repository")
		}
	}
	return "/repos/" + repo, nil
}

// IsRetryable reports only definite rate-limit rejections, never ambiguous mutations.
func IsRetryable(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.State == "retryable"
}
