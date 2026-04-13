package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	ptylib "github.com/creack/pty"
	"github.com/hinshun/vt10x"
	"golang.org/x/term"
)

// --- Protocol ---

const (
	frameData   byte = 1
	frameResize byte = 2
	frameExit   byte = 3
)

const (
	backlogLimit = 1024 * 1024
	maxPayload   = 4096
	idleTimeout  = 10 * time.Second
)

var version = "2026-04-13"

type Request struct {
	Cmd     string `json:"cmd"`
	Name    string `json:"name,omitempty"`
	ID      int    `json:"id,omitempty"`
	Rows    uint16 `json:"rows,omitempty"`
	Cols    uint16 `json:"cols,omitempty"`
	Command string `json:"command,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
}

type Response struct {
	Status int    `json:"status"`
	ID     int    `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	PID    int    `json:"pid,omitempty"`
	Msg    string `json:"msg,omitempty"`
	Marker string `json:"marker,omitempty"`
}

type SessionInfo struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	PID  int    `json:"pid"`
	Exit string `json:"exit,omitempty"`
}

type ListResponse struct {
	Status   int           `json:"status"`
	Sessions []SessionInfo `json:"sessions"`
	Reaped   []SessionInfo `json:"reaped,omitempty"`
}

type ReadResponse struct {
	Status int    `json:"status"`
	Msg    string `json:"msg,omitempty"`
	Screen string `json:"screen,omitempty"`
}

// Wire helpers: 4-byte big-endian length + JSON payload

func sendJSON(conn net.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := conn.Write(hdr[:]); err != nil {
		return err
	}
	_, err = conn.Write(data)
	return err
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func promptInjectCommand(shell, sessionName string) string {
	name := shellSingleQuote("[" + sessionName + "]")

	switch filepath.Base(shell) {
	case "zsh":
		return fmt.Sprintf(
			" PROMPT=$'%%{\\e[01;33m%%}'%s$'%%{\\e[00m%%} '\"$PROMPT\"; clear\n",
			name,
		)
	case "bash":
		return fmt.Sprintf(
			" PS1=$'\\\\[\\e[01;33m\\\\]'%s$'\\\\[\\e[00m\\\\] '\"$PS1\"; clear\n",
			name,
		)
	default:
		return fmt.Sprintf(
			" PS1=$'\\e[01;33m'%s$'\\e[00m '\"$PS1\"; clear\n",
			name,
		)
	}
}

func recvJSON(conn net.Conn, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > 1<<20 {
		return fmt.Errorf("message too large: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}

// Frame helpers: 1-byte type + 4-byte length + payload

func sendFrame(conn net.Conn, typ byte, data []byte) error {
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(data)))
	if _, err := conn.Write(hdr[:]); err != nil {
		return err
	}
	if len(data) > 0 {
		_, err := conn.Write(data)
		return err
	}
	return nil
}

func recvFrame(conn net.Conn) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return 0, nil, err
	}
	typ := hdr[0]
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxPayload {
		return 0, nil, fmt.Errorf("frame too large: %d", n)
	}
	if n == 0 {
		return typ, nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return 0, nil, err
	}
	return typ, buf, nil
}

// --- Session ---

type Session struct {
	ID      int
	Name    string
	PtyFile *os.File
	Cmd     *exec.Cmd

	mu       sync.Mutex
	client   net.Conn
	execConn net.Conn

	backlogMu sync.Mutex
	backlog   []byte

	vt   vt10x.Terminal
	done chan struct{}
}

func (s *Session) pid() int {
	if s.Cmd.Process != nil {
		return s.Cmd.Process.Pid
	}
	return 0
}

func (s *Session) appendBacklog(data []byte) {
	s.backlogMu.Lock()
	defer s.backlogMu.Unlock()
	s.backlog = append(s.backlog, data...)
	if len(s.backlog) > backlogLimit {
		s.backlog = s.backlog[len(s.backlog)-backlogLimit:]
	}
}

