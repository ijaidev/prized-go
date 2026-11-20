package prized

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type testAPI struct {
	t      *testing.T
	server *httptest.Server

	mu                  sync.Mutex
	creates             int
	gets                int
	execs               int
	deletes             int
	pauses              int
	resumes             int
	refreshes           int
	forks               int
	extends             int
	timeouts            int
	childDeletes        int
	uploads             int
	downloads           int
	lists               int
	templateGets        int
	bulkDeletes         int
	connects            int
	tokenCalls          int
	publicCalls         int
	state               string
	deleteStatus        int
	getFailures         int
	createHandler       http.HandlerFunc
	execHandler         http.HandlerFunc
	getHandler          http.HandlerFunc
	pauseHandler        http.HandlerFunc
	resumeHandler       http.HandlerFunc
	refreshHandler      http.HandlerFunc
	forkHandler         http.HandlerFunc
	extendHandler       http.HandlerFunc
	timeoutHandler      http.HandlerFunc
	uploadHandler       http.HandlerFunc
	downloadHandler     http.HandlerFunc
	listHandler         http.HandlerFunc
	templatesHandler    http.HandlerFunc
	deleteLabelsHandler http.HandlerFunc
	connectHandler      http.HandlerFunc
	tokenHandler        http.HandlerFunc
	publicHandler       http.HandlerFunc
	createBody          map[string]any
	forkBody            map[string]any
	extendBody          map[string]any
	timeoutBody         map[string]any
	tokenBody           map[string]any
	publicBody          map[string]any
	idempotencyKey      []string
	forkKey             string
	extendKey           string
	timeoutKey          string
	childStates         map[string]string
	childReasons        map[string]string
	execCommands        []string
	execCwds            []string
	uploadPaths         []string
	uploadBodies        [][]byte
	uploadTypes         []string
	downloadPaths       []string
}

const testPreviewURLTemplate = "https://{port}-sbx.preview.prized.test/"

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	api := &testAPI{
		t:            t,
		state:        StateRunning,
		childStates:  map[string]string{},
		childReasons: map[string]string{},
	}
	api.server = httptest.NewServer(http.HandlerFunc(api.handle))
	t.Cleanup(api.server.Close)
	return api
}

