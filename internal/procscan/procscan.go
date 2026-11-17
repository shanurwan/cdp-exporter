// Package procscan reads a Linux /proc tree and finds Chrome processes.
//
// It works without any help from the browser: every Chrome process is found by
// its executable name, classified by its --type= flag, and grouped under the
// browser process it descends from. This is what lets the exporter see leaks
// and zombies that the browser itself cannot report.
package procscan

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TypeBrowser is the type given to a Chrome process started without --type=.
const TypeBrowser = "browser"

// DefaultNamePattern matches the executable names of Chrome, Chromium and
// chrome-headless-shell processes, and their kernel comm values, which Linux
// truncates to 15 bytes (a zombie has only its comm left).
var DefaultNamePattern = regexp.MustCompile(`^(chrome|chromium|chromium-browser|chromium-browse|google-chrome|headless_shell|headless-shell|chrome-headless-shell|chrome-headless|chrome_crashpad_handler|chrome_crashpad)$`)

// Proc is one process from /proc.
type Proc struct {
	PID   int
	PPID  int
	State byte // R, S, D, Z, ...
	Comm  string
	Args  []string

	Chrome bool   // name matched the Chrome pattern
	Type   string // TypeBrowser, "renderer", "zygote", "gpu-process", "utility", ...

	RSS    uint64 // bytes, from /proc/<pid>/statm
	PSS    uint64 // bytes, from /proc/<pid>/smaps_rollup; valid only if HasPSS
	HasPSS bool

	Start time.Time

	UserDataDir string
	DebugPort   int  // value of --remote-debugging-port, -1 if absent
	DebugPipe   bool // --remote-debugging-pipe (Playwright)
}

// Snapshot is the result of one scan.
type Snapshot struct {
	Procs map[int]*Proc
	// Browsers are the Chrome browser processes (no --type=).
	Browsers []*Proc
	// Children maps a PID to its direct children.
	Children map[int][]int
	Taken    time.Time
}

// Scanner reads a /proc tree.
type Scanner struct {
	Root        string // usually "/proc"
	NamePattern *regexp.Regexp
	// ReadPSS enables reading /proc/<pid>/smaps_rollup. It is more accurate
	// than RSS for Chrome (which shares a lot of memory between processes) but
	// costs more and may be denied for non-dumpable sandboxed processes.
	ReadPSS bool

	pageSize int64
	clkTck   int64
}

// New returns a Scanner for root with the default name pattern.
func New(root string) *Scanner {
	return &Scanner{Root: root, NamePattern: DefaultNamePattern, ReadPSS: true, pageSize: int64(os.Getpagesize()), clkTck: 100}
}

// Scan reads every numeric directory under Root. Processes that exit during the
// scan are skipped silently.
func (s *Scanner) Scan() (*Snapshot, error) {
	if s.pageSize == 0 {
		s.pageSize = int64(os.Getpagesize())
	}
	if s.clkTck == 0 {
		s.clkTck = 100
	}
	if s.NamePattern == nil {
		s.NamePattern = DefaultNamePattern
	}
	btime, err := readBootTime(filepath.Join(s.Root, "stat"))
	if err != nil {
		return nil, fmt.Errorf("read boot time: %w", err)
	}
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{Procs: map[int]*Proc{}, Children: map[int][]int{}, Taken: time.Now()}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		p, err := s.readProc(pid, btime)
		if err != nil {
			continue // exited, or not readable
		}
		snap.Procs[pid] = p
	}
	for pid, p := range snap.Procs {
		snap.Children[p.PPID] = append(snap.Children[p.PPID], pid)
		if p.Chrome && p.Type == TypeBrowser && p.State != 'Z' {
			snap.Browsers = append(snap.Browsers, p)
		}
	}
	return snap, nil
}

func (s *Scanner) readProc(pid int, btime time.Time) (*Proc, error) {
	dir := filepath.Join(s.Root, strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return nil, err
	}
	p, startTicks, err := parseStat(stat)
	if err != nil {
		return nil, err
	}
	p.PID = pid
	p.Start = btime.Add(time.Duration(startTicks) * time.Second / time.Duration(s.clkTck))
	p.DebugPort = -1

	if raw, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
		p.Args = splitCmdline(raw)
	}
	name := p.Comm
	if len(p.Args) > 0 {
		name = filepath.Base(p.Args[0])
	}
	p.Chrome = s.NamePattern.MatchString(name) || s.NamePattern.MatchString(p.Comm)
	if !p.Chrome {
		return p, nil
	}
	classify(p)
	if p.State == 'Z' {
		return p, nil
	}
	if statm, err := os.ReadFile(filepath.Join(dir, "statm")); err == nil {
		if f := strings.Fields(string(statm)); len(f) > 1 {
			if pages, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				p.RSS = uint64(pages * s.pageSize)
			}
		}
	}
	if s.ReadPSS {
		if pss, err := readPSS(filepath.Join(dir, "smaps_rollup")); err == nil {
			p.PSS, p.HasPSS = pss, true
		}
	}
	return p, nil
}