func (s *Session) setClient(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.client = conn
}

func (s *Session) getClient() net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func (s *Session) detachClient() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
}

func (s *Session) setExec(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execConn = conn
}

func (s *Session) getExec() net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execConn
}

func (s *Session) detachExec() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.execConn != nil {
		s.execConn.Close()
		s.execConn = nil
	}
}

// ptyReader runs in a goroutine, reads PTY output and forwards to clients.
func (s *Session) ptyReader() {
	defer close(s.done)
	buf := make([]byte, maxPayload)
	for {
		n, err := s.PtyFile.Read(buf)
		if n > 0 {
			data := buf[:n]
			s.appendBacklog(data)
			s.vt.Write(data)
			if c := s.getClient(); c != nil {
				if sendFrame(c, frameData, data) != nil {
					s.detachClient()
				}
			}
			if c := s.getExec(); c != nil {
				if sendFrame(c, frameData, data) != nil {
					s.detachExec()
				}
			}
		}
		if err != nil {
			// Notify connected clients
			if c := s.getClient(); c != nil {
				sendFrame(c, frameExit, nil)
			}
			if c := s.getExec(); c != nil {
				sendFrame(c, frameExit, nil)
			}
			return
		}
	}
}

// --- Daemon ---

type Daemon struct {
	mu       sync.Mutex
	sessions map[int]*Session
	nextID   int
	listener net.Listener
}

func newDaemon(listener net.Listener) *Daemon {
	return &Daemon{
		sessions: make(map[int]*Session),
		nextID:   1,
		listener: listener,
	}
}

func (d *Daemon) findSession(name string, id int) *Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id > 0 {
		if s, ok := d.sessions[id]; ok {
			return s
		}
	}
	if name != "" {
		for _, s := range d.sessions {
			if s.Name == name {
				return s
			}
		}
	}
	return nil
}

func (d *Daemon) removeSession(id int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.sessions, id)
}

// teardown closes any client/exec connections and removes the session from
// the daemon's map. Safe to call multiple times; each step is idempotent.
func (d *Daemon) teardown(s *Session) {
	s.detachClient()
	s.detachExec()
	d.removeSession(s.ID)
}

// markDead snapshots SessionInfo (with exit status) for a session whose
// process has already exited, then tears it down. Used when we detect a
// session whose cleanup goroutine did not (yet) remove the entry — e.g.
// because the PTY Read stayed blocked on grandchildren holding the slave.
func (d *Daemon) markDead(s *Session) SessionInfo {
	info := SessionInfo{
		ID:   s.ID,
		Name: s.Name,
		PID:  s.pid(),
		Exit: s.Cmd.ProcessState.String(),
	}
	d.teardown(s)
	return info
}

func (d *Daemon) sessionCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sessions)
}

