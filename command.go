package prized

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	workDir = "/home/prized/work"
	runsDir = "/home/prized/.prized/runs"

	// NoTimeout runs a command with no time limit.
	NoTimeout time.Duration = -1

	maxBlockingExec = 240 * time.Second
	execAnswerSlack = 30 * time.Second
	scriptTimeout   = 60 * time.Second

	pollChunkBytes = 1024 * 1024
	pollMinDelay   = 200 * time.Millisecond
	pollMaxDelay   = time.Second

	// maxRetainedBytes is what a detached command's result keeps of each
	// stream; its files keep the rest until Cleanup.
	maxRetainedBytes = 64 * 1024 * 1024

	teardownTimeout = 10 * time.Second
	abandonTimeout  = 20 * time.Second
	cleanupGrace    = 3 * time.Second

	// TruncationOutputLimit marks a result cut at its stream cap.
	TruncationOutputLimit = "output_limit"
	// TruncationWriteFailed marks a full-output result whose file could not be
	// saved whole, most often because the sandbox's disk was full.
	TruncationWriteFailed = "write_failed"
)

// ExecOptions describes one foreground command.
type ExecOptions struct {
	// Cwd is the working directory; empty uses /home/prized/work. Relative
	// paths resolve against it.
	Cwd string
	// Env adds variables for this command.
	Env map[string]string
	// Timeout bounds the command; zero uses DefaultExecTimeout and NoTimeout
	// runs it with no limit. A timeout over 240s, or no limit, runs the
	// command detached and polls it.
	Timeout time.Duration
	// FullOutput keeps up to 100 MiB of each stream instead of the first
	// 4 MiB. It applies to a blocking command; a streamed or long command
	// writes every chunk to the writers, and its result keeps the first
	// 64 MiB of each stream.
	FullOutput bool
	// Stdout and Stderr receive output as it arrives. Setting either streams
	// the command: it runs detached and is polled.
	Stdout io.Writer
	Stderr io.Writer
}

// ExecResult is one finished command.
type ExecResult struct {
	// Stdout and Stderr are decoded UTF-8 output.
	Stdout string
	Stderr string
	// ExitCode is the command's exit code; -1 when it timed out or ended
	// without reporting one.
	ExitCode int
	// Duration is how long it ran, as measured in the sandbox when known.
	Duration time.Duration
	// TimedOut is true when the command hit its timeout and was killed.
	TimedOut bool
	// Truncated is true when a stream was cut at its cap.
	Truncated bool
	// TruncatedReason is output_limit or write_failed when Truncated is true,
	// and empty otherwise. write_failed comes from FullOutput only.
	TruncatedReason string
}

// StartOptions describes one background command.
type StartOptions struct {
	// Cwd is the working directory; empty uses /home/prized/work.
	Cwd string
	// Env adds variables for this command.
	Env map[string]string
	// Timeout bounds the command in the sandbox; zero or NoTimeout means no
	// limit.
	Timeout time.Duration
}

// WaitOptions describes one handle wait.
type WaitOptions struct {
	// Timeout bounds the wait only; the command keeps running when it runs
	// out. Zero waits without a bound.
	Timeout time.Duration
	// Stdout and Stderr receive every chunk as it arrives, replaying from the
	// start, then live.
	Stdout io.Writer
	Stderr io.Writer
}

// CommandStatus is what one status read knows.
type CommandStatus struct {
	// Running is true while the command is still running.
	Running bool
	// ExitCode is the code once it is not running; -1 when it ended without
	// reporting one or its time limit stopped it.
	ExitCode *int
}

// CommandLogs is what a command has printed so far.
type CommandLogs struct {
	Stdout    string
	Stderr    string
	Truncated bool
}

// CommandHandle is a command running in the background. It runs in its own
// session: it keeps running after the caller exits, and while it runs, Kill
// reaches every process it started in its process group.
type CommandHandle struct {
	sandbox   *Sandbox
	id        string
	pid       int
	startedAt time.Time
	timeout   time.Duration
}

// Exec runs one foreground command and returns its output. A non-zero exit
// code is a result, not an error.
func (s *Sandbox) Exec(ctx context.Context, command string, options *ExecOptions) (*ExecResult, error) {
	return s.execCommand(ctx, command, nil, options)
}

// ExecArgv runs one foreground command from arguments the SDK quotes for
// you, with the same options and results as Exec.
func (s *Sandbox) ExecArgv(ctx context.Context, argv []string, options *ExecOptions) (*ExecResult, error) {
	return s.execCommand(ctx, "", argv, options)
}

