package provisioner

import (
	"crypto/rand"
	"encoding/hex"
	"math"
	"strconv"
	"time"
)

// WPCLIKillGrace is how long WP-CLI and its children get to end after
// SIGTERM, when the call's deadline passes or its context ends, before the
// rest are killed.
const WPCLIKillGrace = 10 * time.Second

// Closing an exec stream, or killing the docker CLI, doesn't stop the
// process the container runtime started, so a driver's ExecWPCLI has to
// stop WP-CLI from inside the container, and has to stop its children too.
// GNU timeout(1) doesn't: it sends SIGKILL after --kill-after only while
// its own child runs, so a grandchild that ignores SIGTERM outlives it.
//
// The run script below starts WP-CLI in a session of its own, so that
// WP-CLI and everything it starts form one process group whose ID is the
// process's PID; it records that PID in a file named for the call. Three
// things stop the group, TERM first and KILL after the grace period:
//
//   - the script's own timer, when the deadline passes (the backstop
//     that needs no connection to the container);
//   - the stop script, which the driver runs in a second exec when the
//     call's context ends before the command does;
//   - the end of the command itself, which kills what is left, so that
//     nothing outlives the call, and no orphan holds the output stream
//     open.
//
// Both scripts are plain POSIX sh (the php image has dash, not bash) and
// constant: the call's arguments, which may hold secrets, are argv of the
// run script, and its standard input is passed through untouched.

// wpcliStopFunc stops the process group of $1: TERM, then KILL after $2
// seconds if members remain.
const wpcliStopFunc = `stop() {
	kill -s TERM -- "-$1" 2>/dev/null || return 0
	n=0
	while kill -s 0 -- "-$1" 2>/dev/null && [ "$n" -lt "$2" ]; do sleep 1; n=$((n+1)); done
	kill -s KILL -- "-$1" 2>/dev/null
	return 0
}
`

// wpcliRunScript runs "$@" (WP-CLI and its arguments) after the arguments
// id, seconds until the deadline, and the grace period in seconds.
//
// fd 3 keeps the exec's standard input: a background command without job
// control would otherwise get /dev/null. setsid doesn't fork here, as the
// script's background job isn't a group leader, so $! is the process, and
// the group, of WP-CLI. The timer ends when the script does, so it leaves
// at most a one-second sleep behind; its output is redirected so that
// sleep can't hold the exec stream open.
const wpcliRunScript = wpcliStopFunc + `id=$1 secs=$2 grace=$3
shift 3
pf=/tmp/.pressfleet-wpcli.$id
exec 3<&0
setsid "$@" <&3 3<&- &
pid=$!
echo "$pid" > "$pf"
(
	s=0
	while [ "$s" -lt "$secs" ] && kill -0 "$$" 2>/dev/null; do sleep 1; s=$((s+1)); done
	[ "$s" -ge "$secs" ] && stop "$pid" "$grace"
) >/dev/null 2>&1 </dev/null &
wait "$pid"
rc=$?
kill -s KILL -- "-$pid" 2>/dev/null
rm -f "$pf"
exit "$rc"
`

// wpcliStopScript stops the call whose id is $1, with grace period $2
// seconds. The run script writes the file as soon as WP-CLI is forked,
// before the process has made its group, so it waits a few seconds for the
// file and then for the group; if they never come, the call never started
// or has ended, and there is nothing to stop.
const wpcliStopScript = wpcliStopFunc + `pf=/tmp/.pressfleet-wpcli.$1
i=0 pid=
while [ "$i" -lt 20 ]; do
	read -r pid < "$pf" 2>/dev/null
	case $pid in ''|*[!0-9]*) pid=;; *) kill -s 0 -- "-$pid" 2>/dev/null && break;; esac
	sleep 0.25; i=$((i+1))
done
[ "$i" -lt 20 ] && stop "$pid" "$2"
exit 0
`

// NewWPCLICallID returns an identifier for one WP-CLI call, which the run
// and stop commands share.
func NewWPCLICallID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // Never fails on the platforms Go supports.
	return hex.EncodeToString(b[:])
}

// WPCLIRunArgv is the command, for the php container, that runs argv (WP-CLI
// and its arguments) so that WPCLIStopArgv with the same id can stop it,
// and so that it stops itself after timeout (rounded up, plus a second,
// so that the caller's own deadline passes first). Standard input is
// WP-CLI's.
func WPCLIRunArgv(id string, timeout time.Duration, argv ...string) []string {
	secs := int(math.Ceil(max(timeout, 0).Seconds())) + 1
	cmd := []string{"sh", "-c", wpcliRunScript, "pressfleet-wpcli", id,
		strconv.Itoa(secs), strconv.Itoa(int(WPCLIKillGrace.Seconds()))}
	return append(cmd, argv...)
}

// WPCLIStopArgv is the command, for the php container, that stops the call
// WPCLIRunArgv made with id. It takes up to the grace period and a few
// seconds more to return.
func WPCLIStopArgv(id string) []string {
	return []string{"sh", "-c", wpcliStopScript, "pressfleet-wpcli-stop", id,
		strconv.Itoa(int(WPCLIKillGrace.Seconds()))}
}

// WPCLIStopTimeout bounds the stop command, which takes the grace period
// at most, after the wait for the call's pid file.
const WPCLIStopTimeout = WPCLIKillGrace + 15*time.Second
