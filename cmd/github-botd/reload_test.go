package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/wenext-limited/github-botd/api/v1"
	"github.com/wenext-limited/github-botd/internal/config"
	"github.com/wenext-limited/github-botd/internal/gateway"
	"github.com/wenext-limited/github-botd/internal/store"
)

func writeConfig(t *testing.T, path string, c *config.Config) {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path+".new", raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
}

func reloadFixture(t *testing.T) (*daemonConfig, string, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	for name, value := range map[string]string{"client": strings.Repeat("a", 32), "webhook": strings.Repeat("w", 32)} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := &config.Config{
		Version: 1, StateDir: "state", DryRun: true,
		IPC:          config.IPC{Socket: "/tmp/botd-reload-test.sock"},
		Webhook:      config.Webhook{Sources: map[string]config.Source{"main": {SecretFile: "webhook"}}},
		Repositories: map[string]config.Repository{"repo": {FullName: "acme/repo"}},
		Clients:      map[string]config.Client{"agent": {TokenFile: "client", Repositories: []string{"repo"}, Capabilities: []string{"events.read", "issue.comment.create", "actions.proactive"}}},
		Routes:       []config.Route{{ID: "all", Repositories: []string{"repo"}, Events: []string{"issue_comment"}, Trigger: "any", Actors: []string{"alice"}, Clients: []string{"agent"}}},
	}
	path := filepath.Join(dir, "config.json")
	writeConfig(t, path, c)
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(loaded.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	d, err := newDaemonConfig(gateway.New(loaded, s, nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.current.Load().cancel() })
	return d, path, c
}

func ipcRequest(d *daemonConfig, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", "Bearer "+strings.Repeat(token, 32))
	w := httptest.NewRecorder()
	d.handler(false).ServeHTTP(w, r)
	return w
}

func webhookRequest(d *daemonConfig, actor, token, delivery string) *httptest.ResponseRecorder {
	body := `{"action":"created","repository":{"full_name":"acme/repo"},"sender":{"login":"` + actor + `","type":"User"},"issue":{"number":1},"comment":{"id":1,"body":"hello"}}`
	r := httptest.NewRequest("POST", "/hooks/main", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-GitHub-Event", "issue_comment")
	r.Header.Set("X-GitHub-Delivery", delivery)
	mac := hmac.New(sha256.New, []byte(strings.Repeat(token, 32)))
	mac.Write([]byte(body))
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	d.handler(true).ServeHTTP(w, r)
	return w
}

func TestReloadUpdatesAuthRoutingAndPreservesQueue(t *testing.T) {
	d, path, c := reloadFixture(t)
	old := d.gateway()
	if w := webhookRequest(d, "alice", "w", "before"); w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	// Rotate files in place without changing their configured paths.
	for name, value := range map[string]string{"client": strings.Repeat("b", 32), "webhook": strings.Repeat("x", 32)} {
		if err := os.WriteFile(filepath.Join(filepath.Dir(path), name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c.Routes[0].Actors = []string{"bob"}
	writeConfig(t, path, c)
	if changed, err := d.reload(path, false); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if d.gateway().Store != old.Store {
		t.Fatal("reload replaced durable store")
	}
	if w := ipcRequest(d, "/v1/info", "a"); w.Code != 401 {
		t.Fatal("old bearer accepted", w.Code)
	}
	if w := ipcRequest(d, "/v1/info", "b"); w.Code != 200 {
		t.Fatal("new bearer rejected", w.Code)
	}
	if w := webhookRequest(d, "bob", "w", "bad-signature"); w.Code != 401 {
		t.Fatal("old webhook secret accepted", w.Code)
	}
	for _, actor := range []string{"alice", "bob"} {
		if w := webhookRequest(d, actor, "x", "after-"+actor); w.Code != 202 {
			t.Fatal(w.Code, w.Body)
		}
	}
	rows, err := d.gateway().Store.ClaimAllowed("agent", 10, time.Minute, []string{"repo"})
	if err != nil || len(rows) != 2 {
		t.Fatalf("old queue/new routing: %+v %v", rows, err)
	}
	actors := map[string]bool{}
	for _, row := range rows {
		actors[row.Event.Actor] = true
	}
	if !actors["alice"] || !actors["bob"] {
		t.Fatal("existing queue or new route missing", actors)
	}
	unchanged := d.current.Load()
	if changed, err := d.reload(path, false); err != nil || changed || unchanged != d.current.Load() {
		t.Fatal("unchanged config rebuilt runtime", changed, err)
	}
}

func TestRejectedReloadKeepsActiveGeneration(t *testing.T) {
	for _, kind := range []string{"malformed", "invalid", "listener", "state", "binding", "credentials"} {
		t.Run(kind, func(t *testing.T) {
			d, path, c := reloadFixture(t)
			old := d.current.Load()
			switch kind {
			case "invalid":
				c.Clients = nil
			case "listener":
				c.IPC.Bind = "127.0.0.1:9999"
			case "state":
				c.StateDir = "other-state"
			case "binding":
				c.Repositories["repo"] = config.Repository{FullName: "acme/other"}
			case "credentials":
				c.GitHub.TokenFile = "missing-token"
			}
			writeConfig(t, path, c)
			if kind == "malformed" {
				if err := os.WriteFile(path, []byte(`{"version":`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if changed, err := d.reload(path, false); err == nil || changed {
				t.Fatal("bad reload accepted", changed, err)
			}
			if d.current.Load() != old || old.retired.Err() != nil {
				t.Fatal("failed reload changed active generation")
			}
			if w := ipcRequest(d, "/v1/info", "a"); w.Code != 200 {
				t.Fatal("failed reload interrupted service", w.Code)
			}
		})
	}
}

func TestWatcherRecoversAndServesConcurrentRequests(t *testing.T) {
	d, path, c := reloadFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	signals := make(chan os.Signal, 1)
	go func() { defer close(done); d.watch(ctx, path, signals, 10*time.Millisecond) }()
	t.Cleanup(func() { cancel(); <-done })
	var requests sync.WaitGroup
	for i := 0; i < 4; i++ {
		requests.Add(1)
		go func() {
			defer requests.Done()
			for j := 0; j < 100; j++ {
				if w := ipcRequest(d, "/v1/info", "a"); w.Code != 200 {
					t.Errorf("request during reload: %d", w.Code)
				}
			}
		}()
	}
	old := d.current.Load()
	if err := os.WriteFile(path, []byte(`{"version":`), 0600); err != nil {
		t.Fatal(err)
	}
	signals <- os.Interrupt // watch treats any delivered signal as a forced reload.
	time.Sleep(30 * time.Millisecond)
	if d.current.Load() != old {
		t.Fatal("invalid edit replaced config")
	}
	c.Routes[0].Actors = []string{"bob"}
	writeConfig(t, path, c)
	await(t, func() bool { return d.current.Load() != old })
	requests.Wait()
	old = d.current.Load()
	signals <- os.Interrupt
	await(t, func() bool { return d.current.Load() != old })
}

func TestReloadRetiresLongPoll(t *testing.T) {
	d, path, c := reloadFixture(t)
	// The hook confirms that the request captured the old generation before
	// its first database claim, without depending on a timing sleep.
	old := d.current.Load()
	started := make(chan struct{})
	h := old.ipc
	old.ipc = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); h.ServeHTTP(w, r) })
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- ipcRequest(d, "/v1/events?wait=30", "a") }()
	<-started
	p := c.Clients["agent"]
	p.Capabilities = []string{"issue.comment.create"}
	c.Clients["agent"] = p
	c.Routes = nil
	writeConfig(t, path, c)
	if _, err := d.reload(path, false); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-done:
		var batch struct{ Deliveries []v1.Delivery }
		if err := json.Unmarshal(w.Body.Bytes(), &batch); err != nil || len(batch.Deliveries) != 0 {
			t.Fatal("retired long poll did not return an empty batch", w.Body, err)
		}
	case <-time.After(time.Second):
		t.Fatal("old long poll kept running")
	}
	if w := ipcRequest(d, "/v1/events?wait=0", "a"); w.Code != 403 {
		t.Fatal("revoked grant accepted", w.Code)
	}
}

type blockingGitHub struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (g *blockingGitHub) Execute(ctx context.Context, _ int64, _ v1.Action) (json.RawMessage, error) {
	if g.calls.Add(1) == 1 {
		close(g.started)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.release:
		return json.RawMessage(`{"id":1}`), nil
	}
}
func (g *blockingGitHub) Context(context.Context, int64, v1.ContextRequest) (json.RawMessage, error) {
	return nil, nil
}

func TestReloadPreservesRunningWriteAndRevokesQueuedAction(t *testing.T) {
	d, path, c := reloadFixture(t)
	fake := &blockingGitHub{started: make(chan struct{}), release: make(chan struct{})}
	// Use a controlled adapter to observe the boundary between a write already
	// sent to GitHub and an action still waiting in the durable queue.
	loaded := *d.gateway().Config
	loaded.DryRun = false
	if err := d.install(gateway.New(&loaded, d.gateway().Store, fake)); err != nil {
		t.Fatal(err)
	}
	a := v1.Action{Version: v1.Version, IdempotencyKey: "running", Kind: "issue.comment.create", Repository: "repo", Number: 1, Body: "hello"}
	first, err := d.gateway().Submit("agent", a)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gateway.RunOutbox(ctx, d.gateway) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	select {
	case <-fake.started:
	case <-time.After(2 * time.Second):
		t.Fatal("outbox did not start")
	}
	a.IdempotencyKey = "queued"
	second, err := d.gateway().Submit("agent", a)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Clients["agent"]
	p.Capabilities = []string{"events.read"}
	c.Clients["agent"] = p
	writeConfig(t, path, c)
	if _, err = d.reload(path, false); err != nil {
		t.Fatal(err)
	}
	close(fake.release)
	await(t, func() bool {
		a, _ := d.gateway().Store.Receipt("agent", first.ID)
		b, _ := d.gateway().Store.Receipt("agent", second.ID)
		return a.State == "succeeded" && b.State == "failed"
	})
	if fake.calls.Load() != 1 {
		t.Fatal("revoked queued action executed")
	}
}

func TestForcedReloadReadsGitHubTokenFile(t *testing.T) {
	d, path, c := reloadFixture(t)
	seen := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	c.GitHub = config.GitHub{BaseURL: server.URL, TokenFile: "github-token"}
	tokenPath := filepath.Join(filepath.Dir(path), "github-token")
	if err := os.WriteFile(tokenPath, []byte("old-token"), 0600); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, path, c)
	if _, err := d.reload(path, false); err != nil {
		t.Fatal(err)
	}
	read := func(expected string) {
		t.Helper()
		if _, err := d.gateway().GitHub.Context(context.Background(), 0, v1.ContextRequest{Repository: "acme/repo", Kind: "pull", Number: 1}); err != nil {
			t.Fatal(err)
		}
		if got := <-seen; got != "Bearer "+expected {
			t.Fatal("unexpected GitHub credential", got)
		}
	}
	read("old-token")
	if err := os.WriteFile(tokenPath, []byte("new-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.reload(path, true); err != nil {
		t.Fatal(err)
	}
	read("new-token")
}

func await(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
