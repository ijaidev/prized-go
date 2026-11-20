//go:build live

package prized

import (
	"context"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLiveTracer(t *testing.T) {
	if os.Getenv("PRIZED_API_KEY") == "" {
		t.Skip("PRIZED_API_KEY is not set")
	}
	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	sandbox, err := client.Sandboxes().Create(ctx, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() {
		deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer deleteCancel()
		if err := sandbox.Delete(deleteCtx, &DeleteOptions{Wait: true}); err != nil {
			t.Errorf("Delete: %v", err)
		}
	}()

	result, err := sandbox.Exec(ctx, `python3 -c "print(6 * 7)"`, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", result.ExitCode, result.Stderr)
	}
	if strings.TrimSpace(result.Stdout) != "42" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
}

func TestLiveDiscovery(t *testing.T) {
	if os.Getenv("PRIZED_API_KEY") == "" {
		t.Skip("PRIZED_API_KEY is not set")
	}
	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	labels := map[string]string{"run": "prized-go-live-" + strconv.FormatInt(time.Now().UnixNano(), 10)}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if _, err := client.Sandboxes().DeleteByLabels(cleanupCtx, labels); err != nil {
			t.Logf("cleanup: %v", err)
		}
	}()

	for index := 0; index < 3; index++ {
		sandbox, err := client.Sandboxes().Create(ctx, CreateSandboxOptions{
			Labels:        labels,
			MaxRuntimeMin: 60,
		})
		if err != nil {
			t.Fatalf("Create %d: %v", index, err)
		}
		if sandbox.State() != StateRunning {
			t.Fatalf("sandbox %s state = %s, want running", sandbox.ID(), sandbox.State())
		}
	}

	templates, err := client.Sandboxes().Templates(ctx)
	if err != nil {
		t.Fatalf("Templates: %v", err)
	}
	if len(templates) == 0 {
		t.Fatal("Templates returned none")
	}

	found, err := client.Sandboxes().List(ctx, ListSandboxesOptions{
		Labels: labels,
		States: []string{StateRunning},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(found) != 3 {
		t.Fatalf("list found %d sandboxes, want 3", len(found))
	}
	connected, err := client.Sandboxes().Connect(ctx, found[0].ID())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if connected.State() != StateRunning {
		t.Fatalf("connected state = %s", connected.State())
	}

	ids, err := client.Sandboxes().DeleteByLabels(ctx, labels)
	if err != nil {
		t.Fatalf("DeleteByLabels: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("deleted ids = %#v, want 3", ids)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		left, err := client.Sandboxes().List(ctx, ListSandboxesOptions{Labels: labels})
		if err != nil {
			t.Fatalf("List after delete: %v", err)
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sandboxes are still listed after the delete", len(left))
		}
		time.Sleep(time.Second)
	}
}

func TestLivePorts(t *testing.T) {
	if os.Getenv("PRIZED_API_KEY") == "" {
		t.Skip("PRIZED_API_KEY is not set")
	}
	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	sandbox, err := client.Sandboxes().Create(ctx, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() {
		deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer deleteCancel()
		if err := sandbox.Delete(deleteCtx, &DeleteOptions{Wait: true}); err != nil {
			t.Errorf("Delete: %v", err)
		}
	}()

	if _, err := sandbox.Exec(ctx, `nohup python3 -m http.server 3000 >/tmp/prized-port.log 2>&1 &`, nil); err != nil {
		t.Fatalf("Exec server: %v", err)
	}

	previewURL, err := sandbox.PreviewURL(3000)
	if err != nil {
		t.Fatalf("PreviewURL: %v", err)
	}
	previewClient := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	status, body := fetchPreview(ctx, t, previewClient, previewURL, sandbox.PreviewHeaders())
	if status != http.StatusOK {
		t.Fatalf("preview with key = %d: %s", status, body)
	}

	token, err := sandbox.PreviewToken(ctx, 3000, &PreviewTokenOptions{ExpiresIn: 600})
	if err != nil {
		t.Fatalf("PreviewToken: %v", err)
	}
	if token.Token == "" || token.URL == "" || token.ExpiresAt.Before(time.Now().Add(5*time.Minute)) {
		t.Fatalf("token = %#v", token)
	}
	status, body = fetchPreview(ctx, t, previewClient, token.URL, nil)
	if status != http.StatusOK {
		t.Fatalf("token URL = %d: %s", status, body)
	}

	preview, err := sandbox.SetPortPublic(ctx, 3000, true, &SetPortPublicOptions{ExpiresIn: 300})
	if err != nil {
		t.Fatalf("SetPortPublic true: %v", err)
	}
	if !preview.Public || preview.URL == "" {
		t.Fatalf("preview = %#v", preview)
	}
	defer func() {
		privateCtx, privateCancel := context.WithTimeout(context.Background(), time.Minute)
		defer privateCancel()
		if _, err := sandbox.SetPortPublic(privateCtx, 3000, false, nil); err != nil {
			t.Errorf("SetPortPublic false: %v", err)
		}
	}()
	status, body = fetchPreview(ctx, t, previewClient, preview.URL, nil)
	if status != http.StatusOK {
		t.Fatalf("public URL = %d: %s", status, body)
	}
}

func fetchPreview(ctx context.Context, t *testing.T, client *http.Client, url string, headers map[string]string) (int, string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("preview request: %v", err)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			t.Fatalf("read preview: %v", readErr)
		}
		if response.StatusCode != http.StatusBadGateway || time.Now().After(deadline) {
			return response.StatusCode, string(body)
		}
		time.Sleep(time.Second)
	}
}
