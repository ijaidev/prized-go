package prized

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// maxOutputBytes is the cap for each stream of one blocking call.
	maxOutputBytes = 4 << 20
	// maxFileBytes is the cap for one full-output stream and one transfer.
	maxFileBytes = 100 << 20

	outputDir         = "/home/prized/.prized/exec"
	outputGrace       = 5
	outputStaleMin    = 60
	outputScriptVar   = "PRIZED_EXEC_SCRIPT"
	outputCommandVar  = "PRIZED_EXEC_COMMAND"
	outputRunScript   = `eval "${PRIZED_EXEC_SCRIPT:?}"`
	outputReaderShell = `'cat > "$1" || { true > "$2" || :; cat > /dev/null; exit 1; }'`
	launchPathEnv     = "PATH=" + launchPath
	fileTimeout       = 600 * time.Second
)

var (
	outputIDPattern = regexp.MustCompile(`^exec_[0-9a-f]{16}$`)
)

func newExecID() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "exec_" + fmt.Sprintf("%016x", uint64(time.Now().UnixNano()))
	}
	return "exec_" + hex.EncodeToString(bytes[:])
}

func checkExecID(execID string) error {
	if !outputIDPattern.MatchString(execID) {
		return newError("invalid_argument", "bad exec id "+execID)
	}
	return nil
}

type outputMarker struct {
	out       int
	err       int
	outFailed bool
	errFailed bool
	gone      bool
}

func (s *Sandbox) execFullOutput(ctx context.Context, command, cwd string, env map[string]string, limit time.Duration) (*ExecResult, error) {
	execID := newExecID()
	requestEnv := make(map[string]string, len(env)+2)
	for name, value := range env {
		requestEnv[name] = value
	}
	requestEnv[outputScriptVar] = captureScript(execID)
	requestEnv[outputCommandVar] = command

	body := execRequestBody(outputRunScript, cwd, requestEnv, limit)
	result, err := s.executeBody(ctx, body, requestExec, limit+execAnswerSlack)
	if err != nil {
		s.outputStep(removeScript(execID))
		return nil, err
	}

	marker, ok := parseOutputMarker(result.Stdout, execID)
	if needsCollect(result, marker, ok) {
		collected, collectErr := s.outputStep(collectScript(execID))
		if collectErr == nil && collected != nil {
			marker, ok = parseOutputMarker(collected.Stdout, execID)
		}
	}
	if !ok || marker.gone {
		return result, nil
	}

	stdoutPath, stderrPath := outputPaths(execID)
	var stdout, stderr []byte
	if marker.out > 0 {
		stdout, err = s.downloadOutput(ctx, stdoutPath)
	}
	if err == nil && marker.err > 0 {
		stderr, err = s.downloadOutput(ctx, stderrPath)
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			// The caller left: it cannot read the output, so its files go.
			s.outputStep(removeScript(execID))
			return nil, err
		}
		if result.TimedOut {
			// The timeout is the caller's error and the files go with it.
			s.outputStep(removeScript(execID))
			return result, nil
		}
		var failure *Error
		if errors.As(err, &failure) {
			if failure.Code == "sandbox_deleted" {
				// Nothing is kept: the output went with the sandbox.
				return nil, restateDeletedOutput(failure, result, s.ID())
			}
			// The command ran, and its output stays for the caller to read.
			return nil, outputReadError(err, result, execID, s.ID())
		}
		s.outputStep(removeScript(execID))
		return nil, err
	}
	s.outputStep(removeScript(execID))

	result.Stdout = string(stdout)
	result.Stderr = string(stderr)
	switch {
	case marker.outFailed || marker.errFailed:
		result.Truncated = true
		result.TruncatedReason = TruncationWriteFailed
	case marker.out > maxFileBytes || marker.err > maxFileBytes:
		result.Truncated = true
		result.TruncatedReason = TruncationOutputLimit
	default:
		result.Truncated = false
		result.TruncatedReason = ""
	}
	return result, nil
}

func (s *Sandbox) outputStep(script string) (*ExecResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
	defer cancel()
	return s.runScript(ctx, script, requestSafe, teardownTimeout)
}

func (s *Sandbox) downloadOutput(ctx context.Context, filePath string) ([]byte, error) {
	base, err := s.client.toolboxBaseFor(s.ID(), s.toolboxBaseValue())
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("path", filePath)
	response, err := s.client.do(ctx, requestSpec{
		method:         http.MethodGet,
		url:            base + "/files/download?" + query.Encode(),
		mode:           requestRead,
		sandboxID:      s.ID(),
		headers:        map[string]string{"Accept": "application/octet-stream"},
		attemptTimeout: fileTimeout,
	})
	if err != nil {
		return nil, err
	}
	if len(response.body) > maxFileBytes {
		return nil, &Error{
			Code:      "file_too_large",
			Message:   "the output file is over the 100 MiB transfer limit",
			SandboxID: s.ID(),
			Effect:    EffectRan,
		}
	}
	return response.body, nil
}

