//go:build live

package prized

import (
	"context"
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
