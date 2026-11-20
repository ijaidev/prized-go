package prized

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeRun answers the command endpoint's scripts for one detached run. The
// scripts themselves never execute; the fake keeps the run's files as bytes
// and serves the polls from them.
type fakeRun struct {
	api *testAPI

	mu sync.Mutex

	stdout   []byte
	stderr   []byte
	state    string // running, done, gone, missing
	exitCode int
	timedOut bool
	duration int

	startStdout string
	startStderr string
	startExit   int
	startStatus int
	startCode   string
	startBody   map[string]any
	startCalls  int

	polls        int
	pollStatus   int
	pollCode     string
	pollFailures int
	onPoll       func(n int, run *fakeRun)

	killExit   int
	killStatus int
	killCalls  []string

	cleanupExit  int
	cleanupCalls int

	abandonExit  int
	abandonCalls int

	captures int
	capture  func(w http.ResponseWriter, body map[string]any)
}

func newFakeRun(t *testing.T, api *testAPI) *fakeRun {
	t.Helper()
	run := &fakeRun{
		api:         api,
		state:       "running",
		startStdout: "4242\n",
	}
	api.execHandler = run.handle
	return run
}

func (r *fakeRun) handle(w http.ResponseWriter, request *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(request.Body).Decode(&body)
	command, _ := body["command"].(string)
	switch {
	case strings.Contains(command, "command -v setsid"):
		r.handleStart(w, body)
	case strings.Contains(command, "prized-run $__pz_st"):
		r.handlePoll(w, command)
	case strings.HasPrefix(command, "__pz_d=") && strings.Contains(command, "cleaned"):
		r.handleAbandon(w)
	case strings.HasPrefix(command, "__pz_p="):
		r.handleKill(w, command)
	case strings.HasPrefix(command, "if [ ! -f") && strings.Contains(command, "cleaned"):
		r.handleCleanup(w)
	case strings.HasPrefix(command, "d=") && strings.Contains(command, "prized-output"):
		r.handleCollect(w)
	case strings.Contains(command, "exec_*") && strings.Contains(command, "rm -rf"):
		writeExecResult(w, "", "", 0, false, 0)
	case command == outputRunScript:
		r.handleCapture(w, body)
	default:
		r.api.t.Errorf("unexpected exec command: %.120q", command)
		http.Error(w, "unexpected command", http.StatusInternalServerError)
	}
}

func (r *fakeRun) handleStart(w http.ResponseWriter, body map[string]any) {
	r.mu.Lock()
	r.startCalls++
	r.startBody = body
	status := r.startStatus
	code := r.startCode
	stdout := r.startStdout
	stderr := r.startStderr
	exitCode := r.startExit
	r.mu.Unlock()
	if status != 0 {
		if code == "" {
			code = "sandbox_unreachable"
		}
		writeError(w, status, code, "temporary")
		return
	}
	writeExecResult(w, stdout, stderr, exitCode, false, 0)
}

var stdoutTailPattern = regexp.MustCompile(`tail -c \+([0-9]+) "\$__pz_d/stdout"`)
var stderrTailPattern = regexp.MustCompile(`tail -c \+([0-9]+) "\$__pz_d/stderr"`)

func (r *fakeRun) handlePoll(w http.ResponseWriter, command string) {
	r.mu.Lock()
	r.polls++
	polls := r.polls
	hook := r.onPoll
	r.mu.Unlock()
	if hook != nil {
		hook(polls, r)
	}

	r.mu.Lock()
	status := r.pollStatus
	code := r.pollCode
	if r.pollFailures > 0 {
		r.pollFailures--
		r.mu.Unlock()
		if code == "" {
			code = "sandbox_unreachable"
		}
		writeError(w, status, code, "temporary")
		return
	}
	state := r.state
	exitCode := r.exitCode
	timedOut := r.timedOut
	duration := r.duration
	stdout := append([]byte(nil), r.stdout...)
	stderr := append([]byte(nil), r.stderr...)
	r.mu.Unlock()

	cout := ""
	cerr := ""
	if state == "missing" {
		writeExecResult(w, "prized-run missing - 0 0 0 -", "", 0, false, 0)
		return
	}
	outOffset := pollOffset(command, stdoutTailPattern)
	errOffset := pollOffset(command, stderrTailPattern)
	cout = base64.StdEncoding.EncodeToString(sliceFrom(stdout, outOffset, pollChunkBytes))
	cerr = base64.StdEncoding.EncodeToString(sliceFrom(stderr, errOffset, pollChunkBytes))
	exitText := "-"
	if state == "done" {
		exitText = strconv.Itoa(exitCode)
	}
	durationText := "-"
	if duration > 0 && state != "running" {
		durationText = strconv.Itoa(duration)
	}
	timedOutText := "0"
	if timedOut {
		timedOutText = "1"
	}
	header := fmt.Sprintf("prized-run %s %s %d %d %s %s", state, exitText, len(stdout), len(stderr), timedOutText, durationText)
	writeExecResult(w, header+"\n"+cout+"\n"+cerr+"\n", "", 0, false, 0)
}

