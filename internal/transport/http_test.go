package transport

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/wenext-limited/github-botd/api/v1"
	"github.com/wenext-limited/github-botd/internal/config"
	"github.com/wenext-limited/github-botd/internal/gateway"
	"github.com/wenext-limited/github-botd/internal/store"
)

func setup(t *testing.T) (*gateway.Gateway, http.Handler, http.Handler) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	g := gateway.New(&config.Config{DryRun: true, Limits: config.Limits{LeaseSeconds: 30, MaxBodyBytes: 4096}, Repositories: map[string]config.Repository{"r": {FullName: "acme/repo"}}, Clients: map[string]config.Client{"a": {Token: strings.Repeat("a", 32), Repositories: []string{"r"}, Capabilities: []string{"events.read", "events.replay", "issue.comment.create"}}, "b": {Token: strings.Repeat("b", 32), Repositories: []string{"r"}, Capabilities: []string{"events.read"}}}, Webhook: config.Webhook{Sources: map[string]config.Source{"test": {Secret: strings.Repeat("w", 32)}}}, Routes: []config.Route{{ID: "all", Repositories: []string{"r"}, Events: []string{"issue_comment"}, Trigger: "any", Actors: []string{"alice"}, Clients: []string{"a", "b"}}}}, s, nil)
	return g, IPC(g), Webhook(g)
}
func request(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+strings.Repeat(token, 32))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func signed(h http.Handler, body, delivery string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/hooks/test", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-GitHub-Event", "issue_comment")
	r.Header.Set("X-GitHub-Delivery", delivery)
	mac := hmac.New(sha256.New, []byte(strings.Repeat("w", 32)))
	mac.Write([]byte(body))
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const payload = `{"action":"created","repository":{"full_name":"acme/repo"},"sender":{"login":"alice","type":"User"},"issue":{"number":7},"comment":{"id":88,"body":"hello"}}`

func TestEndToEndWebhookFanoutAckActions(t *testing.T) {
	_, ipc, hook := setup(t)
	if w := signed(hook, payload, "d"); w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	if w := signed(hook, payload, "d"); !strings.Contains(w.Body.String(), `"new":false`) {
		t.Fatal("dedupe lost")
	}
	var da, db v1.Delivery
	for _, client := range []string{"a", "b"} {
		w := request(ipc, "GET", "/v1/events?wait=0", "", client)
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		var reply struct {
			Deliveries []v1.Delivery `json:"deliveries"`
		}
		json.Unmarshal(w.Body.Bytes(), &reply)
		if len(reply.Deliveries) != 1 {
			t.Fatal(w.Body)
		}
		if client == "a" {
			da = reply.Deliveries[0]
		} else {
			db = reply.Deliveries[0]
		}
	}
	if da.ID == db.ID || da.Event.ID != db.Event.ID {
		t.Fatal("fanout isn't independent")
	}
	ack, _ := json.Marshal(v1.AckRequest{LeaseToken: da.LeaseToken})
	if w := request(ipc, "POST", "/v1/events/"+da.ID+"/ack", string(ack), "b"); w.Code != 409 {
		t.Fatal("foreign ack allowed", w.Code)
	}
	if w := request(ipc, "POST", "/v1/events/"+da.ID+"/ack", string(ack), "a"); w.Code != 200 {
		t.Fatal(w.Body)
	}
	action := v1.Action{Version: v1.Version, IdempotencyKey: "respond", Repository: "r", Kind: "issue.comment.create", EventID: da.Event.ID, Number: 7, Body: "done"}
	raw, _ := json.Marshal(action)
	w := request(ipc, "POST", "/v1/actions", string(raw), "a")
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	var receipt v1.Receipt
	json.Unmarshal(w.Body.Bytes(), &receipt)
	if w = request(ipc, "GET", "/v1/actions/"+receipt.ID, "", "b"); w.Code != 404 {
		t.Fatal("receipt disclosure", w.Code)
	}
	if w = request(ipc, "POST", "/v1/actions", string(raw), "b"); w.Code != 403 {
		t.Fatal("capability bypass", w.Code)
	}
}
func TestAuthenticationSignatureAndBounds(t *testing.T) {
	_, ipc, hook := setup(t)
	if w := request(ipc, "GET", "/v1/info", "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(ipc, "GET", "/v1/events?limit=999", "", "a"); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := request(ipc, "POST", "/v1/actions", `{"unknown":1}`, "a"); w.Code != 400 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("GET", "/v1/info", nil)
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	r.Header.Set("Origin", "https://evil.test")
	w := httptest.NewRecorder()
	ipc.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("browser origin allowed")
	}
	r = httptest.NewRequest("POST", "/hooks/test", strings.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	hook.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("unsigned accepted")
	}
	if signed(hook, strings.Repeat("x", 4097), "d").Code != 413 {
		t.Fatal("size cap ignored")
	}
	if VerifySignature("key", "sha256="+strings.Repeat("0", 64), []byte("body")) {
		t.Fatal("bad HMAC accepted")
	}
}
func TestRevokedRepositoryCannotReadQueuedEvents(t *testing.T) {
	g, ipc, hook := setup(t)
	signed(hook, payload, "d")
	p := g.Config.Clients["a"]
	p.Repositories = nil
	g.Config.Clients["a"] = p
	w := request(ipc, "GET", "/v1/events?wait=0", "", "a")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"deliveries":[]`) {
		t.Fatal("revoked repository leaked", w.Body)
	}
}
func TestSecureListenerAndActualUnixHTTP(t *testing.T) {
	g, ipc, _ := setup(t)
	_ = g
	root, err := os.MkdirTemp("", "gbd-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	os.Chmod(root, 0700)
	socket := filepath.Join(root, "s")
	ln, err := ListenUnix(socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: ipc}
	defer server.Close()
	go server.Serve(ln)
	info, _ := os.Stat(socket)
	if info.Mode().Perm() != 0600 {
		t.Fatal("socket isn't private")
	}
	if other, err := ListenUnix(socket); err == nil {
		other.Close()
		t.Fatal("active socket clobbered")
	}
	// Exercise request bytes over the actual Unix transport without TCP exposure.
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
	b, _ := io.ReadAll(conn)
	if !bytes.Contains(b, []byte(`"status":"ok"`)) {
		t.Fatal(string(b))
	}
	file := filepath.Join(root, "regular")
	os.WriteFile(file, []byte("keep"), 0600)
	if l, err := ListenUnix(file); err == nil {
		l.Close()
		t.Fatal("regular file clobbered")
	}
	data, _ := os.ReadFile(file)
	if string(data) != "keep" {
		t.Fatal("file changed")
	}
	if l, err := ListenLoopback("0.0.0.0:0"); err == nil {
		l.Close()
		t.Fatal("nonlocal accepted")
	}
}
