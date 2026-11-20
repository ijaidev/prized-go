package prized

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestPauseWaitsForPaused(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, getsBefore, _, _ := api.counts()

	if err := sandbox.Pause(context.Background(), nil); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if sandbox.State() != StatePaused {
		t.Fatalf("state = %s, want paused", sandbox.State())
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.pauses != 1 {
		t.Fatalf("pauses = %d, want 1", api.pauses)
	}
	if api.gets <= getsBefore {
		t.Fatalf("gets = %d, want more than %d", api.gets, getsBefore)
	}
}

func TestPauseNoWaitReturnsOnAccept(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, getsBefore, _, _ := api.counts()

	if err := sandbox.Pause(context.Background(), &PauseOptions{NoWait: true}); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if sandbox.State() != StatePausing {
		t.Fatalf("state = %s, want pausing", sandbox.State())
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.gets != getsBefore {
		t.Fatalf("gets = %d, want %d (no wait)", api.gets, getsBefore)
	}
}

func TestPauseWaitTimesOut(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	api.mu.Lock()
	api.state = StatePausing
	api.mu.Unlock()
	api.pauseHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeSandbox(w, http.StatusAccepted, StatePausing, api.server.URL)
	}

	err = sandbox.Pause(context.Background(), &PauseOptions{Timeout: 30 * time.Millisecond})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "timeout" {
		t.Fatalf("error = %v, want timeout", err)
	}
	if failure.Effect != EffectRan {
		t.Fatalf("effect = %s, want ran", failure.Effect)
	}
}

func TestResumeWaitsForRunning(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	api.mu.Lock()
	api.state = StatePaused
	api.mu.Unlock()

	if err := sandbox.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if sandbox.State() != StateRunning {
		t.Fatalf("state = %s, want running", sandbox.State())
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.resumes != 1 {
		t.Fatalf("resumes = %d, want 1", api.resumes)
	}
}

func TestKeepAliveSendsRefresh(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sandbox.KeepAlive(context.Background()); err != nil {
		t.Fatalf("KeepAlive: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", api.refreshes)
	}
}

func TestRefreshRereadsFields(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	api.getHandler = func(w http.ResponseWriter, _ *http.Request) {
		document := sandboxFields("sbx_1", StatePaused, "paused by another client", api.server.URL)
		document["template"] = "prized-other-v1"
		writeJSON(w, http.StatusOK, map[string]any{"sandbox": document})
	}

	if err := sandbox.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if sandbox.State() != StatePaused {
		t.Fatalf("state = %s, want paused", sandbox.State())
	}
	if sandbox.Template() != "prized-other-v1" {
		t.Fatalf("template = %q", sandbox.Template())
	}
	if _, reason := sandbox.stateAndReason(); reason != "paused by another client" {
		t.Fatalf("error reason = %q", reason)
	}
}

func TestRefreshGoneSandboxFails(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	api.getHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such sandbox")
	}
	err = sandbox.Refresh(context.Background())
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "not_found" {
		t.Fatalf("error = %v, want not_found", err)
	}
}

func TestExecOnPausedSandboxLetsEdgeResumeIt(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sandbox.Pause(context.Background(), nil); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	api.execHandler = func(w http.ResponseWriter, r *http.Request) {
		if value := r.Header.Get("Prized-Auto-Resume"); value != "" {
			t.Errorf("Prized-Auto-Resume = %q, want unset", value)
		}
		api.mu.Lock()
		api.state = StateRunning
		api.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"stdout": "awake\n", "stderr": "", "exitCode": 0, "durationMs": 1,
		})
	}

	result, err := sandbox.Exec(context.Background(), "true", nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "awake\n" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.resumes != 0 {
		t.Fatalf("resumes = %d, want 0: the edge resumes, not the client", api.resumes)
	}
}

