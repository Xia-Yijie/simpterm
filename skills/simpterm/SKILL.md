---
name: simpterm
description: Execute commands in a persistent, stateful terminal session using simpterm. Use when you need to run shell commands that share state (environment variables, working directory, running processes) across multiple invocations, or when you need a background terminal session.
allowed-tools: Bash
argument-hint: <subcommand> [args...]
---

# simpterm - Persistent Terminal Sessions

simpterm is a minimal terminal multiplexer. It provides persistent background shell sessions that survive detach/reattach. Make sure `simpterm` is in your PATH (run `make` to build if needed).

## Commands

| Command | Description |
|---------|-------------|
| `simpterm n [name]` | Create a new session (name optional, no pure numeric names) |
| `simpterm a <name\|id>` | Attach to a session (interactive, use Ctrl+\ to detach) |
| `simpterm e <name\|id> <timeout> <cmd>` | Execute a command and stream output (best for AI use) |
| `simpterm l` | List all active sessions |
| `simpterm k <name\|id>` | Kill a session |

## How to use (for AI)

The `e` (exec) command is designed for AI tool use. It runs a command in a named session, waits for completion (with marker-based detection), and streams stdout.

### Typical workflow

```bash
# 1. Create a persistent session
simpterm n work

# 2. Execute commands in it (state is preserved between calls)
simpterm e work 30 "cd /some/project"
simpterm e work 30 "export FOO=bar"
simpterm e work 30 "echo $FOO && pwd"   # prints "bar" and "/some/project"

# 3. Run builds, tests, etc. with longer timeouts
simpterm e work 120 "make -j4"
simpterm e work 60 "python test.py"

# 4. Clean up when done
simpterm k work
```

### Important notes

- **State persists**: cd, export, shell variables, background jobs all carry over between `e` calls
- **Timeout**: second argument to `e` is timeout in seconds; if exceeded, returns with error
- **Output contains escape sequences**: raw PTY output includes ANSI colors, prompt strings, command echo, and bracketed paste markers. Parse accordingly.
- **No interactive commands**: do not run sudo, vim, less, or anything requiring stdin interaction via `e`. Use `a` (attach) for interactive use.
- **One exec at a time**: a session cannot handle concurrent `e` calls
- **Daemon auto-manages**: daemon starts on first use, exits after 10s idle with no sessions

## Argument handling

When invoked as `/simpterm <args>`, pass `$ARGUMENTS` directly:

```bash
simpterm $ARGUMENTS
```
