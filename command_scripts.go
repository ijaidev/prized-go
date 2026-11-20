package prized

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	runIDPrefix    = "run_"
	noPIDExit      = 99
	cleanedKeepMin = 60
	maxLimitSec    = 1_000_000_000
	launchPath     = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

func newRunID() string {
	var bytes [8]byte
	if _, err := cryptorand.Read(bytes[:]); err != nil {
		return runIDPrefix + fmt.Sprintf("%016x", uint64(time.Now().UnixNano()))
	}
	return runIDPrefix + hex.EncodeToString(bytes[:])
}

// isRunTest is a shell test: process pid is the run's wrapper, or its
// watchdog, not another process that reused the pid.
func isRunTest(pid, expected string) string {
	return `[ "$(tr "\0\n" "\n " < "/proc/` + pid + `/cmdline" 2> /dev/null | sed -n 4,5p | tr "\n" " ")" = ` + expected + ` ]`
}

var runWrapper = `unset __pz_k __pz_u __pz_w __pz_c __pz_end
__pz_k=
trap "__pz_k=129" HUP
trap "__pz_k=130" INT
trap "__pz_k=131" QUIT
trap "__pz_k=143" TERM
trap ":" USR1 USR2
__pz_u="${PRIZED_RUN_PATH-$PATH}"
unset PRIZED_RUN_PATH
echo "$$" > "$1/pid" || exit 1
date +%s%3N > "$1/start"
__pz_w=
if [ -n "$4" ]; then
  ( trap "" HUP INT QUIT TERM USR1 USR2
    __pz_end=$(($(date +%s) + $4 + 1))
    while [ ! -e "$1/exit" ]; do
      if [ "$(date +%s)" -ge "$__pz_end" ]; then
        mkdir "$1/limit" 2> /dev/null || [ ! -d "$1/limit" ] || exit 0
        true > "$1/timedout"
        kill -s TERM -- "-$$"
        sleep 5
        kill -s KILL -- "-$$"
        exit 0
      fi
      sleep 1
    done ) > /dev/null 2>&1 < /dev/null &
  __pz_w=$!
  echo "$__pz_w" > "$1/watchdog"
fi
if [ -n "$__pz_k" ]; then __pz_c=$__pz_k; else
PATH="$__pz_u" "$2" -c "$3" > "$1/stdout" 2> "$1/stderr" < /dev/null
__pz_c=$?
fi
if [ -n "$__pz_w" ] && { mkdir "$1/limit" 2> /dev/null || [ ! -d "$1/limit" ]; }; then ` +
	isRunTest("$__pz_w", `"prized-run $1 "`) + ` && kill -s KILL "$__pz_w" 2> /dev/null; wait "$__pz_w" 2> /dev/null; fi
date +%s%3N > "$1/end"
echo "$__pz_c" > "$1/exit.tmp" && mv -f "$1/exit.tmp" "$1/exit"`

func startScript(runID, command string, limit time.Duration, unbuffered bool) string {
	seconds := "''"
	if limit > 0 {
		seconds = strconv.Itoa(int(math.Min(math.Max(1, math.Ceil(limit.Seconds())), maxLimitSec)))
	}
	run := `prized-run "$__pz_d" "$__pz_s" ` + shellQuote(command) + ` ` + seconds + ` > /dev/null 2>&1 < /dev/null`
	lines := []string{
		"unset __pz_r __pz_d __pz_s __pz_w __pz_n",
		`export PRIZED_RUN_PATH="${PATH-}"`,
		"PATH=" + launchPath,
	}
	if unbuffered {
		lines = append(lines, `export PYTHONUNBUFFERED="${PYTHONUNBUFFERED-1}"`)
	}
	lines = append(lines,
		`__pz_r=`+shellQuote(runsDir)+`; __pz_d="$__pz_r/`+runID+`"`,
		`command -v setsid > /dev/null 2>&1 || { echo 'setsid is not installed in this sandbox' >&2; exit 97; }`,
		`mkdir -p "$__pz_r" || exit 98`,
		`if mkdir "$__pz_d" 2> /dev/null; then`,
		`  __pz_s=/bin/bash; [ -x "$__pz_s" ] || __pz_s=/bin/sh`,
		`  __pz_w='`+runWrapper+`'`,
		`  if setsid -f true > /dev/null 2>&1; then`,
		`    setsid -f "$__pz_s" -c "$__pz_w" `+run,
		`  else`,
		`    setsid "$__pz_s" -c "$__pz_w" `+run+` &`,
		`  fi`,
		`fi`,
		`if [ -f "$__pz_d/cleaned" ]; then echo 'this run was already cleaned up' >&2; exit 96; fi`,
		`__pz_n=0`,
		`while [ ! -s "$__pz_d/pid" ]; do`,
		`  __pz_n=$((__pz_n + 1))`,
		`  if [ "$__pz_n" -gt 200 ]; then echo 'the command did not start' >&2; exit `+strconv.Itoa(noPIDExit)+`; fi`,
		`  sleep 0.05`,
		`done`,
		`cat "$__pz_d/pid"`,
	)
	return strings.Join(lines, "\n")
}

