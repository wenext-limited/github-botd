package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/wenext-limited/github-botd/api/v1"
	gh "github.com/wenext-limited/github-botd/internal/github"
)

func TestMergeWireAndExecutionRecheck(t *testing.T) {
	for _, scenario := range []string{"merge", "role-changed", "grant-revoked"} {
		t.Run(scenario, func(t *testing.T) {
			g, ipc, _ := setup(t)
			g.Config.DryRun = false
			p := g.Config.Clients["a"]
			p.AllRepositories = true
			p.Capabilities = []string{"context.read", "pull.merge", "actions.proactive"}
			p.MergeChecks = []v1.TrustedCheck{{Name: "compile", AppID: 7}}
			g.Config.Clients["a"] = p
			head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
			var roleChanged, writes atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/reviews"):
					w.Write([]byte(`[]`))
				case strings.HasSuffix(r.URL.Path, "/check-runs"):
					json.NewEncoder(w).Encode(map[string]any{"check_runs": []any{map[string]any{"id": 1, "name": "compile", "head_sha": head, "app": map[string]any{"id": 7}, "status": "completed", "conclusion": "success"}}})
				case r.Method == "PUT":
					writes.Store(true)
					json.NewEncoder(w).Encode(map[string]any{"merged": true, "sha": strings.Repeat("c", 40)})
				default:
					user := 123
					if roleChanged.Load() {
						user = 999
					}
					json.NewEncoder(w).Encode(map[string]any{"state": "open", "mergeable": true, "mergeable_state": "clean", "user": map[string]any{"id": user}, "assignees": []any{}, "head": map[string]any{"sha": head}, "base": map[string]any{"sha": base, "repo": map[string]any{"id": 111, "full_name": "YouPast/FrameUI"}}})
				}
			}))
			defer server.Close()
			t.Setenv("MERGE_TEST_GITHUB_TOKEN", "fixture-token")
			adapter, err := gh.New(gh.AuthConfig{TokenEnv: "MERGE_TEST_GITHUB_TOKEN"}, gh.Options{BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			g.GitHub = adapter
			a := v1.Action{Version: v1.Version, Kind: "pull.merge", Repository: "YouPast/FrameUI", RepositoryID: 111, Number: 69, HeadSHA: head, IdempotencyKey: "merge-test", Merge: &v1.MergeEvidence{RequesterID: 123, BaseSHA: base, ReviewDigest: strings.Repeat("d", 64)}}
			body, _ := json.Marshal(a)
			if response := request(ipc, "POST", "/v1/merge-check", string(body), "a"); response.Code != 200 {
				t.Fatalf("preflight: %d %s", response.Code, response.Body)
			}
			if response := request(ipc, "POST", "/v1/merge-check", string(body), "b"); response.Code == 200 {
				t.Fatal("client without merge capability passed")
			}
			response := request(ipc, "POST", "/v1/actions", string(body), "a")
			if response.Code != 202 {
				t.Fatalf("enqueue: %d %s", response.Code, response.Body)
			}
			var receipt v1.Receipt
			json.Unmarshal(response.Body.Bytes(), &receipt)
			if scenario == "role-changed" {
				roleChanged.Store(true)
			}
			if scenario == "grant-revoked" {
				p.AllRepositories = false
				g.Config.Clients["a"] = p
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- g.RunOutbox(ctx) }()
			defer func() { cancel(); <-done }()
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				receipt, err = g.Store.Receipt("a", receipt.ID)
				if err != nil {
					t.Fatal(err)
				}
				if receipt.State == "succeeded" || receipt.State == "failed" {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if scenario == "merge" {
				if receipt.State != "succeeded" || !writes.Load() {
					t.Fatalf("valid merge: %+v", receipt)
				}
			} else if receipt.State != "failed" || writes.Load() {
				t.Fatalf("stale authorization wrote: %+v", receipt)
			}
		})
	}
}
