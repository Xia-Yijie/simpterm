---
name: dev
description: Development guide for simpterm. Architecture, build, test, and known pitfalls for contributors.
allowed-tools: Bash, Read, Edit, Write, Glob, Grep
argument-hint: [topic]
---

# simpterm Development Guide

## Build & Install

```bash
pixi run compile                          # builds ./simpterm
cp ./simpterm ~/.local/bin/simpterm        # install
```

Go is managed by pixi, not the system. Use `pixi run go ...` for Go commands.

## Architecture

Single-file Go project (`main.go`, ~1200 lines). No frameworks, minimal dependencies.

### Key components

- **Daemon**: background process managing sessions, auto-starts on first use, exits after 10s idle with no sessions. Communicates via Unix socket at `$TMPDIR/simpterm-$UID/daemon.sock`.
- **Session**: wraps a PTY + shell process. Maintains a backlog (last 1MB of raw output) and a `vt10x.Terminal` virtual terminal for screen snapshots.
- **Protocol**: 4-byte big-endian length-prefixed JSON for request/response. Frame protocol (1-byte type + 4-byte length + payload) for streaming PTY data.

### Client commands → daemon handlers

| Client | Daemon handler | Connection |
|--------|---------------|------------|
| `cmdNew` | `handleNew` | closes after response |
| `cmdAttach` | `handleAttach` | stays open (bidirectional PTY stream) |
| `cmdExec` | `handleExec` | stays open (streams output until marker) |
| `cmdSend` | `handleSend` | closes after response |
| `cmdRead` | `handleRead` | closes after response |
| `cmdList` | `handleList` | closes after response |
| `cmdKill` | `handleKill` | closes after response |

## Critical pitfalls

### 1. Replacing binary requires daemon restart

The daemon is a long-running process. After `cp ./simpterm ~/.local/bin/simpterm`, the old daemon is still running the old binary. You MUST kill it:

```bash
# Find and kill the daemon
ps aux | grep "simpterm.*daemon" | grep -v grep   # get PID
kill <PID>
# Next simpterm command auto-starts new daemon
```

If you skip this, the new client may hang because of protocol mismatches with the old daemon. Stale socket files can also cause hangs — if the daemon is dead but the socket file remains, `connectDaemon` hangs. Remove the socket manually if needed:

```bash
rm $TMPDIR/simpterm-$(id -u)/daemon.sock
```

### 2. vt10x.Terminal thread safety

- `vt.Write()` internally acquires its own lock — safe to call from ptyReader goroutine.
- `vt.String()` also internally calls `Lock()`/`Unlock()` — do NOT wrap it with external `vt.Lock()`/`vt.Unlock()` or it will **deadlock** (sync.Mutex is not reentrant in Go).
- `vt.Resize()` does NOT acquire the lock — but it's only called from handleAttach (on attach and on resize frames), which is serialized per session.

### 3. exec marker mechanism

`handleExec` wraps the command as ` <cmd>\n echo '<marker>'\n` (leading space to avoid history). The client scans PTY output for the marker to detect completion. This does NOT work for TUI applications — use send + read instead.

### 4. Shell quoting in --cwd

The exec `--cwd` flag prepends `cd <path> && ` to the command. The path is quoted with `shellSingleQuote()` (POSIX single-quote escaping). Do NOT use Go's `%q` (backslash-escape quoting) — it's not shell-safe and vulnerable to injection.

### 5. Daemon stdio

`daemonMain()` closes stdin/stdout/stderr after starting the listener. This means you cannot see daemon errors in the terminal. For debugging, temporarily write to a log file:

```go
logf, _ := os.OpenFile("/tmp/simpterm-daemon.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
fmt.Fprintf(logf, "debug: %v\n", err)
```

## Testing changes

After making changes:

```bash
pixi run compile
# Kill existing daemon
kill $(ps aux | grep "simpterm.*__daemon" | grep -v grep | awk '{print $2}') 2>/dev/null
# Install
cp ./simpterm ~/.local/bin/simpterm
# Test
simpterm n test
simpterm e test 5 "echo hello"
simpterm r test
simpterm k test
```
