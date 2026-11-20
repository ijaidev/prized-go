package prized

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func discoverySandbox(id, state string, labels map[string]string) map[string]any {
	return map[string]any{
		"id":            id,
		"state":         state,
		"template":      DefaultTemplate,
		"templateBuild": "prized-base-v1-r2",
		"resources": map[string]any{
			"vcpu":     1,
			"memoryMb": 2048,
			"diskGb":   10,
		},
		"labels":        labels,
		"autoPauseMin":  5,
		"autoDeleteMin": 30,
		"maxRuntimeMin": 60,
		"expiresAt":     "2026-11-20T12:00:00Z",
		"createdAt":     "2026-11-20T10:00:00Z",
		"lastActiveAt":  "2026-11-20T10:05:00Z",
		"errorReason":   nil,
		"toolboxBase":   "http://127.0.0.1/v1/sandbox/" + id,
	}
}

func sandboxIDs(sandboxes []*Sandbox) []string {
	ids := make([]string, len(sandboxes))
	for i, sandbox := range sandboxes {
		ids[i] = sandbox.ID()
	}
	return ids
}

func TestCreateSendsAllOptionsAndReflectsThem(t *testing.T) {
	api := newTestAPI(t)
	var wait string
	api.createHandler = func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		wait = r.URL.Query().Get("wait")
		api.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{
			"sandbox": map[string]any{
				"id":            "sbx_full",
				"state":         StateRunning,
				"template":      "prized-agent-v1",
				"templateBuild": "prized-agent-v1-r2",
				"resources": map[string]any{
					"vcpu":     2,
					"memoryMb": 4096,
					"diskGb":   20,
				},
				"labels":        map[string]string{"job": "batch", "run": "42"},
				"autoPauseMin":  10,
				"autoDeleteMin": 60,
				"maxRuntimeMin": 120,
				"expiresAt":     "2026-11-20T12:00:00Z",
				"createdAt":     "2026-11-20T10:00:00Z",
				"lastActiveAt":  "2026-11-20T10:05:00Z",
				"errorReason":   nil,
				"toolboxBase":   api.server.URL + "/v1/sandbox/sbx_full",
			},
		})
	}

	sandbox, err := api.createSandbox(t, CreateSandboxOptions{
		Template:       "prized-agent-v1",
		Resources:      &SandboxResources{VCPU: 2, MemoryMB: 4096, DiskGB: 20},
		EnvVars:        map[string]string{"ANTHROPIC_API_KEY": "secret"},
		Labels:         map[string]string{"job": "batch", "run": "42"},
		AutoPauseMin:   10,
		AutoDeleteMin:  60,
		MaxRuntimeMin:  120,
		IdempotencyKey: "caller-key",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	api.mu.Lock()
	body := api.createBody
	keys := append([]string(nil), api.idempotencyKey...)
	api.mu.Unlock()
	want := map[string]any{
		"template": "prized-agent-v1",
		"resources": map[string]any{
			"vcpu":     float64(2),
			"memoryMb": float64(4096),
			"diskGb":   float64(20),
		},
		"envVars":       map[string]any{"ANTHROPIC_API_KEY": "secret"},
		"labels":        map[string]any{"job": "batch", "run": "42"},
		"autoPauseMin":  float64(10),
		"autoDeleteMin": float64(60),
		"maxRuntimeMin": float64(120),
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("create body = %#v, want %#v", body, want)
	}
	if len(keys) != 1 || keys[0] != "caller-key" {
		t.Fatalf("idempotency keys = %#v", keys)
	}
	if wait != "running" {
		t.Fatalf("wait query = %q, want running", wait)
	}

	if sandbox.ID() != "sbx_full" || sandbox.State() != StateRunning || sandbox.Template() != "prized-agent-v1" {
		t.Fatalf("sandbox = %s/%s/%s", sandbox.ID(), sandbox.State(), sandbox.Template())
	}
	if got := sandbox.Resources(); got != (SandboxResources{VCPU: 2, MemoryMB: 4096, DiskGB: 20}) {
		t.Fatalf("resources = %#v", got)
	}
	if got := sandbox.Resources().MemoryGB(); got != 4 {
		t.Fatalf("memory GB = %v", got)
	}
	if !reflect.DeepEqual(sandbox.Labels(), map[string]string{"job": "batch", "run": "42"}) {
		t.Fatalf("labels = %#v", sandbox.Labels())
	}
	if sandbox.AutoPauseMin() != 10 || sandbox.AutoDeleteMin() != 60 || sandbox.MaxRuntimeMin() != 120 {
		t.Fatalf("timers = %d/%d/%d", sandbox.AutoPauseMin(), sandbox.AutoDeleteMin(), sandbox.MaxRuntimeMin())
	}
	if !sandbox.ExpiresAt().Equal(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("expires at = %s", sandbox.ExpiresAt())
	}
	if !sandbox.CreatedAt().Equal(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("created at = %s", sandbox.CreatedAt())
	}
	if !sandbox.LastActiveAt().Equal(time.Date(2026, 11, 20, 10, 5, 0, 0, time.UTC)) {
		t.Fatalf("last active at = %s", sandbox.LastActiveAt())
	}
	if sandbox.ErrorReason() != "" {
		t.Fatalf("error reason = %q", sandbox.ErrorReason())
	}
	raw := sandbox.Raw()
	if raw["templateBuild"] != "prized-agent-v1-r2" {
		t.Fatalf("raw = %#v", raw)
	}
	raw["template"] = "changed"
	if sandbox.Raw()["template"] != "prized-agent-v1" {
		t.Fatal("Raw exposed the handle's own document")
	}
	labels := sandbox.Labels()
	labels["job"] = "changed"
	if sandbox.Labels()["job"] != "batch" {
		t.Fatal("Labels exposed the handle's own map")
	}
}

func TestCreateSendsOnlyTheShapeFieldsGiven(t *testing.T) {
	api := newTestAPI(t)
	if _, err := api.createSandbox(t, CreateSandboxOptions{
		Resources: &SandboxResources{MemoryMB: 4096},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	api.mu.Lock()
	body := api.createBody
	api.mu.Unlock()
	resources, ok := body["resources"].(map[string]any)
	if !ok {
		t.Fatalf("resources = %#v", body["resources"])
	}
	if !reflect.DeepEqual(resources, map[string]any{"memoryMb": float64(4096)}) {
		t.Fatalf("resources = %#v", resources)
	}
}

func TestCreateNoWaitReturnsBeforeRunning(t *testing.T) {
	api := newTestAPI(t)
	var wait string
	api.createHandler = func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		wait = r.URL.Query().Get("wait")
		api.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{
			"sandbox": discoverySandbox("sbx_starting", StateStarting, nil),
		})
	}

	sandbox, err := api.createSandbox(t, CreateSandboxOptions{NoWait: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sandbox.State() != StateStarting {
		t.Fatalf("state = %s, want starting", sandbox.State())
	}
	if wait != "" {
		t.Fatalf("wait query = %q, want none", wait)
	}
	if _, gets, _, _ := api.counts(); gets != 0 {
		t.Fatalf("gets = %d, want 0", gets)
	}
}

func TestConnectReturnsAnyStateHandle(t *testing.T) {
	api := newTestAPI(t)
	api.connectHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sandboxes/sbx_paused" {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"sandbox": discoverySandbox("sbx_paused", StatePaused, map[string]string{"job": "batch"}),
		})
	}
	client := api.client()

	if _, err := client.Sandboxes().Connect(context.Background(), ""); err == nil {
		t.Fatal("Connect with an empty id succeeded")
	} else {
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "invalid_argument" {
			t.Fatalf("error = %v, want invalid_argument", err)
		}
	}
	if _, _, _, connects := api.discoveryCounts(); connects != 0 {
		t.Fatalf("connects = %d, want 0", connects)
	}

	sandbox, err := client.Sandboxes().Connect(context.Background(), "sbx_paused")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if sandbox.ID() != "sbx_paused" || sandbox.State() != StatePaused {
		t.Fatalf("sandbox = %s/%s", sandbox.ID(), sandbox.State())
	}
	if sandbox.AutoPauseMin() != 5 || sandbox.AutoDeleteMin() != 30 {
		t.Fatalf("timers = %d/%d", sandbox.AutoPauseMin(), sandbox.AutoDeleteMin())
	}
	if !reflect.DeepEqual(sandbox.Labels(), map[string]string{"job": "batch"}) {
		t.Fatalf("labels = %#v", sandbox.Labels())
	}
	if _, _, _, connects := api.discoveryCounts(); connects != 1 {
		t.Fatalf("connects = %d, want 1", connects)
	}
}

