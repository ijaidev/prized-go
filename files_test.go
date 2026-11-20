package prized

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func newFilesFixture(t *testing.T) (*testAPI, *Sandbox) {
	t.Helper()
	api := newTestAPI(t)
	sandbox, err := api.createSandbox(t, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return api, sandbox
}

func fileMode(mode os.FileMode) *os.FileMode {
	return &mode
}

func successfulExec() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"stdout": "", "stderr": "", "exitCode": 0, "durationMs": 1,
		})
	}
}

func countCommands(commands []string, fragment string) int {
	count := 0
	for _, command := range commands {
		if strings.Contains(command, fragment) {
			count++
		}
	}
	return count
}

func TestWriteStagesAndPublishes(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	api.execHandler = successfulExec()

	var mu sync.Mutex
	var uploadedPath, uploadedType string
	var uploadedBody []byte
	api.uploadHandler = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		uploadedPath = r.URL.Query().Get("path")
		uploadedType = r.Header.Get("Content-Type")
		uploadedBody = body
		mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"path": uploadedPath, "size": len(body)})
	}

	info, err := sandbox.Files().Write(context.Background(), "hello.txt", []byte("hi"), nil)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if info.Path != "/home/prized/work/hello.txt" || info.Size != 2 {
		t.Fatalf("info = %#v", info)
	}
	staged := regexp.MustCompile(`^/home/prized/work/\.prized-write-[0-9a-f]{32}$`)
	mu.Lock()
	if !staged.MatchString(uploadedPath) {
		t.Fatalf("upload path = %q", uploadedPath)
	}
	if uploadedType != "application/octet-stream" {
		t.Fatalf("content type = %q", uploadedType)
	}
	if string(uploadedBody) != "hi" {
		t.Fatalf("body = %q", uploadedBody)
	}
	mu.Unlock()

	commands, cwds := api.execLog()
	if len(commands) != 1 {
		t.Fatalf("commands = %d, want 1: %#v", len(commands), commands)
	}
	if cwds[0] != "/home/prized" {
		t.Fatalf("cwd = %q, want /home/prized", cwds[0])
	}
	if !strings.Contains(commands[0], "src="+uploadedPath) {
		t.Fatalf("publish names no staging file: %q", commands[0])
	}
	if !strings.Contains(commands[0], "dst=/home/prized/work/hello.txt") {
		t.Fatalf("publish names no target: %q", commands[0])
	}
	if !strings.Contains(commands[0], `chmod "$(printf %o $((0x${s%% *} & 0777)))" -- "$src" || exit`) {
		t.Fatalf("publish does not preserve the mode: %q", commands[0])
	}
	if !strings.Contains(commands[0], `mv -fT -- "$src" "$dst" || exit`) {
		t.Fatalf("publish does not rename: %q", commands[0])
	}
}

