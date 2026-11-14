package client

import (
	"context"
	"encoding/json"
	"errors"
	v1 "github.com/wenext-limited/github-botd/api/v1"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func configured(t *testing.T, endpoint string) *Client {
	t.Helper()
	t.Setenv("BOT_TEST_TOKEN", "test-token")
	c, e := New(Config{Endpoint: endpoint, TokenEnv: "BOT_TEST_TOKEN"})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(c.Close)
	return c
}
func TestHTTPContract(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing auth")
		}
		switch r.URL.Path {
		case "/v1/info":
			json.NewEncoder(w).Encode(v1.ClientInfo{ID: "demo", Repositories: []string{"alias"}})
		case "/v1/actions":
			var a v1.Action
			if e := json.NewDecoder(r.Body).Decode(&a); e != nil {
				t.Error(e)
			}
			if a.Version != v1.Version || a.IdempotencyKey != "stable" {
				t.Error("wrong action")
			}
			w.WriteHeader(202)
			json.NewEncoder(w).Encode(v1.Receipt{ID: "r1", State: "pending"})
		case "/v1/status":
			w.WriteHeader(403)
			w.Write([]byte(`{"code":"denied","message":"forbidden"}`))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer s.Close()
	c := configured(t, s.URL)
	info, e := c.Info(context.Background())
	if e != nil || info.ID != "demo" {
		t.Fatalf("%+v %v", info, e)
	}
	r, e := c.Submit(context.Background(), v1.Action{IdempotencyKey: "stable"})
	if e != nil || r.ID != "r1" {
		t.Fatalf("%+v %v", r, e)
	}
	_, e = c.Status(context.Background())
	var ae *APIError
	if !errors.As(e, &ae) || ae.StatusCode != 403 || ae.Code != "denied" {
		t.Fatalf("%v", e)
	}
}
func TestUnixIntegration(t *testing.T) {
	dir, e := os.MkdirTemp("", "gbc-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s")
	l, e := net.Listen("unix", path)
	if e != nil {
		t.Fatal(e)
	}
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/info" {
			t.Error(r.URL.Path)
		}
		w.Write([]byte(`{"id":"unix-client"}`))
	})}
	go s.Serve(l)
	defer s.Close()
	c := configured(t, "unix://"+path)
	info, e := c.Info(context.Background())
	if e != nil || info.ID != "unix-client" {
		t.Fatalf("%+v %v", info, e)
	}
}
func TestEndpointAndSecretValidation(t *testing.T) {
	t.Setenv("BOT_TEST_TOKEN", "test-token")
	for _, endpoint := range []string{"https://127.0.0.1:80", "http://example.com:80", "http://localhost:80", "http://127.0.0.1", "http://127.0.0.1:80/path", "http://user:pass@127.0.0.1:80", "unix://host/tmp/a"} {
		if _, e := New(Config{Endpoint: endpoint, TokenEnv: "BOT_TEST_TOKEN"}); e == nil {
			t.Errorf("accepted %s", endpoint)
		}
	}
	if _, e := New(Config{TokenEnv: "BOT_TEST_TOKEN", TokenFile: "/no-file"}); e == nil {
		t.Fatal("accepted ambiguous token sources")
	}
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte("test-token\n"), 0600)
	c, e := New(Config{TokenFile: file})
	if e != nil {
		t.Fatal(e)
	}
	c.Close()
}
func TestRedirectBlocked(t *testing.T) {
	var reached atomic.Bool
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Store(true) }))
	defer dest.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, dest.URL, 302) }))
	defer s.Close()
	c := configured(t, s.URL)
	if _, e := c.Info(context.Background()); e == nil {
		t.Fatal("redirect accepted")
	}
	if reached.Load() {
		t.Fatal("credential sent to redirect")
	}
}
func TestCancellation(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer s.Close()
	c := configured(t, s.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, e := c.Info(ctx)
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
}
func TestSubscribeSuccessThenCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var handled, acked atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ack") {
			var a v1.AckRequest
			json.NewDecoder(r.Body).Decode(&a)
			if handled.Load() != 1 || a.Retry || a.LeaseToken != "lease" {
				t.Error("ACK before success or bad lease")
			}
			acked.Add(1)
			w.Write([]byte(`{}`))
			return
		}
		if acked.Load() > 0 {
			cancel()
			// Wait for the transport to observe cancellation before returning,
			// which would otherwise send an empty 200 response to the client.
			<-r.Context().Done()
			return
		}
		w.Write([]byte(`{"deliveries":[{"id":"d","lease_token":"lease","attempt":2,"event":{"id":"e"}}]}`))
	}))
	defer s.Close()
	c := configured(t, s.URL)
	e := c.Subscribe(ctx, func(_ context.Context, d v1.Delivery) error {
		if d.Attempt != 2 {
			t.Error("lost redelivery attempt")
		}
		handled.Add(1)
		return nil
	})
	if !errors.Is(e, context.Canceled) || acked.Load() != 1 {
		t.Fatalf("%v acks=%d", e, acked.Load())
	}
}
func TestSubscribeFailureRetriesNotAck(t *testing.T) {
	var retried bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ack") {
			var a v1.AckRequest
			json.NewDecoder(r.Body).Decode(&a)
			retried = a.Retry && a.LeaseToken == "lease"
			w.Write([]byte(`{}`))
			return
		}
		w.Write([]byte(`{"deliveries":[{"id":"d","lease_token":"lease"}]}`))
	}))
	defer s.Close()
	c := configured(t, s.URL)
	sentinel := errors.New("handler failed")
	e := c.Subscribe(context.Background(), func(context.Context, v1.Delivery) error { return sentinel })
	if !errors.Is(e, sentinel) || !retried {
		t.Fatalf("%v retried=%v", e, retried)
	}
}
func TestStableKeyAndValidation(t *testing.T) {
	if StableKey("a", "b") != StableKey("a", "b") || StableKey("ab", "c") == StableKey("a", "bc") {
		t.Fatal("unstable or ambiguous keys")
	}
	c := configured(t, "http://127.0.0.1:1")
	if _, e := c.Submit(context.Background(), v1.Action{}); e == nil {
		t.Fatal("missing key accepted")
	}
	if e := c.Ack(context.Background(), "../other", v1.AckRequest{LeaseToken: "x"}); e == nil {
		t.Fatal("path accepted")
	}
	if _, e := c.Events(context.Background(), 0, 0); e == nil {
		t.Fatal("invalid limit accepted")
	}
}

func TestDeadLetters(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/deadletters" || r.URL.Query().Get("limit") != "100" {
			t.Error(r.URL)
		}
		w.Write([]byte(`{"deliveries":[{"id":"dead","event":{"id":"event"},"attempts":8,"error":"exhausted"}]}`))
	}))
	defer s.Close()
	c := configured(t, s.URL)
	ds, err := c.DeadLetters(context.Background(), 100)
	if err != nil || len(ds) != 1 || ds[0].ID != "dead" || ds[0].Attempts != 8 {
		t.Fatalf("%+v %v", ds, err)
	}
	if _, err = c.DeadLetters(context.Background(), 0); err == nil {
		t.Fatal("invalid limit accepted")
	}
}
