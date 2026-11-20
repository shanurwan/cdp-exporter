// Package profiles finds Chrome user-data directories on disk and decides
// whether each one is in use, leaked, or locked by a process that is gone.
//
// A browser killed with SIGKILL (OOM killer, container restart, a crashed
// driver) never deletes its temporary profile and leaves a SingletonLock
// symlink behind. Leaked profiles fill the disk; a stale lock makes the next
// Chrome that reuses the directory refuse to start.
package profiles

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultGlobs are the temporary profile directories created by Puppeteer,
// Playwright and browserless.
var DefaultGlobs = []string{
	"/tmp/puppeteer_dev_chrome_profile-*",
	"/tmp/puppeteer_dev_profile-*",
	"/tmp/playwright_chromiumdev_profile-*",
	"/tmp/browserless-data-dirs/browserless-data-dir-*",
}

// LockState describes a directory's SingletonLock.
type LockState string

const (
	LockNone    LockState = "none"    // no SingletonLock
	LockLive    LockState = "live"    // held by a running browser
	LockStale   LockState = "stale"   // names a PID on this host that is not a running browser
	LockForeign LockState = "foreign" // names another host; cannot be judged here
)

// Dir is one profile directory.
type Dir struct {
	Path    string
	Active  bool // a running browser uses it
	Leaked  bool // matches a glob, no browser uses it, older than the grace period
	Bytes   int64
	ModTime time.Time
	Lock    LockState
}

// Scanner finds profile directories. It is safe for concurrent use.
type Scanner struct {
	Globs []string
	// Grace is how long an unused directory may exist before it counts as
	// leaked, so that a browser that is still starting is not reported.
	Grace time.Duration
	// Hostname is compared with the host part of SingletonLock. Chrome
	// writes the value of gethostname(), so it must be the hostname of the
	// browser's UTS namespace (the pod name in Kubernetes).
	Hostname string
	// SizeTTL caches directory sizes, because walking a profile is the most
	// expensive part of a scrape.
	SizeTTL time.Duration

	mu    sync.Mutex
	sizes map[string]sizeEntry
}

type sizeEntry struct {
	bytes int64
	at    time.Time
}

// Scan returns every known directory: the globs' matches plus every directory
// in active (user-data-dir -> browser PID). isBrowser reports whether a PID is a
// running browser process.
func (s *Scanner) Scan(active map[string]int, isBrowser func(pid int) bool, now time.Time) []Dir {
	seen := map[string]bool{}
	var out []Dir
	add := func(path string, isActive bool) {
		path = filepath.Clean(path)
		if seen[path] {
			return
		}
		fi, err := os.Lstat(path)
		if err != nil || !fi.IsDir() {
			return
		}
		seen[path] = true
		d := Dir{Path: path, Active: isActive, ModTime: fi.ModTime()}
		d.Lock = s.lockState(path, isBrowser)
		if !isActive && now.Sub(fi.ModTime()) >= s.Grace {
			d.Leaked = true
		}
		d.Bytes = s.size(path, now)
		out = append(out, d)
	}
	activeClean := map[string]bool{}
	for dir := range active {
		activeClean[filepath.Clean(dir)] = true
	}
	for dir := range activeClean {
		add(dir, true)
	}
	for _, g := range s.Globs {
		matches, _ := filepath.Glob(g)
		for _, m := range matches {
			add(m, activeClean[filepath.Clean(m)])
		}
	}
	s.prune(seen)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (s *Scanner) lockState(dir string, isBrowser func(int) bool) LockState {
	target, err := os.Readlink(filepath.Join(dir, "SingletonLock"))
	if err != nil {
		return LockNone
	}
	host, pid, ok := ParseLock(target)
	if !ok {
		return LockForeign
	}
	if host != s.Hostname {
		return LockForeign
	}
	if isBrowser != nil && isBrowser(pid) {
		return LockLive
	}
	return LockStale
}

// ParseLock splits a SingletonLock target of the form "<hostname>-<pid>".
// Hostnames may contain '-', so the PID is after the last one.
func ParseLock(target string) (host string, pid int, ok bool) {
	i := strings.LastIndexByte(target, '-')
	if i <= 0 || i == len(target)-1 {
		return "", 0, false
	}
	n, err := strconv.Atoi(target[i+1:])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return target[:i], n, true
}

func (s *Scanner) size(dir string, now time.Time) int64 {
	s.mu.Lock()
	if s.sizes == nil {
		s.sizes = map[string]sizeEntry{}
	}
	if e, ok := s.sizes[dir]; ok && now.Sub(e.at) < s.SizeTTL {
		s.mu.Unlock()
		return e.bytes
	}
	s.mu.Unlock()

	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // a file vanished or is unreadable; count what we can
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	s.mu.Lock()
	s.sizes[dir] = sizeEntry{bytes: total, at: now}
	s.mu.Unlock()
	return total
}

func (s *Scanner) prune(keep map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.sizes {
		if !keep[k] {
			delete(s.sizes, k)
		}
	}
}
