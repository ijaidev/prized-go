package prized

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxFileBytes is the most one file transfer may move: 100 MiB.
	MaxFileBytes = 100 << 20
	// DefaultFileTimeout bounds one file call, its retries included.
	DefaultFileTimeout = 10 * time.Minute
)

const (
	fileWorkDir = "/home/prized/work"
	fileHomeDir = "/home/prized"

	// stagingPrefix starts a write's temporary name, beside its target.
	stagingPrefix = ".prized-write-"

	// The publish script's own exits (the reference SDKs' too).
	exitTargetIsLink     = 47
	exitTargetNotRegular = 48

	// The helper scripts' own exits, as the Python SDK renders them.
	exitNotFound     = 44
	exitNotDirectory = 45
	exitNotEmpty     = 46

	fileScriptTimeout = 60 * time.Second
	fileAnswerSlack   = 30 * time.Second
	discardTimeout    = 10 * time.Second

	// listOutputCap is the most one command's stream keeps; a listing that
	// reaches it may be incomplete.
	listOutputCap = 4 << 20
)

// WriteOptions describes one write or upload.
type WriteOptions struct {
	// Mode sets the file's permission bits, 0 to 0o777 (0o755 for a script).
	// Without it, a file that is replaced keeps its mode, and a new file gets
	// the sandbox's default.
	Mode *os.FileMode
}

// RemoveOptions describes one remove.
type RemoveOptions struct {
	// Recursive removes a directory and everything under it.
	Recursive bool
}

// WriteInfo reports a finished write or upload.
type WriteInfo struct {
	// Path is the absolute path that was written.
	Path string
	// Size is the number of bytes written.
	Size int64
}

// FileInfo is one directory entry.
type FileInfo struct {
	Name      string
	Path      string
	Size      int64
	IsDir     bool
	IsSymlink bool
	// Mode is the permissions as ls -l prints them, such as -rw-r--r--.
	Mode string
	// Modified is the entry's modification time; the zero time when the
	// sandbox reported none.
	Modified time.Time
}

// Files moves files in and out of one sandbox. Paths are absolute, or
// relative to /home/prized/work. Transfers stay inside the sandbox user's
// home (/home/prized) and move at most MaxFileBytes per file.
type Files struct {
	sandbox *Sandbox
}

// Files returns the handle's file operations.
func (s *Sandbox) Files() *Files {
	return &Files{sandbox: s}
}

// Write writes a file, creating parent directories and replacing an existing
// file whole. A file that is replaced keeps its mode; a new file gets the
// sandbox's default. Mode sets the bits either way.
//
// The bytes go up under a fresh name beside the file and one command, never
// sent again, puts them in place with a rename. An interrupted write leaves
// the prior content untouched.
func (f *Files) Write(ctx context.Context, path string, data []byte, options *WriteOptions) (*WriteInfo, error) {
	if f.sandbox.isClosed() {
		return nil, closedError()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	target, err := resolveFilePath(path)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fileTooLarge(f.sandbox.ID())
	}
	mode, err := checkFileMode(options)
	if err != nil {
		return nil, err
	}

	clean, staged := stagingPlan(target)
	if !staged {
		// The files API judges the path itself, as for any upload.
		return f.send(ctx, target, data, target, requestExec)
	}
	suffix, err := stagingSuffix()
	if err != nil {
		return nil, &Error{Code: "internal_error", Message: "could not name the staged write", cause: err}
	}
	staging := clean[:strings.LastIndex(clean, "/")+1] + stagingPrefix + suffix
	if _, err := f.send(ctx, staging, data, clean, requestStaged); err != nil {
		return nil, f.stagedUploadFailed(err, staging, clean)
	}

	result, err := f.runCommand(ctx, publishScript(staging, clean, mode), fileHomeDir, requestExec)
	if err != nil {
		// Unless the command ran (a 2xx), the staged file may still be there;
		// once it ran, the command removed it itself.
		if !answerLanded(err) {
			f.discard(staging)
		}
		return nil, err
	}
	if result.timedOut {
		f.discard(staging) // killed, perhaps before it could remove it
		return nil, &Error{
			Code:      "command_timeout",
			Message:   "putting " + clean + " in place timed out",
			SandboxID: f.sandbox.ID(),
			Effect:    EffectMaybeRan,
		}
	}
	switch result.exitCode {
	case 0:
		return &WriteInfo{Path: clean, Size: int64(len(data))}, nil
	case exitTargetIsLink:
		// A link: the files API follows it or refuses it, as for any upload.
		info, err := f.send(ctx, clean, data, clean, requestExec)
		if err != nil {
			return nil, err
		}
		if mode != nil {
			if err := f.chmod(ctx, clean, *mode); err != nil {
				return nil, err
			}
		}
		return info, nil
	case exitTargetNotRegular:
		return nil, &Error{
			Code:      "unsupported_file_type",
			Message:   clean + " is a directory or another file that is not a regular file; only a regular file can be written",
			SandboxID: f.sandbox.ID(),
			Effect:    EffectRan,
		}
	}
	denied := strings.Contains(result.stderr, "Permission denied") ||
		strings.Contains(result.stderr, "Operation not permitted")
	code := "command_failed"
	if denied {
		code = "permission_denied"
	}
	return nil, &Error{
		Code:      code,
		Message:   "could not write " + clean + ": " + commandDetail(result),
		SandboxID: f.sandbox.ID(),
		Effect:    EffectRan,
	}
}