func needsCollect(result *ExecResult, marker *outputMarker, ok bool) bool {
	return !ok && (result.TimedOut || result.ExitCode == -1)
}

func outputReadError(err error, result *ExecResult, execID, sandboxID string) error {
	reason := ""
	var failure *Error
	if errors.As(err, &failure) {
		reason = failure.Code
	}
	stdoutPath, stderrPath := outputPaths(execID)
	message := fmt.Sprintf("the command ran (exit %d), but its output could not be read back", result.ExitCode)
	if reason != "" {
		message += " (" + reason + ")"
	}
	message += ". It is kept at " + stdoutPath + " and " + stderrPath + " for an hour"
	exitCode := result.ExitCode
	return &OutputReadError{
		Err: &Error{
			Code:      "output_read_failed",
			Message:   message,
			Reason:    reason,
			SandboxID: sandboxID,
			Effect:    EffectRan,
			ExitCode:  &exitCode,
			cause:     err,
		},
		ExitCode:   result.ExitCode,
		StdoutPath: stdoutPath,
		StderrPath: stderrPath,
	}
}

func restateDeletedOutput(failure *Error, result *ExecResult, sandboxID string) *Error {
	copied := *failure
	message := fmt.Sprintf("the command ran (exit %d), but the sandbox was deleted before its output could be read back", result.ExitCode)
	copied.Message = message
	if copied.SandboxID == "" {
		copied.SandboxID = sandboxID
	}
	copied.Effect = EffectRan
	copied.cause = failure
	exitCode := result.ExitCode
	copied.ExitCode = &exitCode
	return &copied
}

func outputPaths(execID string) (string, string) {
	directory := outputDir + "/" + execID
	return directory + "/stdout", directory + "/stderr"
}

func parseOutputMarker(stdout, execID string) (*outputMarker, bool) {
	pattern := regexp.MustCompile(`(?:^|\n)prized-output ` + regexp.QuoteMeta(execID) + ` (?:([0-9]+) ([0-9]+)(?: ([01]) ([01]))?|(gone))\n?\z`)
	match := pattern.FindStringSubmatch(stdout)
	if match == nil {
		return nil, false
	}
	if match[5] != "" {
		return &outputMarker{gone: true}, true
	}
	out, _ := strconv.Atoi(match[1])
	errSize, _ := strconv.Atoi(match[2])
	return &outputMarker{
		out:       out,
		err:       errSize,
		outFailed: match[3] == "1",
		errFailed: match[4] == "1",
	}, true
}

func outputReader(directory, name, fifo string) string {
	blocks := maxFileBytes/512 + 1
	return `( exec > /dev/null 2>&1; ` + launchPathEnv + `; trap '' XFSZ; ( set -o posix ) && set -o posix; ulimit -f ` +
		strconv.Itoa(blocks) + ` || :; exec setsid /bin/sh -c ` + outputReaderShell + ` sh ` +
		directory + `/` + name + ` ` + directory + `/` + name + `.failed < ` + directory + `/` + fifo + ` ) &`
}

func outputRelay(directory, src, dst string) string {
	return `( exec > ` + directory + `/` + dst + ` 2> /dev/null < /dev/null; ` + launchPathEnv +
		`; exec setsid cat ` + directory + `/` + src + ` ) &`
}

func outputIsRelay(pid, fifoPath string) string {
	return `[ "$(tr "\0\n" "\n " < "/proc/` + pid + `/cmdline" 2> /dev/null | tr "\n" " ")" = "cat ` + fifoPath + ` " ]`
}

func outputWatchdog(directory string) string {
	return `( s=; trap '[ -z "$s" ] || kill "$s" 2> /dev/null; exit 0' TERM; sleep ` +
		strconv.Itoa(outputGrace) + ` & s=$!; wait "$s" || :; if ` +
		outputIsRelay("$4", directory+"/o") + `; then kill "$4" 2> /dev/null || :; fi; if ` +
		outputIsRelay("$5", directory+"/e") + `; then kill "$5" 2> /dev/null || :; fi ) > /dev/null 2>&1 &`
}

func outputSizes() []string {
	return []string{
		`o=$(stat -c %s "$d/stdout" 2> /dev/null || echo 0)`,
		`e=$(stat -c %s "$d/stderr" 2> /dev/null || echo 0)`,
	}
}

func outputCut(execID string) []string {
	cap := strconv.Itoa(maxFileBytes)
	return []string{
		`if [ "$o" -gt ` + cap + ` ]; then truncate -s ` + cap + ` "$d/stdout" && true > "$d/stdout.cut" || :; fi`,
		`if [ "$e" -gt ` + cap + ` ]; then truncate -s ` + cap + ` "$d/stderr" && true > "$d/stderr.cut" || :; fi`,
		`echo "prized-output ` + execID + ` $o $e $x $y"`,
	}
}

