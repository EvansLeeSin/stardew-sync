// Command stardew-sync-server is the save sync server.
//
//	stardew-sync-server serve
//	stardew-sync-server token add <device>
//	stardew-sync-server token list
//	stardew-sync-server token revoke <device>
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/EvansLeeSin/stardew-sync/internal/api"
	"github.com/EvansLeeSin/stardew-sync/internal/server"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	dataDir := env("SYNC_DATA_DIR", "./data")
	devices := server.NewDevices(filepath.Join(dataDir, "devices.json"))
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"serve"}
	}
	switch args[0] {
	case "serve":
		serve(dataDir, devices)
	case "token":
		tokenCmd(devices, args[1:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  stardew-sync-server serve                 start server (env SYNC_LISTEN, SYNC_DATA_DIR, SYNC_KEEP_REVISIONS)
  stardew-sync-server token add <device>    create device token (printed once)
  stardew-sync-server token list
  stardew-sync-server token revoke <device>`)
	os.Exit(2)
}

func tokenCmd(d *server.Devices, args []string) {
	if err := os.MkdirAll(filepath.Dir(d.Path()), 0o750); err != nil {
		log.Fatal(err)
	}
	if len(args) == 0 {
		usage()
	}
	switch {
	case args[0] == "add" && len(args) == 2:
		tok, err := d.Add(args[1])
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("device: %s\ntoken:  %s\n(保存好此 token，之后无法再次查看)\n", args[1], tok)
	case args[0] == "list":
		list, err := d.List()
		if err != nil {
			log.Fatal(err)
		}
		for _, dev := range list {
			fmt.Printf("%-20s created %s\n", dev.Name, dev.CreatedAt.Format(time.RFC3339))
		}
	case args[0] == "revoke" && len(args) == 2:
		if err := d.Revoke(args[1]); err != nil {
			log.Fatal(err)
		}
		fmt.Println("revoked", args[1])
	default:
		usage()
	}
}

func serve(dataDir string, devices *server.Devices) {
	logger := log.New(os.Stdout, "", log.LstdFlags)
	keep, _ := strconv.Atoi(env("SYNC_KEEP_REVISIONS", "30"))
	store, err := server.NewStore(dataDir, keep)
	if err != nil {
		logger.Fatal(err)
	}
	if list, _ := devices.List(); len(list) == 0 {
		logger.Printf("WARNING: no devices registered. Run: stardew-sync-server token add <device>")
	}
	s := &server.Server{Store: store, Locks: server.NewLocks(api.LockTTL), Devices: devices, Logger: logger}
	addr := env("SYNC_LISTEN", ":8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		logger.Printf("listening on %s, data dir %s", addr, dataDir)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal(err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
