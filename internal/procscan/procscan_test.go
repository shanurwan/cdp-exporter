package procscan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeProc struct {
	pid, ppid int
	state     byte
	comm      string
	args      []string
	rssPages  int
	pssKB     int // 0 = smaps_rollup unreadable
}

func writeProc(t *testing.T, root string, procs []fakeProc) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("cpu 1 2 3\nbtime 1700000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		dir := filepath.Join(root, fmt.Sprint(p.pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Field 22 (starttime) = 100 ticks = 1 s after boot.
		stat := fmt.Sprintf("%d (%s) %c %d 0 0 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0", p.pid, p.comm, p.state, p.ppid)
		mustWrite(t, filepath.Join(dir, "stat"), stat)
		mustWrite(t, filepath.Join(dir, "cmdline"), strings.Join(p.args, "\x00")+"\x00")
		mustWrite(t, filepath.Join(dir, "statm"), fmt.Sprintf("1000 %d 0 0 0 0 0", p.rssPages))
		if p.pssKB > 0 {
			mustWrite(t, filepath.Join(dir, "smaps_rollup"), fmt.Sprintf("00400000-ffff ---p 0 0:0 0 [rollup]\nRss: 1 kB\nPss: %d kB\n", p.pssKB))
		}
	}
}

func mustWrite(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanClassifiesAndFindsOrphans(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, []fakeProc{
		{pid: 1, ppid: 0, state: 'S', comm: "node", args: []string{"node", "server.js"}, rssPages: 10},
		// A healthy browser with a zygote and a renderer under it.
		{pid: 10, ppid: 1, state: 'S', comm: "chrome", args: []string{"/opt/chrome/chrome", "--headless", "--remote-debugging-port=0", "--user-data-dir=/tmp/puppeteer_dev_chrome_profile-abc"}, rssPages: 100, pssKB: 200},
		{pid: 11, ppid: 10, state: 'S', comm: "chrome", args: []string{"/opt/chrome/chrome", "--type=zygote"}, rssPages: 20, pssKB: 30},
		{pid: 12, ppid: 11, state: 'S', comm: "chrome", args: []string{"/opt/chrome/chrome", "--type=renderer", "--lang=en"}, rssPages: 50},
		// Renderer left behind after its browser was SIGKILLed: reparented to 1.
		{pid: 20, ppid: 1, state: 'S', comm: "chrome", args: []string{"/opt/chrome/chrome", "--type=renderer"}, rssPages: 70},
		// Crashpad is detached on purpose and is not an orphan.
		{pid: 30, ppid: 1, state: 'S', comm: "chrome_crashpad", args: []string{"/opt/chrome/chrome_crashpad_handler", "--database=/tmp/x"}, rssPages: 5},
		// Zombie: only comm is left. chrome-headless-shell is truncated by the kernel.
		{pid: 40, ppid: 1, state: 'Z', comm: "chrome-headless", args: nil},
		// Playwright browser on a pipe.
		{pid: 50, ppid: 1, state: 'S', comm: "chrome-headless", args: []string{"/ms-playwright/chromium_headless_shell-1200/chrome-headless-shell", "--remote-debugging-pipe", "--user-data-dir=/tmp/playwright_chromiumdev_profile-x"}, rssPages: 90},
	})

	s := New(root)
	s.pageSize = 4096
	snap, err := s.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(snap.Browsers); got != 2 {
		t.Fatalf("browsers = %d, want 2", got)
	}
	b := snap.Procs[10]
	if b.Type != TypeBrowser || b.DebugPort != 0 || b.UserDataDir != "/tmp/puppeteer_dev_chrome_profile-abc" {
		t.Errorf("browser parsed wrong: %+v", b)
	}
	if !b.HasPSS || b.PSS != 200*1024 || b.RSS != 100*4096 {
		t.Errorf("memory parsed wrong: rss=%d pss=%d has=%v", b.RSS, b.PSS, b.HasPSS)
	}
	if snap.Procs[12].HasPSS {
		t.Error("renderer without smaps_rollup must not report PSS")
	}
	if pw := snap.Procs[50]; !pw.DebugPipe || pw.Type != TypeBrowser || pw.DebugPort != -1 {
		t.Errorf("playwright browser parsed wrong: %+v", pw)
	}
	if snap.Procs[1].Chrome {
		t.Error("node matched as chrome")
	}
	if z := snap.Procs[40]; !z.Chrome || z.State != 'Z' {
		t.Errorf("zombie chrome-headless not recognised: %+v", z)
	}
	if got := snap.BrowserOf(12); got == nil || got.PID != 10 {
		t.Errorf("BrowserOf(12) = %v, want 10", got)
	}
	orphans := snap.Orphans()
	if len(orphans) != 1 || orphans[0].PID != 20 {
		t.Fatalf("orphans = %+v, want only pid 20", orphans)
	}
	if got := len(snap.Tree(10)); got != 3 {
		t.Errorf("tree(10) has %d procs, want 3", got)
	}
}

func TestParseStatCommWithParens(t *testing.T) {
	p, start, err := parseStat([]byte("77 (we (ir) d) S 5 0 0 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 4242 0 0"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Comm != "we (ir) d" || p.PPID != 5 || p.State != 'S' || start != 4242 {
		t.Errorf("got comm=%q ppid=%d state=%c start=%d", p.Comm, p.PPID, p.State, start)
	}
}

func TestScanRealProc(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	snap, err := New("/proc").Scan()
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Alive(os.Getpid()) {
		t.Error("own process not found in /proc scan")
	}
}