func (d *Daemon) handleNew(conn net.Conn, req Request) {
	name := req.Name
	if name != "" && isNumeric(name) {
		sendJSON(conn, Response{Status: -1, Msg: "session name cannot be purely numeric"})
		return
	}
	if name != "" && d.findSession(name, 0) != nil {
		sendJSON(conn, Response{Status: -1, Msg: "session name already exists"})
		return
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	// Determine session name early for env setup
	sessionName := name
	if sessionName == "" {
		sessionName = fmt.Sprintf("s%d", d.nextID)
	}

	cmd := exec.Command(shell)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	env := os.Environ()
	env = append(env, "SIMPTERM_SESSION="+sessionName)
	cmd.Env = env

	rows, cols := uint16(24), uint16(80)
	if req.Rows > 0 {
		rows = req.Rows
	}
	if req.Cols > 0 {
		cols = req.Cols
	}

	ptmx, err := ptylib.StartWithSize(cmd, &ptylib.Winsize{
		Rows: rows, Cols: cols,
	})
	if err != nil {
		sendJSON(conn, Response{Status: -1, Msg: fmt.Sprintf("forkpty failed: %v", err)})
		return
	}

	d.mu.Lock()
	id := d.nextID
	d.nextID++
	if name == "" {
		name = fmt.Sprintf("s%d", id)
	}
	s := &Session{
		ID:      id,
		Name:    name,
		PtyFile: ptmx,
		Cmd:     cmd,
		vt:      vt10x.New(vt10x.WithSize(int(cols), int(rows))),
		done:    make(chan struct{}),
	}
	d.sessions[id] = s
	d.mu.Unlock()

	go s.ptyReader()

	// Inject PS1 prefix after shell starts to show session name in prompt
	go func() {
		time.Sleep(200 * time.Millisecond)
		inject := promptInjectCommand(shell, sessionName)
		s.PtyFile.Write([]byte(inject))
	}()

	go func() {
		s.Cmd.Wait()
		s.PtyFile.Close()
		// Wait for ptyReader to finish, but don't block forever: orphaned
		// grandchildren may keep the PTY slave open, preventing the master
		// Read from returning even after we close our end. Without a timeout
		// the session would stay listed indefinitely after its process is gone.
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
		}
		d.teardown(s)
	}()

	sendJSON(conn, Response{Status: 0, ID: id, Name: name, PID: s.pid()})
}

func (d *Daemon) handleList(conn net.Conn) {
	d.mu.Lock()
	var dead []*Session
	var infos []SessionInfo
	for _, s := range d.sessions {
		if s.Cmd.ProcessState != nil {
			dead = append(dead, s)
			continue
		}
		infos = append(infos, SessionInfo{ID: s.ID, Name: s.Name, PID: s.pid()})
	}
	d.mu.Unlock()
	var reaped []SessionInfo
	for _, s := range dead {
		reaped = append(reaped, d.markDead(s))
	}
	sendJSON(conn, ListResponse{Status: 0, Sessions: infos, Reaped: reaped})
}

// sessionMembers returns the pids of all processes whose session id equals
// sid (as reported by /proc/PID/stat field 6). The shell we spawned with
// Setsid is a session leader, so every descendant — including backgrounded
// jobs that bash's job control moved to a new pgid — still shares this sid.
// Returns nil if /proc is unavailable (non-Linux).
func sessionMembers(sid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// comm (field 2) is parenthesized and may contain spaces/parens;
		// skip past the final ')' to find the remaining space-separated fields.
		idx := bytes.LastIndexByte(data, ')')
		if idx < 0 || idx+2 >= len(data) {
			continue
		}
		fields := strings.Fields(string(data[idx+2:]))
		// After comm: state, ppid, pgrp, session, ...
		if len(fields) < 4 {
			continue
		}
		s, err := strconv.Atoi(fields[3])
		if err != nil {
			continue
		}
		if s == sid {
			pids = append(pids, pid)
		}
	}
	return pids
}

func (d *Daemon) handleKill(conn net.Conn, req Request) {
	s := d.findSession(req.Name, req.ID)
	if s == nil {
		sendJSON(conn, Response{Status: -1, Msg: "session not found"})
		return
	}
	if s.Cmd.Process != nil {
		// The shell was started with Setsid, so its pid is the session id
		// and every descendant (including bash-job-control'd grandchildren
		// in their own pgids) still reports this as their session.
		sid := s.Cmd.Process.Pid
		members := sessionMembers(sid)
		if len(members) == 0 {
			// /proc unavailable; fall back to the shell's process group.
			members = []int{-sid}
		}
		for _, p := range members {
			syscall.Kill(p, syscall.SIGHUP)
			syscall.Kill(p, syscall.SIGTERM)
		}
		select {
		case <-s.done:
			// Clean exit: PTY reached EOF, so every fd holding the slave
			// was closed — no survivors to SIGKILL.
		case <-time.After(500 * time.Millisecond):
			for _, p := range members {
				syscall.Kill(p, syscall.SIGKILL)
			}
		}
	}
	sendJSON(conn, Response{Status: 0, ID: s.ID, Name: s.Name})
}