// Read returns a file's contents as text (at most MaxFileBytes).
func (f *Files) Read(ctx context.Context, path string) (string, error) {
	data, err := f.ReadBytes(ctx, path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ReadBytes returns a file's contents (at most MaxFileBytes).
func (f *Files) ReadBytes(ctx context.Context, path string) ([]byte, error) {
	if f.sandbox.isClosed() {
		return nil, closedError()
	}
	target, err := resolveFilePath(path)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	base, err := f.toolboxBase()
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, DefaultFileTimeout)
	defer cancel()
	response, err := f.sandbox.client.do(callCtx, requestSpec{
		method:         http.MethodGet,
		url:            base + "/files/download?path=" + url.QueryEscape(target),
		headers:        map[string]string{"Accept": "*/*"},
		mode:           requestRead,
		sandboxID:      f.sandbox.ID(),
		attemptTimeout: -1,
		maxBody:        MaxFileBytes,
	})
	if err != nil {
		return nil, err
	}
	return response.body, nil
}

// Upload copies a local file into the sandbox, as Write does. Local file
// errors are returned as they are, not as API errors.
func (f *Files) Upload(ctx context.Context, localPath, remotePath string, options *WriteOptions) (*WriteInfo, error) {
	if f.sandbox.isClosed() {
		return nil, closedError()
	}
	data, err := readLocalFile(localPath, f.sandbox.ID())
	if err != nil {
		return nil, err
	}
	return f.Write(ctx, remotePath, data, options)
}

// Download copies a file out of the sandbox to a local path, creating local
// parent directories, and returns the bytes written. Local file errors are
// returned as they are, not as API errors.
func (f *Files) Download(ctx context.Context, remotePath, localPath string) (int64, error) {
	data, err := f.ReadBytes(ctx, remotePath)
	if err != nil {
		return 0, err
	}
	if parent := filepath.Dir(localPath); parent != "" && parent != "." {
		if err := os.MkdirAll(parent, 0o777); err != nil {
			return 0, err
		}
	}
	if err := os.WriteFile(localPath, data, 0o666); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

// List returns a directory's entries, sorted by name (default:
// /home/prized/work).
func (f *Files) List(ctx context.Context, path string) ([]FileInfo, error) {
	if f.sandbox.isClosed() {
		return nil, closedError()
	}
	if path == "" {
		path = fileWorkDir
	}
	target, err := resolveFilePath(path)
	if err != nil {
		return nil, err
	}
	result, err := f.runCommand(ctx, listScript(target), "", requestRead)
	if err != nil {
		return nil, err
	}
	if result.exitCode != 0 {
		return nil, fileFailure(result, target, f.sandbox.ID(), "list")
	}
	if len(result.stdout) >= listOutputCap {
		// The listing may be cut short: say so rather than return a short
		// list that looks whole.
		return nil, &Error{
			Code:      "command_failed",
			Message:   target + " has too many entries to list in one call; list a narrower directory",
			SandboxID: f.sandbox.ID(),
			Effect:    EffectRan,
		}
	}
	return parseFileList(result.stdout), nil
}

// Exists reports whether a path exists: a file, a directory, or a link, even
// a dangling one.
func (f *Files) Exists(ctx context.Context, path string) (bool, error) {
	if f.sandbox.isClosed() {
		return false, closedError()
	}
	target, err := resolveFilePath(path)
	if err != nil {
		return false, err
	}
	result, err := f.runCommand(ctx, existsScript(target), "", requestRead)
	if err != nil {
		return false, err
	}
	switch result.exitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, fileFailure(result, target, f.sandbox.ID(), "exists")
}

// Remove deletes a file or an empty directory; Recursive deletes a directory
// and everything under it. A missing path is not an error.
func (f *Files) Remove(ctx context.Context, path string, options *RemoveOptions) error {
	if f.sandbox.isClosed() {
		return closedError()
	}
	target, err := resolveFilePath(path)
	if err != nil {
		return err
	}
	recursive := options != nil && options.Recursive
	// Command retry rules: a lost answer is not sent again, or a path the
	// sandbox recreated in the meantime would be deleted too.
	result, err := f.runCommand(ctx, removeScript(target, recursive), "", requestExec)
	if err != nil {
		return err
	}
	if result.exitCode != 0 {
		return fileFailure(result, target, f.sandbox.ID(), "remove")
	}
	return nil
}

// send uploads one file and reports what the API answered.
func (f *Files) send(ctx context.Context, uploadPath string, data []byte, target string, mode requestMode) (*WriteInfo, error) {
	base, err := f.toolboxBase()
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, DefaultFileTimeout)
	defer cancel()
	response, err := f.sandbox.client.do(callCtx, requestSpec{
		method:         http.MethodPost,
		url:            base + "/files/upload?path=" + url.QueryEscape(uploadPath),
		body:           data,
		headers:        map[string]string{"Accept": "*/*", "Content-Type": "application/octet-stream"},
		mode:           mode,
		sandboxID:      f.sandbox.ID(),
		attemptTimeout: -1,
	})
	if err != nil {
		return nil, err
	}
	info := &WriteInfo{Path: target, Size: int64(len(data))}
	if len(response.body) > 0 {
		var document struct {
			Path string `json:"path"`
			Size *int64 `json:"size"`
		}
		if err := json.Unmarshal(response.body, &document); err != nil {
			return nil, &Error{
				Code:      "bad_response",
				Status:    response.status,
				Message:   "the upload answer could not be read",
				SandboxID: f.sandbox.ID(),
				Effect:    EffectRan,
				cause:     err,
			}
		}
		if uploadPath == target && document.Path != "" {
			info.Path = document.Path
		}
		if document.Size != nil && *document.Size >= 0 {
			info.Size = *document.Size
		}
	}
	return info, nil
}

// stagedUploadFailed words a failed staged upload as what it is: the target
// was not changed, though the bytes may have reached the sandbox. The staging
// file is discarded when a copy may have landed; one whose 2xx arrived but
// whose body could not be read is all there and is left for the caller to
// overwrite.
func (f *Files) stagedUploadFailed(err error, staging, target string) error {
	landed := false
	mayHave := true
	var failure *Error
	if errors.As(err, &failure) {
		landed = failure.Status >= 200 && failure.Status < 300
		mayHave = failure.Effect != EffectNotRun
		if failure.Effect != EffectNotRun {
			message := strings.TrimSuffix(failure.Message, ".")
			failure.Message = message + "; " + target + " was not changed"
		}
		failure.Effect = EffectNotRun
	}
	if !landed && mayHave {
		f.discard(staging)
	}
	return err
}

// discard removes a staged upload that was not put in place, best effort.
// It retries like a read: the name is this call's own.
func (f *Files) discard(staging string) {
	ctx, cancel := context.WithTimeout(context.Background(), discardTimeout)
	defer cancel()
	_, _ = f.runCommand(ctx, removeScript(staging, false), fileHomeDir, requestRead)
}

func (f *Files) chmod(ctx context.Context, path string, mode os.FileMode) error {
	bits := strconv.FormatInt(int64(mode), 8)
	result, err := f.runCommand(ctx, "chmod "+bits+" -- "+shellQuote(path), fileHomeDir, requestRead)
	if err != nil {
		return err
	}
	if result.exitCode != 0 {
		return &Error{
			Code:      "command_failed",
			Message:   path + " was written, but its mode could not be set: " + commandDetail(result),
			SandboxID: f.sandbox.ID(),
			Effect:    EffectRan,
		}
	}
	return nil
}

type scriptResult struct {
	stdout   string
	stderr   string
	exitCode int
	timedOut bool
}

// runCommand runs one of the files API's own helper commands.
func (f *Files) runCommand(ctx context.Context, command, cwd string, mode requestMode) (*scriptResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	base, err := f.toolboxBase()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(struct {
		Command string  `json:"command"`
		Cwd     string  `json:"cwd,omitempty"`
		Timeout float64 `json:"timeout"`
	}{
		Command: command,
		Cwd:     cwd,
		Timeout: fileScriptTimeout.Seconds(),
	})
	if err != nil {
		return nil, &Error{Code: "internal_error", Message: "could not encode the file command", cause: err}
	}
	response, err := f.sandbox.client.do(ctx, requestSpec{
		method:         http.MethodPost,
		url:            base + "/process/execute",
		body:           body,
		headers:        map[string]string{"Content-Type": "application/json"},
		mode:           mode,
		sandboxID:      f.sandbox.ID(),
		attemptTimeout: fileScriptTimeout + fileAnswerSlack,
	})
	if err != nil {
		return nil, err
	}
	var document struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode *int   `json:"exitCode"`
		TimedOut bool   `json:"timedOut"`
	}
	if err := json.Unmarshal(response.body, &document); err != nil || document.ExitCode == nil {
		return nil, &Error{
			Code:      "bad_response",
			Status:    response.status,
			Message:   "the exec answer carried no exit code",
			SandboxID: f.sandbox.ID(),
			Effect:    EffectRan,
			cause:     err,
		}
	}
	return &scriptResult{
		stdout:   document.Stdout,
		stderr:   document.Stderr,
		exitCode: *document.ExitCode,
		timedOut: document.TimedOut,
	}, nil
}