func TestWriteHonorsExplicitMode(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	api.execHandler = successfulExec()

	if _, err := sandbox.Files().Write(context.Background(), "run.sh", []byte("#!/bin/sh\n"), &WriteOptions{Mode: fileMode(0o755)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	commands, _ := api.execLog()
	if len(commands) != 1 || !strings.Contains(commands[0], `chmod 755 -- "$src" || exit`) {
		t.Fatalf("commands = %#v", commands)
	}
	if strings.Contains(commands[0], "printf %o") {
		t.Fatalf("explicit mode should replace the preserved bits: %q", commands[0])
	}
}

func TestWritePathRules(t *testing.T) {
	t.Run("relative path stages in the workspace", func(t *testing.T) {
		api, sandbox := newFilesFixture(t)
		api.execHandler = successfulExec()
		info, err := sandbox.Files().Write(context.Background(), "sub/x.txt", []byte("x"), nil)
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if info.Path != "/home/prized/work/sub/x.txt" {
			t.Fatalf("info = %#v", info)
		}
		paths, _, _ := api.uploadLog()
		if len(paths) != 1 || !regexp.MustCompile(`^/home/prized/work/sub/\.prized-write-[0-9a-f]{32}$`).MatchString(paths[0]) {
			t.Fatalf("upload paths = %#v", paths)
		}
	})

	t.Run("dots and doubled slashes are cleaned", func(t *testing.T) {
		api, sandbox := newFilesFixture(t)
		api.execHandler = successfulExec()
		info, err := sandbox.Files().Write(context.Background(), "/home/prized/work/./a//b.txt", []byte("x"), nil)
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if info.Path != "/home/prized/work/a/b.txt" {
			t.Fatalf("info = %#v", info)
		}
		paths, _, _ := api.uploadLog()
		if len(paths) != 1 || !regexp.MustCompile(`^/home/prized/work/a/\.prized-write-[0-9a-f]{32}$`).MatchString(paths[0]) {
			t.Fatalf("upload paths = %#v", paths)
		}
	})

	t.Run("outside home goes straight to the files API", func(t *testing.T) {
		api, sandbox := newFilesFixture(t)
		if _, err := sandbox.Files().Write(context.Background(), "/tmp/outside.txt", []byte("x"), nil); err != nil {
			t.Fatalf("Write: %v", err)
		}
		paths, _, _ := api.uploadLog()
		if len(paths) != 1 || paths[0] != "/tmp/outside.txt" {
			t.Fatalf("upload paths = %#v", paths)
		}
		if commands, _ := api.execLog(); len(commands) != 0 {
			t.Fatalf("commands = %#v, want none", commands)
		}
	})

	t.Run("dot dot goes straight to the files API", func(t *testing.T) {
		api, sandbox := newFilesFixture(t)
		path := "/home/prized/work/../escape.txt"
		if _, err := sandbox.Files().Write(context.Background(), path, []byte("x"), nil); err != nil {
			t.Fatalf("Write: %v", err)
		}
		paths, _, _ := api.uploadLog()
		if len(paths) != 1 || paths[0] != path {
			t.Fatalf("upload paths = %#v", paths)
		}
	})

	t.Run("empty path is refused before sending", func(t *testing.T) {
		api, sandbox := newFilesFixture(t)
		_, err := sandbox.Files().Write(context.Background(), "", []byte("x"), nil)
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "invalid_argument" {
			t.Fatalf("error = %v, want invalid_argument", err)
		}
		if uploads, _ := api.fileCounts(); uploads != 0 {
			t.Fatalf("uploads = %d, want 0", uploads)
		}
	})
}

func TestWriteRefusesBadModeAndOversize(t *testing.T) {
	api, sandbox := newFilesFixture(t)

	_, err := sandbox.Files().Write(context.Background(), "x", []byte("x"), &WriteOptions{Mode: fileMode(0o1000)})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "invalid_argument" {
		t.Fatalf("mode error = %v, want invalid_argument", err)
	}

	_, err = sandbox.Files().Write(context.Background(), "big.bin", make([]byte, MaxFileBytes+1), nil)
	if !errors.As(err, &failure) || failure.Code != "file_too_large" {
		t.Fatalf("size error = %v, want file_too_large", err)
	}
	if failure.SandboxID != "sbx_1" {
		t.Fatalf("sandbox id = %q, want sbx_1", failure.SandboxID)
	}
	if uploads, _ := api.fileCounts(); uploads != 0 {
		t.Fatalf("uploads = %d, want 0", uploads)
	}
}

func TestWriteStagedUploadRetriesSameName(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	api.execHandler = successfulExec()

	var attempts int
	api.uploadHandler = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		api.mu.Lock()
		attempts++
		current := attempts
		api.mu.Unlock()
		if current == 1 {
			writeError(w, http.StatusBadGateway, "sandbox_unreachable", "try again")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": r.URL.Query().Get("path"), "size": len(body)})
	}

	if _, err := sandbox.Files().Write(context.Background(), "notes.txt", []byte("hello"), nil); err != nil {
		t.Fatalf("Write: %v", err)
	}
	paths, _, _ := api.uploadLog()
	if len(paths) != 2 || paths[0] != paths[1] {
		t.Fatalf("upload paths = %#v, want one staged name sent twice", paths)
	}
	if commands, _ := api.execLog(); countCommands(commands, "mv -fT") != 1 {
		t.Fatalf("commands = %#v, want one publish", commands)
	}
}

func TestWriteStagedUploadServerErrorIsNotRetried(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	api.execHandler = successfulExec()
	api.uploadHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusInternalServerError, "internal_error", "a database blip")
	}

	_, err := sandbox.Files().Write(context.Background(), "notes.txt", []byte("hello"), nil)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "internal_error" {
		t.Fatalf("error = %v, want internal_error", err)
	}
	if failure.Effect != EffectNotRun {
		t.Fatalf("effect = %s, want not_run: the target is unchanged", failure.Effect)
	}
	if !strings.Contains(failure.Message, "notes.txt was not changed") {
		t.Fatalf("message = %q", failure.Message)
	}
	paths, _, _ := api.uploadLog()
	if len(paths) != 1 {
		t.Fatalf("uploads = %d, want 1", len(paths))
	}
	commands, _ := api.execLog()
	if countCommands(commands, "mv -fT") != 0 {
		t.Fatalf("commands = %#v, want no publish", commands)
	}
	if countCommands(commands, "rm -f --") != 1 {
		t.Fatalf("commands = %#v, want one staged discard", commands)
	}
}

