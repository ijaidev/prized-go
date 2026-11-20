package prized

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPreviewURLAndHeaders(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	url, err := sandbox.PreviewURL(3000)
	if err != nil {
		t.Fatalf("PreviewURL: %v", err)
	}
	if want := "https://3000-sbx.preview.prized.test/"; url != want {
		t.Fatalf("url = %q, want %q", url, want)
	}

	headers := sandbox.PreviewHeaders()
	if len(headers) != 1 || headers[PreviewKeyHeader] != "test-key" {
		t.Fatalf("headers = %#v", headers)
	}
	headers[PreviewKeyHeader] = "changed"
	if again := sandbox.PreviewHeaders(); again[PreviewKeyHeader] != "test-key" {
		t.Fatalf("PreviewHeaders returned shared state: %#v", again)
	}

	for _, port := range []int{-1, 0, 22, 81, 65536} {
		_, err := sandbox.PreviewURL(port)
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "bad_request" || failure.SandboxID != "sbx_1" {
			t.Fatalf("port %d error = %v, want bad_request for sbx_1", port, err)
		}
	}
}

func TestPreviewURLUnavailable(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sandbox.mu.Lock()
	sandbox.previewURLTemplate = ""
	sandbox.mu.Unlock()

	_, err = sandbox.PreviewURL(3000)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "preview_unavailable" || failure.SandboxID != "sbx_1" {
		t.Fatalf("error = %v, want preview_unavailable", err)
	}
}

func TestPreviewTokenSendsExpiryAndParses(t *testing.T) {
	api := newTestAPI(t)
	var authorization string
	api.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		writeJSON(w, http.StatusCreated, map[string]any{
			"sandboxId":  "sbx_1",
			"port":       3000,
			"url":        "https://3000-sbx.preview.prized.test/?prized_token=tok",
			"browserUrl": "https://3000-sbx.prized.link/?prized_token=tok",
			"token":      "tok",
			"expiresAt":  "2026-11-20T13:00:00.000Z",
		})
	}
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	token, err := sandbox.PreviewToken(context.Background(), 3000, &PreviewTokenOptions{ExpiresIn: 600})
	if err != nil {
		t.Fatalf("PreviewToken: %v", err)
	}
	if token.URL != "https://3000-sbx.preview.prized.test/?prized_token=tok" {
		t.Fatalf("url = %q", token.URL)
	}
	if token.BrowserURL != "https://3000-sbx.prized.link/?prized_token=tok" {
		t.Fatalf("browser url = %q", token.BrowserURL)
	}
	if token.Token != "tok" {
		t.Fatalf("token = %q", token.Token)
	}
	wantExpiry := time.Date(2026, 11, 20, 13, 0, 0, 0, time.UTC)
	if !token.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("expires at = %s", token.ExpiresAt)
	}
	if authorization != "Bearer test-key" {
		t.Fatalf("authorization = %q", authorization)
	}
	api.mu.Lock()
	if api.tokenCalls != 1 || api.tokenBody["expiresIn"] != float64(600) {
		t.Fatalf("calls/body = %d/%#v", api.tokenCalls, api.tokenBody)
	}
	api.mu.Unlock()

	if _, err := sandbox.PreviewToken(context.Background(), 3000, nil); err != nil {
		t.Fatalf("PreviewToken default: %v", err)
	}
	api.mu.Lock()
	if len(api.tokenBody) != 0 {
		t.Fatalf("default body = %#v, want {}", api.tokenBody)
	}
	api.mu.Unlock()

	if _, err := sandbox.PreviewToken(context.Background(), 3000, &PreviewTokenOptions{ExpiresIn: -1}); err == nil {
		t.Fatal("negative expiresIn succeeded")
	} else {
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "invalid_argument" || failure.SandboxID != "sbx_1" {
			t.Fatalf("error = %v, want invalid_argument", err)
		}
	}
}

