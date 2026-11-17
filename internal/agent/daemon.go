package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/EvansLeeSin/stardew-sync/internal/api"
	"github.com/EvansLeeSin/stardew-sync/internal/client"
)

// DaemonStatus is written to <dataDir>/daemon.json for the WebUI.
type DaemonStatus struct {
	PID         int       `json:"pid"`
	StartedAt   time.Time `json:"started_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	GameRunning bool      `json:"game_running"`
	ScreenOn    bool      `json:"screen_on"`
	HoldingLock bool      `json:"holding_lock"`
	OtherLock   *api.Lock `json:"other_lock,omitempty"`
	Online      bool      `json:"online"`
	// RetryAt is set while network requests are backing off after failures.
	RetryAt   *time.Time `json:"retry_at,omitempty"`
	LastError string     `json:"last_error,omitempty"`
}

func daemonStatusPath(dataDir string) string { return filepath.Join(dataDir, "daemon.json") }

func LoadDaemonStatus(dataDir string) (*DaemonStatus, error) {
	b, err := os.ReadFile(daemonStatusPath(dataDir))
	if err != nil {
		return nil, err
	}
	var s DaemonStatus
	return &s, json.Unmarshal(b, &s)
}

const (
	pollInterval   = 5 * time.Second
	settleDuration = 5 * time.Second
	// periodicSync is only a safety net; remote changes arrive via long-poll and
	// local changes only happen while the game runs (handled on game exit).
	periodicSync = time.Hour
	// statusHeartbeat: daemon.json is rewritten at least this often so the
	// WebUI can tell the daemon is alive (it treats >1min stale as dead).
	statusHeartbeat = 30 * time.Second
	// screenCheckInterval: how often the screen state is sampled.
	screenCheckInterval = 10 * time.Second

	backoffMin = 30 * time.Second
	backoffMax = 10 * time.Minute
)

// Backoff is an exponential backoff shared by all network calls of the daemon.
type Backoff struct {
	mu      sync.Mutex
	fails   int
	until   time.Time
	lastErr string
	now     func() time.Time
}

func (b *Backoff) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// Delay for the n-th consecutive failure: 30s, 1m, 2m, 4m, 8m, 10m, ...
func backoffDelay(fails int) time.Duration {
	d := backoffMin
	for i := 1; i < fails && d < backoffMax; i++ {
		d *= 2
	}
	if d > backoffMax {
		d = backoffMax
	}
	return d
}

// Fail records a failure. Returns the wait and whether this is the first
// failure of a streak (callers log only then, to avoid log spam).
func (b *Backoff) Fail(err error) (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		b.lastErr = err.Error()
	}
	b.fails++
	d := backoffDelay(b.fails)
	b.until = b.clock().Add(d)
	return d, b.fails == 1
}

// Success resets the backoff. Returns true if it was failing before.
func (b *Backoff) Success() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	was := b.fails > 0
	b.fails, b.until, b.lastErr = 0, time.Time{}, ""
	return was
}

// LastError returns the error of the current failure streak ("" when healthy).
func (b *Backoff) LastError() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastErr
}

// Ready reports whether a network call may be attempted now.
func (b *Backoff) Ready() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.clock().Before(b.until)
}

// Until returns the retry time while backing off.
func (b *Backoff) Until() *time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fails == 0 {
		return nil
	}
	t := b.until
	return &t
}

type fileSig struct {
	size  int64
	mtime time.Time
}

// Daemon is the long-running background process.
type Daemon struct {
	DataDir string
	Plat    func(Config) Platform
	Log     *log.Logger

	cfg       Config
	cfgMod    time.Time
	eng       *Engine
	status    DaemonStatus
	remoteSig chan struct{}
	pollStop  context.CancelFunc
	backoff   Backoff

	// screen state, written by the main loop, read by the poller
	screenMu   sync.Mutex
	screenOn   bool
	screenWake chan struct{}

	lastStatusJSON []byte
	lastStatusAt   time.Time
	localErr       string // non-network error (config, local files); main loop only
}

func (d *Daemon) reloadConfig() (bool, error) {
	cfg, mod, err := LoadConfig(d.DataDir)
	if err != nil {
		return false, err
	}
	if d.eng != nil && mod.Equal(d.cfgMod) {
		return false, nil
	}
	d.cfg, d.cfgMod = cfg, mod
	d.eng = NewEngine(d.DataDir, cfg, d.Plat(cfg), d.Log)
	return true, nil
}

// writeStatus writes daemon.json only when the content changed or the
// heartbeat interval elapsed (instead of every loop iteration).
func (d *Daemon) writeStatus() {
	d.status.RetryAt = d.backoff.Until()
	d.status.Online = d.status.RetryAt == nil
	if e := d.backoff.LastError(); e != "" {
		d.status.LastError = e
	} else if d.localErr != "" {
		d.status.LastError = d.localErr
	} else {
		d.status.LastError = ""
	}
	cmp := d.status
	cmp.UpdatedAt = time.Time{}
	b, _ := json.Marshal(cmp)
	if bytes.Equal(b, d.lastStatusJSON) && time.Since(d.lastStatusAt) < statusHeartbeat {
		return
	}
	d.status.UpdatedAt = time.Now()
	out, _ := json.MarshalIndent(d.status, "", "  ")
	if err := writeAtomic(daemonStatusPath(d.DataDir), out, 0o600); err == nil {
		d.lastStatusJSON, d.lastStatusAt = b, time.Now()
	}
}

func (d *Daemon) setScreen(on bool) (changed bool) {
	d.screenMu.Lock()
	defer d.screenMu.Unlock()
	if d.screenOn == on {
		return false
	}
	d.screenOn = on
	if on {
		close(d.screenWake)
	} else {
		d.screenWake = make(chan struct{})
	}
	return true
}

// waitScreenOn blocks while the screen is off. Returns false if ctx is done.
func (d *Daemon) waitScreenOn(ctx context.Context) bool {
	d.screenMu.Lock()
	on, wake := d.screenOn, d.screenWake
	d.screenMu.Unlock()
	if on {
		return true
	}
	select {
	case <-wake:
		return true
	case <-ctx.Done():
		return false
	}
}

// waitBackoff blocks until the backoff allows another attempt.
func (d *Daemon) waitBackoff(ctx context.Context) bool {
	if t := d.backoff.Until(); t != nil {
		if w := time.Until(*t); w > 0 {
			sleepCtx(ctx, w)
		}
	}
	return ctx.Err() == nil
}

// startPoller runs the long-poll loop that signals remote changes. It pauses
// while the screen is off and backs off on network errors.
func (d *Daemon) startPoller(ctx context.Context) {
	if d.pollStop != nil {
		d.pollStop()
	}
	pctx, cancel := context.WithCancel(ctx)
	d.pollStop = cancel
	eng := d.eng
	go func() {
		seen := -1
		for pctx.Err() == nil {
			if !d.waitScreenOn(pctx) || !d.waitBackoff(pctx) {
				return
			}
			id, err := eng.SaveID()
			if err != nil {
				sleepCtx(pctx, time.Minute)
				continue
			}
			if seen < 0 {
				st, _ := LoadState(d.DataDir)
				seen = st.BaseRevision
			}
			// Abort the long-poll as soon as the screen turns off.
			wctx, wcancel := context.WithCancel(pctx)
			stopWatch := d.cancelOnScreenOff(wctx, wcancel)
			sum, changed, err := eng.Client.Wait(wctx, id, seen, api.DefaultWaitTimout)
			stopWatch()
			screenOffAbort := wctx.Err() != nil && pctx.Err() == nil
			wcancel()
			if screenOffAbort {
				continue // paused by screen off; not a network failure
			}
			if err != nil {
				if pctx.Err() == nil {
					d.netFail("long-poll", err)
				}
				continue
			}
			d.netOK()
			if changed && sum.Latest != nil {
				seen = sum.Latest.Rev
				select {
				case d.remoteSig <- struct{}{}:
				default:
				}
			}
		}
	}()
}

// cancelOnScreenOff cancels the long-poll when the screen turns off.
func (d *Daemon) cancelOnScreenOff(ctx context.Context, cancel context.CancelFunc) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				d.screenMu.Lock()
				on := d.screenOn
				d.screenMu.Unlock()
				if !on {
					cancel()
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

// netFail / netOK only touch the (locked) backoff and the logger, so they are
// safe to call from the long-poll goroutine.
func (d *Daemon) netFail(what string, err error) {
	wait, first := d.backoff.Fail(err)
	if first {
		d.Log.Printf("%s: %v（%v 后重试，之后逐步延长，最长 %v）", what, err, wait, backoffMax)
	}
}

func (d *Daemon) netOK() {
	if d.backoff.Success() {
		d.Log.Print("network recovered")
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Run blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	d.remoteSig = make(chan struct{}, 1)
	d.screenOn, d.screenWake = true, make(chan struct{})
	close(d.screenWake)
	d.status = DaemonStatus{PID: os.Getpid(), StartedAt: time.Now(), ScreenOn: true}
	d.Log.Printf("daemon started pid=%d", os.Getpid())

	var (
		wasRunning     bool
		lastHeartbeat  time.Time
		lastSync       time.Time
		lastScreenAt   time.Time
		syncOnScreenOn bool
		lastSig        map[string]fileSig
		sigChangedAt   time.Time
		pendingUpload  bool
		pendingExit    bool // game exited but the sync failed/was deferred
	)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		changed, err := d.reloadConfig()
		if err != nil {
			d.localErr = err.Error()
		} else if changed {
			if err := d.cfg.Validate(); err != nil {
				d.Log.Printf("config: %v", err)
				d.localErr = err.Error()
				if d.pollStop != nil {
					d.pollStop()
					d.pollStop = nil
				}
			} else {
				d.Log.Printf("config loaded: server=%s save=%q", d.cfg.ServerURL, d.cfg.SaveID)
				d.localErr = ""
				d.backoff.Success()
				d.startPoller(ctx)
				lastSync = time.Time{}
			}
		}
		configured := err == nil && d.cfg.Validate() == nil

		if configured {
			running := d.eng.Plat.GameRunning()
			d.status.GameRunning = running

			// Screen state: sampled every screenCheckInterval; the game running implies on.
			if running {
				d.setScreen(true)
			} else if time.Since(lastScreenAt) >= screenCheckInterval {
				lastScreenAt = time.Now()
				on, ok := d.eng.Plat.ScreenOn()
				if !ok {
					on = true
				}
				if d.setScreen(on) {
					if on {
						d.Log.Print("screen on: resume syncing")
						syncOnScreenOn = true
					} else {
						d.Log.Print("screen off: pause syncing")
					}
				}
			}
			d.screenMu.Lock()
			screenOn := d.screenOn
			d.screenMu.Unlock()
			d.status.ScreenOn = screenOn

			id, idErr := d.eng.SaveID()

			switch {
			case running && !wasRunning:
				d.Log.Print("game started")
				lastSig = d.saveSignature(id)
				pendingUpload = false
				if idErr == nil {
					d.acquireLock(ctx, id, true)
					lastHeartbeat = time.Now()
				}
			case !running && wasRunning:
				d.Log.Print("game exited")
				pendingExit = true
				if idErr == nil && d.status.HoldingLock {
					d.eng.Client.Unlock(ctx, id)
				}
				d.status.HoldingLock, d.status.OtherLock = false, nil
			}
			wasRunning = running

			// Uploading the final save after the game exits is done regardless
			// of the screen state (progress must not be lost), but respects backoff.
			if pendingExit && !running && d.backoff.Ready() {
				if d.syncNow(ctx, SyncOptions{AllowUpload: true, AllowDownload: true}) {
					pendingExit = false
				}
				lastSync = time.Now()
			}

			if running && idErr == nil {
				if time.Since(lastHeartbeat) >= api.LockHeartbeat && d.backoff.Ready() {
					d.acquireLock(ctx, id, false)
					lastHeartbeat = time.Now()
				}
				// Upload saves made while playing once files settle.
				sig := d.saveSignature(id)
				if !sigEqual(sig, lastSig) {
					lastSig, sigChangedAt, pendingUpload = sig, time.Now(), true
				} else if pendingUpload && time.Since(sigChangedAt) >= settleDuration && d.backoff.Ready() {
					if d.syncNow(ctx, SyncOptions{AllowUpload: true}) {
						pendingUpload = false
					}
				}
			}

			select {
			case <-d.remoteSig:
				if !running {
					d.syncNow(ctx, SyncOptions{AllowUpload: true, AllowDownload: d.cfg.AutoDownload})
					lastSync = time.Now()
				} else {
					d.syncNow(ctx, SyncOptions{}) // records pending + notifies
				}
			default:
				due := time.Since(lastSync) >= periodicSync || syncOnScreenOn
				if !running && screenOn && due && d.backoff.Ready() {
					// syncOnScreenOn: catch up immediately after waking, without
					// waiting for the long-poll round trip.
					syncOnScreenOn = false
					d.syncNow(ctx, SyncOptions{AllowUpload: true, AllowDownload: d.cfg.AutoDownload})
					lastSync = time.Now()
				}
			}
		}
		d.writeStatus()

		select {
		case <-ctx.Done():
			if d.status.HoldingLock {
				if id, err := d.eng.SaveID(); err == nil {
					c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					d.eng.Client.Unlock(c, id)
					cancel()
				}
			}
			d.Log.Print("daemon stopped")
			return nil
		case <-ticker.C:
		case <-d.remoteSig:
			// re-deliver so the main body handles it
			select {
			case d.remoteSig <- struct{}{}:
			default:
			}
		}
	}
}

// syncNow runs a sync and updates backoff. Returns true on success.
func (d *Daemon) syncNow(ctx context.Context, opt SyncOptions) bool {
	res, err := d.eng.Sync(ctx, opt)
	if err != nil {
		// Only server errors back off; local errors (bad save id, unreadable files) don't.
		if IsNetError(err) {
			d.netFail("sync", err)
		} else {
			if d.localErr != err.Error() {
				d.Log.Printf("sync error: %v", err)
			}
			d.localErr = err.Error()
		}
		return false
	}
	d.netOK()
	d.localErr = ""
	if res.Action != "none" && res.Action != "in_sync" {
		d.Log.Printf("sync: %s %s", res.Action, res.Message)
	}
	return true
}

func (d *Daemon) acquireLock(ctx context.Context, id string, first bool) {
	// Only SaveGameInfo is read here (not the multi-MB main save).
	gameDate := ""
	if m := d.eng.LocalMeta(id); m != nil {
		gameDate = m.GameDate()
	}
	lk, err := d.eng.Client.Lock(ctx, id, false, gameDate)
	switch {
	case errors.Is(err, client.ErrLocked):
		d.netOK()
		d.status.HoldingLock, d.status.OtherLock = false, lk
		if first && lk != nil {
			ago := time.Since(lk.LastHeartbeat).Round(time.Minute)
			d.eng.notify("⚠ 另一台设备正在游玩",
				fmt.Sprintf("%s 正在游玩此存档（%s，%v 前活动），云端可能还有未上传的进度。", lk.Holder, lk.GameDate, ago))
		}
	case err != nil:
		d.netFail("lock", err)
	default:
		d.netOK()
		d.status.HoldingLock, d.status.OtherLock = true, nil
	}
}

func (d *Daemon) saveSignature(id string) map[string]fileSig {
	out := map[string]fileSig{}
	if id == "" {
		return out
	}
	dir := filepath.Join(d.cfg.SavesDir, id)
	for _, n := range []string{id, "SaveGameInfo"} {
		if st, err := os.Stat(filepath.Join(dir, n)); err == nil {
			out[n] = fileSig{st.Size(), st.ModTime()}
		}
	}
	return out
}

func sigEqual(a, b map[string]fileSig) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