func (d *Daemon) handleDetach(conn net.Conn, req Request) {
	s := d.findSession(req.Name, req.ID)
	if s == nil {
		sendJSON(conn, Response{Status: -1, Msg: "session not found"})
		return
	}
	if s.getClient() == nil {
		sendJSON(conn, Response{Status: -1, Msg: "session not attached"})
		return
	}
	s.detachClient()
	sendJSON(conn, Response{Status: 0, ID: s.ID, Name: s.Name})
}

func (d *Daemon) handleAttach(conn net.Conn, req Request) {
	s := d.findSession(req.Name, req.ID)
	if s == nil {
		sendJSON(conn, Response{Status: -1, Msg: "session not found"})
		return
	}
	if s.Cmd.ProcessState != nil {
		info := d.markDead(s)
		sendJSON(conn, Response{Status: -1, Msg: fmt.Sprintf("session %d (%s) died: %s, removed", info.ID, info.Name, info.Exit)})
		return
	}
	if s.getClient() != nil {
		sendJSON(conn, Response{Status: -1, Msg: "session already attached"})
		return
	}

	// Resize PTY and virtual terminal
	if req.Rows > 0 && req.Cols > 0 {
		ptylib.Setsize(s.PtyFile, &ptylib.Winsize{Rows: req.Rows, Cols: req.Cols})
		s.vt.Resize(int(req.Cols), int(req.Rows))
	}

	if err := sendJSON(conn, Response{Status: 0, ID: s.ID, Name: s.Name, PID: s.pid()}); err != nil {
		return
	}

	s.setClient(conn)

	// Flush backlog
	s.backlogMu.Lock()
	bl := make([]byte, len(s.backlog))
	copy(bl, s.backlog)
	s.backlogMu.Unlock()

	for off := 0; off < len(bl); {
		end := off + maxPayload
		if end > len(bl) {
			end = len(bl)
		}
		if sendFrame(conn, frameData, bl[off:end]) != nil {
			s.detachClient()
			return
		}
		off = end
	}

	// Read client input in this goroutine (connection handler)
	go func() {
		defer s.detachClient()
		for {
			typ, data, err := recvFrame(conn)
			if err != nil {
				return
			}
			switch typ {
			case frameData:
				s.PtyFile.Write(data)
			case frameResize:
				if len(data) == 4 {
					rows := binary.BigEndian.Uint16(data[0:2])
					cols := binary.BigEndian.Uint16(data[2:4])
					ptylib.Setsize(s.PtyFile, &ptylib.Winsize{Rows: rows, Cols: cols})
					s.vt.Resize(int(cols), int(rows))
				}
			}
		}
	}()
}

func generateMarker() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("__SIMPTERM_DONE_%x__", b)
}

func (d *Daemon) handleExec(conn net.Conn, req Request) {
	s := d.findSession(req.Name, req.ID)
	if s == nil {
		sendJSON(conn, Response{Status: -1, Msg: "session not found"})
		return
	}
	if s.getExec() != nil {
		sendJSON(conn, Response{Status: -1, Msg: "session has pending exec"})
		return
	}
	if req.Command == "" {
		sendJSON(conn, Response{Status: -1, Msg: "empty command"})
		return
	}

	marker := generateMarker()
	wrapped := fmt.Sprintf(" %s\n echo '%s'\n", req.Command, marker)

	if err := sendJSON(conn, Response{Status: 0, ID: s.ID, Name: s.Name, PID: s.pid(), Marker: marker}); err != nil {
		return
	}

	// Set exec AFTER response is sent to avoid race with ptyReader
	s.setExec(conn)
	s.PtyFile.Write([]byte(wrapped))

	// Monitor exec connection for disconnect
	go func() {
		buf := make([]byte, 1)
		for {
			_, err := conn.Read(buf)
			if err != nil {
				s.detachExec()
				return
			}
		}
	}()
}