func TestPreviewTokenBadResponse(t *testing.T) {
	tests := []struct {
		name string
		body any
	}{
		{name: "missing url", body: map[string]any{"token": "tok", "expiresAt": "2026-11-20T13:00:00Z"}},
		{name: "missing token", body: map[string]any{"url": "https://example.test/", "expiresAt": "2026-11-20T13:00:00Z"}},
		{name: "missing expiry", body: map[string]any{"url": "https://example.test/", "token": "tok"}},
		{name: "bad expiry", body: map[string]any{"url": "https://example.test/", "token": "tok", "expiresAt": "soon"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newTestAPI(t)
			api.tokenHandler = func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusCreated, test.body)
			}
			sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			_, err = sandbox.PreviewToken(context.Background(), 3000, nil)
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != "bad_response" {
				t.Fatalf("error = %v, want bad_response", err)
			}
			if failure.Effect != EffectRan || failure.SandboxID != "sbx_1" || failure.Status != http.StatusCreated {
				t.Fatalf("failure = %#v", failure)
			}
		})
	}
}

func TestPreviewTokenRetryRules(t *testing.T) {
	t.Run("retries 502", func(t *testing.T) {
		api := newTestAPI(t)
		var calls int
		api.tokenHandler = func(w http.ResponseWriter, _ *http.Request) {
			api.mu.Lock()
			calls++
			current := calls
			api.mu.Unlock()
			if current == 1 {
				writeError(w, http.StatusBadGateway, "sandbox_unreachable", "try again")
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{
				"url": "https://example.test/?prized_token=tok", "token": "tok", "expiresAt": "2026-11-20T13:00:00Z",
			})
		}
		sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := sandbox.PreviewToken(context.Background(), 3000, nil); err != nil {
			t.Fatalf("PreviewToken: %v", err)
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		if api.tokenCalls != 2 {
			t.Fatalf("calls = %d, want 2", api.tokenCalls)
		}
	})

	refusals := []struct {
		name   string
		status int
		code   string
		reason string
	}{
		{name: "server error", status: http.StatusInternalServerError, code: "http_error"},
		{name: "paid workspace", status: http.StatusPaymentRequired, code: "share_links_need_payment", reason: "free_credit"},
		{name: "public port limit", status: http.StatusConflict, code: "share_link_limit", reason: "public_ports"},
		{name: "ingress off", status: http.StatusServiceUnavailable, code: "preview_unavailable"},
	}
	for _, test := range refusals {
		t.Run(test.name, func(t *testing.T) {
			api := newTestAPI(t)
			api.tokenHandler = func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, test.status, map[string]any{
					"error":   test.code,
					"message": "refused",
					"reason":  test.reason,
				})
			}
			sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			_, err = sandbox.PreviewToken(context.Background(), 3000, nil)
			var failure *Error
			if !errors.As(err, &failure) {
				t.Fatalf("error = %v, want *Error", err)
			}
			if failure.Code != test.code || failure.Status != test.status {
				t.Fatalf("code/status = %s/%d, want %s/%d", failure.Code, failure.Status, test.code, test.status)
			}
			if test.reason != "" && failure.Reason != test.reason {
				t.Fatalf("reason = %q, want %q", failure.Reason, test.reason)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.tokenCalls != 1 {
				t.Fatalf("calls = %d, want 1", api.tokenCalls)
			}
		})
	}
}

