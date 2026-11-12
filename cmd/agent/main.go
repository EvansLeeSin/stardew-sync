// Command stardew-sync is the Android daemon and CLI.
//
//	stardew-sync daemon                 run background daemon
//	stardew-sync status                 JSON status (used by WebUI)
//	stardew-sync sync                   sync now (upload/download)
//	stardew-sync push | pull            force direction (conflict resolution)
//	stardew-sync revisions              JSON revision list
//	stardew-sync restore <rev>          restore a cloud revision and apply it
//	stardew-sync config get             JSON config (token redacted)
//	stardew-sync config set <k> <v>     set server_url/token/save_id/saves_dir/notify/auto_download
//	stardew-sync test                   check server connectivity and token
//
// Global flag: -data <dir> (default /data/adb/stardew-sync).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/EvansLeeSin/stardew-sync/internal/agent"
	"github.com/EvansLeeSin/stardew-sync/internal/savefmt"
)

var version = "dev"

func main() {
	dataDir := flag.String("data", "/data/adb/stardew-sync", "data directory")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: stardew-sync [-data dir] daemon|status|sync|push|pull|revisions|restore <rev>|config get|config set <k> <v>|test|version")
	}
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		fail(err)
	}
	newPlat := func(c agent.Config) agent.Platform { return &agent.AndroidPlatform{Package: c.Package} }

	switch args[0] {
	case "version":
		fmt.Println(version)
	case "daemon":
		runDaemon(*dataDir, newPlat)
	case "config":
		configCmd(*dataDir, args[1:])
	default:
		cfg, _, err := agent.LoadConfig(*dataDir)
		if err != nil {
			fail(err)
		}
		logger := cliLogger(*dataDir)
		eng := agent.NewEngine(*dataDir, cfg, newPlat(cfg), logger)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		switch args[0] {
		case "status":
			status(ctx, *dataDir, cfg, eng)
		case "test":
			if err := cfg.Validate(); err != nil {
				fail(err)
			}
			dev, err := eng.Client.WhoAmI(ctx)
			if err != nil {
				fail(err)
			}
			out(map[string]any{"ok": true, "device": dev})
		default:
			if err := cfg.Validate(); err != nil {
				fail(err)
			}
			var res agent.Result
			switch args[0] {
			case "sync":
				res, err = eng.Sync(ctx, agent.SyncOptions{AllowUpload: true, AllowDownload: true})
			case "push":
				res, err = eng.Resolve(ctx, "local")
			case "pull":
				res, err = eng.Resolve(ctx, "remote")
			case "restore":
				if len(args) != 2 {
					fail(fmt.Errorf("usage: restore <rev>"))
				}
				rev, perr := strconv.Atoi(args[1])
				if perr != nil {
					fail(perr)
				}
				res, err = eng.Restore(ctx, rev)
			case "revisions":
				id, ierr := eng.SaveID()
				if ierr != nil {
					fail(ierr)
				}
				revs, rerr := eng.Client.Revisions(ctx, id)
				if rerr != nil {
					fail(rerr)
				}
				out(revs)
				return
			default:
				flag.Usage()
				os.Exit(2)
			}
			if err != nil {
				fail(err)
			}
			out(map[string]any{"ok": true, "action": res.Action, "message": res.Message, "rev": res.Rev})
		}
	}
}

func out(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func fail(err error) {
	out(map[string]any{"ok": false, "error": err.Error()})
	os.Exit(1)
}

func openLog(dataDir string) io.Writer {
	p := filepath.Join(dataDir, "sync.log")
	if st, err := os.Stat(p); err == nil && st.Size() > 1<<20 {
		os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return os.Stderr
	}
	return f
}

func cliLogger(dataDir string) *log.Logger {
	return log.New(openLog(dataDir), "[cli] ", log.LstdFlags)
}

func runDaemon(dataDir string, newPlat func(agent.Config) agent.Platform) {
	logger := log.New(openLog(dataDir), "", log.LstdFlags)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	d := &agent.Daemon{DataDir: dataDir, Plat: newPlat, Log: logger}
	if err := d.Run(ctx); err != nil {
		logger.Fatal(err)
	}
}

func status(ctx context.Context, dataDir string, cfg agent.Config, eng *agent.Engine) {
	st, _ := agent.LoadState(dataDir)
	res := map[string]any{"ok": true, "version": version, "config": cfg.Redacted(), "state": st, "configured": cfg.Validate() == nil}
	if ds, err := agent.LoadDaemonStatus(dataDir); err == nil {
		ds2 := *ds
		alive := processAlive(ds.PID) && time.Since(ds.UpdatedAt) < time.Minute
		res["daemon"] = map[string]any{"alive": alive, "info": ds2}
	} else {
		res["daemon"] = map[string]any{"alive": false}
	}
	res["game_running"] = eng.Plat.GameRunning()
	res["game_version"] = eng.Plat.GameVersion()
	if id, err := eng.SaveID(); err == nil {
		res["save_id"] = id
		if snap, err := eng.LocalSnapshot(id); err == nil && snap != nil {
			res["local"] = map[string]any{"hash": snap.Hash, "game_date": snap.Meta.GameDate(), "played": snap.Meta.Played(), "meta": snap.Meta, "changed": snap.Hash != st.BaseHash}
		}
		if cfg.Validate() == nil {
			c, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if sum, err := eng.Client.Latest(c, id); err == nil {
				res["remote"] = sum
			} else {
				res["remote_error"] = err.Error()
			}
		}
	} else {
		res["save_id_error"] = err.Error()
		res["local_saves"] = listSaves(cfg.SavesDir)
	}
	out(res)
}

func listSaves(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var outl []string
	for _, e := range entries {
		if e.IsDir() && savefmt.ValidSaveID(e.Name()) {
			outl = append(outl, e.Name())
		}
	}
	return outl
}

func configCmd(dataDir string, args []string) {
	cfg, _, err := agent.LoadConfig(dataDir)
	if err != nil {
		fail(err)
	}
	if len(args) == 1 && args[0] == "get" {
		out(map[string]any{"ok": true, "config": cfg.Redacted(), "has_token": cfg.Token != ""})
		return
	}
	if len(args) != 3 || args[0] != "set" {
		fail(fmt.Errorf("usage: config get | config set <key> <value>"))
	}
	k, v := args[1], strings.TrimSpace(args[2])
	switch k {
	case "server_url":
		if v != "" && !strings.HasPrefix(v, "https://") && !strings.HasPrefix(v, "http://") {
			fail(fmt.Errorf("server_url 必须以 https:// 开头"))
		}
		cfg.ServerURL = strings.TrimRight(v, "/")
	case "token":
		cfg.Token = v
	case "save_id":
		if v != "" && !savefmt.ValidSaveID(v) {
			fail(fmt.Errorf("invalid save_id"))
		}
		cfg.SaveID = v
	case "saves_dir":
		cfg.SavesDir = v
	case "notify":
		cfg.Notify = v == "true" || v == "1"
	case "auto_download":
		cfg.AutoDownload = v == "true" || v == "1"
	default:
		fail(fmt.Errorf("unknown key %q", k))
	}
	if err := agent.SaveConfig(dataDir, cfg); err != nil {
		fail(err)
	}
	out(map[string]any{"ok": true, "config": cfg.Redacted()})
}
