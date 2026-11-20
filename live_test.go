//go:build live

package prized

import (
	"context"
	"os"
	"path/filepath"
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

func TestLiveFiles(t *testing.T) {
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

	files := sandbox.Files()
	mode := os.FileMode(0o755)
	if _, err := files.Write(ctx, "greet.sh", []byte("#!/bin/sh\necho live-files\n"), &WriteOptions{Mode: &mode}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	result, err := sandbox.Exec(ctx, "./greet.sh > greet.out", nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exec exit = %d, stderr = %q", result.ExitCode, result.Stderr)
	}
	output, err := files.Read(ctx, "greet.out")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if strings.TrimSpace(output) != "live-files" {
		t.Fatalf("output = %q", output)
	}

	local := filepath.Join(t.TempDir(), "greet.out")
	count, err := files.Download(ctx, "greet.out", local)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	content, err := os.ReadFile(local)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if count != int64(len(content)) || strings.TrimSpace(string(content)) != "live-files" {
		t.Fatalf("downloaded %d bytes: %q", count, content)
	}

	exists, err := files.Exists(ctx, "greet.out")
	if err != nil || !exists {
		t.Fatalf("Exists = %v, %v", exists, err)
	}
	entries, err := files.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Name == "greet.sh" {
			found = true
			if entry.Mode != "-rwxr-xr-x" {
				t.Fatalf("mode = %q, want -rwxr-xr-x", entry.Mode)
			}
		}
	}
	if !found {
		t.Fatalf("List did not return greet.sh: %#v", entries)
	}
	if err := files.Remove(ctx, "greet.out", nil); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if exists, err := files.Exists(ctx, "greet.out"); err != nil || exists {
		t.Fatalf("Exists after Remove = %v, %v", exists, err)
	}
}
