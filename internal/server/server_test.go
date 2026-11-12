package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EvansLeeSin/stardew-sync/internal/api"
	"github.com/EvansLeeSin/stardew-sync/internal/savefmt"
)

const saveID = "Sunny_123"

func info(day int) string {
	return `<Farmer><name>E</name><gameVersion>1.6.15</gameVersion><yearForSaveGame>1</yearForSaveGame><seasonForSaveGame>0</seasonForSaveGame><dayOfMonthForSaveGame>` +
		itoa(day) + `</dayOfMonthForSaveGame></Farmer>`
}

func itoa(i int) string { return strconv.Itoa(i) }

func makeZip(t *testing.T, day int) []byte {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, saveID), []byte("<SaveGame day='"+itoa(day)+"'/>"), 0o644)
	os.WriteFile(filepath.Join(dir, savefmt.InfoFile), []byte(info(day)), 0o644)
	b, err := savefmt.PackDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type env struct {
	t     *testing.T
	srv   *httptest.Server
	pc    string
	phone string
}

func newEnv(t *testing.T) *env {
	dir := t.TempDir()
	st, err := NewStore(dir, 30)
	if err != nil {
		t.Fatal(err)
	}
	dev := NewDevices(filepath.Join(dir, "devices.json"))
	pc, _ := dev.Add("pc")
	phone, _ := dev.Add("android")
	s := &Server{Store: st, Locks: NewLocks(api.LockTTL), Devices: dev, Logger: log.New(io.Discard, "", 0)}
	return &env{t: t, srv: httptest.NewServer(s.Handler()), pc: pc, phone: phone}
}

