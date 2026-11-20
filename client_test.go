package prized

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewClientMissingKeySendsNothing(t *testing.T) {
	t.Setenv("PRIZED_API_KEY", "")
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
	}))
	defer server.Close()

	client, err := NewClient(WithBaseURL(server.URL))
	if client != nil {
		t.Fatalf("client = %v, want nil", client)
	}
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "missing_api_key" {
		t.Fatalf("error = %v, want missing_api_key", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func TestNewClientReadsEnvironmentKey(t *testing.T) {
	t.Setenv("PRIZED_API_KEY", "env-key")
	client, err := NewClient(WithBaseURL("http://127.0.0.1:1"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if client.apiKey != "env-key" {
		t.Fatalf("apiKey = %q", client.apiKey)
	}
}

func TestNewClientRejectsInvalidConfiguration(t *testing.T) {
	t.Setenv("PRIZED_API_KEY", "key")
	tests := []struct {
		name   string
		option Option
		code   string
	}{
		{name: "insecure base URL", option: WithBaseURL("http://example.com"), code: "invalid_argument"},
		{name: "bad base URL", option: WithBaseURL("://nope"), code: "invalid_argument"},
		{name: "negative retries", option: WithMaxRetries(-1), code: "invalid_argument"},
		{name: "zero request timeout", option: WithRequestTimeout(0), code: "invalid_argument"},
		{name: "zero poll interval", option: WithPollInterval(0), code: "invalid_argument"},
		{name: "nil HTTP client", option: WithHTTPClient(nil), code: "invalid_argument"},
		{name: "space in key", option: WithAPIKey("two words"), code: "invalid_api_key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewClient(test.option)
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != test.code {
				t.Fatalf("error = %v, want %s", err, test.code)
			}
		})
	}
}

func TestCreateParsesDefaultsAndWaits(t *testing.T) {
	api := newTestAPI(t)

	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sandbox.ID() != "sbx_1" || sandbox.State() != StateRunning {
		t.Fatalf("sandbox = %s/%s", sandbox.ID(), sandbox.State())
	}
	api.mu.Lock()
	body := api.createBody
	key := api.idempotencyKey[0]
	creates, gets, _, _ := api.creates, api.gets, api.execs, api.deletes
	api.mu.Unlock()
	if len(body) != 1 || body["template"] != DefaultTemplate {
		t.Fatalf("create body = %#v", body)
	}
	if !strings.HasPrefix(key, "prized-go-") {
		t.Fatalf("idempotency key = %q", key)
	}
	if creates != 1 || gets < 1 {
		t.Fatalf("creates/gets = %d/%d", creates, gets)
	}
}

func TestCreateRetriesRateLimitAndServerError(t *testing.T) {
	api := newTestAPI(t)
	var attempts int
	api.createHandler = func(w http.ResponseWriter, _ *http.Request) {
		api.mu.Lock()
		attempts++
		current := attempts
		api.mu.Unlock()
		if current == 1 {
			w.Header().Set("Retry-After", "0")
			writeError(w, http.StatusTooManyRequests, "rate_limited", "slow down")
			return
		}
		if current == 2 {
			writeError(w, http.StatusServiceUnavailable, "sandbox_capacity", "no capacity")
			return
		}
		writeSandbox(w, http.StatusCreated, StateStarting, api.server.URL)
	}

	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sandbox.State() != StateRunning {
		t.Fatalf("state = %s", sandbox.State())
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if len(api.idempotencyKey) != 3 {
		t.Fatalf("keys = %#v", api.idempotencyKey)
	}
	for _, key := range api.idempotencyKey {
		if key != api.idempotencyKey[0] {
			t.Fatalf("idempotency keys differ: %#v", api.idempotencyKey)
		}
	}
}

func TestCreateNeverRetriesQuotaOrBalance(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
		reason string
	}{
		{name: "quota", status: http.StatusTooManyRequests, code: "sandbox_quota_exceeded", reason: "sandbox_limit"},
		{name: "balance", status: http.StatusConflict, code: "paid_balance_required", reason: "free_credit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newTestAPI(t)
			api.createHandler = func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, test.status, map[string]any{
					"error":   test.code,
					"message": "refused",
					"reason":  test.reason,
				})
			}
			_, err := api.createSandbox(t, CreateSandboxOptions{})
			var failure *Error
			if !errors.As(err, &failure) {
				t.Fatalf("error = %v, want *Error", err)
			}
			if failure.Code != test.code || failure.Status != test.status || failure.Reason != test.reason {
				t.Fatalf("error = %s/%d/%s", failure.Code, failure.Status, failure.Reason)
			}
			api.mu.Lock()
			creates := api.creates
			api.mu.Unlock()
			if creates != 1 {
				t.Fatalf("creates = %d, want 1", creates)
			}
		})
	}
}