func TestWriteLostPublishAnswerIsNotResent(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	var publishAttempts int
	api.execHandler = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(body, &request)
		if strings.Contains(request.Command, "mv -fT") {
			api.mu.Lock()
			publishAttempts++
			api.mu.Unlock()
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
		writeJSON(w, http.StatusOK, map[string]any{"stdout": "", "stderr": "", "exitCode": 0, "durationMs": 1})
	}

	_, err := sandbox.Files().Write(context.Background(), "notes.txt", []byte("hello"), nil)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "network_error" {
		t.Fatalf("error = %v, want network_error", err)
	}
	if failure.Effect != EffectMaybeRan {
		t.Fatalf("effect = %s, want maybe_ran", failure.Effect)
	}
	api.mu.Lock()
	gotPublishAttempts := publishAttempts
	api.mu.Unlock()
	if gotPublishAttempts != 1 {
		t.Fatalf("publish attempts = %d, want 1", gotPublishAttempts)
	}
	commands, _ := api.execLog()
	if countCommands(commands, "mv -fT") != 1 {
		t.Fatalf("commands = %#v, want exactly one publish", commands)
	}
	if countCommands(commands, `rm -f -- "$p"`) != 1 {
		t.Fatalf("commands = %#v, want one staged discard", commands)
	}
}

func TestWriteStagedUploadRetriesDroppedConnection(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	api.execHandler = successfulExec()
	var attempts int
	api.uploadHandler = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
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
		writeJSON(w, http.StatusOK, map[string]any{"path": r.URL.Query().Get("path"), "size": len(body)})
	}

	if _, err := sandbox.Files().Write(context.Background(), "notes.txt", []byte("hello"), nil); err != nil {
		t.Fatalf("Write: %v", err)
	}
	paths, _, _ := api.uploadLog()
	if len(paths) != 2 || paths[0] != paths[1] {
		t.Fatalf("upload paths = %#v, want one staged name sent twice", paths)
	}
}

func TestWriteDirectUploadIsNotRetried(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	api.uploadHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusBadGateway, "sandbox_unreachable", "try again")
	}

	_, err := sandbox.Files().Write(context.Background(), "/tmp/outside.txt", []byte("hello"), nil)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "sandbox_unreachable" {
		t.Fatalf("error = %v, want sandbox_unreachable", err)
	}
	if failure.Effect != EffectMaybeRan {
		t.Fatalf("effect = %s, want maybe_ran", failure.Effect)
	}
	paths, _, _ := api.uploadLog()
	if len(paths) != 1 {
		t.Fatalf("uploads = %d, want 1: a lost-answer upload is not repeated", len(paths))
	}
}