func TestForkWaitsForEveryCopy(t *testing.T) {
	api := newTestAPI(t)
	parent, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	copies, err := parent.Fork(context.Background(), &ForkOptions{
		Count:  2,
		Labels: map[string]string{"run": "7"},
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(copies) != 2 {
		t.Fatalf("copies = %d, want 2", len(copies))
	}
	if copies[0].ID() != "sbx_f1" || copies[1].ID() != "sbx_f2" {
		t.Fatalf("ids = %s, %s", copies[0].ID(), copies[1].ID())
	}
	for _, copy := range copies {
		if copy.State() != StateRunning {
			t.Fatalf("copy %s state = %s, want running", copy.ID(), copy.State())
		}
	}
	if parent.State() != StateRunning {
		t.Fatalf("parent state = %s, want running", parent.State())
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.forks != 1 {
		t.Fatalf("forks = %d, want 1", api.forks)
	}
	if count, _ := api.forkBody["count"].(float64); count != 2 {
		t.Fatalf("count = %v, want 2", api.forkBody["count"])
	}
	labels, _ := api.forkBody["labels"].(map[string]any)
	if labels["run"] != "7" {
		t.Fatalf("labels = %#v", api.forkBody["labels"])
	}
	if api.forkKey == "" {
		t.Fatal("fork sent no idempotency key")
	}
}

func TestForkFailureDeletesEveryCopy(t *testing.T) {
	api := newTestAPI(t)
	parent, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	api.mu.Lock()
	api.childStates["sbx_f1"] = StateStarting
	api.childStates["sbx_f2"] = StateError
	api.childReasons["sbx_f2"] = "disk full"
	api.mu.Unlock()

	copies, err := parent.Fork(context.Background(), &ForkOptions{Count: 2})
	if copies != nil {
		t.Fatalf("copies = %v, want nil", copies)
	}
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "sandbox_error" {
		t.Fatalf("error = %v, want sandbox_error", err)
	}
	if failure.Reason != "disk full" || failure.SandboxID != "sbx_f2" {
		t.Fatalf("reason/id = %q/%q", failure.Reason, failure.SandboxID)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.childDeletes != 2 {
		t.Fatalf("child deletes = %d, want 2", api.childDeletes)
	}
}

func TestForkCallerKeyLeavesCopiesOnFailure(t *testing.T) {
	api := newTestAPI(t)
	parent, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	api.mu.Lock()
	api.childStates["sbx_f1"] = StateStarting
	api.childStates["sbx_f2"] = StateError
	api.childReasons["sbx_f2"] = "disk full"
	api.mu.Unlock()

	_, err = parent.Fork(context.Background(), &ForkOptions{Count: 2, IdempotencyKey: "caller-key"})
	if err == nil {
		t.Fatal("Fork succeeded, want a copy failure")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.forkKey != "caller-key" {
		t.Fatalf("idempotency key = %q", api.forkKey)
	}
	if api.childDeletes != 0 {
		t.Fatalf("child deletes = %d, want 0 under the caller's key", api.childDeletes)
	}
}

func TestForkCapacityReportsFits(t *testing.T) {
	api := newTestAPI(t)
	parent, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	api.forkHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "fork_capacity",
			"message": "the parent's machine has room for 2 more copies",
			"fits":    2,
		})
	}

	_, err = parent.Fork(context.Background(), &ForkOptions{Count: 3})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "fork_capacity" {
		t.Fatalf("error = %v, want fork_capacity", err)
	}
	fits, ok := failure.Fits()
	if !ok || fits != 2 {
		t.Fatalf("fits = %d/%t, want 2/true", fits, ok)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.forks != 1 {
		t.Fatalf("forks = %d, want 1", api.forks)
	}
	if api.deletes != 0 {
		t.Fatalf("deletes = %d, want 0", api.deletes)
	}
}

func TestForkRejectsInvalidCount(t *testing.T) {
	api := newTestAPI(t)
	parent, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = parent.Fork(context.Background(), &ForkOptions{Count: 257})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "invalid_argument" {
		t.Fatalf("error = %v, want invalid_argument", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.forks != 0 {
		t.Fatalf("forks = %d, want 0", api.forks)
	}
}

func TestExtendMovesExpiry(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	runTime, err := sandbox.Extend(context.Background(), ExtendOptions{By: 2 * time.Hour})
	if err != nil {
		t.Fatalf("Extend: %v", err)
	}
	want := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	if !runTime.ExpiresAt.Equal(want) {
		t.Fatalf("expires at = %s, want %s", runTime.ExpiresAt, want)
	}
	if !runTime.Capped {
		t.Fatal("capped = false, want true")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if minutes, _ := api.extendBody["byMin"].(float64); minutes != 120 {
		t.Fatalf("byMin = %v, want 120", api.extendBody["byMin"])
	}
	if api.extendKey == "" {
		t.Fatal("extend sent no idempotency key")
	}
	if api.extends != 1 {
		t.Fatalf("extends = %d, want 1", api.extends)
	}
}

func TestExtendRejectsPartialMinutes(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = sandbox.Extend(context.Background(), ExtendOptions{By: 90 * time.Second})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "invalid_argument" {
		t.Fatalf("error = %v, want invalid_argument", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.extends != 0 {
		t.Fatalf("extends = %d, want 0", api.extends)
	}
}

func TestSetTimeoutSetsExactTime(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	at := time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC)
	runTime, err := sandbox.SetTimeout(context.Background(), TimeoutOptions{At: at})
	if err != nil {
		t.Fatalf("SetTimeout: %v", err)
	}
	if !runTime.ExpiresAt.Equal(at) {
		t.Fatalf("expires at = %s, want %s", runTime.ExpiresAt, at)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	value, _ := api.timeoutBody["expiresAt"].(string)
	parsed, parseErr := time.Parse(time.RFC3339, value)
	if parseErr != nil || !parsed.Equal(at) {
		t.Fatalf("expiresAt = %q, want %s", value, at)
	}
}

func TestSetTimeoutInMinutes(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := sandbox.SetTimeout(context.Background(), TimeoutOptions{In: 30 * time.Minute}); err != nil {
		t.Fatalf("SetTimeout: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if minutes, _ := api.timeoutBody["inMin"].(float64); minutes != 30 {
		t.Fatalf("inMin = %v, want 30", api.timeoutBody["inMin"])
	}
	if _, ok := api.timeoutBody["expiresAt"]; ok {
		t.Fatalf("body = %#v, want only inMin", api.timeoutBody)
	}
}

func TestSetTimeoutRequiresExactlyOneTime(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	at := time.Now().Add(time.Hour)
	tests := []TimeoutOptions{
		{},
		{In: time.Hour, At: at},
	}
	for _, options := range tests {
		_, err := sandbox.SetTimeout(context.Background(), options)
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "invalid_argument" {
			t.Fatalf("error = %v, want invalid_argument", err)
		}
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.timeouts != 0 {
		t.Fatalf("timeouts = %d, want 0", api.timeouts)
	}
}

func TestSetTimeoutCapRefusalSurfaces(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	api.timeoutHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":        "sandbox_quota_exceeded",
			"reason":       "max_runtime",
			"message":      "that is past the run time this workspace allows",
			"limit":        10080,
			"maxExpiresAt": "2026-11-27T12:00:00Z",
		})
	}

	_, err = sandbox.SetTimeout(context.Background(), TimeoutOptions{In: 30 * 24 * time.Hour})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "sandbox_quota_exceeded" {
		t.Fatalf("error = %v, want sandbox_quota_exceeded", err)
	}
	if failure.Reason != "max_runtime" || failure.Status != http.StatusTooManyRequests {
		t.Fatalf("reason/status = %q/%d", failure.Reason, failure.Status)
	}
	if failure.Body["maxExpiresAt"] != "2026-11-27T12:00:00Z" {
		t.Fatalf("body = %#v", failure.Body)
	}
}

func TestDeleteWaitToleratesGoneDuringPoll(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	api.getHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such sandbox")
	}
	if err := sandbox.Delete(context.Background(), &DeleteOptions{Wait: true}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if sandbox.State() != StateDeleted {
		t.Fatalf("state = %s, want deleted", sandbox.State())
	}
}

func TestClosedHandleRejectsLifecycle(t *testing.T) {
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	api.mu.Lock()
	before := struct {
		gets, deletes, pauses, resumes, refreshes, forks, extends, timeouts int
	}{api.gets, api.deletes, api.pauses, api.resumes, api.refreshes, api.forks, api.extends, api.timeouts}
	api.mu.Unlock()
	sandbox.Close()

	ctx := context.Background()
	failures := map[string]error{
		"refresh":   sandbox.Refresh(ctx),
		"keepalive": sandbox.KeepAlive(ctx),
		"pause":     sandbox.Pause(ctx, nil),
		"resume":    sandbox.Resume(ctx, nil),
	}
	_, failures["extend"] = sandbox.Extend(ctx, ExtendOptions{By: time.Hour})
	_, failures["timeout"] = sandbox.SetTimeout(ctx, TimeoutOptions{In: time.Hour})
	_, failures["fork"] = sandbox.Fork(ctx, nil)
	for name, err := range failures {
		if !errors.Is(err, ErrHandleClosed) {
			t.Fatalf("%s error = %v, want ErrHandleClosed", name, err)
		}
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	after := struct {
		gets, deletes, pauses, resumes, refreshes, forks, extends, timeouts int
	}{api.gets, api.deletes, api.pauses, api.resumes, api.refreshes, api.forks, api.extends, api.timeouts}
	if after != before {
		t.Fatalf("closed handle sent requests: before %+v, after %+v", before, after)
	}
}