func TestCreateRetriesLostConnection(t *testing.T) {
	api := newTestAPI(t)
	var attempts int
	api.createHandler = func(w http.ResponseWriter, _ *http.Request) {
		api.mu.Lock()
		attempts++
		current := attempts
		api.mu.Unlock()
		if current == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("hijacking is not supported")
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("Hijack: %v", err)
				return
			}
			_ = connection.Close()
			return
		}
		writeSandbox(w, http.StatusCreated, StateStarting, api.server.URL)
	}

	client := api.client()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sandbox, err := client.Sandboxes().Create(ctx, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sandbox.State() != StateRunning {
		t.Fatalf("state = %s", sandbox.State())
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestCreateReplaysKeyAndDeletesWhenAnswerIsLost(t *testing.T) {
	api := newTestAPI(t)
	var attempts int
	api.createHandler = func(w http.ResponseWriter, _ *http.Request) {
		api.mu.Lock()
		attempts++
		current := attempts
		api.mu.Unlock()
		if current <= 2 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("hijacking is not supported")
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("Hijack: %v", err)
				return
			}
			_ = connection.Close()
			return
		}
		writeSandbox(w, http.StatusOK, StateRunning, api.server.URL)
	}

	client := api.client(WithMaxRetries(1))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.Sandboxes().Create(ctx, CreateSandboxOptions{})
	if err == nil {
		t.Fatal("Create succeeded, want a lost-answer error")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if attempts != 3 {
		t.Fatalf("create attempts = %d, want 3 (two sends and one replay)", attempts)
	}
	if api.deletes != 1 {
		t.Fatalf("deletes = %d, want 1", api.deletes)
	}
}

func TestCreateFailedWaitDeletesSandbox(t *testing.T) {
	api := newTestAPI(t)
	api.state = StateError

	_, err := api.createSandbox(t, CreateSandboxOptions{})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "sandbox_error" {
		t.Fatalf("error = %v, want sandbox_error", err)
	}
	if failure.SandboxID != "sbx_1" {
		t.Fatalf("sandbox id = %q", failure.SandboxID)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.deletes != 1 {
		t.Fatalf("deletes = %d, want 1", api.deletes)
	}
}

func TestCreateTimeoutDeletesSandbox(t *testing.T) {
	api := newTestAPI(t)
	api.state = StateStarting

	_, err := api.createSandbox(t, CreateSandboxOptions{Timeout: 30 * time.Millisecond})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "timeout" {
		t.Fatalf("error = %v, want timeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error does not wrap context.DeadlineExceeded: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.deletes != 1 {
		t.Fatalf("deletes = %d, want 1", api.deletes)
	}
}

func TestCreateCancellationDeletesSandbox(t *testing.T) {
	api := newTestAPI(t)
	api.state = StateStarting
	polled := make(chan struct{})
	var once bool
	api.createHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeSandbox(w, http.StatusCreated, StateStarting, api.server.URL)
	}
	api.execHandler = nil
	api.mu.Lock()
	api.getFailures = 0
	api.mu.Unlock()

	client := api.client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			api.mu.Lock()
			gets := api.gets
			api.mu.Unlock()
			if gets > 0 {
				if !once {
					once = true
					close(polled)
				}
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	go func() {
		<-polled
		cancel()
	}()

	_, err := client.Sandboxes().Create(ctx, CreateSandboxOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.deletes != 1 {
		t.Fatalf("deletes = %d, want 1", api.deletes)
	}
}

func TestCreateCancellationBeforeSendSendsNothing(t *testing.T) {
	api := newTestAPI(t)
	client := api.client()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.Sandboxes().Create(ctx, CreateSandboxOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	creates, _, _, deletes := api.counts()
	if creates != 0 || deletes != 0 {
		t.Fatalf("creates/deletes = %d/%d, want 0/0", creates, deletes)
	}
}

func TestCreateWithCallerKeyLeavesSandbox(t *testing.T) {
	api := newTestAPI(t)
	api.state = StateStarting

	_, err := api.createSandbox(t, CreateSandboxOptions{
		IdempotencyKey: "caller-key",
		Timeout:        30 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("Create succeeded, want timeout")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.deletes != 0 {
		t.Fatalf("deletes = %d, want 0", api.deletes)
	}
	if api.idempotencyKey[0] != "caller-key" {
		t.Fatalf("idempotency key = %q", api.idempotencyKey[0])
	}
}

func TestCreateReadRetriesOtherServerErrors(t *testing.T) {
	api := newTestAPI(t)
	api.state = StateRunning
	api.getFailures = 1

	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sandbox.State() != StateRunning {
		t.Fatalf("state = %s", sandbox.State())
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.gets != 2 {
		t.Fatalf("gets = %d, want 2", api.gets)
	}
}

func TestCreateDoesNotFollowRedirectWithKey(t *testing.T) {
	api := newTestAPI(t)
	var redirected int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected++
	}))
	defer target.Close()
	api.createHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}
	_, err := api.createSandbox(t, CreateSandboxOptions{})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "unexpected_redirect" {
		t.Fatalf("error = %v, want unexpected_redirect", err)
	}
	if redirected != 0 {
		t.Fatalf("redirect target got %d requests", redirected)
	}
}

func TestParsedRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if got := parseRetryAfter("2", now); got != 2*time.Second {
		t.Fatalf("seconds = %s", got)
	}
	if got := parseRetryAfter(now.Add(3*time.Second).Format(http.TimeFormat), now); got < 2*time.Second || got > 3*time.Second {
		t.Fatalf("date = %s", got)
	}
	if got := parseRetryAfter("nonsense", now); got != 0 {
		t.Fatalf("nonsense = %s", got)
	}
}
