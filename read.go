package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/term"
)

// --- File tree ---

type fileEntry struct {
	path    string // relative to workspace
	name    string // display name (indented)
	isDir   bool
	depth   int
	expand  bool // only for dirs
}

func scanWorkspace(root string) []fileEntry {
	var entries []fileEntry
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		// Skip hidden files/dirs and common noise
		base := filepath.Base(rel)
		if strings.HasPrefix(base, ".") || base == "node_modules" || base == "__pycache__" || base == ".pixi" {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		depth := strings.Count(rel, string(os.PathSeparator))
		indent := strings.Repeat("  ", depth)
		display := indent + base
		if info.IsDir() {
			display += "/"
		}
		entries = append(entries, fileEntry{
			path:  rel,
			name:  display,
			isDir: info.IsDir(),
			depth: depth,
		})
		return nil
	})
	sort.Slice(entries, func(i, j int) bool {
		// Sort dirs before files at same level, then alphabetically
		pi := strings.ToLower(entries[i].path)
		pj := strings.ToLower(entries[j].path)
		return pi < pj
	})
	return entries
}

// --- Terminal drawing ---

type viewer struct {
	fd       int
	width    int
	height   int
	oldState *term.State
}

func newViewer() (*viewer, error) {
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	w, h, _ := term.GetSize(fd)
	if w == 0 {
		w = 80
	}
	if h == 0 {
		h = 24
	}
	// Enable SGR mouse mode (scroll wheel reporting)
	fmt.Fprint(os.Stdout, "\033[?1000h\033[?1006h")
	return &viewer{fd: fd, width: w, height: h, oldState: oldState}, nil
}

func (v *viewer) close() {
	fmt.Fprint(os.Stdout, "\033[?1006l\033[?1000l") // disable mouse
	fmt.Fprint(os.Stdout, "\033[?25h")              // show cursor
	fmt.Fprint(os.Stdout, "\033[2J\033[H")          // clear screen
	term.Restore(v.fd, v.oldState)
}

func (v *viewer) refreshSize() {
	w, h, _ := term.GetSize(v.fd)
	if w > 0 {
		v.width = w
	}
	if h > 0 {
		v.height = h
	}
}

func (v *viewer) clear() {
	fmt.Fprint(os.Stdout, "\033[2J\033[H")
}

func (v *viewer) moveTo(row, col int) {
	fmt.Fprintf(os.Stdout, "\033[%d;%dH", row, col)
}

func (v *viewer) hideCursor() {
	fmt.Fprint(os.Stdout, "\033[?25l")
}

func (v *viewer) writeLineTrunc(row int, line string, maxWidth int) {
	v.moveTo(row, 1)
	// Truncate to maxWidth (simple byte truncation, good enough for ASCII)
	runes := []rune(line)
	if len(runes) > maxWidth {
		runes = runes[:maxWidth]
	}
	fmt.Fprint(os.Stdout, string(runes))
	// Clear rest of line
	fmt.Fprint(os.Stdout, "\033[K")
}

// Read a single key (handles escape sequences for arrow keys and mouse)
func (v *viewer) readKey() string {
	buf := make([]byte, 32)
	n, err := os.Stdin.Read(buf)
	if err != nil || n == 0 {
		return ""
	}
	// SGR mouse: \033[<btn;x;yM or \033[<btn;x;ym
	if n >= 6 && buf[0] == 0x1b && buf[1] == '[' && buf[2] == '<' {
		s := string(buf[3:n])
		// Parse button number (before first ';')
		semi := 0
		for i, c := range s {
			if c == ';' {
				semi = i
				break
			}
		}
		if semi > 0 {
			btn := 0
			for _, c := range s[:semi] {
				if c >= '0' && c <= '9' {
					btn = btn*10 + int(c-'0')
				}
			}
			if btn == 64 {
				return "scrollup"
			}
			if btn == 65 {
				return "scrolldown"
			}
		}
		return "" // ignore other mouse events
	}
	// Escape sequences
	if n >= 3 && buf[0] == 0x1b && buf[1] == '[' {
		switch buf[2] {
		case 'A':
			return "up"
		case 'B':
			return "down"
		case 'C':
			return "right"
		case 'D':
			return "left"
		case '5': // Page Up
			if n >= 4 && buf[3] == '~' {
				return "pgup"
			}
		case '6': // Page Down
			if n >= 4 && buf[3] == '~' {
				return "pgdn"
			}
		}
	}
	switch buf[0] {
	case 'q', 'Q':
		return "q"
	case 'j':
		return "down"
	case 'k':
		return "up"
	case 'g':
		return "top"
	case 'G':
		return "bottom"
	case 'h':
		return "left"
	case 'l':
		return "right"
	case 13: // Enter
		return "enter"
	case '/':
		return "search"
	case 'n':
		return "next"
	case ' ':
		return "pgdn"
	case 0x1b: // bare Escape
		return "q"
	}
	return ""
}

