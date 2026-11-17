package agent

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EvansLeeSin/stardew-sync/internal/api"
	"github.com/EvansLeeSin/stardew-sync/internal/server"
)

func TestBackoffDelay(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for i, w := range want {
		if got := backoffDelay(i + 1); got != w {
			t.Errorf("fail %d: %v want %v", i+1, got, w)
		}
	}
	var b Backoff
	now := time.Now()
	b.now = func() time.Time { return now }
	if !b.Ready() || b.Until() != nil {
		t.Fatal("fresh backoff should be ready")
	}
	if _, first := b.Fail(nil); !first {
		t.Fatal("first failure")
	}
	if _, first := b.Fail(nil); first {
		t.Fatal("second failure must not be first")
	}
	if b.Ready() {
		t.Fatal("should wait")
	}
	now = now.Add(time.Minute + time.Second)
	if !b.Ready() {
		t.Fatal("should be ready after delay")
	}
	if !b.Success() || b.Until() != nil {
		t.Fatal("success resets")
	}
}

func TestParseScreenState(t *testing.T) {
	cases := map[string][2]bool{ // out -> {on, ok}
		"  mWakefulness=Awake\n":               {true, true},
		"  mWakefulness=Asleep\n":              {false, true},
		"  mWakefulness=Dozing\n":              {true, false},
		"Display Power: state=OFF\n":           {false, true},
		"Display Power: state=ON\n":            {true, true},
		"mScreenState=DOZE_SUSPEND\n":          {false, true},
		"something else":                       {true, false},
		"mScreenState=ON\nmWakefulness=Asleep": {true, true}, // first match wins
	}
	for in, w := range cases {
		on, ok := parseScreenState(in)
		if on != w[0] || ok != w[1] {
			t.Errorf("%q => %v,%v want %v,%v", in, on, ok, w[0], w[1])
		}
	}
}

// countingServer wraps the real server handler and counts requests by path.
type countingServer struct {
	mu     sync.Mutex
	counts map[string]int
	down   atomic.Bool
}

func (c *countingServer) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		if strings.HasSuffix(r.URL.Path, "/wait") {
			key = "WAIT"
		}
		c.mu.Lock()
		c.counts[key]++
		c.mu.Unlock()
		if c.down.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (c *countingServer) get(k string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[k]
}

type screenPlat struct {
	mockPlat
	mu sync.Mutex
	on bool
}

func (p *screenPlat) ScreenOn() (bool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.on, true
}

func (p *screenPlat) set(on bool) {
	p.mu.Lock()
	p.on = on
	p.mu.Unlock()
}

func newDaemonEnv(t *testing.T) (*Daemon, *screenPlat, *countingServer, string) {
	dir := t.TempDir()
	st, _ := server.NewStore(filepath.Join(dir, "srv"), 30)
	devs := server.NewDevices(filepath.Join(dir, "srv", "devices.json"))
	tok, _ := devs.Add("android")
	s := &server.Server{Store: st, Locks: server.NewLocks(api.LockTTL), Devices: devs, Logger: log.New(io.Discard, "", 0)}
	cs := &countingServer{counts: map[string]int{}}
	srv := httptest.NewServer(cs.wrap(s.Handler()))
	t.Cleanup(srv.Close)

	saves := filepath.Join(dir, "Saves")
	writeSave(t, saves, 1, "1.6.15")
	data := filepath.Join(dir, "data")
	cfg := DefaultConfig()
	cfg.ServerURL, cfg.Token, cfg.SavesDir, cfg.SaveID = srv.URL, tok, saves, sid
	if err := SaveConfig(data, cfg); err != nil {
		t.Fatal(err)
	}
	plat := &screenPlat{mockPlat: mockPlat{version: "1.6.15"}, on: true}
	d := &Daemon{DataDir: data, Plat: func(Config) Platform { return plat }, Log: log.New(io.Discard, "", 0)}
	return d, plat, cs, data
}

func TestDaemonScreenOffPausesLongPoll(t *testing.T) {
	d, plat, cs, _ := newDaemonEnv(t)
	plat.set(false) // screen off from the start
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	time.Sleep(1500 * time.Millisecond)
	// Screen is sampled on the first loop iteration, but the poller goroutine
	// may race ahead of that first sample and issue at most one long-poll,
	// which must then be aborted within ~1s of the screen-off detection.
	if n := cs.get("WAIT"); n > 1 {
		t.Fatalf("long-poll while screen off: %d", n)
	}
	before := cs.get("WAIT")
	time.Sleep(1500 * time.Millisecond)
	if cs.get("WAIT") != before {
		t.Fatal("new long-polls issued while screen off")
	}
	// No uploads with the screen off and the game not running.
	if n := cs.get("POST /api/v1/saves/" + sid + "/revisions"); n != 0 {
		t.Fatalf("uploaded while screen off: %d", n)
	}
}

func TestDaemonStatusWriteThrottled(t *testing.T) {
	d, _, _, data := newDaemonEnv(t)
	d.status = DaemonStatus{PID: 1}
	d.writeStatus()
	p := daemonStatusPath(data)
	st1, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	d.writeStatus() // unchanged -> skipped
	if st2, _ := os.Stat(p); !st2.ModTime().Equal(st1.ModTime()) {
		t.Fatal("unchanged status rewritten")
	}
	d.status.GameRunning = true
	d.writeStatus() // changed -> written
	if s, _ := LoadDaemonStatus(data); s == nil || !s.GameRunning {
		t.Fatal("changed status not written")
	}
}

func TestDaemonBacksOffWhenServerDown(t *testing.T) {
	d, _, cs, _ := newDaemonEnv(t)
	cs.down.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	time.Sleep(3 * time.Second)
	total := 0
	cs.mu.Lock()
	for _, n := range cs.counts {
		total += n
	}
	cs.mu.Unlock()
	// Without backoff the 5s loop + long-poll would retry; with backoff (30s min)
	// only the first attempts happen within 3 seconds.
	if total > 3 {
		t.Fatalf("too many requests while server down: %v", cs.counts)
	}
	if d.backoff.Until() == nil {
		t.Fatal("backoff not engaged")
	}
}

func TestDaemonResumesOnScreenOn(t *testing.T) {
	d, plat, cs, _ := newDaemonEnv(t)
	plat.set(false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	time.Sleep(1500 * time.Millisecond)
	waits, gets := cs.get("WAIT"), cs.get("GET /api/v1/saves/"+sid)
	plat.set(true)
	// screen is sampled every 10s; wait for the next sample + sync
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cs.get("WAIT") > waits && cs.get("GET /api/v1/saves/"+sid) > gets {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if cs.get("WAIT") <= waits {
		t.Fatal("long-poll not resumed after screen on")
	}
	if cs.get("GET /api/v1/saves/"+sid) <= gets {
		t.Fatal("no catch-up sync after screen on")
	}
	// local save (never uploaded) is uploaded by the catch-up sync
	if cs.get("POST /api/v1/saves/"+sid+"/revisions") != 1 {
		t.Fatalf("expected one upload: %v", cs.counts)
	}
}
