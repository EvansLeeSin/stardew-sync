package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
	HoldingLock bool      `json:"holding_lock"`
	OtherLock   *api.Lock `json:"other_lock,omitempty"`
	Online      bool      `json:"online"`
	LastError   string    `json:"last_error,omitempty"`
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
	periodicSync   = 10 * time.Minute
	settleDuration = 5 * time.Second
)

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

func (d *Daemon) writeStatus() {
	d.status.UpdatedAt = time.Now()
	b, _ := json.MarshalIndent(d.status, "", "  ")
	writeAtomic(daemonStatusPath(d.DataDir), b, 0o600)
}

// startPoller runs the long-poll loop that signals remote changes.
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
			id, err := eng.SaveID()
			if err != nil {
				sleepCtx(pctx, time.Minute)
				continue
			}
			if seen < 0 {
				st, _ := LoadState(d.DataDir)
				seen = st.BaseRevision
			}
			sum, changed, err := eng.Client.Wait(pctx, id, seen, api.DefaultWaitTimout)
			if err != nil {
				if pctx.Err() == nil {
					d.Log.Printf("long-poll: %v", err)
					sleepCtx(pctx, 30*time.Second)
				}
				continue
			}
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
	d.status = DaemonStatus{PID: os.Getpid(), StartedAt: time.Now()}
	d.Log.Printf("daemon started pid=%d", os.Getpid())

	var (
		wasRunning    bool
		lastHeartbeat time.Time
		lastSync      time.Time
		lastSig       map[string]fileSig
		sigChangedAt  time.Time
		pendingUpload bool
	)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		changed, err := d.reloadConfig()
		if err != nil {
			d.status.LastError = err.Error()
			d.writeStatus()
		} else if changed {
			if err := d.cfg.Validate(); err != nil {
				d.Log.Printf("config: %v", err)
				d.status.LastError = err.Error()
				if d.pollStop != nil {
					d.pollStop()
					d.pollStop = nil
				}
			} else {
				d.Log.Printf("config loaded: server=%s save=%q", d.cfg.ServerURL, d.cfg.SaveID)
				d.status.LastError = ""
				d.startPoller(ctx)
				lastSync = time.Time{}
			}
		}
		configured := err == nil && d.cfg.Validate() == nil

		if configured {
			running := d.eng.Plat.GameRunning()
			d.status.GameRunning = running
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
				d.syncNow(ctx, SyncOptions{AllowUpload: true, AllowDownload: true})
				lastSync = time.Now()
				if idErr == nil && d.status.HoldingLock {
					d.eng.Client.Unlock(ctx, id)
				}
				d.status.HoldingLock, d.status.OtherLock = false, nil
			}
			wasRunning = running

			if running && idErr == nil {
				if time.Since(lastHeartbeat) >= api.LockHeartbeat {
					d.acquireLock(ctx, id, false)
					lastHeartbeat = time.Now()
				}
				// Upload saves made while playing once files settle.
				sig := d.saveSignature(id)
				if !sigEqual(sig, lastSig) {
					lastSig, sigChangedAt, pendingUpload = sig, time.Now(), true
				} else if pendingUpload && time.Since(sigChangedAt) >= settleDuration {
					pendingUpload = false
					d.syncNow(ctx, SyncOptions{AllowUpload: true})
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
				if !running && time.Since(lastSync) >= periodicSync {
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

func (d *Daemon) syncNow(ctx context.Context, opt SyncOptions) {
	res, err := d.eng.Sync(ctx, opt)
	if err != nil {
		d.status.Online = false
		d.status.LastError = err.Error()
		d.Log.Printf("sync error: %v", err)
		return
	}
	d.status.Online, d.status.LastError = true, ""
	if res.Action != "none" && res.Action != "in_sync" {
		d.Log.Printf("sync: %s %s", res.Action, res.Message)
	}
}

func (d *Daemon) acquireLock(ctx context.Context, id string, first bool) {
	gameDate := ""
	if s, _ := d.eng.LocalSnapshot(id); s != nil {
		gameDate = s.Meta.GameDate()
	}
	lk, err := d.eng.Client.Lock(ctx, id, false, gameDate)
	switch {
	case errors.Is(err, client.ErrLocked):
		d.status.HoldingLock, d.status.OtherLock = false, lk
		if first && lk != nil {
			ago := time.Since(lk.LastHeartbeat).Round(time.Minute)
			d.eng.notify("⚠ 另一台设备正在游玩",
				fmt.Sprintf("%s 正在游玩此存档（%s，%v 前活动），云端可能还有未上传的进度。", lk.Holder, lk.GameDate, ago))
		}
	case err != nil:
		d.Log.Printf("lock: %v", err)
	default:
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