func TestListFiltersDeletedStatesAndLabels(t *testing.T) {
	api := newTestAPI(t)
	var queries [][]string
	api.listHandler = func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		queries = append(queries, append([]string(nil), r.URL.Query()["label"]...))
		api.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"sandboxes": []any{
				discoverySandbox("sbx_new", StateRunning, map[string]string{"job": "batch", "team": "a"}),
				discoverySandbox("sbx_old", StatePaused, map[string]string{"job": "batch"}),
				discoverySandbox("sbx_gone", StateDeleted, map[string]string{"job": "batch"}),
				discoverySandbox("sbx_other", StateRunning, map[string]string{"job": "other"}),
			},
		})
	}
	client := api.client()

	found, err := client.Sandboxes().List(context.Background(), ListSandboxesOptions{
		Labels: map[string]string{"job": "batch"},
		States: []string{StateRunning, StatePaused},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := sandboxIDs(found); !reflect.DeepEqual(got, []string{"sbx_new", "sbx_old"}) {
		t.Fatalf("ids = %#v", got)
	}
	if found[0].Resources() != (SandboxResources{VCPU: 1, MemoryMB: 2048, DiskGB: 10}) {
		t.Fatalf("resources = %#v", found[0].Resources())
	}
	if found[0].CreatedAt().IsZero() || found[0].ExpiresAt().IsZero() {
		t.Fatal("list handles carry no timestamps")
	}

	found, err = client.Sandboxes().List(context.Background(), ListSandboxesOptions{
		Labels: map[string]string{"job": "batch", "team": "a"},
	})
	if err != nil {
		t.Fatalf("List with two labels: %v", err)
	}
	if got := sandboxIDs(found); !reflect.DeepEqual(got, []string{"sbx_new"}) {
		t.Fatalf("ids = %#v", got)
	}

	found, err = client.Sandboxes().List(context.Background(), ListSandboxesOptions{
		States: []string{StatePaused},
	})
	if err != nil {
		t.Fatalf("List paused: %v", err)
	}
	if got := sandboxIDs(found); !reflect.DeepEqual(got, []string{"sbx_old"}) {
		t.Fatalf("ids = %#v", got)
	}

	found, err = client.Sandboxes().List(context.Background(), ListSandboxesOptions{})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if got := sandboxIDs(found); !reflect.DeepEqual(got, []string{"sbx_new", "sbx_old", "sbx_other"}) {
		t.Fatalf("ids = %#v", got)
	}

	wantQueries := [][]string{
		{"job=batch"},
		{"job=batch", "team=a"},
		nil,
		nil,
	}
	if !reflect.DeepEqual(queries, wantQueries) {
		t.Fatalf("label queries = %#v, want %#v", queries, wantQueries)
	}
}

func TestTemplatesExposeDefaultsAndBounds(t *testing.T) {
	api := newTestAPI(t)
	api.templatesHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"templates": []any{
				map[string]any{
					"id":    "prized-base-v1",
					"build": "prized-base-v1-r2",
					"resources": map[string]any{
						"defaults": map[string]any{"vcpu": 4, "memoryMb": 8192, "diskGb": 10},
						"min":      map[string]any{"vcpu": 1, "memoryMb": 1024, "diskGb": 10},
						"max":      map[string]any{"vcpu": 32, "memoryMb": 131072, "diskGb": 128},
					},
					"durability":           "ephemeral",
					"defaultMaxRuntimeMin": 1440,
				},
				map[string]any{"build": "no-id"},
			},
		})
	}

	templates, err := api.client().Sandboxes().Templates(context.Background())
	if err != nil {
		t.Fatalf("Templates: %v", err)
	}
	want := []Template{{
		ID:                   "prized-base-v1",
		Build:                "prized-base-v1-r2",
		Defaults:             SandboxResources{VCPU: 4, MemoryMB: 8192, DiskGB: 10},
		Min:                  SandboxResources{VCPU: 1, MemoryMB: 1024, DiskGB: 10},
		Max:                  SandboxResources{VCPU: 32, MemoryMB: 131072, DiskGB: 128},
		Durability:           "ephemeral",
		DefaultMaxRuntimeMin: 1440,
	}}
	if !reflect.DeepEqual(templates, want) {
		t.Fatalf("templates = %#v, want %#v", templates, want)
	}

	api.templatesHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{})
	}
	_, err = api.client().Sandboxes().Templates(context.Background())
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "bad_response" {
		t.Fatalf("error = %v, want bad_response", err)
	}
}