func pollScript(runID string, outOffset, errOffset, maxChunk int) string {
	piece := func(name string, offset int, size string) []string {
		return []string{
			fmt.Sprintf(`__pz_n=$((%s - %d)); [ "$__pz_n" -gt %d ] && __pz_n=%d`, size, offset, maxChunk, maxChunk),
			fmt.Sprintf(`if [ "$__pz_n" -gt 0 ]; then tail -c +%d "$__pz_d/%s" | head -c "$__pz_n" | base64 -w 0; fi`, offset+1, name),
			`echo`,
		}
	}
	lines := []string{
		`__pz_d=` + shellQuote(runsDir+"/"+runID),
		`if [ ! -d "$__pz_d" ] || [ -f "$__pz_d/cleaned" ]; then echo "prized-run missing - 0 0 0 -"; exit 0; fi`,
		`if [ -f "$__pz_d/exit" ]; then __pz_st=done; __pz_c=$(cat "$__pz_d/exit")`,
		`elif __pz_p=$(cat "$__pz_d/pid" 2> /dev/null) && [ -n "$__pz_p" ] && kill -0 "$__pz_p" 2> /dev/null && ` + ours(runID) + `; then __pz_st=running; __pz_c=-`,
		`elif [ -f "$__pz_d/exit" ]; then __pz_st=done; __pz_c=$(cat "$__pz_d/exit")`,
		`else __pz_st=gone; __pz_c=-`,
		`fi`,
		`__pz_to=0; [ -e "$__pz_d/timedout" ] && __pz_to=1`,
		`__pz_du=-`,
		`if [ -f "$__pz_d/end" ]; then`,
		`  __pz_a=$(cat "$__pz_d/start" 2> /dev/null); __pz_b=$(cat "$__pz_d/end" 2> /dev/null)`,
		`  case "$__pz_a:$__pz_b" in *[!0-9:]* | :* | *:) ;; *) __pz_du=$((__pz_b - __pz_a)) ;; esac`,
		`fi`,
		`__pz_o=$(stat -c %s "$__pz_d/stdout" 2> /dev/null || echo 0)`,
		`__pz_e=$(stat -c %s "$__pz_d/stderr" 2> /dev/null || echo 0)`,
		`echo "prized-run $__pz_st $__pz_c $__pz_o $__pz_e $__pz_to $__pz_du"`,
	}
	lines = append(lines, piece("stdout", outOffset, "$__pz_o")...)
	lines = append(lines, piece("stderr", errOffset, "$__pz_e")...)
	return strings.Join(lines, "\n")
}

func ours(runID string) string {
	return isRunTest("$__pz_p", shellQuote("prized-run "+runsDir+"/"+runID+" "))
}

func killScript(runID, signal string) string {
	d := shellQuote(runsDir + "/" + runID)
	return strings.Join([]string{
		`__pz_p=$(cat ` + d + `/pid 2> /dev/null) && [ -n "$__pz_p" ] || exit 3`,
		`[ ! -f ` + d + `/exit ] && kill -0 "$__pz_p" 2> /dev/null && ` + ours(runID) + ` || exit 4`,
		`kill -s ` + signal + ` -- "-$__pz_p" 2> /dev/null || exit 4`,
	}, "\n")
}

func cleanupScript(runID string) string {
	d := shellQuote(runsDir + "/" + runID)
	runs := shellQuote(runsDir)
	marks := shellQuote(runsDir + "/run_*/cleaned")
	return strings.Join([]string{
		`if [ ! -f ` + d + `/exit ] && __pz_p=$(cat ` + d + `/pid 2> /dev/null) && [ -n "$__pz_p" ] && kill -0 "$__pz_p" 2> /dev/null && ` + ours(runID) + `; then exit 5; fi`,
		`find ` + runs + ` -mindepth 2 -maxdepth 2 -path ` + marks + ` -mmin +` + strconv.Itoa(cleanedKeepMin) + ` -printf '%h\0' 2> /dev/null | xargs -0 -r rm -rf --`,
		`find ` + runs + ` -mindepth 1 -maxdepth 1 -type d -name 'run_*' -empty -mmin +` + strconv.Itoa(cleanedKeepMin) + ` -delete 2> /dev/null`,
		`[ -d ` + d + ` ] || exit 0`,
		`find ` + d + ` -mindepth 1 -maxdepth 1 ! -name cleaned -exec rm -rf -- {} +`,
		`: > ` + d + `/cleaned`,
	}, "\n")
}

func abandonScript(runID string) string {
	d := shellQuote(runsDir + "/" + runID)
	runs := shellQuote(runsDir)
	return strings.Join([]string{
		`__pz_d=` + d,
		`mkdir -p ` + runs + ` 2> /dev/null`,
		`if { mkdir "$__pz_d" 2> /dev/null && true > "$__pz_d/cleaned"; } || [ -f "$__pz_d/cleaned" ]; then exit 0; fi`,
		`__pz_n=0`,
		`while [ ! -s "$__pz_d/pid" ]; do`,
		`  __pz_n=$((__pz_n + 1))`,
		`  if [ "$__pz_n" -gt 200 ]; then exit 3; fi`,
		`  sleep 0.05`,
		`done`,
		`__pz_p=$(cat "$__pz_d/pid")`,
		`if kill -0 "$__pz_p" 2> /dev/null && ` + ours(runID) + `; then kill -s KILL -- "-$__pz_p" 2> /dev/null; fi`,
		`find "$__pz_d" -mindepth 1 -maxdepth 1 ! -name cleaned -exec rm -rf -- {} +`,
		`: > "$__pz_d/cleaned"`,
		`exit 0`,
	}, "\n")
}