// parseStat parses /proc/<pid>/stat. comm is in parentheses and may itself
// contain spaces and parentheses, so the last ')' ends it.
func parseStat(b []byte) (*Proc, uint64, error) {
	open := bytes.IndexByte(b, '(')
	end := bytes.LastIndexByte(b, ')')
	if open < 0 || end < open || end+2 >= len(b) {
		return nil, 0, errors.New("malformed stat")
	}
	p := &Proc{Comm: string(b[open+1 : end])}
	f := strings.Fields(string(b[end+2:]))
	// f[0]=state f[1]=ppid ... f[19]=starttime (field 22 overall)
	if len(f) < 20 {
		return nil, 0, errors.New("short stat")
	}
	p.State = f[0][0]
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return nil, 0, err
	}
	p.PPID = ppid
	start, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return nil, 0, err
	}
	return p, start, nil
}

func splitCmdline(raw []byte) []string {
	raw = bytes.TrimRight(raw, "\x00")
	if len(raw) == 0 {
		return nil
	}
	parts := bytes.Split(raw, []byte{0})
	if len(parts) == 1 && bytes.IndexByte(raw, ' ') > 0 {
		// Some processes rewrite argv into one space-separated string.
		parts = bytes.Fields(raw)
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out
}

func classify(p *Proc) {
	p.Type = TypeBrowser
	name := p.Comm
	if len(p.Args) > 0 {
		name = filepath.Base(p.Args[0])
	}
	if strings.HasPrefix(name, "chrome_crashpad") {
		p.Type = "crashpad-handler"
	}
	if len(p.Args) == 0 && p.State != 'Z' {
		// No cmdline (kernel thread or exiting); we cannot tell its role.
		p.Type = "unknown"
	}
	for _, a := range p.Args[min(1, len(p.Args)):] {
		switch {
		case strings.HasPrefix(a, "--type="):
			p.Type = strings.TrimPrefix(a, "--type=")
		case strings.HasPrefix(a, "--user-data-dir="):
			p.UserDataDir = strings.Trim(strings.TrimPrefix(a, "--user-data-dir="), `"`)
		case strings.HasPrefix(a, "--remote-debugging-port="):
			if n, err := strconv.Atoi(strings.TrimPrefix(a, "--remote-debugging-port=")); err == nil {
				p.DebugPort = n
			}
		case a == "--remote-debugging-pipe":
			p.DebugPipe = true
		}
	}
}

func readPSS(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "Pss:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				break
			}
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return kb * 1024, nil
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fs.ErrNotExist
}

func readBootTime(path string) (time.Time, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "btime ") {
			n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(n, 0), nil
		}
	}
	return time.Time{}, errors.New("no btime line")
}

// BrowserOf returns the browser process that pid descends from, or nil.
func (s *Snapshot) BrowserOf(pid int) *Proc {
	seen := 0
	for cur := s.Procs[pid]; cur != nil && seen < 64; seen++ {
		if cur.Chrome && cur.Type == TypeBrowser && cur.State != 'Z' {
			return cur
		}
		parent, ok := s.Procs[cur.PPID]
		if !ok || parent.PID == cur.PID {
			return nil
		}
		cur = parent
	}
	return nil
}

// Tree returns pid and all of its live descendants.
func (s *Snapshot) Tree(pid int) []*Proc {
	var out []*Proc
	stack := []int{pid}
	seen := map[int]bool{}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if p, ok := s.Procs[cur]; ok {
			out = append(out, p)
		}
		stack = append(stack, s.Children[cur]...)
	}
	return out
}

// Orphans returns live Chrome child processes (renderers, zygotes, utilities)
// that no longer descend from any browser process. They are what is left
// after a browser is killed without its children: they keep their memory and
// are never cleaned up by Chrome itself. The crashpad handler is excluded
// because Chrome deliberately detaches it from the browser.
func (s *Snapshot) Orphans() []*Proc {
	var out []*Proc
	for _, p := range s.Procs {
		if !p.Chrome || p.State == 'Z' || p.Type == TypeBrowser || p.Type == "crashpad-handler" || p.Type == "unknown" {
			continue
		}
		if s.BrowserOf(p.PID) == nil {
			out = append(out, p)
		}
	}
	return out
}

// Alive reports whether pid exists and is not a zombie.
func (s *Snapshot) Alive(pid int) bool {
	p, ok := s.Procs[pid]
	return ok && p.State != 'Z'
}
