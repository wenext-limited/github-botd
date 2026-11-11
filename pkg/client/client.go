// Package client talks only to github-botd, never directly to GitHub.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	v1 "github.com/wenext-limited/github-botd/api/v1"
)

const DefaultEndpoint = "unix:///tmp/github-botd/github-botd.sock"

// Config accepts a gateway credential only through an environment variable or file.
// TCP endpoints must use a literal loopback IP; proxies and redirects are disabled.
type Config struct {
	Endpoint  string
	TokenEnv  string
	TokenFile string
	Timeout   time.Duration
}

type Client struct {
	http        *http.Client
	base, token string
}

type APIError struct {
	StatusCode    int
	Code, Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github-botd HTTP %d (%s): %s", e.StatusCode, e.Code, e.Message)
}

func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultEndpoint
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 35 * time.Second
	}
	if cfg.Timeout < time.Second {
		return nil, errors.New("timeout must be at least one second")
	}
	if cfg.TokenEnv != "" && cfg.TokenFile != "" {
		return nil, errors.New("choose token environment variable or file, not both")
	}
	var token string
	if cfg.TokenFile != "" {
		b, err := os.ReadFile(cfg.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("read token file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	} else {
		if cfg.TokenEnv == "" {
			cfg.TokenEnv = "GITHUB_BOTD_TOKEN"
		}
		token = strings.TrimSpace(os.Getenv(cfg.TokenEnv))
	}
	if token == "" || strings.ContainsAny(token, "\r\n\t ") {
		return nil, errors.New("gateway bearer token missing or contains whitespace")
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, errors.New("invalid endpoint")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("endpoint cannot include credentials, query, or fragment")
	}
	tr := &http.Transport{Proxy: nil, ResponseHeaderTimeout: cfg.Timeout, IdleConnTimeout: 90 * time.Second}
	base := "http://localhost"
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	switch u.Scheme {
	case "unix":
		if u.Host != "" || !filepath.IsAbs(u.Path) {
			return nil, errors.New("unix endpoint must contain an absolute socket path")
		}
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", u.Path)
		}
	case "http":
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() || u.Port() == "" || (u.Path != "" && u.Path != "/") {
			return nil, errors.New("HTTP endpoint requires a literal loopback IP, explicit port, and no path")
		}
		tr.DialContext = dialer.DialContext
		base = strings.TrimSuffix(cfg.Endpoint, "/")
	default:
		return nil, errors.New("endpoint must use unix:// or loopback http://")
	}
	return &Client{http: &http.Client{Transport: tr, Timeout: cfg.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("gateway redirects are forbidden") }}, base: base, token: token}, nil
}

// Close frees idle connections. Cancel the subscription context and wait for it
// to return before Close; handlers must honor their context for prompt shutdown.
func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("gateway request: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024+1))
	if err != nil {
		return err
	}
	if len(b) > 8*1024*1024 {
		return errors.New("gateway response exceeds 8 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e v1.Error
		_ = json.Unmarshal(b, &e)
		return &APIError{resp.StatusCode, e.Code, e.Message}
	}
	if out != nil {
		if err = json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("decode gateway response: %w", err)
		}
	}
	return nil
}
func (c *Client) Info(ctx context.Context) (v1.ClientInfo, error) {
	var x v1.ClientInfo
	err := c.do(ctx, "GET", "/v1/info", nil, &x)
	return x, err
}
func (c *Client) Status(ctx context.Context) (json.RawMessage, error) {
	var x json.RawMessage
	err := c.do(ctx, "GET", "/v1/status", nil, &x)
	return x, err
}
func (c *Client) Events(ctx context.Context, limit, wait int) ([]v1.Delivery, error) {
	if limit < 1 || limit > 100 || wait < 0 || wait > 30 {
		return nil, errors.New("limit must be 1..100 and wait 0..30 seconds")
	}
	var x struct {
		Deliveries []v1.Delivery `json:"deliveries"`
	}
	err := c.do(ctx, "GET", "/v1/events?limit="+strconv.Itoa(limit)+"&wait="+strconv.Itoa(wait), nil, &x)
	return x.Deliveries, err
}
func escapedID(id string) (string, error) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\?#") {
		return "", errors.New("invalid resource ID")
	}
	return url.PathEscape(id), nil
}
func (c *Client) Ack(ctx context.Context, id string, a v1.AckRequest) error {
	p, e := escapedID(id)
	if e != nil {
		return e
	}
	if a.LeaseToken == "" {
		return errors.New("lease token required")
	}
	return c.do(ctx, "POST", "/v1/events/"+p+"/ack", a, nil)
}
func (c *Client) Replay(ctx context.Context, id string) error {
	p, e := escapedID(id)
	if e != nil {
		return e
	}
	return c.do(ctx, "POST", "/v1/events/"+p+"/replay", nil, nil)
}
func (c *Client) Submit(ctx context.Context, a v1.Action) (v1.Receipt, error) {
	var x v1.Receipt
	if a.Version == "" {
		a.Version = v1.Version
	}
	if a.IdempotencyKey == "" {
		return x, errors.New("explicit stable idempotency key required")
	}
	err := c.do(ctx, "POST", "/v1/actions", a, &x)
	return x, err
}
func (c *Client) Receipt(ctx context.Context, id string) (v1.Receipt, error) {
	var x v1.Receipt
	p, e := escapedID(id)
	if e != nil {
		return x, e
	}
	err := c.do(ctx, "GET", "/v1/actions/"+p, nil, &x)
	return x, err
}
func (c *Client) Context(ctx context.Context, r v1.ContextRequest) (json.RawMessage, error) {
	var x json.RawMessage
	err := c.do(ctx, "POST", "/v1/context", r, &x)
	return x, err
}

// StableKey is deterministic across processes and redeliveries. Parts should
// include client purpose, event ID, and operation revision, never delivery ID.
func StableKey(parts ...string) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Subscribe sequentially leases one event at a time. Handler errors retry the
// delivery and return; transport/ACK errors return without discarding the lease.
// Redelivery may occur after a crash or lost ACK: persist your own handler state,
// or submit actions using StableKey. There is no exactly-once handler guarantee.
func (c *Client) Subscribe(ctx context.Context, handler func(context.Context, v1.Delivery) error) error {
	if handler == nil {
		return errors.New("handler required")
	}
	for {
		ds, err := c.Events(ctx, 1, 25)
		if err != nil {
			return err
		}
		for _, d := range ds {
			if err = ctx.Err(); err != nil {
				return err
			}
			handlerErr := handler(ctx, d)
			if handlerErr != nil {
				ackErr := c.Ack(ctx, d.ID, v1.AckRequest{LeaseToken: d.LeaseToken, Retry: true, Error: "client handler failed"})
				return errors.Join(handlerErr, ackErr)
			}
			if err = c.Ack(ctx, d.ID, v1.AckRequest{LeaseToken: d.LeaseToken}); err != nil {
				return err
			}
		}
		if len(ds) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

// DeadLetters lists this client's exhausted deliveries eligible for replay.
func (c *Client) DeadLetters(ctx context.Context, limit int) ([]v1.DeadLetter, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("limit must be 1..100")
	}
	var x struct {
		Deliveries []v1.DeadLetter `json:"deliveries"`
	}
	err := c.do(ctx, "GET", "/v1/deadletters?limit="+strconv.Itoa(limit), nil, &x)
	return x.Deliveries, err
}
