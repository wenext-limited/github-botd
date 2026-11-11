package github

import (
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
	v1 "github.com/wenext-limited/github-botd/api/v1"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func machine(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	t.Setenv("BOTD_TEST_TOKEN", "test-secret")
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	c, e := New(AuthConfig{TokenEnv: "BOTD_TEST_TOKEN"}, Options{BaseURL: s.URL})
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func TestAuthJWTAndCache(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	path := filepath.Join(t.TempDir(), "key.pem")
	os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600)
	var exchanges atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/123/access_tokens" {
			exchanges.Add(1)
			parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
			if len(parts) != 3 {
				t.Error("invalid JWT")
				w.WriteHeader(401)
				return
			}
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, hash[:], sig) != nil {
				t.Error("invalid signature")
			}
			claim, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var claims map[string]any
			json.Unmarshal(claim, &claims)
			if claims["iss"] != "42" {
				t.Error(claims)
			}
			if claims["exp"].(float64)-claims["iat"].(float64) > 600 {
				t.Error("JWT exceeds GitHub maximum")
			}
			fmt.Fprintf(w, `{"token":"install-secret","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		if r.Header.Get("Authorization") != "Bearer install-secret" {
			t.Error("bad token")
		}
		fmt.Fprint(w, `{"id":1}`)
	}))
	defer s.Close()
	c, e := New(AuthConfig{Apps: []AppConfig{{AppID: 42, PrivateKeyFile: path, InstallationIDs: []int64{123}}}}, Options{BaseURL: s.URL})
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := c.Context(context.Background(), 123, v1.ContextRequest{Repository: "o/r", Number: 1, Kind: "pull"}); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if exchanges.Load() != 1 {
		t.Fatal(exchanges.Load())
	}
	c.tokens[123] = cachedToken{"expired", time.Now().Add(-time.Second)}
	if _, e = c.credential(context.Background(), 123); e != nil {
		t.Fatal(e)
	}
	if exchanges.Load() != 2 {
		t.Fatal("not refreshed")
	}
	if _, e = c.credential(context.Background(), 456); e == nil {
		t.Fatal("unowned installation accepted")
	}
}
func TestRequestClassificationAndSanitization(t *testing.T) {
	for _, tc := range []struct {
		code int
		rate bool
		want string
	}{{400, false, "failed"}, {401, false, "failed"}, {403, false, "failed"}, {403, true, "retryable"}, {429, false, "retryable"}, {500, false, "uncertain"}, {503, false, "uncertain"}} {
		t.Run(fmt.Sprint(tc.code, tc.rate), func(t *testing.T) {
			c := machine(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.rate {
					w.Header().Set("X-RateLimit-Remaining", "0")
				}
				w.WriteHeader(tc.code)
				fmt.Fprint(w, `{"message":"test-secret"}`)
			})
			_, e := c.Execute(context.Background(), 0, v1.Action{Kind: "issue.comment.create", Repository: "o/r", Number: 1, Body: "hi"})
			var ae *APIError
			if !errors.As(e, &ae) || ae.State != tc.want {
				t.Fatalf("%+v", e)
			}
			if strings.Contains(e.Error(), "test-secret") {
				t.Fatal("secret leak")
			}
		})
	}
	c := machine(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"temp_clone_token":"hidden","body":"test-secret","nested":{"access_token":"hidden"},"ok":1}`)
	})
	b, e := c.Context(context.Background(), 0, v1.ContextRequest{Repository: "o/r", Number: 1, Kind: "pull"})
	if e != nil || strings.Contains(string(b), "secret") || strings.Contains(string(b), "hidden") {
		t.Fatalf("%s %v", b, e)
	}
}
func TestTransportAmbiguity(t *testing.T) {
	c := machine(t, func(w http.ResponseWriter, r *http.Request) { conn, _, _ := w.(http.Hijacker).Hijack(); conn.Close() })
	_, e := c.Execute(context.Background(), 0, v1.Action{Kind: "issue.comment.create", Repository: "o/r", Number: 1, Body: "hi"})
	var ae *APIError
	if !errors.As(e, &ae) || ae.State != "uncertain" {
		t.Fatal(e)
	}
}
func TestPaginationNeverFollowsHost(t *testing.T) {
	var leaked atomic.Bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer evil.Close()
	var pages int
	c := machine(t, func(w http.ResponseWriter, r *http.Request) {
		pages++
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", "<"+evil.URL+`/steal>; rel="next"`)
		}
		fmt.Fprintf(w, `[{"id":%d}]`, pages)
	})
	b, e := c.Context(context.Background(), 0, v1.ContextRequest{Repository: "o/r", Number: 1, Kind: "files"})
	if e != nil || string(b) != `[{"id":1},{"id":2}]` || leaked.Load() {
		t.Fatalf("%s %v %v", b, e, leaked.Load())
	}
}
func TestRedirectBlocked(t *testing.T) {
	c := machine(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "https://example.com/steal", 302) })
	_, e := c.Context(context.Background(), 0, v1.ContextRequest{Repository: "o/r", Number: 1, Kind: "pull"})
	var ae *APIError
	if !errors.As(e, &ae) || ae.StatusCode != 302 {
		t.Fatal(e)
	}
}
func TestActionSafety(t *testing.T) {
	for _, kind := range []string{"pull.merge", "review.approve", "review.request_changes", "repository.delete", "GET"} {
		if ValidateAction(v1.Action{Kind: kind, Repository: "o/r"}) == nil {
			t.Fatal(kind)
		}
	}
	for _, repo := range []string{"../r", "o/..", "o/r/evil", "o/r?x", "https://evil/r"} {
		if ValidateAction(v1.Action{Kind: "issue.comment.create", Repository: repo, Number: 1, Body: "hi"}) == nil {
			t.Fatal(repo)
		}
	}
	sha := strings.Repeat("a", 40)
	writes := 0
	c := machine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprintf(w, `{"head":{"sha":%q}}`, sha)
			return
		}
		writes++
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		if p["event"] != "COMMENT" {
			t.Errorf("unsafe review: %v", p)
		}
		fmt.Fprint(w, `{"id":1}`)
	})
	a := v1.Action{Kind: "review.submit", Repository: "o/r", Number: 1, Body: "hi", HeadSHA: sha}
	if _, e := c.Execute(context.Background(), 0, a); e != nil {
		t.Fatal(e)
	}
	a.HeadSHA = strings.Repeat("b", 40)
	if _, e := c.Execute(context.Background(), 0, a); e == nil {
		t.Fatal("stale head allowed")
	}
	if writes != 1 {
		t.Fatal(writes)
	}
}
func TestCommentOwnership(t *testing.T) {
	for _, own := range []bool{true, false} {
		t.Run(fmt.Sprint(own), func(t *testing.T) {
			writes := 0
			c := machine(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "PATCH" {
					writes++
					fmt.Fprint(w, `{"id":2}`)
					return
				}
				if r.URL.Path == "/user" {
					fmt.Fprint(w, `{"id":7}`)
					return
				}
				id := 8
				if own {
					id = 7
				}
				fmt.Fprintf(w, `{"user":{"id":%d},"issue_url":%q}`, id, "http://"+r.Host+"/repos/o/r/issues/1")
			})
			_, e := c.Execute(context.Background(), 0, v1.Action{Kind: "issue.comment.update", Repository: "o/r", Number: 1, CommentID: 2, Body: "updated"})
			if own && (e != nil || writes != 1) {
				t.Fatal(e, writes)
			}
			if !own && (e == nil || writes != 0) {
				t.Fatal("modified another user's comment")
			}
		})
	}
}
func TestBoundedResponse(t *testing.T) {
	c := machine(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", maxBody+1)) })
	_, e := c.Context(context.Background(), 0, v1.ContextRequest{Kind: "pull", Repository: "o/r", Number: 1})
	if e == nil {
		t.Fatal("oversized body accepted")
	}
}

func TestMutationRoutes(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		a            v1.Action
		method, path string
	}{
		{v1.Action{Kind: "issue.comment.create", Number: 3, Body: "hi"}, "POST", "/repos/o/r/issues/3/comments"},
		{v1.Action{Kind: "review.reply", Number: 3, CommentID: 7, Body: "hi"}, "POST", "/repos/o/r/pulls/3/comments/7/replies"},
		{v1.Action{Kind: "review.comment", Number: 3, HeadSHA: sha, Body: "hi", Path: "a.go", Line: 2, Side: "RIGHT"}, "POST", "/repos/o/r/pulls/3/comments"},
		{v1.Action{Kind: "check.create", HeadSHA: sha, Name: "bot", Status: "in_progress", Summary: "working", DetailsURL: "https://example.com/check"}, "POST", "/repos/o/r/check-runs"},
		{v1.Action{Kind: "check.update", CheckRunID: 7, Status: "completed", Conclusion: "success", Summary: "done"}, "PATCH", "/repos/o/r/check-runs/7"},
	} {
		t.Run(tc.a.Kind, func(t *testing.T) {
			writes := 0
			c := machine(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					fmt.Fprintf(w, `{"head":{"sha":%q},"app":{"id":42}}`, sha)
					return
				}
				writes++
				if r.Method != tc.method || r.URL.Path != tc.path {
					t.Error(r.Method, r.URL.Path)
				}
				var body map[string]any
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid JSON")
				}
				if tc.a.Kind == "review.comment" && (body["commit_id"] != sha || body["line"] != float64(2)) {
					t.Error(body)
				}
				fmt.Fprint(w, `{"id":1}`)
			})
			tc.a.Repository = "o/r"
			installationID := int64(0)
			if strings.HasPrefix(tc.a.Kind, "check.") {
				installationID = 123
				c.apps[123] = &app{id: 42}
				c.tokens[123] = cachedToken{"test-secret", time.Now().Add(time.Hour)}
			}
			if _, e := c.Execute(context.Background(), installationID, tc.a); e != nil {
				t.Fatal(e)
			}
			if writes != 1 {
				t.Fatal(writes)
			}
		})
	}
}
func TestContextRoutes(t *testing.T) {
	for _, tc := range []struct {
		kind, path string
		list       bool
	}{{"pull", "/pulls/3", false}, {"files", "/pulls/3/files", true}, {"issue_comments", "/issues/3/comments", true}, {"review_comments", "/pulls/3/comments", true}, {"reviews", "/pulls/3/reviews", true}, {"workflow_run", "/actions/runs/7", false}, {"check_run", "/check-runs/7", false}} {
		t.Run(tc.kind, func(t *testing.T) {
			c := machine(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/repos/o/r"+tc.path {
					t.Error(r.Method, r.URL.Path)
				}
				if tc.list {
					fmt.Fprint(w, `[]`)
				} else {
					fmt.Fprint(w, `{}`)
				}
			})
			if _, e := c.Context(context.Background(), 0, v1.ContextRequest{Repository: "o/r", Kind: tc.kind, Number: 3, ID: 7}); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestInvalidPayloadsNoNetwork(t *testing.T) {
	for _, a := range []v1.Action{
		{Kind: "issue.comment.create", Number: 1, Body: " "},
		{Kind: "review.reply", Number: 1, Body: "hi"},
		{Kind: "review.submit", Number: 1, Body: "hi"},
		{Kind: "review.comment", Number: 1, Body: "hi", Path: "a", Line: 1, Side: "RIGHT"},
		{Kind: "check.create", Name: "test"},
		{Kind: "check.update", CheckRunID: 1, Status: "completed"},
		{Kind: "check.update", CheckRunID: 1, Conclusion: "success"},
		{Kind: "check.update", CheckRunID: 1, Status: "approved"},
		{Kind: "check.update", CheckRunID: 1, Status: "completed", Conclusion: "approved"},
		{Kind: "check.update", CheckRunID: 1, DetailsURL: "http://evil/x"},
	} {
		a.Repository = "o/r"
		if e := ValidateAction(a); e == nil {
			t.Fatalf("accepted %+v", a)
		}
	}
}
func TestAuthConfigurationFailures(t *testing.T) {
	t.Setenv("BOTD_EMPTY", "")
	for _, a := range []AuthConfig{{}, {TokenEnv: "BOTD_EMPTY"}, {TokenFile: "/nonexistent/botd"}, {TokenEnv: "X", TokenFile: "Y"}, {Apps: []AppConfig{{AppID: 1}}}, {Apps: []AppConfig{{AppID: 1, InstallationIDs: []int64{1}, PrivateKeyFile: "/nonexistent/key"}}}} {
		if _, e := New(a, Options{}); e == nil {
			t.Fatal("accepted invalid auth")
		}
	}
	for _, u := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com?token=abc", "https://example.com#secret", "garbage"} {
		if _, e := New(AuthConfig{}, Options{BaseURL: u}); e == nil {
			t.Fatal(u)
		}
	}
}

func TestCommentUpdateIssueCorrelation(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		number    int64
		allowed   bool
	}{
		{"same issue", "/repos/o/r/issues/1", 1, true},
		{"different issue", "/repos/o/r/issues/2", 1, false},
		{"different repository", "/repos/o/other/issues/1", 1, false},
		{"wrong host", "https://evil.invalid/repos/o/r/issues/1", 1, false},
		{"URL query", "/repos/o/r/issues/1?ignored=true", 1, false},
		{"missing issue URL", "", 1, false},
		{"missing number", "/repos/o/r/issues/1", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			c := machine(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "PATCH" {
					writes++
					fmt.Fprint(w, `{"id":2}`)
					return
				}
				if r.URL.Path == "/user" {
					fmt.Fprint(w, `{"id":7}`)
					return
				}
				issueURL := tc.url
				if strings.HasPrefix(issueURL, "/") {
					issueURL = "http://" + r.Host + issueURL
				}
				fmt.Fprintf(w, `{"user":{"id":7},"issue_url":%q}`, issueURL)
			})
			_, err := c.Execute(context.Background(), 0, v1.Action{Kind: "issue.comment.update", Repository: "o/r", Number: tc.number, CommentID: 2, Body: "update"})
			if tc.allowed {
				if err != nil || writes != 1 {
					t.Fatal(err, writes)
				}
			} else if err == nil || writes != 0 {
				t.Fatal("cross-issue mutation allowed", err, writes)
			}
		})
	}
}