func TestDeleteByLabelsRequiresALabel(t *testing.T) {
	api := newTestAPI(t)
	client := api.client()

	if _, err := client.Sandboxes().DeleteByLabels(context.Background(), nil); err == nil {
		t.Fatal("DeleteByLabels with no labels succeeded")
	} else {
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "invalid_argument" {
			t.Fatalf("error = %v, want invalid_argument", err)
		}
	}
	if _, err := client.Sandboxes().DeleteByLabels(context.Background(), map[string]string{"bad=key": "value"}); err == nil {
		t.Fatal("DeleteByLabels with an unselectable label succeeded")
	}
	if _, _, bulkDeletes, _ := api.discoveryCounts(); bulkDeletes != 0 {
		t.Fatalf("bulk deletes = %d, want 0", bulkDeletes)
	}
}

func TestDeleteByLabelsDeletesEveryPageAndReturnsIDs(t *testing.T) {
	api := newTestAPI(t)
	var calls int
	var queries [][]string
	api.deleteLabelsHandler = func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		calls++
		current := calls
		queries = append(queries, append([]string(nil), r.URL.Query()["label"]...))
		api.mu.Unlock()
		if current == 1 {
			writeJSON(w, http.StatusAccepted, map[string]any{
				"count": 2,
				"ids":   []string{"sbx_1", "sbx_2"},
				"more":  true,
			})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"count": 2,
			"ids":   []string{"sbx_2", "sbx_3"},
			"more":  false,
		})
	}

	ids, err := api.client().Sandboxes().DeleteByLabels(context.Background(), map[string]string{
		"job":  "batch",
		"team": "a",
	})
	if err != nil {
		t.Fatalf("DeleteByLabels: %v", err)
	}
	if !reflect.DeepEqual(ids, []string{"sbx_1", "sbx_2", "sbx_3"}) {
		t.Fatalf("ids = %#v", ids)
	}
	api.mu.Lock()
	gotCalls := calls
	gotQueries := queries
	api.mu.Unlock()
	if gotCalls != 2 {
		t.Fatalf("calls = %d, want 2", gotCalls)
	}
	wantQuery := []string{"job=batch", "team=a"}
	for _, query := range gotQueries {
		if !reflect.DeepEqual(query, wantQuery) {
			t.Fatalf("label query = %#v, want %#v", query, wantQuery)
		}
	}
}