func TestShellQuote(t *testing.T) {
	tests := map[string]string{
		"plain":    "plain",
		"a/b.txt":  "a/b.txt",
		"a b":      "'a b'",
		"it's":     `'it'"'"'s'`,
		"$(touch)": "'$(touch)'",
		"":         "''",
	}
	for value, want := range tests {
		if got := shellQuote(value); got != want {
			t.Fatalf("shellQuote(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestWritePublishTimeoutDiscardsStaging(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	api.execHandler = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(body, &request)
		if strings.Contains(request.Command, "mv -fT") {
			writeJSON(w, http.StatusOK, map[string]any{"stdout": "", "stderr": "", "exitCode": -1, "timedOut": true, "durationMs": 60000})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"stdout": "", "stderr": "", "exitCode": 0, "durationMs": 1})
	}

	_, err := sandbox.Files().Write(context.Background(), "notes.txt", []byte("hello"), nil)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "command_timeout" {
		t.Fatalf("error = %v, want command_timeout", err)
	}
	if failure.Effect != EffectMaybeRan {
		t.Fatalf("effect = %s, want maybe_ran", failure.Effect)
	}
	commands, _ := api.execLog()
	if countCommands(commands, `rm -f -- "$p"`) != 1 {
		t.Fatalf("commands = %#v, want one staged discard", commands)
	}
}

func TestWriteSymlinkFallsBackToUpload(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	api.execHandler = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(body, &request)
		if strings.Contains(request.Command, "mv -fT") {
			writeJSON(w, http.StatusOK, map[string]any{"stdout": "", "stderr": "", "exitCode": exitTargetIsLink, "durationMs": 1})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"stdout": "", "stderr": "", "exitCode": 0, "durationMs": 1})
	}

	info, err := sandbox.Files().Write(context.Background(), "link.txt", []byte("through"), &WriteOptions{Mode: fileMode(0o755)})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if info.Path != "/home/prized/work/link.txt" || info.Size != 7 {
		t.Fatalf("info = %#v", info)
	}
	paths, _, _ := api.uploadLog()
	if len(paths) != 2 || paths[1] != "/home/prized/work/link.txt" {
		t.Fatalf("upload paths = %#v", paths)
	}
	commands, _ := api.execLog()
	if countCommands(commands, "mv -fT") != 1 {
		t.Fatalf("commands = %#v", commands)
	}
	if countCommands(commands, `chmod 755 -- /home/prized/work/link.txt`) != 1 {
		t.Fatalf("commands = %#v, want the explicit mode", commands)
	}
}

func TestWriteTargetNotRegular(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	api.execHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"stdout": "", "stderr": "", "exitCode": exitTargetNotRegular, "durationMs": 1})
	}

	_, err := sandbox.Files().Write(context.Background(), "adir", []byte("x"), nil)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "unsupported_file_type" {
		t.Fatalf("error = %v, want unsupported_file_type", err)
	}
	if failure.Effect != EffectRan {
		t.Fatalf("effect = %s, want ran", failure.Effect)
	}
	paths, _, _ := api.uploadLog()
	if len(paths) != 1 {
		t.Fatalf("uploads = %d, want only the staged copy", len(paths))
	}
}

func TestReadUsesDownloadAndResolvesPaths(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	api.downloadHandler = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}

	text, err := sandbox.Files().Read(context.Background(), "notes.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if text != "hello" {
		t.Fatalf("text = %q", text)
	}
	if paths := api.downloadLog(); len(paths) != 1 || paths[0] != "/home/prized/work/notes.txt" {
		t.Fatalf("download paths = %#v", paths)
	}
}

func TestReadRefusesOversizeDownload(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	api.downloadHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(MaxFileBytes+1))
		w.WriteHeader(http.StatusOK)
	}

	_, err := sandbox.Files().ReadBytes(context.Background(), "big.bin")
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "file_too_large" {
		t.Fatalf("error = %v, want file_too_large", err)
	}
}

func TestReadRetriesServerError(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	var attempts int
	api.downloadHandler = func(w http.ResponseWriter, _ *http.Request) {
		api.mu.Lock()
		attempts++
		current := attempts
		api.mu.Unlock()
		if current == 1 {
			writeError(w, http.StatusInternalServerError, "internal_error", "a blip")
			return
		}
		_, _ = w.Write([]byte("ok"))
	}

	text, err := sandbox.Files().Read(context.Background(), "notes.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if text != "ok" {
		t.Fatalf("text = %q", text)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestReadMissingCarriesTypedError(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	api.downloadHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
	}

	_, err := sandbox.Files().Read(context.Background(), "missing.txt")
	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want *Error", err)
	}
	if failure.Code != "not_found" || failure.Status != http.StatusNotFound || failure.SandboxID != "sbx_1" {
		t.Fatalf("error = %#v", failure)
	}
}