func (d *Daemon) handleSend(conn net.Conn, req Request) {
	s := d.findSession(req.Name, req.ID)
	if s == nil {
		sendJSON(conn, Response{Status: -1, Msg: "session not found"})
		return
	}
	if req.Command == "" {
		sendJSON(conn, Response{Status: -1, Msg: "empty input"})
		return
	}
	if _, err := s.PtyFile.Write([]byte(req.Command)); err != nil {
		sendJSON(conn, Response{Status: -1, Msg: fmt.Sprintf("write failed: %v", err)})
		return
	}
	sendJSON(conn, Response{Status: 0, ID: s.ID, Name: s.Name})
}

func (d *Daemon) handleRead(conn net.Conn, req Request) {
	s := d.findSession(req.Name, req.ID)
	if s == nil {
		sendJSON(conn, ReadResponse{Status: -1, Msg: "session not found"})
		return
	}
	screen := s.vt.String()
	sendJSON(conn, ReadResponse{Status: 0, Screen: screen})
}

func (d *Daemon) handleConn(conn net.Conn) {
	var req Request
	if err := recvJSON(conn, &req); err != nil {
		conn.Close()
		return
	}

	switch req.Cmd {
	case "new":
		d.handleNew(conn, req)
		conn.Close()
	case "list":
		d.handleList(conn)
		conn.Close()
	case "kill":
		d.handleKill(conn, req)
		conn.Close()
	case "detach":
		d.handleDetach(conn, req)
		conn.Close()
	case "attach":
		d.handleAttach(conn, req)
		// conn stays open, managed by session
	case "exec":
		d.handleExec(conn, req)
		// conn stays open, managed by session
	case "send":
		d.handleSend(conn, req)
		conn.Close()
	case "read":
		d.handleRead(conn, req)
		conn.Close()
	default:
		conn.Close()
	}
}

func (d *Daemon) run() {
	for {
		if d.sessionCount() == 0 {
			d.listener.(*net.UnixListener).SetDeadline(time.Now().Add(idleTimeout))
		} else {
			d.listener.(*net.UnixListener).SetDeadline(time.Time{})
		}

		conn, err := d.listener.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if d.sessionCount() == 0 {
					return // idle exit
				}
				continue
			}
			return
		}
		go d.handleConn(conn)
	}
}

// --- Paths ---

func runtimeDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("simpterm-%d", os.Getuid()))
}

func socketPath() string {
	return filepath.Join(runtimeDir(), "daemon.sock")
}

func ensureRuntimeDir() {
	dir := runtimeDir()
	os.MkdirAll(dir, 0700)
	os.Chmod(dir, 0700)
}

// --- Daemon lifecycle ---

func connectDaemon() (net.Conn, error) {
	return net.DialTimeout("unix", socketPath(), 2*time.Second)
}

func spawnDaemon() {
	exe, err := os.Executable()
	if err != nil {
		die("cannot find executable: %v", err)
	}
	cmd := exec.Command(exe, "__daemon")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		die("spawn daemon failed: %v", err)
	}
	cmd.Process.Release()
}

func ensureDaemon() {
	conn, err := connectDaemon()
	if err == nil {
		conn.Close()
		return
	}
	spawnDaemon()
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		conn, err = connectDaemon()
		if err == nil {
			conn.Close()
			return
		}
	}
	die("failed to start daemon")
}

func daemonMain() {
	ensureRuntimeDir()
	path := socketPath()
	os.Remove(path)

	listener, err := net.Listen("unix", path)
	if err != nil {
		os.Exit(1)
	}
	defer listener.Close()
	defer os.Remove(path)

	// Detach stdio
	os.Stdin.Close()
	os.Stdout.Close()
	os.Stderr.Close()

	d := newDaemon(listener)
	d.run()
}

