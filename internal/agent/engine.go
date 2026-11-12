package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/EvansLeeSin/stardew-sync/internal/api"
	"github.com/EvansLeeSin/stardew-sync/internal/client"
	"github.com/EvansLeeSin/stardew-sync/internal/savefmt"
)

const backupKeep = 10

// Engine performs sync operations. All mutating operations hold a
// cross-process file lock so the daemon and CLI (WebUI) never race.
type Engine struct {
	DataDir string
	Cfg     Config
	Client  *client.Client
	Plat    Platform
	Log     *log.Logger
}

func NewEngine(dataDir string, cfg Config, plat Platform, logger *log.Logger) *Engine {
	return &Engine{DataDir: dataDir, Cfg: cfg, Client: client.New(cfg.ServerURL, cfg.Token), Plat: plat, Log: logger}
}

// SaveID returns the configured save id, or auto-detects it when exactly
// one save folder exists locally.
func (e *Engine) SaveID() (string, error) {
	if e.Cfg.SaveID != "" {
		if !savefmt.ValidSaveID(e.Cfg.SaveID) {
			return "", fmt.Errorf("invalid save_id %q", e.Cfg.SaveID)
		}
		return e.Cfg.SaveID, nil
	}
	entries, err := os.ReadDir(e.Cfg.SavesDir)
	if err != nil {
		return "", fmt.Errorf("save_id 未配置且无法读取存档目录: %w", err)
	}
	var found []string
	for _, en := range entries {
		if en.IsDir() && savefmt.ValidSaveID(en.Name()) {
			if _, err := os.Stat(filepath.Join(e.Cfg.SavesDir, en.Name(), savefmt.InfoFile)); err == nil {
				found = append(found, en.Name())
			}
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("save_id 未配置，本地找到 %d 个存档 %v，请执行 config set save_id <名称>", len(found), found)
	}
	return found[0], nil
}

func (e *Engine) localDir(id string) string { return filepath.Join(e.Cfg.SavesDir, id) }

// LocalSnapshot returns the local save snapshot or nil if missing.
func (e *Engine) LocalSnapshot(id string) (*savefmt.Snapshot, error) {
	s, err := savefmt.ReadDir(e.localDir(id), id)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// SyncOptions control what Sync may do.
type SyncOptions struct {
	AllowDownload bool
	AllowUpload   bool
}

// Result describes what a sync did.
type Result struct {
	Action  string // none, in_sync, uploaded, downloaded, conflict, pending, error
	Message string
	Rev     int
}

func (e *Engine) withLock(fn func() error) error {
	unlock, err := lockFile(filepath.Join(e.DataDir, "sync.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// Sync runs one sync cycle according to the decision table in DESIGN.md.
func (e *Engine) Sync(ctx context.Context, opt SyncOptions) (Result, error) {
	var res Result
	err := e.withLock(func() error {
		var err error
		res, err = e.syncLocked(ctx, opt)
		return err
	})
	return res, err
}

func (e *Engine) syncLocked(ctx context.Context, opt SyncOptions) (Result, error) {
	id, err := e.SaveID()
	if err != nil {
		return Result{Action: "error", Message: err.Error()}, err
	}
	st, err := LoadState(e.DataDir)
	if err != nil {
		return Result{}, err
	}
	if st.SaveID != id {
		st = State{SaveID: id}
	}
	res, err := e.decide(ctx, id, &st, opt)
	st.LastSync = time.Now()
	if err != nil {
		st.LastError = err.Error()
	} else {
		st.LastError = ""
		if res.Action != "none" && res.Action != "in_sync" {
			st.LastAction = fmt.Sprintf("%s %s", time.Now().Format("01-02 15:04"), res.Message)
		}
	}
	if serr := SaveState(e.DataDir, st); serr != nil && err == nil {
		err = serr
	}
	return res, err
}

func (e *Engine) decide(ctx context.Context, id string, st *State, opt SyncOptions) (Result, error) {
	local, err := e.LocalSnapshot(id)
	if err != nil {
		return Result{}, fmt.Errorf("读取本地存档: %w", err)
	}
	var remote *api.Revision
	sum, err := e.Client.Latest(ctx, id)
	switch {
	case errors.Is(err, client.ErrNotFound):
	case err != nil:
		return Result{}, fmt.Errorf("查询云端: %w", err)
	default:
		remote = sum.Latest
	}

	if local != nil && remote != nil && local.Hash == remote.Hash {
		st.BaseRevision, st.BaseHash, st.Conflict, st.PendingRemote = remote.Rev, remote.Hash, nil, 0
		return Result{Action: "in_sync", Message: "已是最新", Rev: remote.Rev}, nil
	}
	localChanged := local != nil && local.Hash != st.BaseHash
	remoteChanged := remote != nil && remote.Rev > st.BaseRevision

	switch {
	case localChanged && remoteChanged:
		isNew := st.Conflict == nil || st.Conflict.LocalHash != local.Hash || st.Conflict.RemoteRev != remote.Rev
		st.Conflict = &Conflict{
			DetectedAt: time.Now(), LocalHash: local.Hash, LocalGameDate: local.Meta.GameDate(), LocalPlayed: local.Meta.Played(),
			RemoteRev: remote.Rev, RemoteDevice: remote.Device, RemoteGameDate: remote.GameDate, RemotePlayed: remote.Meta.Played(),
		}
		msg := fmt.Sprintf("冲突：本地 %s / 云端 %s（来自 %s）", local.Meta.GameDate(), remote.GameDate, remote.Device)
		if isNew {
			e.notify("⚠ 存档冲突", msg+"，请在模块 WebUI 中选择保留哪份")
		}
		return Result{Action: "conflict", Message: msg}, nil
	case localChanged:
		if !opt.AllowUpload {
			return Result{Action: "none"}, nil
		}
		return e.upload(ctx, id, st, false)
	case remoteChanged:
		return e.download(ctx, id, st, remote, opt)
	}
	return Result{Action: "none", Message: "无变化"}, nil
}

func (e *Engine) upload(ctx context.Context, id string, st *State, force bool) (Result, error) {
	data, err := savefmt.PackDir(e.localDir(id))
	if err != nil {
		return Result{}, err
	}
	arc, err := savefmt.ReadArchive(data, id)
	if err != nil {
		return Result{}, err
	}
	ur, err := e.Client.Upload(ctx, id, data, st.BaseRevision, force)
	var ce *client.ConflictError
	if errors.As(err, &ce) {
		// Someone uploaded meanwhile; next cycle will classify it as conflict.
		return Result{Action: "pending", Message: ce.Error()}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("上传: %w", err)
	}
	st.BaseRevision, st.BaseHash, st.Conflict = ur.Revision.Rev, arc.Hash, nil
	msg := fmt.Sprintf("已上传 #%d（%s）", ur.Revision.Rev, arc.Meta.GameDate())
	e.Log.Print(msg)
	return Result{Action: "uploaded", Message: msg, Rev: ur.Revision.Rev}, nil
}

func (e *Engine) download(ctx context.Context, id string, st *State, remote *api.Revision, opt SyncOptions) (Result, error) {
	if running := e.Plat.GameRunning(); !opt.AllowDownload || running {
		if st.PendingRemote != remote.Rev {
			st.PendingRemote = remote.Rev
			hint := "可在模块 WebUI 中手动同步"
			if running {
				hint = "退出游戏后会自动同步"
			}
			e.notify("云端有新存档", fmt.Sprintf("%s 上传了 %s 的进度，%s", remote.Device, remote.GameDate, hint))
		}
		return Result{Action: "pending", Message: fmt.Sprintf("云端 #%d（%s）待应用", remote.Rev, remote.GameDate)}, nil
	}
	if lv := e.Plat.GameVersion(); savefmt.CompareVersions(remote.Meta.GameVersion, lv) > 0 {
		if st.PendingRemote != remote.Rev {
			st.PendingRemote = remote.Rev
			e.notify("⚠ 请先更新游戏", fmt.Sprintf("云端存档版本 %s 高于本机游戏 %s", remote.Meta.GameVersion, lv))
		}
		return Result{Action: "pending", Message: fmt.Sprintf("云端存档版本 %s 高于本机 %s", remote.Meta.GameVersion, lv)}, nil
	}
	data, err := e.Client.Download(ctx, id, remote.Rev)
	if err != nil {
		return Result{}, fmt.Errorf("下载: %w", err)
	}
	arc, err := savefmt.ReadArchive(data, id)
	if err != nil {
		return Result{}, err
	}
	if arc.Hash != remote.Hash {
		return Result{}, errors.New("下载内容哈希不匹配")
	}
	if _, err := Backup(e.DataDir, e.Cfg.SavesDir, id, "before-download", backupKeep); err != nil {
		return Result{}, fmt.Errorf("备份失败，已取消覆盖: %w", err)
	}
	if e.Plat.GameRunning() { // re-check right before writing
		return Result{Action: "pending", Message: "游戏已启动，取消覆盖"}, nil
	}
	if err := e.Plat.Apply(e.Cfg.SavesDir, id, arc); err != nil {
		return Result{}, fmt.Errorf("写入存档: %w", err)
	}
	st.BaseRevision, st.BaseHash, st.Conflict, st.PendingRemote = remote.Rev, remote.Hash, nil, 0
	msg := fmt.Sprintf("已同步云端 #%d（%s，来自 %s）", remote.Rev, remote.GameDate, remote.Device)
	e.Log.Print(msg)
	e.notify("存档已同步", msg)
	return Result{Action: "downloaded", Message: msg, Rev: remote.Rev}, nil
}

// Resolve resolves a conflict by keeping "local" or "remote".
func (e *Engine) Resolve(ctx context.Context, keep string) (Result, error) {
	var res Result
	err := e.withLock(func() error {
		id, err := e.SaveID()
		if err != nil {
			return err
		}
		st, err := LoadState(e.DataDir)
		if err != nil {
			return err
		}
		if st.SaveID != id {
			st = State{SaveID: id}
		}
		sum, err := e.Client.Latest(ctx, id)
		if err != nil && !errors.Is(err, client.ErrNotFound) {
			return err
		}
		switch keep {
		case "local":
			if local, _ := e.LocalSnapshot(id); local == nil {
				return errors.New("本地没有存档")
			}
			if sum != nil && sum.Latest != nil {
				st.BaseRevision = sum.Latest.Rev
			}
			res, err = e.upload(ctx, id, &st, true)
		case "remote":
			if sum == nil || sum.Latest == nil {
				return errors.New("云端没有存档")
			}
			st.BaseHash = "" // treat local as unchanged-but-different
			st.BaseRevision = 0
			res, err = e.download(ctx, id, &st, sum.Latest, SyncOptions{AllowDownload: true})
			if err == nil && res.Action != "downloaded" {
				err = errors.New(res.Message)
			}
		default:
			return fmt.Errorf("keep must be local or remote")
		}
		if err != nil {
			return err
		}
		st.Conflict = nil
		st.LastAction = fmt.Sprintf("%s %s", time.Now().Format("01-02 15:04"), res.Message)
		return SaveState(e.DataDir, st)
	})
	return res, err
}

// Restore makes an old revision the latest on the server, then syncs it down.
func (e *Engine) Restore(ctx context.Context, rev int) (Result, error) {
	id, err := e.SaveID()
	if err != nil {
		return Result{}, err
	}
	if _, err := e.Client.Restore(ctx, id, rev); err != nil {
		return Result{}, err
	}
	return e.Resolve(ctx, "remote")
}

func (e *Engine) notify(title, text string) {
	if e.Cfg.Notify && e.Plat != nil {
		e.Plat.Notify(title, text)
	}
}
