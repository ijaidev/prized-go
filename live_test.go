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