func (a *testAPI) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/api/v1/sandboxes":
		a.handleCreate(w, r)
	case r.Method == http.MethodGet && path == "/api/v1/sandboxes":
		a.handleList(w, r)
	case r.Method == http.MethodGet && path == "/api/v1/sandbox-templates":
		a.handleTemplates(w, r)
	case r.Method == http.MethodGet && path == "/api/v1/sandboxes/sbx_1":
		a.handleGet(w, r, "sbx_1")
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/sandboxes/"):
		a.handleConnect(w, r, strings.TrimPrefix(path, "/api/v1/sandboxes/"))
	case r.Method == http.MethodDelete && path == "/api/v1/sandboxes":
		a.handleBulkDelete(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/api/v1/sandboxes/"):
		a.handleDelete(w, strings.TrimPrefix(path, "/api/v1/sandboxes/"))
	case r.Method == http.MethodPost && path == "/api/v1/sandboxes/sbx_1/ports/3000/token":
		a.handlePortToken(w, r)
	case r.Method == http.MethodPut && path == "/api/v1/sandboxes/sbx_1/ports/3000":
		a.handlePortPublic(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/v1/sandboxes/"):
		id, verb, found := strings.Cut(strings.TrimPrefix(path, "/api/v1/sandboxes/"), "/")
		if !found {
			http.NotFound(w, r)
			return
		}
		a.handleVerb(w, r, id, verb)
	case r.Method == http.MethodPost && path == "/v1/sandbox/sbx_1/process/execute":
		body, readErr := io.ReadAll(r.Body)
		var request struct {
			Command string `json:"command"`
			Cwd     string `json:"cwd"`
		}
		_ = json.Unmarshal(body, &request)
		a.mu.Lock()
		a.execs++
		a.execCommands = append(a.execCommands, request.Command)
		a.execCwds = append(a.execCwds, request.Cwd)
		handler := a.execHandler
		a.mu.Unlock()
		if readErr != nil {
			http.Error(w, "could not read the command", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if handler != nil {
			handler(w, r)
			return
		}
		http.Error(w, "no exec handler", http.StatusInternalServerError)
	case r.Method == http.MethodPost && path == "/v1/sandbox/sbx_1/files/upload":
		body, readErr := io.ReadAll(r.Body)
		path := r.URL.Query().Get("path")
		a.mu.Lock()
		a.uploads++
		a.uploadPaths = append(a.uploadPaths, path)
		a.uploadBodies = append(a.uploadBodies, body)
		a.uploadTypes = append(a.uploadTypes, r.Header.Get("Content-Type"))
		handler := a.uploadHandler
		a.mu.Unlock()
		if readErr != nil {
			http.Error(w, "could not read the upload", http.StatusBadRequest)
			return
		}
		if handler != nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			handler(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": path, "size": len(body)})
	case r.Method == http.MethodGet && path == "/v1/sandbox/sbx_1/files/download":
		a.mu.Lock()
		a.downloads++
		a.downloadPaths = append(a.downloadPaths, r.URL.Query().Get("path"))
		handler := a.downloadHandler
		a.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		http.Error(w, "no download handler", http.StatusNotFound)
	default:
		http.NotFound(w, r)
	}
}

func (a *testAPI) handleCreate(w http.ResponseWriter, r *http.Request) {
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
}

func (a *testAPI) handleList(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.lists++
	handler := a.listHandler
	a.mu.Unlock()
	if handler != nil {
		handler(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": []any{}})
}

func (a *testAPI) handleTemplates(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.templateGets++
	handler := a.templatesHandler
	a.mu.Unlock()
	if handler != nil {
		handler(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": []any{}})
}

func (a *testAPI) handleBulkDelete(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.bulkDeletes++
	handler := a.deleteLabelsHandler
	a.mu.Unlock()
	if handler != nil {
		handler(w, r)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"count": 0, "ids": []string{}, "more": false})
}

func (a *testAPI) handleGet(w http.ResponseWriter, r *http.Request, id string) {
	a.mu.Lock()
	a.gets++
	handler := a.getHandler
	if a.getFailures > 0 {
		a.getFailures--
		a.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "http_error", "temporary")
		return
	}
	if handler != nil {
		a.mu.Unlock()
		handler(w, r)
		return
	}
	state := a.state
	a.mu.Unlock()
	if state == "" {
		writeError(w, http.StatusNotFound, "not_found", "no such sandbox")
		return
	}
	writeSandboxAt(w, http.StatusOK, id, state, "", a.server.URL)
}

func (a *testAPI) handleConnect(w http.ResponseWriter, r *http.Request, id string) {
	a.mu.Lock()
	a.connects++
	handler := a.connectHandler
	a.mu.Unlock()
	if handler != nil {
		handler(w, r)
		return
	}
	a.mu.Lock()
	state := a.childStates[id]
	reason := a.childReasons[id]
	if state == StateStarting {
		// A fork copy boots between the fork answer and the first read.
		state = StateRunning
		a.childStates[id] = StateRunning
	}
	a.mu.Unlock()
	if state == "" {
		writeError(w, http.StatusNotFound, "not_found", "no such sandbox")
		return
	}
	writeSandboxAt(w, http.StatusOK, id, state, reason, a.server.URL)
}

func (a *testAPI) handleDelete(w http.ResponseWriter, id string) {
	a.mu.Lock()
	a.deletes++
	status := a.deleteStatus
	if id != "sbx_1" {
		a.childDeletes++
		a.childStates[id] = StateDeleted
	}
	a.mu.Unlock()
	if status == 0 {
		status = http.StatusNoContent
	}
	w.WriteHeader(status)
}

func (a *testAPI) handleVerb(w http.ResponseWriter, r *http.Request, id, verb string) {
	switch verb {
	case "pause":
		a.handlePause(w, r, id)
	case "resume":
		a.handleResume(w, r, id)
	case "refresh":
		a.handleRefresh(w, r)
	case "fork":
		a.handleFork(w, r, id)
	case "extend":
		a.handleExtend(w, r, id)
	case "timeout":
		a.handleTimeout(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (a *testAPI) handlePause(w http.ResponseWriter, r *http.Request, id string) {
	a.mu.Lock()
	a.pauses++
	handler := a.pauseHandler
	if handler == nil {
		a.state = StatePaused
	}
	a.mu.Unlock()
	if handler != nil {
		handler(w, r)
		return
	}
	writeSandbox(w, http.StatusAccepted, StatePausing, a.server.URL)
}

func (a *testAPI) handleResume(w http.ResponseWriter, r *http.Request, id string) {
	a.mu.Lock()
	a.resumes++
	handler := a.resumeHandler
	if handler == nil {
		a.state = StateRunning
	}
	a.mu.Unlock()
	if handler != nil {
		handler(w, r)
		return
	}
	writeSandbox(w, http.StatusAccepted, StateStarting, a.server.URL)
}

func (a *testAPI) handleRefresh(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.refreshes++
	handler := a.refreshHandler
	a.mu.Unlock()
	if handler != nil {
		handler(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *testAPI) handleFork(w http.ResponseWriter, r *http.Request, parentID string) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	a.mu.Lock()
	a.forks++
	a.forkBody = body
	a.forkKey = r.Header.Get("Idempotency-Key")
	handler := a.forkHandler
	if len(a.childStates) == 0 {
		a.childStates["sbx_f1"] = StateStarting
		a.childStates["sbx_f2"] = StateStarting
	}
	ids := make([]string, 0, 2)
	documents := make([]map[string]any, 0, 2)
	for _, id := range []string{"sbx_f1", "sbx_f2"} {
		if _, ok := a.childStates[id]; !ok {
			continue
		}
		ids = append(ids, id)
		documents = append(documents, sandboxFields(id, a.childStates[id], a.childReasons[id], a.server.URL))
	}
	a.mu.Unlock()
	if handler != nil {
		handler(w, r)
		return
	}
	count := 2
	if value, ok := body["count"].(float64); ok {
		count = int(value)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"fork": map[string]any{
			"id":         "frk_1",
			"parentId":   parentID,
			"count":      count,
			"status":     "active",
			"sandboxIds": ids,
		},
		"sandboxes": documents,
	})
}

func (a *testAPI) handleExtend(w http.ResponseWriter, r *http.Request, id string) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	a.mu.Lock()
	a.extends++
	a.extendBody = body
	a.extendKey = r.Header.Get("Idempotency-Key")
	handler := a.extendHandler
	state := a.state
	a.mu.Unlock()
	if handler != nil {
		handler(w, r)
		return
	}
	document := sandboxFields(id, state, "", a.server.URL)
	document["expiresAt"] = "2026-11-21T12:00:00Z"
	writeJSON(w, http.StatusOK, map[string]any{"sandbox": document, "capped": true})
}

func (a *testAPI) handleTimeout(w http.ResponseWriter, r *http.Request, id string) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	a.mu.Lock()
	a.timeouts++
	a.timeoutBody = body
	a.timeoutKey = r.Header.Get("Idempotency-Key")
	handler := a.timeoutHandler
	state := a.state
	a.mu.Unlock()
	if handler != nil {
		handler(w, r)
		return
	}
	document := sandboxFields(id, state, "", a.server.URL)
	document["expiresAt"] = "2026-11-21T12:00:00Z"
	if value, ok := body["expiresAt"].(string); ok {
		document["expiresAt"] = value
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandbox": document, "capped": false})
}

func (a *testAPI) handlePortToken(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.tokenCalls++
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
		a.tokenBody = body
	}
	handler := a.tokenHandler
	a.mu.Unlock()
	if handler != nil {
		handler(w, r)
		return
	}
	http.Error(w, "no token handler", http.StatusInternalServerError)
}

func (a *testAPI) handlePortPublic(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.publicCalls++
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
		a.publicBody = body
	}
	handler := a.publicHandler
	a.mu.Unlock()
	if handler != nil {
		handler(w, r)
		return
	}
	http.Error(w, "no public handler", http.StatusInternalServerError)
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

func (a *testAPI) fileCounts() (uploads, downloads int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.uploads, a.downloads
}

func (a *testAPI) execLog() (commands, cwds []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.execCommands...), append([]string(nil), a.execCwds...)
}

func (a *testAPI) uploadLog() (paths []string, bodies [][]byte, types []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, body := range a.uploadBodies {
		bodies = append(bodies, append([]byte(nil), body...))
	}
	return append([]string(nil), a.uploadPaths...), bodies, append([]string(nil), a.uploadTypes...)
}

func (a *testAPI) downloadLog() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.downloadPaths...)
}

func (a *testAPI) discoveryCounts() (lists, templates, bulkDeletes, connects int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lists, a.templateGets, a.bulkDeletes, a.connects
}

func writeSandbox(w http.ResponseWriter, status int, state, base string) {
	writeSandboxAt(w, status, "sbx_1", state, "", base)
}

func writeSandboxAt(w http.ResponseWriter, status int, id, state, reason, base string) {
	writeJSON(w, status, map[string]any{"sandbox": sandboxFields(id, state, reason, base)})
}

func sandboxFields(id, state, reason, base string) map[string]any {
	document := map[string]any{
		"id":                 id,
		"state":              state,
		"template":           DefaultTemplate,
		"toolboxBase":        base + "/v1/sandbox/" + id,
		"previewUrlTemplate": testPreviewURLTemplate,
	}
	if reason != "" {
		document["errorReason"] = reason
	}
	return document
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": code, "message": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