// --- Client commands ---

func getWinsize() (uint16, uint16) {
	w, h, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil {
		return 80, 24
	}
	return uint16(h), uint16(w)
}

func cmdNew(name string, cwd string) {
	ensureDaemon()
	conn, err := connectDaemon()
	if err != nil {
		die("connect daemon failed: %v", err)
	}
	defer conn.Close()

	rows, cols := getWinsize()
	req := Request{Cmd: "new", Name: name, Rows: rows, Cols: cols, Cwd: cwd}
	if err := sendJSON(conn, req); err != nil {
		die("send failed: %v", err)
	}
	var resp Response
	if err := recvJSON(conn, &resp); err != nil {
		die("daemon request failed")
	}
	if resp.Status < 0 {
		die("%s", resp.Msg)
	}
	fmt.Printf("%s\t%d\n", resp.Name, resp.ID)
}

func cmdList() {
	ensureDaemon()
	conn, err := connectDaemon()
	if err != nil {
		die("connect daemon failed: %v", err)
	}
	defer conn.Close()

	if err := sendJSON(conn, Request{Cmd: "list"}); err != nil {
		die("send failed: %v", err)
	}
	var resp ListResponse
	if err := recvJSON(conn, &resp); err != nil {
		die("daemon request failed")
	}
	for _, s := range resp.Reaped {
		fmt.Fprintf(os.Stderr, "session %d (%s) died: %s, removed\n", s.ID, s.Name, s.Exit)
	}
	fmt.Println("ID\tNAME\tPID")
	for _, s := range resp.Sessions {
		fmt.Printf("%d\t%s\t%d\n", s.ID, s.Name, s.PID)
	}
}

func cmdKill(target string) {
	simpleCmd("kill", target)
}

func cmdDetach(target string) {
	simpleCmd("detach", target)
}

func simpleCmd(cmd, target string) {
	ensureDaemon()
	conn, err := connectDaemon()
	if err != nil {
		die("connect daemon failed: %v", err)
	}
	defer conn.Close()

	req := Request{Cmd: cmd}
	if isNumeric(target) {
		req.ID, _ = strconv.Atoi(target)
	} else {
		req.Name = target
	}
	if err := sendJSON(conn, req); err != nil {
		die("send failed: %v", err)
	}
	var resp Response
	if err := recvJSON(conn, &resp); err != nil {
		die("daemon request failed")
	}
	if resp.Status < 0 {
		die("%s", resp.Msg)
	}
}

func cmdAttach(target string) {
	ensureDaemon()
	conn, err := connectDaemon()
	if err != nil {
		die("connect daemon failed: %v", err)
	}

	rows, cols := getWinsize()
	req := Request{Cmd: "attach", Rows: rows, Cols: cols}
	if isNumeric(target) {
		req.ID, _ = strconv.Atoi(target)
	} else {
		req.Name = target
	}
	if err := sendJSON(conn, req); err != nil {
		conn.Close()
		die("send failed: %v", err)
	}
	var resp Response
	if err := recvJSON(conn, &resp); err != nil {
		conn.Close()
		die("daemon request failed")
	}
	if resp.Status < 0 {
		conn.Close()
		die("%s", resp.Msg)
	}

	// Enter raw mode
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		conn.Close()
		die("failed to set raw mode: %v", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// Handle signals for clean restore
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT)
	go func() {
		<-sigCh
		term.Restore(int(os.Stdin.Fd()), oldState)
		conn.Close()
		os.Exit(0)
	}()

	// Handle SIGWINCH
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	go func() {
		for range winchCh {
			r, c := getWinsize()
			var buf [4]byte
			binary.BigEndian.PutUint16(buf[0:2], r)
			binary.BigEndian.PutUint16(buf[2:4], c)
			sendFrame(conn, frameResize, buf[:])
		}
	}()

	// Send initial resize
	{
		var buf [4]byte
		binary.BigEndian.PutUint16(buf[0:2], rows)
		binary.BigEndian.PutUint16(buf[2:4], cols)
		sendFrame(conn, frameResize, buf[:])
	}

	done := make(chan struct{})

	// Read from daemon -> stdout
	go func() {
		defer close(done)
		for {
			typ, data, err := recvFrame(conn)
			if err != nil || typ == frameExit {
				return
			}
			if typ == frameData {
				os.Stdout.Write(data)
			}
		}
	}()

	// Read stdin -> daemon (filter Ctrl+\)
	go func() {
		buf := make([]byte, maxPayload)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				conn.Close()
				return
			}
			// Scan for detach key (Ctrl+\, 0x1c)
			for i := 0; i < n; i++ {
				if buf[i] == 0x1c {
					if i > 0 {
						sendFrame(conn, frameData, buf[:i])
					}
					conn.Close()
					return
				}
			}
			sendFrame(conn, frameData, buf[:n])
		}
	}()

	<-done
}

