//go:build live

package prized

import (
	"context"
	"os"
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

func TestLiveLifecycle(t *testing.T) {
	if os.Getenv("PRIZED_API_KEY") == "" {
		t.Skip("PRIZED_API_KEY is not set")
	}
	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	sandbox, err := client.Sandboxes().Create(ctx, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() {
		deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer deleteCancel()
		if err := sandbox.Delete(deleteCtx, &DeleteOptions{Wait: true}); err != nil {
			t.Errorf("Delete parent: %v", err)
		}
	}()

	if err := sandbox.Pause(ctx, nil); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if sandbox.State() != StatePaused {
		t.Fatalf("state after pause = %s", sandbox.State())
	}

	copies, err := sandbox.Fork(ctx, &ForkOptions{Count: 2})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(copies) != 2 {
		t.Fatalf("copies = %d, want 2", len(copies))
	}
	defer func() {
		deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer deleteCancel()
		for _, copy := range copies {
			if err := copy.Delete(deleteCtx, &DeleteOptions{Wait: true}); err != nil {
				t.Errorf("Delete copy %s: %v", copy.ID(), err)
			}
		}
	}()

	if err := sandbox.Resume(ctx, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if sandbox.State() != StateRunning {
		t.Fatalf("state after resume = %s", sandbox.State())
	}

	runTime, err := sandbox.Extend(ctx, ExtendOptions{By: time.Hour})
	if err != nil {
		t.Fatalf("Extend: %v", err)
	}
	if runTime.ExpiresAt.IsZero() || !runTime.ExpiresAt.After(time.Now()) {
		t.Fatalf("expires at = %s", runTime.ExpiresAt)
	}
}
