---
name: simpterm
description: Execute commands in a persistent, stateful terminal session using simpterm. Use when you need to run shell commands that share state (environment variables, working directory, running processes) across multiple invocations, or when you need a background terminal session.
allowed-tools: Bash
argument-hint: <subcommand> [args...]
---

# simpterm - Persistent Terminal Sessions

simpterm is a minimal terminal multiplexer. It provides persistent background shell sessions that survive detach/reattach. Make sure `simpterm` is in your PATH.

## Commands

| Command | Description |
|---------|-------------|
| `simpterm n [name] [--cwd <dir>]` | Create a new session (name optional, no pure numeric names) |
| `simpterm a <name\|id>` | Attach to a session (interactive, use Ctrl+\ to detach) |
| `simpterm e <name\|id> <timeout> [--cwd <dir>] <cmd>` | Execute a command and stream output (marker-based completion) |
| `simpterm s <name\|id> <input>` | Send raw input to a session (no wait, fire-and-forget) |
| `simpterm r <name\|id>` | Read current screen content (virtual terminal snapshot) |
| `simpterm l` | List all active sessions |
| `simpterm k <name\|id>` | Kill a session |

## How to use (for AI)

### exec: for normal shell commands

The `e` (exec) command runs a command in a named session, waits for completion (with marker-based detection), and streams stdout.

```bash
# 1. Create a persistent session
simpterm n work --cwd /some/project

# 2. Execute commands (state preserved between calls)
simpterm e work 30 "export FOO=bar"
simpterm e work 30 "echo $FOO && pwd"

# 3. Run builds/tests with longer timeouts
simpterm e work 120 "make -j4"

# 4. Use --cwd to cd before running
simpterm e work 60 --cwd /other/project "cargo test"

# 5. Clean up
simpterm k work
```

### send + read: for TUI / interactive applications

The `s` (send) + `r` (read) pair is for interacting with TUI applications (vim, htop, fzf, etc.) where exec's marker mechanism doesn't work.

```bash
# Launch a TUI
simpterm e work 5 "htop" &   # or use send
simpterm s work "htop\n"

# Wait for it to render, then read the screen
sleep 1
simpterm r work              # returns plain text screen snapshot

# Send keystrokes
simpterm s work "q"          # quit htop

# Send special keys (use ANSI escape sequences)
simpterm s work $'\x03'      # Ctrl+C
simpterm s work $'\x1b[A'    # arrow up
simpterm s work $'\n'        # Enter
```

### Important notes

- **State persists**: cd, export, shell variables, background jobs all carry over between `e` calls
- **Timeout**: second argument to `e` is timeout in seconds. Exit code 124 on timeout (same as `timeout` command), exit code 1 for other errors
- **Output contains escape sequences**: raw PTY output from `e` includes ANSI colors and prompt strings. Use `r` (read) for clean plain-text screen snapshots
- **No interactive commands via exec**: do not run vim, htop, or anything requiring stdin via `e`. Use `s` (send) + `r` (read) instead
- **One exec at a time**: a session cannot handle concurrent `e` calls, but `s` and `r` can be used anytime
- **Daemon auto-manages**: daemon starts on first use, exits after 10s idle with no sessions

## Argument handling

When invoked as `/simpterm <args>`, pass `$ARGUMENTS` directly:

```bash
simpterm $ARGUMENTS
```