func TestSetPortPublicSendsAndParses(t *testing.T) {
	api := newTestAPI(t)
	api.publicHandler = func(w http.ResponseWriter, _ *http.Request) {
		api.mu.Lock()
		body := api.publicBody
		api.mu.Unlock()
		public, _ := body["public"].(bool)
		answer := map[string]any{
			"sandboxId":  "sbx_1",
			"port":       3000,
			"url":        "https://3000-sbx.preview.prized.test/",
			"browserUrl": "https://3000-sbx.prized.link/",
			"public":     public,
		}
		if public {
			answer["publicExpiresAt"] = "2026-11-27T13:00:00Z"
		}
		writeJSON(w, http.StatusOK, map[string]any{"preview": answer})
	}
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	preview, err := sandbox.SetPortPublic(context.Background(), 3000, true, nil)
	if err != nil {
		t.Fatalf("SetPortPublic: %v", err)
	}
	if !preview.Public || preview.SandboxID != "sbx_1" || preview.Port != 3000 {
		t.Fatalf("preview = %#v", preview)
	}
	if preview.URL != "https://3000-sbx.preview.prized.test/" || preview.BrowserURL != "https://3000-sbx.prized.link/" {
		t.Fatalf("urls = %q / %q", preview.URL, preview.BrowserURL)
	}
	wantExpiry := time.Date(2026, 11, 27, 13, 0, 0, 0, time.UTC)
	if preview.PublicExpiresAt == nil || !preview.PublicExpiresAt.Equal(wantExpiry) {
		t.Fatalf("public expires at = %v", preview.PublicExpiresAt)
	}
	api.mu.Lock()
	if api.publicCalls != 1 || len(api.publicBody) != 1 || api.publicBody["public"] != true {
		t.Fatalf("calls/body = %d/%#v", api.publicCalls, api.publicBody)
	}
	api.mu.Unlock()

	if _, err := sandbox.SetPortPublic(context.Background(), 3000, true, &SetPortPublicOptions{ExpiresIn: 604800}); err != nil {
		t.Fatalf("SetPortPublic with expiry: %v", err)
	}
	api.mu.Lock()
	if api.publicBody["expiresIn"] != float64(604800) {
		t.Fatalf("body = %#v", api.publicBody)
	}
	api.mu.Unlock()

	private, err := sandbox.SetPortPublic(context.Background(), 3000, false, nil)
	if err != nil {
		t.Fatalf("SetPortPublic private: %v", err)
	}
	if private.Public || private.PublicExpiresAt != nil {
		t.Fatalf("private preview = %#v", private)
	}
	api.mu.Lock()
	if len(api.publicBody) != 1 || api.publicBody["public"] != false {
		t.Fatalf("private body = %#v", api.publicBody)
	}
	calls := api.publicCalls
	api.mu.Unlock()

	_, err = sandbox.SetPortPublic(context.Background(), 3000, false, &SetPortPublicOptions{ExpiresIn: 300})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "invalid_argument" {
		t.Fatalf("error = %v, want invalid_argument", err)
	}
	api.mu.Lock()
	if api.publicCalls != calls {
		t.Fatalf("invalid private expiry sent a request: %d -> %d", calls, api.publicCalls)
	}
	api.mu.Unlock()
}

func TestSetPortPublicRefusals(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
		reason string
	}{
		{name: "paid workspace", status: http.StatusPaymentRequired, code: "share_links_need_payment", reason: "free_credit"},
		{name: "not the owner", status: http.StatusForbidden, code: "not_allowed", reason: "owner_key"},
		{name: "public port limit", status: http.StatusConflict, code: "share_link_limit", reason: "public_ports"},
		{name: "policy", status: http.StatusForbidden, code: "policy_denied", reason: "anyoneLinksOwnerOnly"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newTestAPI(t)
			api.publicHandler = func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, test.status, map[string]any{
					"error":   test.code,
					"message": "refused",
					"reason":  test.reason,
					"billing": map[string]any{"url": "https://prized.dev/dashboard/billing"},
				})
			}
			sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			_, err = sandbox.SetPortPublic(context.Background(), 3000, true, nil)
			var failure *Error
			if !errors.As(err, &failure) {
				t.Fatalf("error = %v, want *Error", err)
			}
			if failure.Code != test.code || failure.Status != test.status || failure.Reason != test.reason {
				t.Fatalf("error = %s/%d/%s", failure.Code, failure.Status, failure.Reason)
			}
			if failure.Body == nil || failure.Body["billing"] == nil {
				t.Fatalf("body = %#v", failure.Body)
			}
			if failure.Effect != EffectNotRun {
				t.Fatalf("effect = %s, want not_run", failure.Effect)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.publicCalls != 1 {
				t.Fatalf("calls = %d, want 1", api.publicCalls)
			}
		})
	}
}