// --- File browser mode ---

func (v *viewer) fileBrowser(root string, entries []fileEntry) {
	cursor := 0
	scroll := 0

	for {
		v.refreshSize()
		v.hideCursor()
		v.clear()

		// Header
		header := fmt.Sprintf(" [workspace] %s", root)
		v.writeLineTrunc(1, "\033[1;36m"+header+"\033[0m", v.width)
		footer := " j/k:navigate  Enter:open  q:quit"
		v.writeLineTrunc(v.height, "\033[7m"+footer+strings.Repeat(" ", v.width-len(footer))+"\033[0m", v.width)

		// File list
		listHeight := v.height - 2
		if cursor < scroll {
			scroll = cursor
		}
		if cursor >= scroll+listHeight {
			scroll = cursor - listHeight + 1
		}

		for i := 0; i < listHeight; i++ {
			idx := scroll + i
			row := i + 2
			if idx >= len(entries) {
				v.writeLineTrunc(row, "", v.width)
				continue
			}
			e := entries[idx]
			line := " " + e.name
			if idx == cursor {
				if e.isDir {
					line = "\033[1;33m>" + e.name + "\033[0m"
				} else {
					line = "\033[1;37m>" + e.name + "\033[0m"
				}
			} else {
				if e.isDir {
					line = " \033[1;34m" + e.name + "\033[0m"
				} else {
					line = " " + e.name
				}
			}
			v.writeLineTrunc(row, line, v.width)
		}

		key := v.readKey()
		switch key {
		case "q":
			return
		case "down", "scrolldown":
			if cursor < len(entries)-1 {
				cursor++
			}
		case "up", "scrollup":
			if cursor > 0 {
				cursor--
			}
		case "pgdn":
			cursor += listHeight
			if cursor >= len(entries) {
				cursor = len(entries) - 1
			}
		case "pgup":
			cursor -= listHeight
			if cursor < 0 {
				cursor = 0
			}
		case "top":
			cursor = 0
		case "bottom":
			cursor = len(entries) - 1
		case "enter", "right":
			if cursor < len(entries) {
				e := entries[cursor]
				if !e.isDir {
					fullPath := filepath.Join(root, e.path)
					v.fileViewer(fullPath, e.path)
				}
			}
		}
	}
}

// --- File viewer mode ---