func cmdExec(target string, timeoutSec int, command string) {
	ensureDaemon()
	conn, err := connectDaemon()
	if err != nil {
		die("connect daemon failed: %v", err)
	}

	req := Request{Cmd: "exec", Command: command}
	if isNumeric(target) {
		req.ID, _ = strconv.Atoi(target)
	} else {
		req.Name = target
	}
	if err := sendJSON(conn, req); err != nil {
		conn.Close()
		die("send failed: %v", err)
	}
	var resp Response
	if err := recvJSON(conn, &resp); err != nil {
		conn.Close()
		die("daemon request failed")
	}
	if resp.Status < 0 {
		conn.Close()
		die("%s", resp.Msg)
	}

	marker := []byte(resp.Marker)
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)

	var scanBuf []byte

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			fmt.Fprintf(os.Stderr, "\nsimpterm: exec timed out\n")
			conn.Close()
			os.Exit(124)
		}
		conn.SetReadDeadline(time.Now().Add(remaining))

		typ, data, err := recvFrame(conn)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				fmt.Fprintf(os.Stderr, "\nsimpterm: exec timed out\n")
				conn.Close()
				os.Exit(124)
			}
			break
		}
		if typ == frameExit {
			break
		}
		if typ != frameData {
			continue
		}

		scanBuf = append(scanBuf, data...)

		if idx := bytes.Index(scanBuf, marker); idx >= 0 {
			// Find start of marker line
			lineStart := idx
			for lineStart > 0 && scanBuf[lineStart-1] != '\n' {
				lineStart--
			}
			if lineStart > 0 {
				os.Stdout.Write(scanBuf[:lineStart])
			}
			conn.Close()
			return
		}

		// Flush data that can't contain the marker
		keep := len(marker) - 1
		if keep < 0 {
			keep = 0
		}
		if len(scanBuf) > keep {
			flush := len(scanBuf) - keep
			os.Stdout.Write(scanBuf[:flush])
			scanBuf = scanBuf[flush:]
		}
	}

	// Flush remaining
	if len(scanBuf) > 0 {
		os.Stdout.Write(scanBuf)
	}
	conn.Close()
}

func cmdSend(target string, input string) {
	ensureDaemon()
	conn, err := connectDaemon()
	if err != nil {
		die("connect daemon failed: %v", err)
	}
	defer conn.Close()

	req := Request{Cmd: "send", Command: input}
	if isNumeric(target) {
		req.ID, _ = strconv.Atoi(target)
	} else {
		req.Name = target
	}
	if err := sendJSON(conn, req); err != nil {
		die("send failed: %v", err)
	}
	var resp Response
	if err := recvJSON(conn, &resp); err != nil {
		die("daemon request failed")
	}
	if resp.Status < 0 {
		die("%s", resp.Msg)
	}
}