// Start starts a command in its own session and returns once it has started.
// The command keeps running after the caller exits; use the handle to read it,
// wait for it, or stop it.
func (s *Sandbox) Start(ctx context.Context, command string, options *StartOptions) (*CommandHandle, error) {
	return s.startCommand(ctx, command, nil, options)
}

// StartArgv starts a background command from arguments the SDK quotes for
// you, with the same options as Start.
func (s *Sandbox) StartArgv(ctx context.Context, argv []string, options *StartOptions) (*CommandHandle, error) {
	return s.startCommand(ctx, "", argv, options)
}

func (s *Sandbox) execCommand(ctx context.Context, command string, argv []string, options *ExecOptions) (*ExecResult, error) {
	if s.isClosed() {
		return nil, closedError()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	text, err := commandText(command, argv)
	if err != nil {
		return nil, err
	}

	var limit time.Duration
	noLimit := false
	if options == nil {
		limit = DefaultExecTimeout
	} else {
		switch {
		case options.Timeout == 0:
			limit = DefaultExecTimeout
		case options.Timeout == NoTimeout:
			noLimit = true
		case options.Timeout < 0:
			return nil, newError("invalid_argument", "timeout must be greater than zero, NoTimeout, or zero for the default")
		default:
			limit = options.Timeout
		}
	}

	cwd, err := resolveCommandCwd(optionsCwd(options))
	if err != nil {
		return nil, err
	}
	var env map[string]string
	var stdout, stderr io.Writer
	fullOutput := false
	if options != nil {
		env = options.Env
		stdout = options.Stdout
		stderr = options.Stderr
		fullOutput = options.FullOutput
	}

	if stdout != nil || stderr != nil || noLimit || limit > maxBlockingExec {
		startLimit := limit
		if noLimit {
			startLimit = 0
		}
		handle, err := s.startCommand(ctx, text, nil, &StartOptions{Cwd: cwd, Env: env, Timeout: startLimit})
		if err != nil {
			return nil, err
		}
		result, ended, err := handle.await(ctx, stdout, stderr, 0)
		if err != nil {
			return nil, s.failedRun(handle, err, ended)
		}
		handle.cleanupBestEffort()
		if result.TimedOut {
			return result, commandTimeoutError(result, startLimit, s.ID())
		}
		return result, nil
	}

	if fullOutput {
		result, err := s.execFullOutput(ctx, text, cwd, env, limit)
		if err != nil {
			return nil, err
		}
		if result.TimedOut {
			return result, commandTimeoutError(result, limit, s.ID())
		}
		return result, nil
	}

	body := execRequestBody(text, cwd, env, limit)
	result, err := s.executeBody(ctx, body, requestExec, limit+execAnswerSlack)
	if err != nil {
		return nil, err
	}
	if result.TimedOut {
		return result, commandTimeoutError(result, limit, s.ID())
	}
	return result, nil
}

func (s *Sandbox) startCommand(ctx context.Context, command string, argv []string, options *StartOptions) (*CommandHandle, error) {
	if s.isClosed() {
		return nil, closedError()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	text, err := commandText(command, argv)
	if err != nil {
		return nil, err
	}
	var limit time.Duration
	cwd := ""
	var env map[string]string
	if options != nil {
		limit = options.Timeout
		cwd = options.Cwd
		env = options.Env
	}
	if limit == NoTimeout {
		limit = 0
	}
	if limit < 0 {
		return nil, newError("invalid_argument", "timeout must be zero or greater, or NoTimeout")
	}
	cwd, err = resolveCommandCwd(cwd)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, contextError(err, s.ID(), EffectNotRun)
	}

	runID := newRunID()
	startedAt := time.Now()
	body := execRequestBody(startScript(runID, text, limit, true), cwd, env, scriptTimeout)
	outage := &runOutage{window: s.client.pollOutage}
	acted := false

	for {
		if err := ctx.Err(); err != nil {
			stopped := s.abandonRun(runID)
			failure := contextError(err, s.ID(), EffectNotRun)
			if acted {
				failure.Effect = EffectMaybeRan
				return nil, commandFailure(failure, runID, stopped)
			}
			return nil, failure
		}
		sentAt := time.Now()
		result, err := s.executeBody(ctx, body, requestSafe, scriptTimeout+execAnswerSlack)
		if err != nil {
			var failure *Error
			if !errors.As(err, &failure) {
				return nil, err
			}
			acted = acted || failure.Effect != EffectNotRun
			pause, giveUpWith, ok := outage.fail(failure, sentAt, time.Now(), false)
			if ok {
				if sleepErr := sleepContext(ctx, pause); sleepErr != nil {
					stopped := s.abandonRun(runID)
					cut := contextError(sleepErr, s.ID(), EffectNotRun)
					if acted {
						cut.Effect = EffectMaybeRan
						return nil, commandFailure(cut, runID, stopped)
					}
					return nil, cut
				}
				continue
			}
			stopped := s.abandonRun(runID)
			if acted {
				return nil, commandFailure(giveUpWith, runID, stopped)
			}
			return nil, giveUpWith
		}

		pid, parseErr := parseStart(result, s.ID())
		if parseErr == nil {
			return &CommandHandle{
				sandbox:   s,
				id:        runID,
				pid:       pid,
				startedAt: startedAt,
				timeout:   limit,
			}, nil
		}
		stopped := s.abandonRun(runID)
		if result.ExitCode == noPIDExit {
			return nil, commandFailure(parseErr, runID, stopped)
		}
		return nil, parseErr
	}
}

// ID returns the run's id.
func (h *CommandHandle) ID() string {
	if h == nil {
		return ""
	}
	return h.id
}

// PID returns the command's process group id inside the sandbox.
func (h *CommandHandle) PID() int {
	if h == nil {
		return 0
	}
	return h.pid
}

// SandboxID returns the sandbox the run belongs to.
func (h *CommandHandle) SandboxID() string {
	if h == nil || h.sandbox == nil {
		return ""
	}
	return h.sandbox.ID()
}

// Status reports whether the command is still running, and its exit code once
// it is not.
func (h *CommandHandle) Status(ctx context.Context) (*CommandStatus, error) {
	if err := h.checkOpen(); err != nil {
		return nil, err
	}
	report, err := h.poll(ctx, 0, 0, 0)
	if err != nil {
		return nil, err
	}
	if report.state == "running" {
		return &CommandStatus{Running: true}, nil
	}
	code := report.endedCode()
	return &CommandStatus{ExitCode: &code}, nil
}

// Logs returns what the command has printed so far: up to the first 64 MiB of
// each stream (Truncated past that). Its files keep all of it until Cleanup.
func (h *CommandHandle) Logs(ctx context.Context) (*CommandLogs, error) {
	if err := h.checkOpen(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	out := newStreamReader(nil, maxRetainedBytes)
	errOut := newStreamReader(nil, maxRetainedBytes)
	var targets *[2]int
	for {
		report, err := h.poll(ctx, out.offset, errOut.offset, pollChunkBytes)
		if err != nil {
			return nil, err
		}
		if targets == nil {
			targets = &[2]int{report.outSize, report.errSize}
		}
		if !out.truncated {
			_ = out.push(clipBytes(report.stdout, targets[0]-out.offset))
		}
		if !errOut.truncated {
			_ = errOut.push(clipBytes(report.stderr, targets[1]-errOut.offset))
		}
		outDone := out.truncated || out.offset >= targets[0] || len(report.stdout) == 0
		errDone := errOut.truncated || errOut.offset >= targets[1] || len(report.stderr) == 0
		if outDone && errDone {
			out.finish()
			errOut.finish()
			return &CommandLogs{
				Stdout:    out.text(),
				Stderr:    errOut.text(),
				Truncated: out.truncated || errOut.truncated,
			}, nil
		}
	}
}

// Wait waits for the command to finish and returns what it printed. A command
// that ran past its own timeout was killed: Wait returns its partial result
// and a *CommandTimeoutError. A wait timeout leaves the command running and
// the handle good.
func (h *CommandHandle) Wait(ctx context.Context, options *WaitOptions) (*ExecResult, error) {
	if err := h.checkOpen(); err != nil {
		return nil, err
	}
	var stdout, stderr io.Writer
	var waitLimit time.Duration
	if options != nil {
		if options.Timeout < 0 {
			return nil, newError("invalid_argument", "timeout must be zero or greater")
		}
		waitLimit = options.Timeout
		stdout = options.Stdout
		stderr = options.Stderr
	}
	result, _, err := h.await(ctx, stdout, stderr, waitLimit)
	if err != nil {
		return nil, err
	}
	if result.TimedOut {
		return result, commandTimeoutError(result, h.timeout, h.SandboxID())
	}
	return result, nil
}

// Kill sends a signal (default SIGTERM when empty) to the command and its
// process group. It returns false when the run had already ended.
func (h *CommandHandle) Kill(ctx context.Context, signal string) (bool, error) {
	if err := h.checkOpen(); err != nil {
		return false, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	name, err := signalName(signal)
	if err != nil {
		return false, err
	}
	result, err := h.sandbox.runScript(ctx, killScript(h.id, name), requestExec, teardownTimeout)
	if err != nil {
		return false, err
	}
	switch result.ExitCode {
	case 0:
		return true, nil
	case 3:
		return false, &Error{
			Code:      "not_found",
			Message:   "no background command " + h.id + " in this sandbox",
			SandboxID: h.SandboxID(),
		}
	default:
		return false, nil
	}
}

// Cleanup removes the run's output files once the command has ended. A
// command still running keeps its files, and with them its time limit. It is
// best effort: an error means the files may remain.
func (h *CommandHandle) Cleanup(ctx context.Context) error {
	if err := h.checkOpen(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := h.sandbox.runScript(ctx, cleanupScript(h.id), requestSafe, teardownTimeout)
	if err != nil {
		return err
	}
	if result.ExitCode == 0 || result.ExitCode == 5 {
		return nil
	}
	return commandFailed("could not clean up "+h.id+": "+strings.TrimSpace(result.Stderr), h.SandboxID())
}

func (h *CommandHandle) checkOpen() error {
	if h == nil || h.sandbox == nil {
		return newError("invalid_argument", "the command handle is empty")
	}
	if h.sandbox.isClosed() {
		return closedError()
	}
	return nil
}

func (h *CommandHandle) await(ctx context.Context, stdout, stderr io.Writer, waitLimit time.Duration) (*ExecResult, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var deadline time.Time
	if waitLimit > 0 {
		deadline = time.Now().Add(waitLimit)
	}
	out := newStreamReader(stdout, maxRetainedBytes)
	errOut := newStreamReader(stderr, maxRetainedBytes)
	outage := &runOutage{window: h.sandbox.client.pollOutage, waitDeadline: deadline}
	delay := pollMinDelay
	finished := false
	var targets *[2]int

	for {
		sentAt := time.Now()
		if !finished && !deadline.IsZero() && !sentAt.Before(deadline) {
			return nil, finished, h.waitTimeout(waitLimit)
		}
		bound := time.Time{}
		if !finished {
			bound = deadline
		}
		if outageBound := outage.deadline(sentAt); !outageBound.IsZero() && (bound.IsZero() || outageBound.Before(bound)) {
			bound = outageBound
		}
		cut := !bound.IsZero() && (deadline.IsZero() || finished || !bound.Equal(deadline))

		pollCtx := ctx
		cancel := func() {}
		if !bound.IsZero() {
			pollCtx, cancel = context.WithDeadline(ctx, bound)
		}
		report, err := h.poll(pollCtx, out.offset, errOut.offset, pollChunkBytes)
		cancel()
		if err != nil {
			now := time.Now()
			if ctx.Err() != nil {
				return nil, finished, contextError(ctx.Err(), h.SandboxID(), EffectRan)
			}
			if !finished && !deadline.IsZero() && !now.Before(deadline) {
				return nil, finished, h.waitTimeout(waitLimit)
			}
			var failure *Error
			if !errors.As(err, &failure) {
				return nil, finished, err
			}
			pause, giveUpWith, ok := outage.fail(failure, sentAt, now, cut)
			if !ok {
				return nil, finished, giveUpWith
			}
			if sleepErr := sleepContext(ctx, pause); sleepErr != nil {
				return nil, finished, contextError(sleepErr, h.SandboxID(), EffectRan)
			}
			continue
		}
		outage.answered()

		if report.state != "running" && !finished {
			finished = true
			outage.finish()
			targets = &[2]int{report.outSize, report.errSize}
		}
		if targets == nil {
			if err := out.push(report.stdout); err != nil {
				return nil, finished, err
			}
			if err := errOut.push(report.stderr); err != nil {
				return nil, finished, err
			}
		} else {
			if err := out.push(clipBytes(report.stdout, targets[0]-out.offset)); err != nil {
				return nil, finished, err
			}
			if err := errOut.push(clipBytes(report.stderr, targets[1]-errOut.offset)); err != nil {
				return nil, finished, err
			}
		}

		more := (len(report.stdout) > 0 && out.offset < report.outSize) ||
			(len(report.stderr) > 0 && errOut.offset < report.errSize)
		if targets != nil {
			caughtUp := (out.offset >= targets[0] || len(report.stdout) == 0) &&
				(errOut.offset >= targets[1] || len(report.stderr) == 0)
			if caughtUp {
				more = false
			}
		}
		if report.state != "running" && !more {
			out.finish()
			errOut.finish()
			duration := time.Since(h.startedAt)
			if report.durationMS != nil {
				duration = time.Duration(*report.durationMS) * time.Millisecond
			}
			result := &ExecResult{
				Stdout:    out.text(),
				Stderr:    errOut.text(),
				ExitCode:  report.endedCode(),
				Duration:  duration,
				TimedOut:  report.timedOut,
				Truncated: out.truncated || errOut.truncated,
			}
			if result.Truncated {
				result.TruncatedReason = TruncationOutputLimit
			}
			return result, true, nil
		}
		if report.state == "running" && !deadline.IsZero() && !time.Now().Before(deadline) {
			return nil, finished, h.waitTimeout(waitLimit)
		}
		if more {
			delay = pollMinDelay
			continue
		}
		if len(report.stdout) > 0 || len(report.stderr) > 0 {
			delay = pollMinDelay
		}
		pause := delay
		if !finished && !deadline.IsZero() {
			left := time.Until(deadline)
			room := 2 * time.Second
			if left <= room {
				pause = 0
			} else if pause > left-room {
				pause = left - room
			}
		}
		if pause > 0 {
			if err := sleepContext(ctx, pause); err != nil {
				return nil, finished, contextError(err, h.SandboxID(), EffectRan)
			}
		}
		delay *= 2
		if delay > pollMaxDelay {
			delay = pollMaxDelay
		}
	}
}

func (h *CommandHandle) waitTimeout(limit time.Duration) *Error {
	message := h.id + " was still running"
	if limit > 0 {
		message += " after " + limit.String()
	}
	return &Error{
		Code:      "timeout",
		Message:   message,
		SandboxID: h.SandboxID(),
		Effect:    EffectRan,
	}
}

func (h *CommandHandle) poll(ctx context.Context, outOffset, errOffset, maxChunk int) (*runReport, error) {
	result, err := h.sandbox.runScript(ctx, pollScript(h.id, outOffset, errOffset, maxChunk), requestSafe, scriptTimeout+execAnswerSlack)
	if err != nil {
		return nil, err
	}
	return parseRunReport(result, h.id, h.SandboxID())
}

func (s *Sandbox) failedRun(handle *CommandHandle, err error, ended bool) error {
	stopped := ended
	if !ended {
		stopped = handle.stopBestEffort()
	}
	if stopped {
		handle.cleanupOnceGone()
	}
	var failure *Error
	if errors.As(err, &failure) {
		return commandFailure(failure, handle.id, stopped)
	}
	return err
}

func (h *CommandHandle) stopBestEffort() bool {
	ctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
	defer cancel()
	result, err := h.sandbox.runScript(ctx, killScript(h.id, "KILL"), requestSafe, teardownTimeout)
	if err != nil {
		return false
	}
	return result.ExitCode == 0 || result.ExitCode == 3 || result.ExitCode == 4
}

func (h *CommandHandle) cleanupBestEffort() {
	ctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
	defer cancel()
	_, _ = h.sandbox.runScript(ctx, cleanupScript(h.id), requestSafe, teardownTimeout)
}

func (h *CommandHandle) cleanupOnceGone() {
	deadline := time.Now().Add(cleanupGrace)
	for {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		result, err := h.sandbox.runScript(ctx, cleanupScript(h.id), requestSafe, cleanupGrace)
		cancel()
		if err != nil || result.ExitCode != 5 {
			return
		}
		if time.Now().Add(200 * time.Millisecond).After(deadline) {
			return
		}
		if err := sleepContext(context.Background(), 200*time.Millisecond); err != nil {
			return
		}
	}
}

func (s *Sandbox) abandonRun(runID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), abandonTimeout)
	defer cancel()
	result, err := s.runScript(ctx, abandonScript(runID), requestSafe, abandonTimeout)
	if err != nil {
		return false
	}
	return result.ExitCode == 0
}

func (s *Sandbox) runScript(ctx context.Context, script string, mode requestMode, attemptTimeout time.Duration) (*ExecResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	body := execRequestBody(script, "", nil, scriptTimeout)
	return s.executeBody(ctx, body, mode, attemptTimeout)
}

func (s *Sandbox) executeBody(ctx context.Context, body []byte, mode requestMode, attemptTimeout time.Duration) (*ExecResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	base, err := s.client.toolboxBaseFor(s.ID(), s.toolboxBaseValue())
	if err != nil {
		return nil, err
	}
	response, err := s.client.do(ctx, requestSpec{
		method:         http.MethodPost,
		url:            base + "/process/execute",
		body:           body,
		headers:        map[string]string{"Content-Type": "application/json"},
		mode:           mode,
		sandboxID:      s.ID(),
		attemptTimeout: attemptTimeout,
	})
	if err != nil {
		return nil, err
	}
	return parseExecResult(response.status, response.body, s.ID())
}

func commandText(command string, argv []string) (string, error) {
	if argv != nil {
		if len(argv) == 0 {
			return "", newError("invalid_argument", "the command is empty")
		}
		parts := make([]string, len(argv))
		for i, argument := range argv {
			parts[i] = shellQuote(argument)
		}
		command = strings.Join(parts, " ")
	}
	if strings.TrimSpace(command) == "" {
		return "", newError("invalid_argument", "the command is empty")
	}
	return command, nil
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(value); i++ {
		if !safeShellByte(value[i]) {
			safe = false
			break
		}
	}
	if safe {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func safeShellByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	switch b {
	case '_', '/', '.', ',', ':', '=', '@', '%', '+', '-':
		return true
	}
	return false
}

func optionsCwd(options *ExecOptions) string {
	if options == nil {
		return ""
	}
	return options.Cwd
}

func resolveCommandCwd(cwd string) (string, error) {
	if cwd == "" {
		return "", nil
	}
	if strings.ContainsRune(cwd, 0) {
		return "", newError("invalid_argument", "cwd must not contain NUL")
	}
	if strings.HasPrefix(cwd, "/") {
		return cwd, nil
	}
	return path.Join(workDir, cwd), nil
}

func execRequestBody(command, cwd string, env map[string]string, timeout time.Duration) []byte {
	body := map[string]any{"command": command}
	if cwd != "" {
		body["cwd"] = cwd
	}
	if len(env) > 0 {
		body["env"] = env
	}
	if timeout > 0 {
		body["timeout"] = timeout.Seconds()
	}
	encoded, _ := json.Marshal(body)
	return encoded
}

func parseExecResult(status int, body []byte, sandboxID string) (*ExecResult, error) {
	var document struct {
		Stdout     string   `json:"stdout"`
		Stderr     string   `json:"stderr"`
		ExitCode   *int     `json:"exitCode"`
		DurationMS *float64 `json:"durationMs"`
		TimedOut   bool     `json:"timedOut"`
		Truncated  bool     `json:"truncated"`
	}
	if len(body) == 0 || json.Unmarshal(body, &document) != nil || document.ExitCode == nil {
		return nil, &Error{
			Code:      "bad_response",
			Status:    status,
			Message:   "the exec answer carried no exit code",
			SandboxID: sandboxID,
			Effect:    EffectRan,
		}
	}
	result := &ExecResult{
		Stdout:   document.Stdout,
		Stderr:   document.Stderr,
		ExitCode: *document.ExitCode,
		TimedOut: document.TimedOut,
	}
	if document.DurationMS != nil {
		result.Duration = time.Duration(*document.DurationMS * float64(time.Millisecond))
	}
	if document.Truncated || len(document.Stdout) >= maxOutputBytes || len(document.Stderr) >= maxOutputBytes {
		result.Truncated = true
		result.TruncatedReason = TruncationOutputLimit
	}
	if result.TimedOut {
		result.ExitCode = -1
	}
	return result, nil
}

func parseStart(result *ExecResult, sandboxID string) (int, error) {
	text := strings.TrimSpace(result.Stdout)
	if result.ExitCode == 0 {
		if pid, err := strconv.Atoi(text); err == nil && pid > 0 && strconv.Itoa(pid) == text {
			return pid, nil
		}
	}
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if len(detail) > 300 {
		detail = detail[:300]
	}
	message := "could not start the background command"
	if detail != "" {
		message += ": " + detail
	}
	return 0, &Error{
		Code:      "command_failed",
		Message:   message,
		SandboxID: sandboxID,
		Effect:    EffectRan,
	}
}

func commandFailed(message, sandboxID string) error {
	return &Error{Code: "command_failed", Message: message, SandboxID: sandboxID, Effect: EffectRan}
}

func commandTimeoutError(result *ExecResult, limit time.Duration, sandboxID string) error {
	message := "the command ran past its timeout and was killed; the result has what it printed"
	if limit > 0 {
		message = "the command ran past its " + limit.String() + " timeout and was killed; the result has what it printed"
	}
	return &CommandTimeoutError{
		Err: &Error{
			Code:      "command_timeout",
			Message:   message,
			SandboxID: sandboxID,
			Effect:    EffectRan,
		},
		Result:  result,
		Timeout: limit,
	}
}

func commandFailure(err error, runID string, stopped bool) error {
	var failure *Error
	if !errors.As(err, &failure) {
		return err
	}
	copied := *failure
	message := strings.TrimSpace(copied.Message)
	if message == "" {
		message = defaultMessage(copied.Code)
	}
	message = strings.TrimRight(message, ". ")
	if stopped {
		message += ". The SDK confirmed " + runID + " is no longer running (stopped, or it had already ended)."
	} else {
		message += ". The SDK could not confirm it stopped: it may still be running as " + runID + "."
	}
	copied.Message = message
	if copied.Effect < EffectRan {
		copied.Effect = EffectRan
	}
	return &CommandError{Err: &copied, RunID: runID, Stopped: stopped}
}

func signalName(signal string) (string, error) {
	name := strings.ToUpper(strings.TrimSpace(signal))
	name = strings.TrimPrefix(name, "SIG")
	if name == "" {
		name = "TERM"
	}
	switch name {
	case "TERM", "KILL", "INT", "HUP", "QUIT", "USR1", "USR2", "STOP", "CONT":
		return name, nil
	}
	return "", newError("invalid_argument", "unsupported signal "+signal+"; use TERM, KILL, INT, HUP, QUIT, USR1, USR2, STOP or CONT")
}

type runReport struct {
	state      string
	exitCode   *int
	outSize    int
	errSize    int
	timedOut   bool
	durationMS *int
	stdout     []byte
	stderr     []byte
}

func (r *runReport) endedCode() int {
	if r.timedOut || r.state != "done" || r.exitCode == nil {
		return -1
	}
	return *r.exitCode
}

var runReportPattern = regexp.MustCompile(`^prized-run (running|done|gone|missing) (-|-?[0-9]+) ([0-9]+) ([0-9]+) ([01]) (-|-?[0-9]+)$`)

func parseRunReport(result *ExecResult, runID, sandboxID string) (*runReport, error) {
	lines := strings.Split(result.Stdout, "\n")
	match := []string(nil)
	if len(lines) > 0 {
		match = runReportPattern.FindStringSubmatch(lines[0])
	}
	if result.ExitCode != 0 || match == nil {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" && len(lines) > 0 {
			detail = strings.TrimSpace(lines[0])
		}
		if len(detail) > 200 {
			detail = detail[:200]
		}
		message := "could not read the background command's state"
		if detail != "" {
			message += ": " + detail
		}
		return nil, &Error{
			Code:      "bad_response",
			Message:   message,
			SandboxID: sandboxID,
			Effect:    EffectRan,
		}
	}
	if match[1] == "missing" {
		return nil, &Error{
			Code:      "not_found",
			Message:   "no background command " + runID + " in this sandbox",
			SandboxID: sandboxID,
		}
	}
	report := &runReport{
		state:    match[1],
		timedOut: match[5] == "1",
	}
	if match[2] != "-" {
		if code, err := strconv.Atoi(match[2]); err == nil {
			report.exitCode = &code
		}
	}
	report.outSize, _ = strconv.Atoi(match[3])
	report.errSize, _ = strconv.Atoi(match[4])
	if match[6] != "-" {
		if duration, err := strconv.Atoi(match[6]); err == nil {
			report.durationMS = &duration
		}
	}
	if len(lines) > 1 {
		stdout, err := decodeBase64Lines(lines[1])
		if err != nil {
			return nil, badRunReport("unreadable report for "+runID+": "+err.Error(), sandboxID)
		}
		report.stdout = stdout
	}
	if len(lines) > 2 {
		stderr, err := decodeBase64Lines(lines[2])
		if err != nil {
			return nil, badRunReport("unreadable report for "+runID+": "+err.Error(), sandboxID)
		}
		report.stderr = stderr
	}
	return report, nil
}

func badRunReport(message, sandboxID string) *Error {
	return &Error{
		Code:      "bad_response",
		Message:   message,
		SandboxID: sandboxID,
		Effect:    EffectRan,
	}
}

func decodeBase64Lines(text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(text)
}

type runOutage struct {
	window       time.Duration
	waitDeadline time.Time
	finished     bool
	end          time.Time
	room         time.Duration
	last         *Error
	tries        int
}

func (o *runOutage) finish() {
	o.finished = true
}

func (o *runOutage) deadline(now time.Time) time.Time {
	if o.end.IsZero() {
		return time.Time{}
	}
	lastLook := o.end.Add(-o.room)
	if now.Before(lastLook) {
		return lastLook
	}
	return o.end
}

func (o *runOutage) answered() {
	o.end = time.Time{}
	o.last = nil
	o.tries = 0
}

func (o *runOutage) fail(failure *Error, sentAt, now time.Time, cut bool) (time.Duration, *Error, bool) {
	if o.window <= 0 || !ridesOut(failure) {
		return 0, failure, false
	}
	if o.end.IsZero() {
		end := now.Add(o.window)
		if !o.finished && !o.waitDeadline.IsZero() && end.After(o.waitDeadline) {
			end = o.waitDeadline
		}
		o.end = end
		room := end.Sub(now) / 2
		if room > 2*time.Second {
			room = 2 * time.Second
		}
		if room < 0 {
			room = 0
		}
		o.room = room
	}
	if !cut {
		o.last = failure
		o.tries++
	}
	giveUpWith := failure
	if o.last != nil {
		giveUpWith = o.last
	}
	if !o.end.After(sentAt.Add(o.room)) {
		return 0, giveUpWith, false
	}
	toLast := o.end.Add(-o.room).Sub(now)
	if toLast < 0 {
		toLast = 0
	}
	if cut {
		return toLast, giveUpWith, true
	}
	pause := o.backoff(o.tries)
	if failure.RetryAfter > pause {
		pause = failure.RetryAfter
	}
	if pause > 60*time.Second {
		pause = 60 * time.Second
	}
	if pause > toLast {
		pause = toLast
	}
	if pause < 0 {
		pause = 0
	}
	return pause, giveUpWith, true
}

func (o *runOutage) backoff(tries int) time.Duration {
	if tries < 1 {
		tries = 1
	}
	pause := 500 * time.Millisecond
	for i := 0; i < tries && pause < 8*time.Second; i++ {
		pause *= 2
	}
	if pause > 8*time.Second {
		pause = 8 * time.Second
	}
	return pause
}

func ridesOut(failure *Error) bool {
	switch failure.Code {
	case "not_found", "sandbox_deleted", "sandbox_not_running":
		return false
	}
	if failure.Status == 0 {
		switch failure.Code {
		case "network_error", "timeout", "bad_response":
			return true
		}
		return false
	}
	return failure.Status >= 500 || failure.Status == http.StatusTooManyRequests || failure.Code == "bad_response"
}

func sleepContext(ctx context.Context, pause time.Duration) error {
	if pause <= 0 {
		return nil
	}
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type streamReader struct {
	writer    io.Writer
	parts     []string
	kept      int
	limit     int
	offset    int
	pending   []byte
	truncated bool
}

func newStreamReader(writer io.Writer, limit int) *streamReader {
	return &streamReader{writer: writer, limit: limit}
}

func (r *streamReader) push(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	r.offset += len(data)
	text := takeText(&r.pending, data, false)
	return r.emit(text)
}

func (r *streamReader) finish() {
	text := takeText(&r.pending, nil, true)
	_ = r.emit(text)
}

func (r *streamReader) emit(text string) error {
	if text == "" {
		return nil
	}
	room := r.limit - r.kept
	if room > 0 {
		kept := text
		if len(kept) > room {
			kept = kept[:room]
		}
		r.parts = append(r.parts, kept)
		r.kept += len(kept)
	}
	if len(text) > room {
		r.truncated = true
	}
	if r.writer != nil {
		if _, err := io.WriteString(r.writer, text); err != nil {
			return err
		}
	}
	return nil
}

func (r *streamReader) text() string {
	return strings.Join(r.parts, "")
}

func takeText(pending *[]byte, data []byte, final bool) string {
	buffer := make([]byte, 0, len(*pending)+len(data))
	buffer = append(buffer, *pending...)
	buffer = append(buffer, data...)
	if len(buffer) == 0 {
		*pending = nil
		return ""
	}
	if final {
		*pending = nil
		return strings.ToValidUTF8(string(buffer), "\uFFFD")
	}
	cut := len(buffer)
	for i := 0; i < len(buffer); {
		if buffer[i] < utf8.RuneSelf {
			i++
			continue
		}
		if !utf8.FullRune(buffer[i:]) {
			cut = i
			break
		}
		_, size := utf8.DecodeRune(buffer[i:])
		i += size
	}
	*pending = append((*pending)[:0], buffer[cut:]...)
	return strings.ToValidUTF8(string(buffer[:cut]), "\uFFFD")
}

func clipBytes(data []byte, limit int) []byte {
	if limit <= 0 {
		return nil
	}
	if len(data) > limit {
		return data[:limit]
	}
	return data
}
