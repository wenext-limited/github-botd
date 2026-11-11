package transport

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/wenext-limited/github-botd/api/v1"
	"github.com/wenext-limited/github-botd/internal/config"
	gh "github.com/wenext-limited/github-botd/internal/github"
)

type flowTransport func(*http.Request) (*http.Response, error)

func (f flowTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// This exercises real signature/routing/store/IPC/outbox/REST adaptation while
// replacing only the external GitHub network. It is not live GitHub verification.
func TestThreePRFlowsThroughGatewayAndGitHubAdapter(t *testing.T) {
	g, ipc, _ := setup(t)
	g.Config.DryRun = false
	p := g.Config.Clients["a"]
	p.Capabilities = append(p.Capabilities, "review.submit", "review.reply")
	g.Config.Clients["a"] = p
	g.Config.Routes = []config.Route{
		{ID: "mentions", Repositories: []string{"r"}, Events: []string{"issue_comment", "pull_request_review_comment"}, Actions: []string{"created"}, Trigger: "mention", Names: []string{"bot"}, Actors: []string{"alice"}, Clients: []string{"a"}},
		{ID: "review-requests", Repositories: []string{"r"}, Events: []string{"pull_request"}, Actions: []string{"review_requested"}, Trigger: "review_requested", Names: []string{"bot"}, Actors: []string{"alice"}, Clients: []string{"a"}},
	}
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	t.Setenv("FLOW_TOKEN", "synthetic-test-token-never-real")
	var mu sync.Mutex
	writes := []string{}
	fake := &http.Client{Transport: flowTransport(func(r *http.Request) (*http.Response, error) {
		if r.Host != "api.github.test" {
			t.Errorf("unexpected API host: %s", r.Host)
		}
		result := `{"id":123}`
		status := 201
		if r.Method == "GET" && r.URL.Path == "/repos/acme/repo/pulls/7" {
			result = `{"head":{"sha":"` + sha + `"}}`
			status = 200
		} else {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			switch r.URL.Path {
			case "/repos/acme/repo/pulls/7/reviews":
				if body["event"] != "COMMENT" || body["commit_id"] != sha {
					t.Errorf("unsafe review payload: %+v", body)
				}
			case "/repos/acme/repo/pulls/7/comments/500/replies":
				if body["body"] != "inline answer" {
					t.Errorf("wrong reply: %+v", body)
				}
			default:
				t.Errorf("unexpected write %s %s", r.Method, r.URL.Path)
			}
			mu.Lock()
			writes = append(writes, r.URL.Path)
			mu.Unlock()
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(result)), Request: r}, nil
	})}
	adapter, err := gh.New(gh.AuthConfig{TokenEnv: "FLOW_TOKEN"}, gh.Options{BaseURL: "https://api.github.test", HTTPClient: fake})
	if err != nil {
		t.Fatal(err)
	}
	g.GitHub = adapter
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- g.RunOutbox(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	rows := []struct {
		event, body, kind string
		comment           int64
	}{
		{"issue_comment", `{"action":"created","repository":{"full_name":"acme/repo"},"sender":{"login":"alice","type":"User"},"issue":{"number":7,"pull_request":{"url":"https://api.github.test/repos/acme/repo/pulls/7"}},"comment":{"id":1,"body":"@bot review"}}`, "review.submit", 0},
		{"pull_request", `{"action":"review_requested","repository":{"full_name":"acme/repo"},"sender":{"login":"alice","type":"User"},"number":7,"pull_request":{"number":7,"head":{"sha":"` + sha + `"}},"requested_reviewer":{"login":"bot"}}`, "review.submit", 0},
		{"pull_request_review_comment", `{"action":"created","repository":{"full_name":"acme/repo"},"sender":{"login":"alice","type":"User"},"pull_request":{"number":7,"head":{"sha":"` + sha + `"}},"comment":{"id":501,"in_reply_to_id":500,"body":"@bot explain"}}`, "review.reply", 500},
	}
	for _, row := range rows {
		t.Run(row.event, func(t *testing.T) {
			w := signedEvent(Webhook(g), row.body, row.event)
			if w.Code != 202 {
				t.Fatal(w.Code, w.Body)
			}
			w = request(ipc, "GET", "/v1/events?wait=0", "", "a")
			var batch struct {
				Deliveries []v1.Delivery `json:"deliveries"`
			}
			if json.Unmarshal(w.Body.Bytes(), &batch) != nil || len(batch.Deliveries) != 1 {
				t.Fatal("expected routed delivery", w.Body)
			}
			d := batch.Deliveries[0]
			a := v1.Action{Version: v1.Version, IdempotencyKey: row.event, Kind: row.kind, Repository: "r", EventID: d.Event.ID, Number: 7, HeadSHA: sha, Body: "caller-supplied findings", CommentID: row.comment}
			if row.kind == "review.reply" {
				a.Body = "inline answer"
			}
			b, _ := json.Marshal(a)
			w = request(ipc, "POST", "/v1/actions", string(b), "a")
			if w.Code != 202 {
				t.Fatal(w.Code, w.Body)
			}
			var receipt v1.Receipt
			json.Unmarshal(w.Body.Bytes(), &receipt)
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				w = request(ipc, "GET", "/v1/actions/"+receipt.ID, "", "a")
				json.Unmarshal(w.Body.Bytes(), &receipt)
				if receipt.State == "succeeded" {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if receipt.State != "succeeded" {
				t.Fatalf("receipt not completed: %+v", receipt)
			}
			ack, _ := json.Marshal(v1.AckRequest{LeaseToken: d.LeaseToken})
			if w = request(ipc, "POST", "/v1/events/"+d.ID+"/ack", string(ack), "a"); w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if len(writes) != 3 {
		t.Fatalf("expected exactly3 outbound writes, got %v", writes)
	}
}

func signedEvent(h http.Handler, body, eventType string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/hooks/test", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-GitHub-Event", eventType)
	r.Header.Set("X-GitHub-Delivery", "flow-"+eventType)
	mac := hmac.New(sha256.New, []byte(strings.Repeat("w", 32)))
	mac.Write([]byte(body))
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestWorkflowToAppCheckAndCheckUpdate(t *testing.T) {
	g, ipc, _ := setup(t)
	g.Config.DryRun = false
	g.Config.Repositories["r"] = config.Repository{FullName: "acme/repo", InstallationID: 1}
	source := g.Config.Webhook.Sources["test"]
	source.InstallationIDs = []int64{1}
	g.Config.Webhook.Sources["test"] = source
	p := g.Config.Clients["a"]
	p.Capabilities = append(p.Capabilities, "check.create", "check.update")
	g.Config.Clients["a"] = p
	g.Config.Routes = []config.Route{{ID: "ci", Repositories: []string{"r"}, Events: []string{"workflow_run", "check_run"}, Trigger: "any", Actors: []string{"alice"}, Clients: []string{"a"}}}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "key.pem")
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	const sha = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	var mu sync.Mutex
	writes := []string{}
	fake := &http.Client{Transport: flowTransport(func(r *http.Request) (*http.Response, error) {
		result := `{"id":900,"app":{"id":77}}`
		status := 200
		switch {
		case r.URL.Path == "/app/installations/1/access_tokens":
			result = `{"token":"synthetic-installation-token","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`
			status = 201
		case r.Method == "GET" && r.URL.Path == "/repos/acme/repo/check-runs/900":
		case (r.Method == "POST" && r.URL.Path == "/repos/acme/repo/check-runs") || (r.Method == "PATCH" && r.URL.Path == "/repos/acme/repo/check-runs/900"):
			var b map[string]any
			if e := json.NewDecoder(r.Body).Decode(&b); e != nil {
				t.Error(e)
			}
			if r.Method == "POST" && b["head_sha"] != sha {
				t.Error("wrong check commit")
			}
			if b["conclusion"] != "success" {
				t.Error("wrong check result")
			}
			mu.Lock()
			writes = append(writes, r.Method)
			mu.Unlock()
			status = 201
		default:
			t.Errorf("unexpected GitHub path %s %s", r.Method, r.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(result)), Request: r}, nil
	})}
	adapter, err := gh.New(gh.AuthConfig{Apps: []gh.AppConfig{{AppID: 77, PrivateKeyFile: keyPath, InstallationIDs: []int64{1}, BotLogin: "ci[bot]"}}}, gh.Options{BaseURL: "https://api.github.test", HTTPClient: fake})
	if err != nil {
		t.Fatal(err)
	}
	g.GitHub = adapter
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- g.RunOutbox(ctx) }()
	defer func() {
		cancel()
		if e := <-done; e != nil {
			t.Error(e)
		}
	}()
	for _, kind := range []string{"workflow_run", "check_run"} {
		body := `{"action":"completed","repository":{"full_name":"acme/repo"},"installation":{"id":1},"sender":{"login":"alice","type":"User"},"` + kind + `":{"id":900,"head_sha":"` + sha + `","status":"completed","conclusion":"success"}}`
		w := signedEvent(Webhook(g), body, kind)
		if w.Code != 202 {
			t.Fatal(w.Code, w.Body)
		}
		w = request(ipc, "GET", "/v1/events?wait=0", "", "a")
		var ds struct {
			Deliveries []v1.Delivery `json:"deliveries"`
		}
		json.Unmarshal(w.Body.Bytes(), &ds)
		if len(ds.Deliveries) != 1 {
			t.Fatal(w.Body)
		}
		d := ds.Deliveries[0]
		action := v1.Action{Version: v1.Version, IdempotencyKey: kind, Repository: "r", EventID: d.Event.ID, Kind: "check.create", Name: "Local CI", HeadSHA: sha, Status: "completed", Conclusion: "success", Summary: "Synthetic local result"}
		if kind == "check_run" {
			action.Kind = "check.update"
			action.CheckRunID = d.Event.CheckRunID
		}
		b, _ := json.Marshal(action)
		w = request(ipc, "POST", "/v1/actions", string(b), "a")
		if w.Code != 202 {
			t.Fatal(w.Code, w.Body)
		}
		var receipt v1.Receipt
		json.Unmarshal(w.Body.Bytes(), &receipt)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			w = request(ipc, "GET", "/v1/actions/"+receipt.ID, "", "a")
			json.Unmarshal(w.Body.Bytes(), &receipt)
			if receipt.State == "succeeded" {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if receipt.State != "succeeded" {
			t.Fatal(receipt)
		}
		ack, _ := json.Marshal(v1.AckRequest{LeaseToken: d.LeaseToken})
		if w = request(ipc, "POST", "/v1/events/"+d.ID+"/ack", string(ack), "a"); w.Code != 200 {
			t.Fatal(w.Body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(writes, ",") != "POST,PATCH" {
		t.Fatalf("wrong CI writes %v", writes)
	}
}