func (f *Files) toolboxBase() (string, error) {
	sandbox := f.sandbox
	return sandbox.client.toolboxBaseFor(sandbox.ID(), sandbox.toolboxBaseValue())
}

// resolveFilePath joins a relative path to the workspace, as commands start
// there. Absolute paths pass through.
func resolveFilePath(path string) (string, error) {
	if path == "" {
		return "", newError("invalid_argument", "path must be a non-empty string")
	}
	if strings.HasPrefix(path, "/") {
		return path, nil
	}
	return fileWorkDir + "/" + path, nil
}

func checkFileMode(options *WriteOptions) (*os.FileMode, error) {
	if options == nil || options.Mode == nil {
		return nil, nil
	}
	mode := *options.Mode
	if mode&^os.FileMode(0o777) != 0 {
		return nil, newError("invalid_argument", "mode must be permission bits from 0 to 0o777 (0o755 for a script)")
	}
	return &mode, nil
}

// stagingPlan returns the target as the files API reads it (empty and "."
// parts gone). It reports false when the files API has to judge the path
// itself (outside /home/prized, a ".." part or NUL): the write is then
// uploaded as it is.
func stagingPlan(target string) (clean string, ok bool) {
	if !strings.HasPrefix(target, "/") || strings.ContainsRune(target, 0) {
		return "", false
	}
	parts := make([]string, 0, 8)
	for _, part := range strings.Split(target, "/") {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return "", false
		}
		parts = append(parts, part)
	}
	clean = "/" + strings.Join(parts, "/")
	if !strings.HasPrefix(clean, fileHomeDir+"/") {
		return "", false
	}
	return clean, true
}