func TestCredentialFileBoundaries(t *testing.T) {
	dir := t.TempDir()
	private := filepath.Join(dir, "token")
	if e := os.WriteFile(private, []byte("test-secret\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := New(AuthConfig{TokenFile: private}, Options{}); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		name string
		mode os.FileMode
		body string
	}{{"shared", 0644, "test-secret"}, {"group", 0640, "test-secret"}, {"whitespace", 0600, "test secret"}, {"newline", 0600, "test\nsecret"}, {"control", 0600, "test\x00secret"}, {"large", 0600, strings.Repeat("x", (64<<10)+1)}} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, tc.name)
			if e := os.WriteFile(p, []byte(tc.body), tc.mode); e != nil {
				t.Fatal(e)
			}
			if _, e := New(AuthConfig{TokenFile: p}, Options{}); e == nil {
				t.Fatal("unsafe credential accepted")
			}
		})
	}
	link := filepath.Join(dir, "symlink")
	if e := os.Symlink(private, link); e != nil {
		t.Fatal(e)
	}
	if _, e := New(AuthConfig{TokenFile: link}, Options{}); e == nil {
		t.Fatal("symlink accepted")
	}
	if _, e := readSecretFile(dir, 64<<10); e == nil {
		t.Fatal("directory accepted")
	}
	p := filepath.Join(dir, "largepem")
	os.WriteFile(p, []byte(strings.Repeat("x", (1<<20)+1)), 0600)
	if _, e := readSecretFile(p, 1<<20); e == nil {
		t.Fatal("large PEM accepted")
	}
	for _, value := range []string{"test secret", "test\nsecret", "test\tsecret", strings.Repeat("x", (64<<10)+1)} {
		t.Setenv("BOTD_INVALID_TOKEN", value)
		if _, e := New(AuthConfig{TokenEnv: "BOTD_INVALID_TOKEN"}, Options{}); e == nil {
			t.Fatal("invalid env token accepted")
		}
	}
}

