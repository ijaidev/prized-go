package prized

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestExecReturnsStdoutStderrExitAndDuration(t *testing.T) {
	api := newTestAPI(t)
	var mu sync.Mutex
	var command string
	var timeout float64
	var authorization string
	api.execHandler = func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Command string  `json:"command"`
			Timeout float64 `json:"timeout"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		command = body.Command
		timeout = body.Timeout
		authorization = r.Header.Get("Authorization")
		mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"stdout":     "forty-two\n",
			"stderr":     "warning\n",
			"exitCode":   7,
			"durationMs": 125,
		})
	}

	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	result, err := sandbox.Exec(context.Background(), `printf "forty-two\n"`, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "forty-two\n" || result.Stderr != "warning\n" {
		t.Fatalf("result = %#v", result)
	}
	if result.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7", result.ExitCode)
	}
	if result.Duration != 125*time.Millisecond {
		t.Fatalf("duration = %s", result.Duration)
	}
	mu.Lock()
	defer mu.Unlock()
	if command != `printf "forty-two\n"` {
		t.Fatalf("command = %q", command)
	}
	if timeout != DefaultExecTimeout.Seconds() {
		t.Fatalf("timeout = %v", timeout)
	}
	if authorization != "Bearer test-key" {
		t.Fatalf("authorization = %q", authorization)
	}
}

func TestExecRetriesRateLimit(t *testing.T) {
	api := newTestAPI(t)
	var calls int
	api.execHandler = func(w http.ResponseWriter, _ *http.Request) {
		api.mu.Lock()
		calls++
		current := calls
		api.mu.Unlock()
		if current == 1 {
			w.Header().Set("Retry-After", "0")
			writeError(w, http.StatusTooManyRequests, "rate_limited", "slow down")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"stdout": "ok", "stderr": "", "exitCode": 0, "durationMs": 1})
	}
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := sandbox.Exec(context.Background(), "true", nil); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestExecDoesNotRetryLostAnswer(t *testing.T) {
	api := newTestAPI(t)
	var calls int
	api.execHandler = func(w http.ResponseWriter, _ *http.Request) {
		api.mu.Lock()
		calls++
		api.mu.Unlock()
		writeError(w, http.StatusBadGateway, "sandbox_unreachable", "the sandbox did not answer")
	}
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = sandbox.Exec(context.Background(), "true", nil)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "sandbox_unreachable" {
		t.Fatalf("error = %v, want sandbox_unreachable", err)
	}
	if failure.Effect != EffectMaybeRan {
		t.Fatalf("effect = %s, want maybe_ran", failure.Effect)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

type cancelTransport struct {
	started chan struct{}
	done    chan struct{}
}

func (t *cancelTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	close(t.started)
	<-request.Context().Done()
	close(t.done)
	return nil, request.Context().Err()
}

func TestExecCancellationStopsRequest(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	transport := &cancelTransport{started: make(chan struct{}), done: make(chan struct{})}
	sandbox.client = api.client(WithHTTPClient(&http.Client{Transport: transport}))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-transport.started
		cancel()
	}()
	_, err = sandbox.Exec(ctx, "sleep 30", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	select {
	case <-transport.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the request context was not canceled")
	}
}

func TestExecBadResponseSaysItRan(t *testing.T) {
	api := newTestAPI(t)
	api.execHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"stdout": "no exit code"})
	}
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = sandbox.Exec(context.Background(), "true", nil)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "bad_response" {
		t.Fatalf("error = %v, want bad_response", err)
	}
	if failure.Effect != EffectRan {
		t.Fatalf("effect = %s, want ran", failure.Effect)
	}
}

func TestClosedHandleSendsNothing(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, _, execsBefore, deletesBefore := api.counts()
	sandbox.Close()
	sandbox.Close()

	_, err = sandbox.Exec(context.Background(), "true", nil)
	if !errors.Is(err, ErrHandleClosed) {
		t.Fatalf("exec error = %v, want ErrHandleClosed", err)
	}
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "handle_closed" {
		t.Fatalf("exec error = %v, want handle_closed", err)
	}
	if err := sandbox.Delete(context.Background(), nil); !errors.Is(err, ErrHandleClosed) {
		t.Fatalf("delete error = %v, want ErrHandleClosed", err)
	}
	_, _, execsAfter, deletesAfter := api.counts()
	if execsAfter != execsBefore || deletesAfter != deletesBefore {
		t.Fatalf("closed handle sent requests: exec %d->%d, delete %d->%d", execsBefore, execsAfter, deletesBefore, deletesAfter)
	}
}

func TestDeleteAcceptsAndGoneIsSuccess(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sandbox.Delete(context.Background(), nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if sandbox.State() != StateDeleting {
		t.Fatalf("state = %s, want deleting", sandbox.State())
	}
	_, _, _, deletes := api.counts()
	if deletes != 1 {
		t.Fatalf("deletes = %d, want 1", deletes)
	}

	api.mu.Lock()
	api.deleteStatus = http.StatusNotFound
	api.mu.Unlock()
	second, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	if err := second.Delete(context.Background(), nil); err != nil {
		t.Fatalf("Delete gone sandbox: %v", err)
	}
	if second.State() != StateDeleted {
		t.Fatalf("state = %s, want deleted", second.State())
	}
}

func TestDeleteWaitUntilGone(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	api.mu.Lock()
	api.state = StateDeleted
	api.mu.Unlock()

	if err := sandbox.Delete(context.Background(), &DeleteOptions{Wait: true}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if sandbox.State() != StateDeleted {
		t.Fatalf("state = %s, want deleted", sandbox.State())
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.deletes != 1 {
		t.Fatalf("deletes = %d, want 1", api.deletes)
	}
	if api.gets < 2 {
		t.Fatalf("gets = %d, want at least 2", api.gets)
	}
}

func TestCreateErrorCarriesDocument(t *testing.T) {
	api := newTestAPI(t)
	api.createHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "paid_balance_required",
			"message": "top up to run sandboxes",
			"reason":  "free_credit",
			"billing": map[string]any{"url": "https://prized.dev/dashboard/billing"},
		})
	}
	_, err := api.createSandbox(t, CreateSandboxOptions{})
	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want *Error", err)
	}
	if failure.Code != "paid_balance_required" || failure.Status != http.StatusConflict {
		t.Fatalf("code/status = %s/%d", failure.Code, failure.Status)
	}
	if failure.Message != "top up to run sandboxes" || failure.Reason != "free_credit" {
		t.Fatalf("message/reason = %q/%q", failure.Message, failure.Reason)
	}
	if failure.Body == nil || failure.Body["billing"] == nil {
		t.Fatalf("body = %#v", failure.Body)
	}
	if failure.SandboxID != "" {
		t.Fatalf("sandbox id = %q", failure.SandboxID)
	}
	if failure.Effect != EffectNotRun {
		t.Fatalf("effect = %s, want not_run", failure.Effect)
	}
}