func stagingSuffix() (string, error) {
	var bytes [16]byte
	if _, err := cryptorand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

// publishScript puts the staged upload in place with one rename. A regular
// file there lends its permission bits (setuid, setgid and sticky are not
// carried) and, where the sandbox user may, its owner and group; mode, when
// given, sets the bits instead. A link exits 47 and a directory or other
// non-regular file exits 48, changing nothing.
func publishScript(staging, target string, mode *os.FileMode) string {
	chmod := func(bits string) string {
		return `chmod ` + bits + ` -- "$src" || exit`
	}
	var bits []string
	if mode == nil {
		bits = []string{`  ` + chmod(`"$(printf %o $((0x${s%% *} & 0777)))"`), "fi"}
	} else {
		bits = []string{"fi", chmod(strconv.FormatInt(int64(*mode), 8))}
	}
	lines := []string{
		"src=" + shellQuote(staging),
		"dst=" + shellQuote(target),
		`trap 'rm -f -- "$src"' EXIT`,
		// One lstat: the type, the bits and the owner come from the same file.
		`if s=$(stat -c '%f +%u:+%g' -- "$dst" 2> /dev/null); then`,
		`  t=$((0x${s%% *} & 0170000))`,
		`  [ "$t" -eq $((0120000)) ] && exit 47`,
		`  [ "$t" -eq $((0100000)) ] || exit 48`,
		`  chown -- "${s#* }" "$src" 2> /dev/null || chgrp -- "${s##*:}" "$src" 2> /dev/null`,
	}
	lines = append(lines, bits...)
	lines = append(lines,
		`mv -fT -- "$src" "$dst" || exit`,
		`sync -- "${dst%/*}" 2> /dev/null`,
		"exit 0",
	)
	return strings.Join(lines, "\n")
}

// listScript prints one NUL-separated record per entry: type, size, mtime,
// mode, name, full path. Exit 44 missing, 45 not a directory.
func listScript(path string) string {
	p := shellQuote(path)
	return "p=" + p + "; " +
		`if [ ! -e "$p" ] && [ ! -L "$p" ]; then exit 44; fi; ` +
		`if [ ! -d "$p" ]; then exit 45; fi; ` +
		`exec find -H "$p" -mindepth 1 -maxdepth 1 -printf '%y\0%s\0%T@\0%M\0%f\0%p\0'`
}

// existsScript exits 0 when the path exists (a dangling link counts) and 1
// when it does not.
func existsScript(path string) string {
	return "p=" + shellQuote(path) + `; if [ -e "$p" ] || [ -L "$p" ]; then exit 0; else exit 1; fi`
}

// removeScript removes a path; a missing path is not an error. A directory
// removed without recursion must be empty (exit 46 otherwise).
func removeScript(path string, recursive bool) string {
	onDirectory := `rm -rf -- "$p"`
	if !recursive {
		onDirectory = `rmdir -- "$p" 2> /dev/null || exit 46`
	}
	return "p=" + shellQuote(path) +
		`; if [ -d "$p" ] && [ ! -L "$p" ]; then ` + onDirectory + `; else rm -f -- "$p"; fi`
}

func parseFileList(output string) []FileInfo {
	fields := strings.Split(output, "\x00")
	if len(fields) > 0 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	entries := make([]FileInfo, 0, len(fields)/6)
	for i := 0; i+6 <= len(fields); i += 6 {
		kind, size, mtime, mode, name, full := fields[i], fields[i+1], fields[i+2], fields[i+3], fields[i+4], fields[i+5]
		entry := FileInfo{
			Name:      name,
			Path:      full,
			IsDir:     kind == "d",
			IsSymlink: kind == "l",
			Mode:      mode,
		}
		if value, err := strconv.ParseInt(size, 10, 64); err == nil {
			entry.Size = value
		}
		if seconds, err := strconv.ParseFloat(mtime, 64); err == nil && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) {
			whole := int64(seconds)
			entry.Modified = time.Unix(whole, int64((seconds-float64(whole))*float64(time.Second)))
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
}

func fileFailure(result *scriptResult, path, sandboxID, operation string) *Error {
	switch result.exitCode {
	case exitNotFound:
		return &Error{Code: "not_found", Message: path + " does not exist", SandboxID: sandboxID, Effect: EffectRan}
	case exitNotDirectory:
		return &Error{Code: "not_directory", Message: path + " is not a directory", SandboxID: sandboxID, Effect: EffectRan}
	case exitNotEmpty:
		return &Error{
			Code:      "directory_not_empty",
			Message:   path + " is a directory with files in it; pass Recursive to remove it all",
			SandboxID: sandboxID,
			Effect:    EffectRan,
		}
	}
	return &Error{
		Code:      "command_failed",
		Message:   operation + " " + path + ": " + commandDetail(result),
		SandboxID: sandboxID,
		Effect:    EffectRan,
	}
}

func commandDetail(result *scriptResult) string {
	detail := strings.Join(strings.Fields(result.stderr), " ")
	if len(detail) > 300 {
		detail = detail[:300]
	}
	if detail == "" {
		detail = fmt.Sprintf("exit %d", result.exitCode)
	}
	return detail
}

func answerLanded(err error) bool {
	var failure *Error
	return errors.As(err, &failure) && failure.Status >= 200 && failure.Status < 300
}

// shellQuote renders one argument as a POSIX shell word, as the reference
// SDKs do.
func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		switch c {
		case '_', '/', '.', ',', ':', '=', '@', '%', '+', '-':
			continue
		}
		safe = false
		break
	}
	if safe {
		return value
	}
	return `'` + strings.ReplaceAll(value, `'`, `'"'"'`) + `'`
}

func readLocalFile(path, sandboxID string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fileTooLarge(sandboxID)
	}
	return data, nil
}

func fileTooLarge(sandboxID string) *Error {
	return &Error{
		Code:      "file_too_large",
		Message:   defaultMessage("file_too_large"),
		SandboxID: sandboxID,
		Effect:    EffectNotRun,
	}
}