func (e *env) do(method, path, token string, body io.Reader, ctype string) (*http.Response, []byte) {
	req, _ := http.NewRequest(method, e.srv.URL+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (e *env) upload(token string, data []byte, base int, force bool) (int, []byte) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	m, _ := json.Marshal(api.UploadMeta{BaseRevision: base, Force: force})
	mw.WriteField("meta", string(m))
	fw, _ := mw.CreateFormFile("archive", "save.zip")
	fw.Write(data)
	mw.Close()
	resp, b := e.do("POST", "/api/v1/saves/"+saveID+"/revisions", token, &buf, mw.FormDataContentType())
	return resp.StatusCode, b
}

func TestUploadConflictRestore(t *testing.T) {
	e := newEnv(t)
	defer e.srv.Close()

	if r, _ := e.do("GET", "/api/v1/saves", "bad", nil, ""); r.StatusCode != 401 {
		t.Fatal("expected 401")
	}
	if r, _ := e.do("GET", "/api/v1/saves/"+saveID, e.pc, nil, ""); r.StatusCode != 404 {
		t.Fatal("expected 404")
	}
	code, b := e.upload(e.pc, makeZip(t, 1), 0, false)
	if code != 201 {
		t.Fatalf("upload1 %d %s", code, b)
	}
	// duplicate
	code, b = e.upload(e.phone, makeZip(t, 1), 0, false)
	var ur api.UploadResult
	json.Unmarshal(b, &ur)
	if code != 200 || !ur.Duplicate || ur.Revision.Rev != 1 {
		t.Fatalf("dup %d %s", code, b)
	}
	code, _ = e.upload(e.phone, makeZip(t, 2), 1, false)
	if code != 201 {
		t.Fatal("upload2", code)
	}
	// stale base -> 409
	code, b = e.upload(e.pc, makeZip(t, 3), 1, false)
	var ce api.ConflictError
	json.Unmarshal(b, &ce)
	if code != 409 || ce.Latest.Rev != 2 || ce.Latest.Device != "android" {
		t.Fatalf("conflict %d %s", code, b)
	}
	// force
	code, _ = e.upload(e.pc, makeZip(t, 3), 1, true)
	if code != 201 {
		t.Fatal("force", code)
	}
	// restore rev 1 -> rev 4
	r, b := e.do("POST", "/api/v1/saves/"+saveID+"/restore/1", e.pc, nil, "")
	json.Unmarshal(b, &ur)
	if r.StatusCode != 201 || ur.Revision.Rev != 4 || ur.Revision.RestoredFrom != 1 || ur.Revision.Meta.Day != 1 {
		t.Fatalf("restore %d %s", r.StatusCode, b)
	}
	r, b = e.do("GET", "/api/v1/saves/"+saveID+"/revisions/4/archive", e.phone, nil, "")
	if r.StatusCode != 200 {
		t.Fatal("download")
	}
	if arc, err := savefmt.ReadArchive(b, saveID); err != nil || arc.Meta.Day != 1 {
		t.Fatal("archive", err)
	}
	_, b = e.do("GET", "/api/v1/saves/"+saveID+"/revisions", e.pc, nil, "")
	var revs []api.Revision
	json.Unmarshal(b, &revs)
	if len(revs) != 4 || revs[0].Rev != 4 {
		t.Fatalf("revs %s", b)
	}
	// bad zip
	if code, _ := e.upload(e.pc, []byte("nope"), 4, false); code != 400 {
		t.Fatal("bad zip", code)
	}
}

func TestWait(t *testing.T) {
	e := newEnv(t)
	defer e.srv.Close()
	r, _ := e.do("GET", "/api/v1/saves/"+saveID+"/wait?after=0&timeout=1", e.phone, nil, "")
	if r.StatusCode != 204 {
		t.Fatal("expected 204", r.StatusCode)
	}
	done := make(chan int)
	go func() {
		r, _ := e.do("GET", "/api/v1/saves/"+saveID+"/wait?after=0&timeout=10", e.phone, nil, "")
		done <- r.StatusCode
	}()
	time.Sleep(200 * time.Millisecond)
	e.upload(e.pc, makeZip(t, 1), 0, false)
	select {
	case c := <-done:
		if c != 200 {
			t.Fatal(c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return")
	}
}

func TestLock(t *testing.T) {
	e := newEnv(t)
	defer e.srv.Close()
	p := "/api/v1/saves/" + saveID + "/lock"
	r, _ := e.do("PUT", p, e.pc, strings.NewReader(`{"game_date":"第1年 春 1日"}`), "application/json")
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	r, b := e.do("PUT", p, e.phone, nil, "")
	var lr api.LockResponse
	json.Unmarshal(b, &lr)
	if r.StatusCode != 423 || lr.Lock.Holder != "pc" || lr.Lock.GameDate == "" {
		t.Fatalf("expected 423 %s", b)
	}
	if r, _ := e.do("DELETE", p, e.phone, nil, ""); r.StatusCode != 423 {
		t.Fatal("non-holder release must fail")
	}
	if r, _ := e.do("PUT", p, e.phone, strings.NewReader(`{"force":true}`), ""); r.StatusCode != 200 {
		t.Fatal("force")
	}
	if r, _ := e.do("DELETE", p, e.phone, nil, ""); r.StatusCode != 204 {
		t.Fatal("release")
	}
	if r, _ := e.do("PUT", p, e.pc, nil, ""); r.StatusCode != 200 {
		t.Fatal("reacquire")
	}
}

func TestLockExpiry(t *testing.T) {
	l := NewLocks(time.Minute)
	now := time.Now()
	l.now = func() time.Time { return now }
	l.Acquire("s", "pc", false, "")
	if _, ok := l.Acquire("s", "android", false, ""); ok {
		t.Fatal("should be held")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := l.Acquire("s", "android", false, ""); !ok {
		t.Fatal("should have expired")
	}
}

func TestPrune(t *testing.T) {
	st := &Store{keepLast: 3}
	idx := &saveIndex{}
	for i := 1; i <= 8; i++ {
		season := 0
		if i > 4 {
			season = 1
		}
		idx.Revisions = append(idx.Revisions, api.Revision{Rev: i, Meta: savefmt.Meta{Year: 1, Season: season, Day: i}})
	}
	removed := st.prune(idx)
	// keep 6,7,8 (last 3) + 4 (last of spring) ; remove 1,2,3,5
	if len(removed) != 4 || len(idx.Revisions) != 4 || idx.Revisions[0].Rev != 4 {
		t.Fatalf("removed %v kept %v", removed, idx.Revisions)
	}
}