func outputFailed() []string {
	cap := strconv.Itoa(maxFileBytes)
	return []string{
		`if [ "$x" -eq 0 ] || [ "$o" -gt ` + cap + ` ]; then x=0; else x=1; fi`,
		`if [ "$y" -eq 0 ] || [ "$e" -gt ` + cap + ` ]; then y=0; else y=1; fi`,
	}
}

func captureScript(execID string) string {
	if checkExecID(execID) != nil {
		return ""
	}
	directory := outputDir + "/" + execID
	lines := []string{
		`set -- "${PRIZED_EXEC_COMMAND:?}" /bin/bash; unset PRIZED_EXEC_COMMAND PRIZED_EXEC_SCRIPT; [ -x "$2" ] || set -- "$1" /bin/sh`,
		`if ( ` + launchPathEnv + `; mkdir -p ` + outputDir + ` && mkdir ` + directory + ` ) 2> /dev/null; then`,
		`if ( ` + launchPathEnv + `; [ -x /bin/sh ] && command -v setsid && command -v cat && mkfifo ` +
			directory + `/o ` + directory + `/e ` + directory + `/p ` + directory + `/q ) > /dev/null 2>&1; then`,
		outputReader(directory, "stdout", "p"),
		`set -- "$1" "$2" "$!"`,
		outputReader(directory, "stderr", "q"),
		`set -- "$1" "$2" "$3" "$!"`,
		outputRelay(directory, "o", "p"),
		`set -- "$1" "$2" "$3" "$4" "$!"`,
		outputRelay(directory, "e", "q"),
		`set -- "$1" "$2" "$3" "$4" "$5" "$!"`,
		`if { "$2" -c "$1" > ` + directory + `/o 2> ` + directory + `/e < /dev/null; } 2> /dev/null; ` +
			`then set -- 0 "$3" "$4" "$5" "$6"; else set -- "$?" "$3" "$4" "$5" "$6"; fi`,
		launchPathEnv,
		`c=$1; d=` + directory,
		outputWatchdog(directory),
		`w=$!`,
		`if wait "$2" 2> /dev/null; then x=0; else x=$?; fi`,
		`if wait "$3" 2> /dev/null; then y=0; else y=$?; fi`,
		`kill "$w" 2> /dev/null || :`,
	}
	lines = append(lines, outputSizes()...)
	lines = append(lines, outputFailed()...)
	lines = append(lines,
		`if [ "$o" -lt `+strconv.Itoa(maxOutputBytes)+` ] && [ "$e" -lt `+strconv.Itoa(maxOutputBytes)+` ] && [ "$x$y" = 00 ]; then `+
			`cat "$d/stdout" 2> /dev/null || :; cat "$d/stderr" >&2 2> /dev/null || :; `+
			`rm -rf "$d" || :; exit "$c"; fi`,
	)
	lines = append(lines, outputCut(execID)...)
	lines = append(lines,
		`exit "$c"`,
		`fi`,
		`( `+launchPathEnv+`; rm -rf `+directory+` ) 2> /dev/null || :`,
		`fi`,
		`exec "$2" -c "$1" < /dev/null`,
	)
	return strings.Join(lines, "\n")
}

func collectScript(execID string) string {
	if checkExecID(execID) != nil {
		return ""
	}
	directory := outputDir + "/" + execID
	lines := []string{
		`d=` + directory,
		launchPathEnv,
		`if [ ! -f "$d/stdout" ] || [ ! -f "$d/stderr" ]; then rm -rf "$d" || :; echo "prized-output ` + execID + ` gone"; exit 0; fi`,
		`if [ -e "$d/stdout.failed" ]; then x=1; else x=0; fi`,
		`if [ -e "$d/stderr.failed" ]; then y=1; else y=0; fi`,
	}
	lines = append(lines, outputSizes()...)
	lines = append(lines,
		`if [ -e "$d/stdout.cut" ]; then o=$((`+strconv.Itoa(maxFileBytes)+` + 512)); fi`,
		`if [ -e "$d/stderr.cut" ]; then e=$((`+strconv.Itoa(maxFileBytes)+` + 512)); fi`,
	)
	lines = append(lines, outputFailed()...)
	lines = append(lines, outputCut(execID)...)
	return strings.Join(lines, "\n")
}

func removeScript(execID string) string {
	if checkExecID(execID) != nil {
		return ""
	}
	return launchPathEnv + `; rm -rf ` + outputDir + `/` + execID + `; ` +
		`find ` + outputDir + ` -mindepth 1 -maxdepth 1 -name 'exec_*' -mmin +` +
		strconv.Itoa(outputStaleMin) + ` -exec rm -rf -- {} + 2> /dev/null || :`
}
