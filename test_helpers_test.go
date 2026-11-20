package prized

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type testAPI struct {
	t      *testing.T
	server *httptest.Server

	mu             sync.Mutex
	creates        int
	gets           int
	execs          int
	deletes        int
	state          string
	deleteStatus   int
	getFailures    int
	createHandler  http.HandlerFunc
	execHandler    http.HandlerFunc
	getHandler     http.HandlerFunc
	createBody     map[string]any
	idempotencyKey []string
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	api := &testAPI{t: t, state: StateRunning}
	api.server = httptest.NewServer(http.HandlerFunc(api.handle))
	t.Cleanup(api.server.Close)
	return api
}

func (a *testAPI) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sandboxes":
		a.mu.Lock()
		a.creates++
		a.idempotencyKey = append(a.idempotencyKey, r.Header.Get("Idempotency-Key"))
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			a.createBody = body
		}
		handler := a.createHandler
		a.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		writeSandbox(w, http.StatusCreated, StateStarting, a.server.URL)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sandboxes/sbx_1":
		a.mu.Lock()
		a.gets++
		state := a.state
		handler := a.getHandler
		if a.getFailures > 0 {
			a.getFailures--
			a.mu.Unlock()
			writeError(w, http.StatusInternalServerError, "http_error", "temporary")
			return
		}
		a.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		writeSandbox(w, http.StatusOK, state, a.server.URL)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sandbox/sbx_1/process/execute":
		a.mu.Lock()
		a.execs++
		handler := a.execHandler
		a.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		http.Error(w, "no exec handler", http.StatusInternalServerError)
	case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/sandboxes/sbx_1":
		a.mu.Lock()
		a.deletes++
		status := a.deleteStatus
		a.mu.Unlock()
		if status == 0 {
			status = http.StatusNoContent
		}
		w.WriteHeader(status)
	default:
		http.NotFound(w, r)
	}
}

func (a *testAPI) client(options ...Option) *Client {
	a.t.Helper()
	all := []Option{
		WithAPIKey("test-key"),
		WithBaseURL(a.server.URL),
		WithRequestTimeout(2 * time.Second),
		WithPollInterval(2 * time.Millisecond),
	}
	all = append(all, options...)
	client, err := NewClient(all...)
	if err != nil {
		a.t.Fatalf("NewClient: %v", err)
	}
	client.retryBase = 0
	return client
}

func (a *testAPI) createSandbox(t *testing.T, options CreateSandboxOptions) (*Sandbox, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return a.client().Sandboxes().Create(ctx, options)
}

func (a *testAPI) counts() (creates, gets, execs, deletes int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.creates, a.gets, a.execs, a.deletes
}

func writeSandbox(w http.ResponseWriter, status int, state, base string) {
	writeJSON(w, status, map[string]any{
		"sandbox": map[string]any{
			"id":          "sbx_1",
			"state":       state,
			"template":    DefaultTemplate,
			"toolboxBase": base + "/v1/sandbox/sbx_1",
		},
	})
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": code, "message": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