func TestSetPortPublicFallbacksAndBadResponses(t *testing.T) {
	t.Run("fallbacks", func(t *testing.T) {
		api := newTestAPI(t)
		api.publicHandler = func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"preview": map[string]any{
				"url": "https://3000-sbx.preview.prized.test/", "public": true,
			}})
		}
		sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		preview, err := sandbox.SetPortPublic(context.Background(), 3000, true, nil)
		if err != nil {
			t.Fatalf("SetPortPublic: %v", err)
		}
		if preview.SandboxID != "sbx_1" || preview.Port != 3000 || preview.PublicExpiresAt != nil {
			t.Fatalf("preview = %#v", preview)
		}
	})

	tests := []struct {
		name string
		body any
	}{
		{name: "missing preview", body: map[string]any{}},
		{name: "missing url", body: map[string]any{"preview": map[string]any{"public": true}}},
		{name: "missing public", body: map[string]any{"preview": map[string]any{"url": "https://example.test/"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newTestAPI(t)
			api.publicHandler = func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, test.body)
			}
			sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			_, err = sandbox.SetPortPublic(context.Background(), 3000, true, nil)
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != "bad_response" || failure.Effect != EffectRan {
				t.Fatalf("error = %v, want bad_response", err)
			}
		})
	}
}

func TestSetPortPublicIgnoresBadExpiry(t *testing.T) {
	api := newTestAPI(t)
	api.publicHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"preview": map[string]any{
			"url": "https://example.test/", "public": true, "publicExpiresAt": "later",
		}})
	}
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	preview, err := sandbox.SetPortPublic(context.Background(), 3000, true, nil)
	if err != nil {
		t.Fatalf("SetPortPublic: %v", err)
	}
	if preview.PublicExpiresAt != nil {
		t.Fatalf("public expires at = %v, want nil", preview.PublicExpiresAt)
	}
}

func TestPortCallsDoNotWakePausedSandbox(t *testing.T) {
	api := newTestAPI(t)
	api.tokenHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"url": "https://example.test/?prized_token=tok", "token": "tok", "expiresAt": "2026-11-20T13:00:00Z",
		})
	}
	api.publicHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"preview": map[string]any{
			"url": "https://example.test/", "public": true,
		}})
	}
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	api.mu.Lock()
	api.state = StatePaused
	getsBefore := api.gets
	api.mu.Unlock()
	sandbox.mu.Lock()
	sandbox.state = StatePaused
	sandbox.mu.Unlock()

	if _, err := sandbox.PreviewToken(context.Background(), 3000, nil); err != nil {
		t.Fatalf("PreviewToken: %v", err)
	}
	if _, err := sandbox.SetPortPublic(context.Background(), 3000, true, nil); err != nil {
		t.Fatalf("SetPortPublic: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.gets != getsBefore {
		t.Fatalf("port calls read the sandbox: gets %d -> %d", getsBefore, api.gets)
	}
	if sandbox.State() != StatePaused {
		t.Fatalf("state = %s, want paused", sandbox.State())
	}
}

func TestPreviewRedirectNeverFollowedWithKey(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer target.Close()

	var key string
	preview := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get(PreviewKeyHeader)
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer preview.Close()

	api := newTestAPI(t)
	client := api.client()
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	request, err := http.NewRequest(http.MethodGet, preview.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for name, value := range sandbox.PreviewHeaders() {
		request.Header.Set(name, value)
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", response.StatusCode)
	}
	if redirected.Load() != 0 {
		t.Fatalf("redirect target got %d requests", redirected.Load())
	}
	if key != "test-key" {
		t.Fatalf("preview host key = %q", key)
	}
	if location := response.Header.Get("Location"); location != target.URL {
		t.Fatalf("location = %q, want %q", location, target.URL)
	}
}

func TestClosedHandlePortCallsSendNothing(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sandbox.Close()

	if _, err := sandbox.PreviewToken(context.Background(), 3000, nil); !errors.Is(err, ErrHandleClosed) {
		t.Fatalf("PreviewToken error = %v, want ErrHandleClosed", err)
	}
	if _, err := sandbox.SetPortPublic(context.Background(), 3000, true, nil); !errors.Is(err, ErrHandleClosed) {
		t.Fatalf("SetPortPublic error = %v, want ErrHandleClosed", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.tokenCalls != 0 || api.publicCalls != 0 {
		t.Fatalf("closed handle sent requests: token %d, public %d", api.tokenCalls, api.publicCalls)
	}
}
