package prized

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCommandScriptsParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	scripts := map[string]string{
		"start":   startScript("run_0123456789abcdef", "printf 'hello world\\n'", 90*time.Second, true),
		"poll":    pollScript("run_0123456789abcdef", 0, 0, pollChunkBytes),
		"kill":    killScript("run_0123456789abcdef", "TERM"),
		"cleanup": cleanupScript("run_0123456789abcdef"),
		"abandon": abandonScript("run_0123456789abcdef"),
		"capture": captureScript("exec_0123456789abcdef"),
		"collect": collectScript("exec_0123456789abcdef"),
		"remove":  removeScript("exec_0123456789abcdef"),
		"wrapper": runWrapper,
	}
	for name, script := range scripts {
		command := exec.Command(bash, "-n")
		command.Stdin = strings.NewReader(script)
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("%s script does not parse: %v\n%s", name, err, output)
		}
	}
}