func TestDeleteByLabelsRetriesRateLimit(t *testing.T) {
	api := newTestAPI(t)
	var calls int
	api.deleteLabelsHandler = func(w http.ResponseWriter, _ *http.Request) {
		api.mu.Lock()
		calls++
		current := calls
		api.mu.Unlock()
		if current == 1 {
			w.Header().Set("Retry-After", "0")
			writeError(w, http.StatusTooManyRequests, "rate_limited", "slow down")
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"count": 1,
			"ids":   []string{"sbx_1"},
			"more":  false,
		})
	}

	ids, err := api.client().Sandboxes().DeleteByLabels(context.Background(), map[string]string{"job": "batch"})
	if err != nil {
		t.Fatalf("DeleteByLabels: %v", err)
	}
	if !reflect.DeepEqual(ids, []string{"sbx_1"}) {
		t.Fatalf("ids = %#v", ids)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestDeleteByLabelsStopsWhenNoProgressIsMade(t *testing.T) {
	api := newTestAPI(t)
	api.deleteLabelsHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"count": 1,
			"ids":   []string{"sbx_1"},
			"more":  true,
		})
	}

	_, err := api.client().Sandboxes().DeleteByLabels(context.Background(), map[string]string{"job": "batch"})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "delete_incomplete" {
		t.Fatalf("error = %v, want delete_incomplete", err)
	}
	if _, _, bulkDeletes, _ := api.discoveryCounts(); bulkDeletes != 2 {
		t.Fatalf("bulk deletes = %d, want 2", bulkDeletes)
	}
}

func TestDeleteByLabelsDoesNotResendLostAnswer(t *testing.T) {
	api := newTestAPI(t)
	api.deleteLabelsHandler = func(w http.ResponseWriter, _ *http.Request) {
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
	}

	_, err := api.client().Sandboxes().DeleteByLabels(context.Background(), map[string]string{"job": "batch"})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "network_error" {
		t.Fatalf("error = %v, want network_error", err)
	}
	if failure.Effect != EffectMaybeRan {
		t.Fatalf("effect = %s, want maybe_ran", failure.Effect)
	}
	if _, _, bulkDeletes, _ := api.discoveryCounts(); bulkDeletes != 1 {
		t.Fatalf("bulk deletes = %d, want 1", bulkDeletes)
	}
}

func TestDeleteByLabelsDoesNotResendServerError(t *testing.T) {
	api := newTestAPI(t)
	var calls int
	api.deleteLabelsHandler = func(w http.ResponseWriter, _ *http.Request) {
		api.mu.Lock()
		calls++
		current := calls
		api.mu.Unlock()
		if current == 1 {
			writeError(w, http.StatusBadGateway, "control_plane_unavailable", "try again")
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"count": 1, "ids": []string{"sbx_1"}, "more": false})
	}

	// A 502 from a proxy may come after the delete ran, so an exec-like
	// mutation is not sent again.
	_, err := api.client().Sandboxes().DeleteByLabels(context.Background(), map[string]string{"job": "batch"})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "control_plane_unavailable" {
		t.Fatalf("error = %v, want control_plane_unavailable", err)
	}
	if failure.Effect != EffectMaybeRan {
		t.Fatalf("effect = %s, want maybe_ran", failure.Effect)
	}
	if _, _, bulkDeletes, _ := api.discoveryCounts(); bulkDeletes != 1 {
		t.Fatalf("bulk deletes = %d, want 1", bulkDeletes)
	}
}