func TestListParsesAndSortsEntries(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	records := [][]string{
		{"f", "12", "1763637000.5", "-rw-r--r--", "b.txt", "/home/prized/work/b.txt"},
		{"d", "4096", "1763637001", "drwxr-xr-x", "adir", "/home/prized/work/adir"},
		{"l", "7", "1763637002.25", "lrwxrwxrwx", "link", "/home/prized/work/link"},
	}
	var output strings.Builder
	for _, record := range records {
		output.WriteString(strings.Join(record, "\x00"))
		output.WriteString("\x00")
	}
	api.execHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"stdout": output.String(), "stderr": "", "exitCode": 0, "durationMs": 1})
	}

	entries, err := sandbox.Files().List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %#v", entries)
	}
	if entries[0].Name != "adir" || !entries[0].IsDir {
		t.Fatalf("first = %#v", entries[0])
	}
	if entries[1].Name != "b.txt" || entries[1].Size != 12 || entries[1].Mode != "-rw-r--r--" {
		t.Fatalf("second = %#v", entries[1])
	}
	if entries[1].Modified.Unix() != 1763637000 || entries[1].Modified.Nanosecond() != 500000000 {
		t.Fatalf("modified = %s", entries[1].Modified)
	}
	if entries[2].Name != "link" || !entries[2].IsSymlink {
		t.Fatalf("third = %#v", entries[2])
	}
	commands, _ := api.execLog()
	if len(commands) != 1 || !strings.Contains(commands[0], "find -H") || !strings.Contains(commands[0], "/home/prized/work") {
		t.Fatalf("commands = %#v", commands)
	}
}

func TestListAndExistsFailures(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	api.execHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"stdout": "", "stderr": "", "exitCode": exitNotFound, "durationMs": 1})
	}

	_, err := sandbox.Files().List(context.Background(), "missing")
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "not_found" {
		t.Fatalf("list error = %v, want not_found", err)
	}

	api.mu.Lock()
	api.execHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"stdout": "", "stderr": "", "exitCode": exitNotDirectory, "durationMs": 1})
	}
	api.mu.Unlock()

	_, err = sandbox.Files().List(context.Background(), "file")
	if !errors.As(err, &failure) || failure.Code != "not_directory" {
		t.Fatalf("list error = %v, want not_directory", err)
	}
}

func TestListRefusesTruncatedOutput(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	api.execHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"stdout": strings.Repeat("x", listOutputCap), "stderr": "", "exitCode": 0, "durationMs": 1,
		})
	}

	_, err := sandbox.Files().List(context.Background(), "")
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "command_failed" {
		t.Fatalf("error = %v, want command_failed", err)
	}
	if !strings.Contains(failure.Message, "too many entries") {
		t.Fatalf("message = %q", failure.Message)
	}
}

func TestExistsReportsBothAnswers(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	api.execHandler = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		code := 0
		if strings.Contains(string(body), "missing") {
			code = 1
		}
		writeJSON(w, http.StatusOK, map[string]any{"stdout": "", "stderr": "", "exitCode": code, "durationMs": 1})
	}

	present, err := sandbox.Files().Exists(context.Background(), "present.txt")
	if err != nil || !present {
		t.Fatalf("Exists(present) = %v, %v", present, err)
	}
	missing, err := sandbox.Files().Exists(context.Background(), "missing.txt")
	if err != nil || missing {
		t.Fatalf("Exists(missing) = %v, %v", missing, err)
	}
}