func (v *viewer) fileViewer(fullPath string, relPath string) {
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")
	// Remove trailing empty line from Split
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	scroll := 0
	searchTerm := ""
	searchMatches := []int{} // line indices that match
	searchIdx := -1          // current match index

	gutterWidth := len(fmt.Sprintf("%d", len(lines))) + 1

	for {
		v.refreshSize()
		v.hideCursor()
		v.clear()

		// Header
		header := fmt.Sprintf(" %s (%d lines)", relPath, len(lines))
		v.writeLineTrunc(1, "\033[1;36m"+header+"\033[0m", v.width)

		// Footer
		searchInfo := ""
		if searchTerm != "" {
			searchInfo = fmt.Sprintf("  /%s [%d/%d]", searchTerm, searchIdx+1, len(searchMatches))
		}
		footer := fmt.Sprintf(" j/k:scroll  g/G:top/bottom  /:search  n:next  q:back%s", searchInfo)
		v.writeLineTrunc(v.height, "\033[7m"+footer+strings.Repeat(" ", v.width)+"\033[0m", v.width)

		// Content
		contentHeight := v.height - 2
		maxScroll := len(lines) - contentHeight
		if maxScroll < 0 {
			maxScroll = 0
		}
		if scroll > maxScroll {
			scroll = maxScroll
		}
		if scroll < 0 {
			scroll = 0
		}

		contentWidth := v.width - gutterWidth - 1
		if contentWidth < 1 {
			contentWidth = 1
		}

		for i := 0; i < contentHeight; i++ {
			lineIdx := scroll + i
			row := i + 2
			if lineIdx >= len(lines) {
				v.writeLineTrunc(row, "\033[90m~\033[0m", v.width)
				continue
			}
			lineNum := fmt.Sprintf("%*d", gutterWidth, lineIdx+1)
			content := lines[lineIdx]

			// Highlight search matches in this line
			if searchTerm != "" && strings.Contains(strings.ToLower(content), strings.ToLower(searchTerm)) {
				line := fmt.Sprintf("\033[90m%s\033[0m \033[43m%s\033[0m", lineNum, content)
				v.writeLineTrunc(row, line, v.width+20) // extra for escape codes
			} else {
				line := fmt.Sprintf("\033[90m%s\033[0m %s", lineNum, content)
				v.writeLineTrunc(row, line, v.width+10)
			}
		}

		key := v.readKey()
		switch key {
		case "q", "left":
			return
		case "down":
			if scroll < maxScroll {
				scroll++
			}
		case "up":
			if scroll > 0 {
				scroll--
			}
		case "scrolldown":
			scroll += 3
			if scroll > maxScroll {
				scroll = maxScroll
			}
		case "scrollup":
			scroll -= 3
			if scroll < 0 {
				scroll = 0
			}
		case "pgdn":
			scroll += contentHeight
			if scroll > maxScroll {
				scroll = maxScroll
			}
		case "pgup":
			scroll -= contentHeight
			if scroll < 0 {
				scroll = 0
			}
		case "top":
			scroll = 0
		case "bottom":
			scroll = maxScroll
		case "search":
			// Simple search input
			v.moveTo(v.height, 1)
			fmt.Fprint(os.Stdout, "\033[K/")
			fmt.Fprint(os.Stdout, "\033[?25h") // show cursor

			// Read search term character by character
			term := []byte{}
			for {
				b := make([]byte, 1)
				os.Stdin.Read(b)
				if b[0] == 13 { // Enter
					break
				}
				if b[0] == 27 { // Escape - cancel
					term = nil
					break
				}
				if b[0] == 127 || b[0] == 8 { // Backspace
					if len(term) > 0 {
						term = term[:len(term)-1]
						v.moveTo(v.height, 1)
						fmt.Fprintf(os.Stdout, "\033[K/%s", string(term))
					}
					continue
				}
				term = append(term, b[0])
				fmt.Fprint(os.Stdout, string(b))
			}
			v.hideCursor()

			if term != nil && len(term) > 0 {
				searchTerm = string(term)
				searchMatches = nil
				for i, l := range lines {
					if strings.Contains(strings.ToLower(l), strings.ToLower(searchTerm)) {
						searchMatches = append(searchMatches, i)
					}
				}
				if len(searchMatches) > 0 {
					searchIdx = 0
					scroll = searchMatches[0]
					if scroll > maxScroll {
						scroll = maxScroll
					}
				} else {
					searchIdx = -1
				}
			}
		case "next":
			if len(searchMatches) > 0 {
				searchIdx = (searchIdx + 1) % len(searchMatches)
				scroll = searchMatches[searchIdx]
				maxS := len(lines) - (v.height - 2)
				if maxS < 0 {
					maxS = 0
				}
				if scroll > maxS {
					scroll = maxS
				}
			}
		}
	}
}

// --- Entry point ---

func cmdRead(workspace string) {
	// Resolve to absolute path
	absPath, err := filepath.Abs(workspace)
	if err != nil {
		die("invalid path: %v", err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		die("cannot access %s: %v", workspace, err)
	}
	if !info.IsDir() {
		die("%s is not a directory", workspace)
	}

	entries := scanWorkspace(absPath)
	if len(entries) == 0 {
		die("no files found in %s", workspace)
	}

	v, err := newViewer()
	if err != nil {
		die("failed to init terminal: %v", err)
	}
	defer v.close()

	v.fileBrowser(absPath, entries)
}