func TestCheckWritesRequireOwnedApp(t *testing.T) {
	for _, tc := range []struct {
		name                string
		installation, owner int64
		allowed             bool
	}{{"machine denied", 0, 42, false}, {"own app", 123, 42, true}, {"other app", 123, 43, false}, {"missing app ID", 123, 0, false}} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			c := machine(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					fmt.Fprintf(w, `{"app":{"id":%d}}`, tc.owner)
					return
				}
				writes++
				fmt.Fprint(w, `{"id":7}`)
			})
			c.apps[123] = &app{id: 42}
			c.tokens[123] = cachedToken{"install-secret", time.Now().Add(time.Hour)}
			_, err := c.Execute(context.Background(), tc.installation, v1.Action{Kind: "check.update", Repository: "o/r", CheckRunID: 7, Status: "completed", Conclusion: "success"})
			if tc.allowed {
				if err != nil || writes != 1 {
					t.Fatal(err, writes)
				}
			} else if err == nil || writes != 0 {
				t.Fatal("unauthorized check mutation", err, writes)
			}
		})
	}
	c := machine(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected machine check request") })
	if _, err := c.Execute(context.Background(), 0, v1.Action{Kind: "check.create", Repository: "o/r", Name: "bot", HeadSHA: strings.Repeat("a", 40)}); err == nil {
		t.Fatal("machine check creation accepted")
	}
}