func pollOffset(command string, pattern *regexp.Regexp) int {
	match := pattern.FindStringSubmatch(command)
	if match == nil {
		return 1
	}
	offset, _ := strconv.Atoi(match[1])
	if offset < 1 {
		return 1
	}
	return offset
}

func sliceFrom(data []byte, offset, limit int) []byte {
	start := offset - 1
	if start >= len(data) {
		return nil
	}
	data = data[start:]
	if len(data) > limit {
		data = data[:limit]
	}
	return data
}

func (r *fakeRun) handleKill(w http.ResponseWriter, command string) {
	signal := "TERM"
	if match := regexp.MustCompile(`kill -s ([A-Z]+) --`).FindStringSubmatch(command); match != nil {
		signal = match[1]
	}
	r.mu.Lock()
	r.killCalls = append(r.killCalls, signal)
	status := r.killStatus
	exitCode := r.killExit
	r.mu.Unlock()
	if status != 0 {
		writeError(w, status, "sandbox_unreachable", "temporary")
		return
	}
	writeExecResult(w, "", "", exitCode, false, 0)
}

func (r *fakeRun) handleCleanup(w http.ResponseWriter) {
	r.mu.Lock()
	r.cleanupCalls++
	exitCode := r.cleanupExit
	r.mu.Unlock()
	writeExecResult(w, "", "", exitCode, false, 0)
}

func (r *fakeRun) handleAbandon(w http.ResponseWriter) {
	r.mu.Lock()
	r.abandonCalls++
	exitCode := r.abandonExit
	r.mu.Unlock()
	writeExecResult(w, "", "", exitCode, false, 0)
}

func (r *fakeRun) handleCollect(w http.ResponseWriter) {
	writeExecResult(w, "", "", 0, false, 0)
}

func (r *fakeRun) handleCapture(w http.ResponseWriter, body map[string]any) {
	r.mu.Lock()
	r.captures++
	capture := r.capture
	r.mu.Unlock()
	if capture == nil {
		writeExecResult(w, "", "", 0, false, 0)
		return
	}
	capture(w, body)
}

func (r *fakeRun) setState(state string, exitCode int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = state
	r.exitCode = exitCode
}

func (r *fakeRun) setTimedOut(exitCode int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = "done"
	r.exitCode = exitCode
	r.timedOut = true
}

func (r *fakeRun) appendStdout(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stdout = append(r.stdout, text...)
}

func (r *fakeRun) appendStderr(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stderr = append(r.stderr, text...)
}

func (r *fakeRun) failPolls(status int, code string, count int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pollStatus = status
	r.pollCode = code
	r.pollFailures = count
}

func (r *fakeRun) snapshot() (startCalls, polls, cleanupCalls, abandonCalls int, kills []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startCalls, r.polls, r.cleanupCalls, r.abandonCalls, append([]string(nil), r.killCalls...)
}

func writeExecResult(w http.ResponseWriter, stdout, stderr string, exitCode int, timedOut bool, durationMS float64) {
	document := map[string]any{
		"stdout":   stdout,
		"stderr":   stderr,
		"exitCode": exitCode,
	}
	if timedOut {
		document["timedOut"] = true
	}
	if durationMS > 0 {
		document["durationMs"] = durationMS
	}
	writeJSON(w, http.StatusOK, document)
}