func cmdRead(target string) {
	ensureDaemon()
	conn, err := connectDaemon()
	if err != nil {
		die("connect daemon failed: %v", err)
	}
	defer conn.Close()

	req := Request{Cmd: "read"}
	if isNumeric(target) {
		req.ID, _ = strconv.Atoi(target)
	} else {
		req.Name = target
	}
	if err := sendJSON(conn, req); err != nil {
		die("send failed: %v", err)
	}
	var resp ReadResponse
	if err := recvJSON(conn, &resp); err != nil {
		die("daemon request failed")
	}
	if resp.Status < 0 {
		die("%s", resp.Msg)
	}
	fmt.Print(resp.Screen)
}

// --- Helpers ---

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// --- Main ---

func usage() {
	fmt.Fprintf(os.Stderr, `simpterm %s

usage:
  simpterm [n]ew [name] [--cwd <dir>]
      Create a new session
  simpterm [a]ttach <name|id>
      Attach to a session (Ctrl+\ to detach)
  simpterm [d]etach <name|id>
      Detach a session remotely
  simpterm [e]xec <name|id> <timeout> [--cwd <dir>] <cmd>
      Execute a command and stream output
  simpterm [s]end <name|id> <input>
      Send input to a session (no wait)
  simpterm [r]ead <name|id>
      Read current screen content
  simpterm [l]ist
      List all sessions
  simpterm [k]ill <name|id>
      Kill a session
`, version)
}

var cmdAliases = map[string]string{
	"n": "n", "new": "n",
	"a": "a", "attach": "a",
	"d": "d", "detach": "d",
	"e": "e", "exec": "e",
	"s": "s", "send": "s",
	"r": "r", "read": "r",
	"l": "l", "list": "l",
	"k": "k", "kill": "k",
}

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "__daemon" {
		daemonMain()
		return
	}

	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd, ok := cmdAliases[os.Args[1]]
	if !ok {
		usage()
		os.Exit(1)
	}

	switch cmd {
	case "n":
		args := os.Args[2:]
		var name, cwd string
		for len(args) > 0 {
			if args[0] == "--cwd" {
				if len(args) < 2 {
					usage()
					os.Exit(1)
				}
				cwd = args[1]
				args = args[2:]
			} else {
				if name != "" {
					usage()
					os.Exit(1)
				}
				name = args[0]
				args = args[1:]
			}
		}
		if name != "" && isNumeric(name) {
			die("session name cannot be purely numeric")
		}
		cmdNew(name, cwd)
	case "a":
		if len(os.Args) != 3 {
			usage()
			os.Exit(1)
		}
		cmdAttach(os.Args[2])
	case "d":
		if len(os.Args) != 3 {
			usage()
			os.Exit(1)
		}
		cmdDetach(os.Args[2])
	case "e":
		if len(os.Args) < 5 {
			usage()
			os.Exit(1)
		}
		if !isNumeric(os.Args[3]) {
			die("timeout must be a number (seconds)")
		}
		timeout, _ := strconv.Atoi(os.Args[3])
		args := os.Args[4:]
		var cwd string
		if len(args) >= 2 && args[0] == "--cwd" {
			cwd = args[1]
			args = args[2:]
		}
		if len(args) != 1 {
			usage()
			os.Exit(1)
		}
		cmd := args[0]
		if cwd != "" {
			cmd = fmt.Sprintf("cd %s && %s", shellSingleQuote(cwd), cmd)
		}
		cmdExec(os.Args[2], timeout, cmd)
	case "s":
		if len(os.Args) != 4 {
			usage()
			os.Exit(1)
		}
		cmdSend(os.Args[2], os.Args[3])
	case "r":
		if len(os.Args) != 3 {
			usage()
			os.Exit(1)
		}
		cmdRead(os.Args[2])
	case "l":
		cmdList()
	case "k":
		if len(os.Args) != 3 {
			usage()
			os.Exit(1)
		}
		cmdKill(os.Args[2])
	}
}
