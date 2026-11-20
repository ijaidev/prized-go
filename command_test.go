package prized

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func newCommandSandbox(t *testing.T, api *testAPI, options ...Option) *Sandbox {
	t.Helper()
	client := api.client(options...)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sandbox, err := client.Sandboxes().Create(ctx, CreateSandboxOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return sandbox
}

func TestExecArgvQuotesCwdEnvAndTimeout(t *testing.T) {
	api := newTestAPI(t)
	var mu sync.Mutex
	var body map[string]any
	api.execHandler = func(w http.ResponseWriter, r *http.Request) {
		var document map[string]any
		_ = json.NewDecoder(r.Body).Decode(&document)
		mu.Lock()
		body = document
		mu.Unlock()
		writeExecResult(w, "ok\n", "", 0, false, 12)
	}
	sandbox := newCommandSandbox(t, api)

	result, err := sandbox.ExecArgv(context.Background(), []string{
		"git", "commit", "-m", "it's done", "a b",
	}, &ExecOptions{
		Cwd:     "app",
		Env:     map[string]string{"CI": "1"},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("ExecArgv: %v", err)
	}
	if result.Stdout != "ok\n" || result.Duration != 12*time.Millisecond {
		t.Fatalf("result = %#v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	want := `git commit -m 'it'"'"'s done' 'a b'`
	if body["command"] != want {
		t.Fatalf("command = %q, want %q", body["command"], want)
	}
	if body["cwd"] != "/home/prized/work/app" {
		t.Fatalf("cwd = %v", body["cwd"])
	}
	env, _ := body["env"].(map[string]any)
	if env["CI"] != "1" {
		t.Fatalf("env = %#v", body["env"])
	}
	if body["timeout"] != float64(30) {
		t.Fatalf("timeout = %v", body["timeout"])
	}
}

func TestExecShellFormPassesThrough(t *testing.T) {
	api := newTestAPI(t)
	var command string
	api.execHandler = func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Command string `json:"command"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		command = body.Command
		writeExecResult(w, "", "", 0, false, 1)
	}
	sandbox := newCommandSandbox(t, api)
	if _, err := sandbox.Exec(context.Background(), `printf "a b" | wc -l`, nil); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if command != `printf "a b" | wc -l` {
		t.Fatalf("command = %q", command)
	}
}

func TestExecMarksTruncationAtStreamCap(t *testing.T) {
	api := newTestAPI(t)
	api.execHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeExecResult(w, strings.Repeat("a", maxOutputBytes), "", 0, false, 5)
	}
	sandbox := newCommandSandbox(t, api)
	result, err := sandbox.Exec(context.Background(), "yes", nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !result.Truncated || result.TruncatedReason != TruncationOutputLimit {
		t.Fatalf("truncated = %v/%q", result.Truncated, result.TruncatedReason)
	}
}

func TestExecTimeoutReturnsPartialResultAndTypedError(t *testing.T) {
	api := newTestAPI(t)
	api.execHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeExecResult(w, "partial\n", "still here\n", -1, true, 1005)
	}
	sandbox := newCommandSandbox(t, api)

	result, err := sandbox.Exec(context.Background(), "sleep 30", &ExecOptions{Timeout: time.Second})
	if result == nil {
		t.Fatal("result = nil, want the partial result")
	}
	if result.Stdout != "partial\n" || result.Stderr != "still here\n" || !result.TimedOut || result.ExitCode != -1 {
		t.Fatalf("result = %#v", result)
	}
	var timeout *CommandTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("error = %v, want *CommandTimeoutError", err)
	}
	if timeout.Result != result || timeout.Timeout != time.Second {
		t.Fatalf("timeout = %#v", timeout)
	}
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "command_timeout" || failure.Effect != EffectRan {
		t.Fatalf("typed error = %#v", failure)
	}
}

func TestExecFullOutputReadsFilesBack(t *testing.T) {
	api := newTestAPI(t)
	run := newFakeRun(t, api)
	execIDPattern := regexp.MustCompile(`exec_[0-9a-f]{16}`)
	run.capture = func(w http.ResponseWriter, body map[string]any) {
		env, _ := body["env"].(map[string]any)
		script, _ := env[outputScriptVar].(string)
		if env[outputCommandVar] != "make all" {
			t.Errorf("command env = %v", env[outputCommandVar])
		}
		execID := execIDPattern.FindString(script)
		if execID == "" {
			t.Errorf("capture script has no exec id")
			return
		}
		writeExecResult(w, "prized-output "+execID+" 5 3\n", "", 0, false, 25)
	}
	api.downloadHandler = func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Query().Get("path"), "/stdout"):
			_, _ = w.Write([]byte("hello"))
		case strings.HasSuffix(r.URL.Query().Get("path"), "/stderr"):
			_, _ = w.Write([]byte("err"))
		default:
			http.NotFound(w, r)
		}
	}
	sandbox := newCommandSandbox(t, api)

	result, err := sandbox.Exec(context.Background(), "make all", &ExecOptions{FullOutput: true})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "hello" || result.Stderr != "err" || result.ExitCode != 0 || result.Truncated {
		t.Fatalf("result = %#v", result)
	}
	run.mu.Lock()
	captures := run.captures
	run.mu.Unlock()
	if captures != 1 {
		t.Fatalf("captures = %d, want 1", captures)
	}
	if removed := run.removeCount(); removed != 1 {
		t.Fatalf("remove requests = %d, want 1 after a successful read", removed)
	}
}

func TestExecFullOutputTruncationReasons(t *testing.T) {
	tests := []struct {
		name   string
		marker string
		reason string
	}{
		{name: "output limit", marker: "0 0", reason: TruncationOutputLimit},
		{name: "write failed", marker: "2 2 1 0", reason: TruncationWriteFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newTestAPI(t)
			run := newFakeRun(t, api)
			execIDPattern := regexp.MustCompile(`exec_[0-9a-f]{16}`)
			run.capture = func(w http.ResponseWriter, body map[string]any) {
				env, _ := body["env"].(map[string]any)
				execID := execIDPattern.FindString(env[outputScriptVar].(string))
				size := test.marker
				if test.reason == TruncationOutputLimit {
					size = strconv.Itoa(maxFileBytes+1) + " 0"
				}
				writeExecResult(w, "prized-output "+execID+" "+size+"\n", "", 0, false, 1)
			}
			api.downloadHandler = func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Query().Get("path"), "/stdout") {
					_, _ = w.Write([]byte("hi"))
					return
				}
				_, _ = w.Write([]byte("er"))
			}
			sandbox := newCommandSandbox(t, api)
			result, err := sandbox.Exec(context.Background(), "make all", &ExecOptions{FullOutput: true})
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if !result.Truncated || result.TruncatedReason != test.reason {
				t.Fatalf("truncated = %v/%q, want %q", result.Truncated, result.TruncatedReason, test.reason)
			}
		})
	}
}

func TestExecFullOutputReadFailureIsDistinct(t *testing.T) {
	api := newTestAPI(t)
	run := newFakeRun(t, api)
	execIDPattern := regexp.MustCompile(`exec_[0-9a-f]{16}`)
	var execID string
	run.capture = func(w http.ResponseWriter, body map[string]any) {
		env, _ := body["env"].(map[string]any)
		execID = execIDPattern.FindString(env[outputScriptVar].(string))
		writeExecResult(w, "prized-output "+execID+" 5 0\n", "", 0, false, 2)
	}
	api.downloadHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
	}
	sandbox := newCommandSandbox(t, api)

	_, err := sandbox.Exec(context.Background(), "make all", &ExecOptions{FullOutput: true})
	var readError *OutputReadError
	if !errors.As(err, &readError) {
		t.Fatalf("error = %v, want *OutputReadError", err)
	}
	if readError.ExitCode != 0 || readError.StdoutPath != outputDir+"/"+execID+"/stdout" {
		t.Fatalf("read error = %#v", readError)
	}
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "output_read_failed" || failure.Reason != "not_found" {
		t.Fatalf("typed error = %#v", failure)
	}
	if removed := run.removeCount(); removed != 0 {
		t.Fatalf("remove requests = %d, want 0 (the files stay for the caller)", removed)
	}
}

func TestExecFullOutputSandboxDeletedRestatesCode(t *testing.T) {
	api := newTestAPI(t)
	run := newFakeRun(t, api)
	execIDPattern := regexp.MustCompile(`exec_[0-9a-f]{16}`)
	run.capture = func(w http.ResponseWriter, body map[string]any) {
		env, _ := body["env"].(map[string]any)
		execID := execIDPattern.FindString(env[outputScriptVar].(string))
		writeExecResult(w, "prized-output "+execID+" 5 0\n", "", 7, false, 2)
	}
	api.downloadHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusGone, "sandbox_deleted", "the sandbox was deleted")
	}
	sandbox := newCommandSandbox(t, api)

	_, err := sandbox.Exec(context.Background(), "make all", &ExecOptions{FullOutput: true})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "sandbox_deleted" {
		t.Fatalf("error = %v, want sandbox_deleted", err)
	}
	if failure.ExitCode == nil || *failure.ExitCode != 7 {
		t.Fatalf("exit code = %v, want 7", failure.ExitCode)
	}
	if !strings.Contains(failure.Message, "before its output could be read back") {
		t.Fatalf("message = %q", failure.Message)
	}
	if removed := run.removeCount(); removed != 0 {
		t.Fatalf("remove requests = %d, want 0 (the output went with the sandbox)", removed)
	}
}

func TestExecFullOutputCancellationRemovesAndRethrows(t *testing.T) {
	api := newTestAPI(t)
	run := newFakeRun(t, api)
	execIDPattern := regexp.MustCompile(`exec_[0-9a-f]{16}`)
	run.capture = func(w http.ResponseWriter, body map[string]any) {
		env, _ := body["env"].(map[string]any)
		execID := execIDPattern.FindString(env[outputScriptVar].(string))
		writeExecResult(w, "prized-output "+execID+" 5 0\n", "", 0, false, 2)
	}
	downloadStarted := make(chan struct{})
	api.downloadHandler = func(_ http.ResponseWriter, r *http.Request) {
		close(downloadStarted)
		<-r.Context().Done()
	}
	sandbox := newCommandSandbox(t, api)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := sandbox.Exec(ctx, "make all", &ExecOptions{FullOutput: true})
		done <- err
	}()
	select {
	case <-downloadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the download did not start")
	}
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	var readError *OutputReadError
	if errors.As(err, &readError) {
		t.Fatalf("error = %v, want the cancellation, not an output-read error", err)
	}
	if removed := run.removeCount(); removed == 0 {
		t.Fatal("the canceled run's files were not removed")
	}
}

func TestExecReplacementCharsDoNotMistakeCap(t *testing.T) {
	api := newTestAPI(t)
	api.execHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeExecResult(w, strings.Repeat("\uFFFD", 2*1024*1024), "", 0, false, 1)
	}
	sandbox := newCommandSandbox(t, api)
	result, err := sandbox.Exec(context.Background(), "binary", nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Truncated || result.TruncatedReason != "" {
		t.Fatalf("truncated = %v/%q, want not truncated", result.Truncated, result.TruncatedReason)
	}
}

type channelWriter struct {
	chunks chan string
}

func (w *channelWriter) Write(data []byte) (int, error) {
	w.chunks <- string(data)
	return len(data), nil
}

func TestStreamedExecDeliversChunksLive(t *testing.T) {
	api := newTestAPI(t)
	run := newFakeRun(t, api)
	run.appendStdout("first\n")
	release := make(chan struct{})
	run.onPoll = func(n int, run *fakeRun) {
		if n == 2 {
			select {
			case <-release:
			case <-time.After(2 * time.Second):
				t.Error("the first chunk was not delivered before the command ended")
			}
			run.appendStdout("second\n")
			run.setState("done", 0)
		}
	}
	sandbox := newCommandSandbox(t, api)
	chunks := &channelWriter{chunks: make(chan string, 8)}
	type outcome struct {
		result *ExecResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := sandbox.Exec(context.Background(), "loop", &ExecOptions{Stdout: chunks})
		done <- outcome{result: result, err: err}
	}()

	select {
	case chunk := <-chunks.chunks:
		if chunk != "first\n" {
			t.Fatalf("first chunk = %q", chunk)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no chunk arrived while the command was still running")
	}
	close(release)

	select {
	case finished := <-done:
		if finished.err != nil {
			t.Fatalf("Exec: %v", finished.err)
		}
		if finished.result.Stdout != "first\nsecond\n" {
			t.Fatalf("stdout = %q", finished.result.Stdout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Exec did not finish")
	}
}

func TestStreamedExecRidesOutPollOutage(t *testing.T) {
	api := newTestAPI(t)
	run := newFakeRun(t, api)
	run.appendStdout("survived\n")
	run.setState("done", 0)
	run.failPolls(http.StatusServiceUnavailable, "sandbox_unreachable", 1)
	sandbox := newCommandSandbox(t, api, WithMaxRetries(0), WithPollOutage(300*time.Millisecond))

	result, err := sandbox.Exec(context.Background(), "build", &ExecOptions{Stdout: io.Discard})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "survived\n" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
	startCalls, polls, _, _, _ := run.snapshot()
	if startCalls != 1 || polls < 2 {
		t.Fatalf("startCalls/polls = %d/%d", startCalls, polls)
	}
}

func TestStreamedExecCancellationReportsStop(t *testing.T) {
	tests := []struct {
		name         string
		killStatus   int
		killExit     int
		wantStopped  bool
		wantSentence string
	}{
		{name: "confirmed", killStatus: 0, killExit: 0, wantStopped: true, wantSentence: "no longer running"},
		{name: "unconfirmed", killStatus: http.StatusServiceUnavailable, wantStopped: false, wantSentence: "may still be running"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newTestAPI(t)
			run := newFakeRun(t, api)
			run.killStatus = test.killStatus
			run.killExit = test.killExit
			polled := make(chan struct{})
			var once sync.Once
			run.onPoll = func(int, *fakeRun) {
				once.Do(func() { close(polled) })
			}
			sandbox := newCommandSandbox(t, api, WithMaxRetries(0))
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				<-polled
				cancel()
			}()

			_, err := sandbox.Exec(ctx, "sleep 30", &ExecOptions{Stdout: io.Discard})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			var commandErr *CommandError
			if !errors.As(err, &commandErr) {
				t.Fatalf("error = %v, want *CommandError", err)
			}
			if commandErr.Stopped != test.wantStopped || commandErr.MayStillRun() == test.wantStopped {
				t.Fatalf("stopped = %v, mayStillRun = %v", commandErr.Stopped, commandErr.MayStillRun())
			}
			if !strings.Contains(commandErr.Error(), test.wantSentence) {
				t.Fatalf("error = %q, want %q", commandErr.Error(), test.wantSentence)
			}
			_, _, _, _, kills := run.snapshot()
			if len(kills) != 1 || kills[0] != "KILL" {
				t.Fatalf("kills = %#v", kills)
			}
		})
	}
}

func TestStartReturnsBackgroundHandle(t *testing.T) {
	api := newTestAPI(t)
	run := newFakeRun(t, api)
	run.appendStdout("out\n")
	run.appendStderr("err\n")
	run.duration = 150
	sandbox := newCommandSandbox(t, api)

	handle, err := sandbox.Start(context.Background(), "python3 -m http.server 8000", &StartOptions{
		Cwd:     "app",
		Env:     map[string]string{"PORT": "8000"},
		Timeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.HasPrefix(handle.ID(), "run_") || len(handle.ID()) != len("run_")+16 {
		t.Fatalf("id = %q", handle.ID())
	}
	if handle.PID() != 4242 || handle.SandboxID() != "sbx_1" {
		t.Fatalf("handle = %d/%s", handle.PID(), handle.SandboxID())
	}

	run.mu.Lock()
	body := run.startBody
	run.mu.Unlock()
	command, _ := body["command"].(string)
	if !strings.Contains(command, "setsid") || !strings.Contains(command, `PYTHONUNBUFFERED="${PYTHONUNBUFFERED-1}"`) {
		t.Fatalf("start script is not detached or unbuffered: %.200q", command)
	}
	if body["cwd"] != "/home/prized/work/app" {
		t.Fatalf("start cwd = %v", body["cwd"])
	}
	env, _ := body["env"].(map[string]any)
	if env["PORT"] != "8000" {
		t.Fatalf("start env = %#v", body["env"])
	}
	if body["timeout"] != float64(60) {
		t.Fatalf("start timeout = %v", body["timeout"])
	}

	status, err := handle.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Running || status.ExitCode != nil {
		t.Fatalf("status = %#v", status)
	}

	logs, err := handle.Logs(context.Background())
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if logs.Stdout != "out\n" || logs.Stderr != "err\n" {
		t.Fatalf("logs = %#v", logs)
	}

	run.setState("done", 3)
	result, err := handle.Wait(context.Background(), nil)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.ExitCode != 3 || result.Stdout != "out\n" || result.Stderr != "err\n" || result.Duration != 150*time.Millisecond {
		t.Fatalf("result = %#v", result)
	}

	run.mu.Lock()
	run.killExit = 4
	run.mu.Unlock()
	killed, err := handle.Kill(context.Background(), "")
	if err != nil || killed {
		t.Fatalf("Kill = %v/%v, want false/nil", killed, err)
	}
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	_, _, cleanups, _, _ := run.snapshot()
	if cleanups == 0 {
		t.Fatal("cleanup was not sent")
	}
}

func TestStreamedExecTimeoutReturnsPartialAndStops(t *testing.T) {
	api := newTestAPI(t)
	run := newFakeRun(t, api)
	run.appendStdout("partial\n")
	run.setTimedOut(-1)
	sandbox := newCommandSandbox(t, api)

	result, err := sandbox.Exec(context.Background(), "sleep 30", &ExecOptions{
		Timeout: time.Second,
		Stdout:  io.Discard,
	})
	if result == nil || result.Stdout != "partial\n" || !result.TimedOut || result.ExitCode != -1 {
		t.Fatalf("result = %#v", result)
	}
	var timeout *CommandTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("error = %v, want *CommandTimeoutError", err)
	}
	_, _, cleanups, _, _ := run.snapshot()
	if cleanups == 0 {
		t.Fatal("the timed-out run's files were not cleaned up")
	}
}

func TestBackgroundCommandTimeoutReturnsPartialResult(t *testing.T) {
	api := newTestAPI(t)
	run := newFakeRun(t, api)
	run.appendStdout("partial\n")
	run.setTimedOut(-1)
	sandbox := newCommandSandbox(t, api)

	handle, err := sandbox.Start(context.Background(), "sleep 30", &StartOptions{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := handle.Wait(context.Background(), nil)
	if result == nil || result.Stdout != "partial\n" || !result.TimedOut || result.ExitCode != -1 {
		t.Fatalf("result = %#v", result)
	}
	var timeout *CommandTimeoutError
	if !errors.As(err, &timeout) || timeout.Result != result {
		t.Fatalf("error = %v, want *CommandTimeoutError", err)
	}
}

func TestHandleWaitTimeoutKeepsRunning(t *testing.T) {
	api := newTestAPI(t)
	run := newFakeRun(t, api)
	sandbox := newCommandSandbox(t, api)
	handle, err := sandbox.Start(context.Background(), "sleep 30", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, err = handle.Wait(context.Background(), &WaitOptions{Timeout: 20 * time.Millisecond})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "timeout" || !strings.Contains(failure.Message, "still running") {
		t.Fatalf("error = %v, want a wait timeout", err)
	}
	_, _, _, _, kills := run.snapshot()
	if len(kills) != 0 {
		t.Fatalf("wait sent a stop: %#v", kills)
	}

	run.setState("done", 0)
	if _, err := handle.Wait(context.Background(), nil); err != nil {
		t.Fatalf("second Wait: %v", err)
	}
}

func TestStartFailureAbandonsRun(t *testing.T) {
	for _, abandonExit := range []int{0, 3} {
		t.Run(map[int]string{0: "confirmed", 3: "unconfirmed"}[abandonExit], func(t *testing.T) {
			api := newTestAPI(t)
			run := newFakeRun(t, api)
			run.startStdout = ""
			run.startStderr = "the command did not start"
			run.startExit = noPIDExit
			run.abandonExit = abandonExit
			sandbox := newCommandSandbox(t, api)

			_, err := sandbox.Start(context.Background(), "sleep 30", nil)
			var commandErr *CommandError
			if !errors.As(err, &commandErr) {
				t.Fatalf("error = %v, want *CommandError", err)
			}
			if commandErr.Stopped != (abandonExit == 0) || !strings.HasPrefix(commandErr.RunID, "run_") {
				t.Fatalf("command error = %#v", commandErr)
			}
			startCalls, _, _, abandons, _ := run.snapshot()
			if startCalls != 1 || abandons != 1 {
				t.Fatalf("startCalls/abandons = %d/%d", startCalls, abandons)
			}
		})
	}
}

func TestHandleWaitCancellationStopsPollingOnly(t *testing.T) {
	api := newTestAPI(t)
	run := newFakeRun(t, api)
	polled := make(chan struct{})
	var once sync.Once
	run.onPoll = func(int, *fakeRun) {
		once.Do(func() { close(polled) })
	}
	sandbox := newCommandSandbox(t, api)
	handle, err := sandbox.Start(context.Background(), "sleep 30", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-polled
		cancel()
	}()
	_, err = handle.Wait(ctx, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	_, _, _, _, kills := run.snapshot()
	if len(kills) != 0 {
		t.Fatalf("wait cancellation sent a stop: %#v", kills)
	}
}

func TestKillSignals(t *testing.T) {
	api := newTestAPI(t)
	run := newFakeRun(t, api)
	sandbox := newCommandSandbox(t, api)
	handle, err := sandbox.Start(context.Background(), "sleep 30", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Kill(context.Background(), "SIGBOGUS"); err == nil {
		t.Fatal("Kill(SIGBOGUS) succeeded")
	}
	_, _, _, _, kills := run.snapshot()
	if len(kills) != 0 {
		t.Fatalf("invalid signal sent: %#v", kills)
	}
	ok, err := handle.Kill(context.Background(), "")
	if err != nil || !ok {
		t.Fatalf("Kill default = %v/%v", ok, err)
	}
	ok, err = handle.Kill(context.Background(), "SIGKILL")
	if err != nil || !ok {
		t.Fatalf("Kill SIGKILL = %v/%v", ok, err)
	}
	_, _, _, _, kills = run.snapshot()
	if len(kills) != 2 || kills[0] != "TERM" || kills[1] != "KILL" {
		t.Fatalf("kills = %#v", kills)
	}
}

func TestLongTimeoutRunsDetached(t *testing.T) {
	api := newTestAPI(t)
	run := newFakeRun(t, api)
	run.appendStdout("long\n")
	run.setState("done", 0)
	sandbox := newCommandSandbox(t, api)

	result, err := sandbox.Exec(context.Background(), "build", &ExecOptions{Timeout: 5 * time.Minute})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "long\n" {
		t.Fatalf("result = %#v", result)
	}
	startCalls, polls, _, _, _ := run.snapshot()
	if startCalls != 1 || polls < 1 {
		t.Fatalf("startCalls/polls = %d/%d", startCalls, polls)
	}
}
