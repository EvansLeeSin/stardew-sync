package agent

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/EvansLeeSin/stardew-sync/internal/api"
	"github.com/EvansLeeSin/stardew-sync/internal/savefmt"
	"github.com/EvansLeeSin/stardew-sync/internal/server"
)

const sid = "Sunny_123"

type mockPlat struct {
	running bool
	version string
	notes   []string
}

func (m *mockPlat) GameRunning() bool   { return m.running }
func (m *mockPlat) GameVersion() string { return m.version }
func (m *mockPlat) Notify(t, s string)  { m.notes = append(m.notes, t+": "+s) }
func (m *mockPlat) Apply(dir, id string, a *savefmt.Archive) error {
	return applyReplace(dir, id, a)
}

func writeSave(t *testing.T, savesDir string, day int, ver string) {
	d := filepath.Join(savesDir, sid)
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, sid), []byte("<SaveGame day='"+strconv.Itoa(day)+"'/>"), 0o644)
	info := `<Farmer><name>E</name><gameVersion>` + ver + `</gameVersion><yearForSaveGame>1</yearForSaveGame><seasonForSaveGame>0</seasonForSaveGame><dayOfMonthForSaveGame>` + strconv.Itoa(day) + `</dayOfMonthForSaveGame></Farmer>`
	os.WriteFile(filepath.Join(d, savefmt.InfoFile), []byte(info), 0o644)
}

func localDay(t *testing.T, savesDir string) int {
	s, err := savefmt.ReadDir(filepath.Join(savesDir, sid), sid)
	if err != nil {
		t.Fatal(err)
	}
	return s.Meta.Day
}

type device struct {
	eng   *Engine
	plat  *mockPlat
	saves string
}

func setup(t *testing.T) (dev1, dev2 *device, srv *httptest.Server) {
	dir := t.TempDir()
	st, _ := server.NewStore(filepath.Join(dir, "srv"), 30)
	devs := server.NewDevices(filepath.Join(dir, "srv", "devices.json"))
	t1, _ := devs.Add("pc")
	t2, _ := devs.Add("android")
	s := &server.Server{Store: st, Locks: server.NewLocks(api.LockTTL), Devices: devs, Logger: log.New(io.Discard, "", 0)}
	srv = httptest.NewServer(s.Handler())
	mk := func(name, tok string) *device {
		saves := filepath.Join(dir, name, "Saves")
		os.MkdirAll(saves, 0o755)
		cfg := DefaultConfig()
		cfg.ServerURL, cfg.Token, cfg.SavesDir = srv.URL, tok, saves
		p := &mockPlat{version: "1.6.15"}
		return &device{eng: NewEngine(filepath.Join(dir, name, "data"), cfg, p, log.New(io.Discard, "", 0)), plat: p, saves: saves}
	}
	return mk("pc", t1), mk("android", t2), srv
}

var all = SyncOptions{AllowUpload: true, AllowDownload: true}

func TestEngineFlow(t *testing.T) {
	pc, phone, srv := setup(t)
	defer srv.Close()
	ctx := context.Background()

	writeSave(t, pc.saves, 1, "1.6.15")
	if r, err := pc.eng.Sync(ctx, all); err != nil || r.Action != "uploaded" || r.Rev != 1 {
		t.Fatalf("pc upload %+v %v", r, err)
	}
	// phone has no save yet; save_id must be configured
	phone.eng.Cfg.SaveID = sid
	if r, err := phone.eng.Sync(ctx, all); err != nil || r.Action != "downloaded" {
		t.Fatalf("phone download %+v %v", r, err)
	}
	if localDay(t, phone.saves) != 1 {
		t.Fatal("phone day")
	}
	if r, _ := phone.eng.Sync(ctx, all); r.Action != "in_sync" {
		t.Fatalf("expected in_sync %+v", r)
	}

	// phone plays, uploads day 2
	writeSave(t, phone.saves, 2, "1.6.15")
	if r, _ := phone.eng.Sync(ctx, all); r.Action != "uploaded" || r.Rev != 2 {
		t.Fatalf("phone upload %+v", r)
	}
	// pc: game running -> pending, not applied
	pc.plat.running = true
	if r, _ := pc.eng.Sync(ctx, all); r.Action != "pending" || localDay(t, pc.saves) != 1 {
		t.Fatalf("pending %+v", r)
	}
	pc.plat.running = false
	if r, _ := pc.eng.Sync(ctx, all); r.Action != "downloaded" || localDay(t, pc.saves) != 2 {
		t.Fatalf("pc download %+v", r)
	}
	// backup created before overwrite
	if b, _ := os.ReadDir(filepath.Join(pc.eng.DataDir, "backups")); len(b) != 1 {
		t.Fatalf("backups %d", len(b))
	}

	// conflict: both change
	writeSave(t, pc.saves, 3, "1.6.15")
	writeSave(t, phone.saves, 4, "1.6.15")
	if r, _ := phone.eng.Sync(ctx, all); r.Action != "uploaded" {
		t.Fatal("phone upload 4")
	}
	r, _ := pc.eng.Sync(ctx, all)
	st, _ := LoadState(pc.eng.DataDir)
	if r.Action != "conflict" || st.Conflict == nil || st.Conflict.RemoteRev != 3 {
		t.Fatalf("conflict %+v %+v", r, st.Conflict)
	}
	// keep local on pc
	if r, err := pc.eng.Resolve(ctx, "local"); err != nil || r.Action != "uploaded" || r.Rev != 4 {
		t.Fatalf("resolve local %+v %v", r, err)
	}
	if r, _ := phone.eng.Sync(ctx, all); r.Action != "downloaded" || localDay(t, phone.saves) != 3 {
		t.Fatalf("phone after resolve %+v", r)
	}

	// restore rev 1 from phone
	if r, err := phone.eng.Restore(ctx, 1); err != nil || r.Action != "downloaded" || localDay(t, phone.saves) != 1 {
		t.Fatalf("restore %+v %v", r, err)
	}
	if r, _ := pc.eng.Sync(ctx, all); r.Action != "downloaded" || localDay(t, pc.saves) != 1 {
		t.Fatalf("pc after restore %+v", r)
	}

	// version guard
	writeSave(t, pc.saves, 9, "1.7.0")
	pc.eng.Sync(ctx, all)
	if r, _ := phone.eng.Sync(ctx, all); r.Action != "pending" || localDay(t, phone.saves) != 1 {
		t.Fatalf("version guard %+v", r)
	}
}

func TestAutoDetectSaveID(t *testing.T) {
	pc, _, srv := setup(t)
	defer srv.Close()
	if _, err := pc.eng.SaveID(); err == nil {
		t.Fatal("expected error with no saves")
	}
	writeSave(t, pc.saves, 1, "1.6.15")
	if id, err := pc.eng.SaveID(); err != nil || id != sid {
		t.Fatal(id, err)
	}
}