func TestRemoveScriptsAndFailures(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	api.execHandler = successfulExec()

	if err := sandbox.Files().Remove(context.Background(), "old.txt", nil); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	commands, _ := api.execLog()
	if !strings.Contains(commands[len(commands)-1], `rmdir -- "$p"`) {
		t.Fatalf("remove command = %q", commands[len(commands)-1])
	}
	if !strings.Contains(commands[len(commands)-1], `rm -f -- "$p"`) {
		t.Fatalf("remove does not fall back to rm -f: %q", commands[len(commands)-1])
	}

	if err := sandbox.Files().Remove(context.Background(), "tree", &RemoveOptions{Recursive: true}); err != nil {
		t.Fatalf("Remove recursive: %v", err)
	}
	commands, _ = api.execLog()
	if !strings.Contains(commands[len(commands)-1], `rm -rf -- "$p"`) {
		t.Fatalf("recursive remove command = %q", commands[len(commands)-1])
	}

	api.mu.Lock()
	api.execHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"stdout": "", "stderr": "", "exitCode": exitNotEmpty, "durationMs": 1})
	}
	api.mu.Unlock()
	err := sandbox.Files().Remove(context.Background(), "dir", nil)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "directory_not_empty" {
		t.Fatalf("error = %v, want directory_not_empty", err)
	}
}

func TestUploadAndDownloadLocalFiles(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	api.execHandler = successfulExec()
	local := filepath.Join(t.TempDir(), "in.txt")
	if err := os.WriteFile(local, []byte("from disk"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	info, err := sandbox.Files().Upload(context.Background(), local, "uploaded.txt", nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if info.Path != "/home/prized/work/uploaded.txt" || info.Size != 9 {
		t.Fatalf("info = %#v", info)
	}
	_, bodies, _ := api.uploadLog()
	if len(bodies) != 1 || string(bodies[0]) != "from disk" {
		t.Fatalf("bodies = %#v", bodies)
	}

	api.downloadHandler = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("to disk"))
	}
	target := filepath.Join(t.TempDir(), "deep", "nested", "out.bin")
	count, err := sandbox.Files().Download(context.Background(), "out.bin", target)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if count != 7 {
		t.Fatalf("count = %d, want 7", count)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(content) != "to disk" {
		t.Fatalf("content = %q", content)
	}
}

func TestUploadLocalErrorStaysLocal(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	_, err := sandbox.Files().Upload(context.Background(), filepath.Join(t.TempDir(), "nope"), "x", nil)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want os.ErrNotExist", err)
	}
	var failure *Error
	if errors.As(err, &failure) {
		t.Fatalf("local error became an API error: %#v", failure)
	}
	if uploads, _ := api.fileCounts(); uploads != 0 {
		t.Fatalf("uploads = %d, want 0", uploads)
	}
}

func TestClosedHandleFileCallsSendNothing(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	sandbox.Close()
	files := sandbox.Files()
	calls := map[string]error{}
	_, calls["write"] = files.Write(context.Background(), "x", []byte("x"), nil)
	_, calls["read"] = files.Read(context.Background(), "x")
	_, calls["read bytes"] = files.ReadBytes(context.Background(), "x")
	_, calls["list"] = files.List(context.Background(), "")
	_, calls["exists"] = files.Exists(context.Background(), "x")
	calls["remove"] = files.Remove(context.Background(), "x", nil)
	_, calls["upload"] = files.Upload(context.Background(), "x", "y", nil)
	_, calls["download"] = files.Download(context.Background(), "x", "y")

	for name, err := range calls {
		if !errors.Is(err, ErrHandleClosed) {
			t.Fatalf("%s error = %v, want ErrHandleClosed", name, err)
		}
	}
	uploads, downloads := api.fileCounts()
	_, _, execs, deletes := api.counts()
	if uploads != 0 || downloads != 0 || execs != 0 || deletes != 0 {
		t.Fatalf("closed handle sent requests: uploads %d, downloads %d, execs %d, deletes %d", uploads, downloads, execs, deletes)
	}
}

func TestRemoveLostAnswerIsNotResent(t *testing.T) {
	api, sandbox := newFilesFixture(t)
	var attempts int
	api.execHandler = func(w http.ResponseWriter, _ *http.Request) {
		api.mu.Lock()
		attempts++
		api.mu.Unlock()
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

	err := sandbox.Files().Remove(context.Background(), "old.txt", nil)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "network_error" {
		t.Fatalf("error = %v, want network_error", err)
	}
	if failure.Effect != EffectMaybeRan {
		t.Fatalf("effect = %s, want maybe_ran", failure.Effect)
	}
	api.mu.Lock()
	gotAttempts := attempts
	api.mu.Unlock()
	if gotAttempts != 1 {
		t.Fatalf("attempts = %d, want 1", gotAttempts)
	}
}
