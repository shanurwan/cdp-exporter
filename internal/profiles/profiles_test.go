package profiles

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseLock(t *testing.T) {
	cases := []struct {
		in   string
		host string
		pid  int
		ok   bool
	}{
		{"browser-7d9f-abcde-123", "browser-7d9f-abcde", 123, true},
		{"host-1", "host", 1, true},
		{"nopid", "", 0, false},
		{"host-", "", 0, false},
		{"-5", "", 0, false},
		{"host-x", "", 0, false},
	}
	for _, c := range cases {
		h, p, ok := ParseLock(c.in)
		if h != c.host || p != c.pid || ok != c.ok {
			t.Errorf("ParseLock(%q) = %q,%d,%v want %q,%d,%v", c.in, h, p, ok, c.host, c.pid, c.ok)
		}
	}
}

func TestScanStates(t *testing.T) {
	tmp := t.TempDir()
	mk := func(name string, bytes int, lock string, age time.Duration) string {
		dir := filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Default", "Cookies"), make([]byte, bytes), 0o644); err != nil {
			t.Fatal(err)
		}
		if lock != "" {
			if err := os.Symlink(lock, filepath.Join(dir, "SingletonLock")); err != nil {
				t.Fatal(err)
			}
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	active := mk("puppeteer_dev_chrome_profile-live", 100, "pod-a-10", time.Hour)
	leaked := mk("puppeteer_dev_chrome_profile-dead", 5000, "pod-a-99", time.Hour)
	fresh := mk("puppeteer_dev_chrome_profile-new", 7, "", time.Second)
	foreign := mk("puppeteer_dev_chrome_profile-other", 1, "pod-b-10", time.Hour)

	s := &Scanner{Globs: []string{filepath.Join(tmp, "puppeteer_dev_chrome_profile-*")}, Grace: time.Minute, Hostname: "pod-a", SizeTTL: time.Minute}
	isBrowser := func(pid int) bool { return pid == 10 }
	dirs := s.Scan(map[string]int{active: 10}, isBrowser, time.Now())
	got := map[string]Dir{}
	for _, d := range dirs {
		got[d.Path] = d
	}
	if len(got) != 4 {
		t.Fatalf("found %d dirs, want 4: %+v", len(got), dirs)
	}
	if d := got[active]; !d.Active || d.Leaked || d.Lock != LockLive || d.Bytes != 100 {
		t.Errorf("active dir: %+v", d)
	}
	if d := got[leaked]; d.Active || !d.Leaked || d.Lock != LockStale || d.Bytes != 5000 {
		t.Errorf("leaked dir: %+v", d)
	}
	if d := got[fresh]; d.Leaked || d.Active || d.Lock != LockNone {
		t.Errorf("fresh dir must be pending: %+v", d)
	}
	if d := got[foreign]; d.Lock != LockForeign {
		t.Errorf("foreign lock: %+v", d)
	}
}
