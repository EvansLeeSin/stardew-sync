package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/EvansLeeSin/stardew-sync/internal/api"
	"github.com/EvansLeeSin/stardew-sync/internal/savefmt"
)

// Server wires the HTTP API.
type Server struct {
	Store   *Store
	Locks   *Locks
	Devices *Devices
	Logger  *log.Logger
}

type ctxKey struct{}

func deviceFrom(r *http.Request) string {
	v, _ := r.Context().Value(ctxKey{}).(string)
	return v
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.Handle("GET /api/v1/whoami", s.auth(s.whoami))
	mux.Handle("GET /api/v1/saves", s.auth(s.listSaves))
	mux.Handle("GET /api/v1/saves/{id}", s.auth(s.getSave))
	mux.Handle("GET /api/v1/saves/{id}/revisions", s.auth(s.listRevisions))
	mux.Handle("GET /api/v1/saves/{id}/revisions/{rev}/archive", s.auth(s.download))
	mux.Handle("POST /api/v1/saves/{id}/revisions", s.auth(s.upload))
	mux.Handle("POST /api/v1/saves/{id}/restore/{rev}", s.auth(s.restore))
	mux.Handle("GET /api/v1/saves/{id}/wait", s.auth(s.wait))
	mux.Handle("GET /api/v1/saves/{id}/lock", s.auth(s.getLock))
	mux.Handle("PUT /api/v1/saves/{id}/lock", s.auth(s.putLock))
	mux.Handle("DELETE /api/v1/saves/{id}/lock", s.auth(s.deleteLock))
	return s.logging(mux)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: 200}
		rw.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(rw, r)
		if r.URL.Path != "/healthz" {
			s.Logger.Printf("%s %s %d %s dev=%s", r.Method, r.URL.Path, rw.status, time.Since(start).Round(time.Millisecond), deviceFrom(r))
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) auth(h func(http.ResponseWriter, *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		dev, valid := "", false
		if ok {
			dev, valid = s.Devices.Authenticate(strings.TrimSpace(token))
		}
		if !valid {
			writeErr(w, http.StatusUnauthorized, "invalid or missing token")
			return
		}
		if id := r.PathValue("id"); id != "" && !savefmt.ValidSaveID(id) {
			writeErr(w, http.StatusBadRequest, "invalid save id")
			return
		}
		*r = *r.WithContext(context.WithValue(r.Context(), ctxKey{}, dev))
		h(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, api.ErrorBody{Error: msg})
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"device": deviceFrom(r)})
}

func (s *Server) listSaves(w http.ResponseWriter, r *http.Request) {
	list := s.Store.List()
	for i := range list {
		list[i].Lock = s.Locks.Get(list[i].ID)
	}
	writeJSON(w, 200, list)
}

func (s *Server) getSave(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rev, err := s.Store.Latest(id)
	if err != nil {
		writeErr(w, 404, "no revisions")
		return
	}
	writeJSON(w, 200, api.SaveSummary{ID: id, Latest: rev, Lock: s.Locks.Get(id)})
}

func (s *Server) listRevisions(w http.ResponseWriter, r *http.Request) {
	revs, err := s.Store.Revisions(r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		revs = []api.Revision{}
	}
	writeJSON(w, 200, revs)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rev, err := strconv.Atoi(r.PathValue("rev"))
	if err != nil {
		writeErr(w, 400, "invalid revision")
		return
	}
	p, err := s.Store.ArchivePath(id, rev)
	if err != nil {
		writeErr(w, 404, "revision not found")
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeErr(w, 500, "archive missing")
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+"-"+strconv.Itoa(rev)+`.zip"`)
	io.Copy(w, f)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, savefmt.MaxArchiveSize+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeErr(w, 400, "invalid multipart: "+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()
	var meta api.UploadMeta
	if err := json.Unmarshal([]byte(r.FormValue("meta")), &meta); err != nil {
		writeErr(w, 400, "invalid meta")
		return
	}
	f, _, err := r.FormFile("archive")
	if err != nil {
		writeErr(w, 400, "missing archive")
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, savefmt.MaxArchiveSize+1))
	f.Close()
	if err != nil {
		writeErr(w, 400, "read archive: "+err.Error())
		return
	}
	arc, err := savefmt.ReadArchive(data, id)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.put(w, id, data, arc, meta.BaseRevision, meta.Force, deviceFrom(r), 0)
}

func (s *Server) put(w http.ResponseWriter, id string, data []byte, arc *savefmt.Archive, base int, force bool, dev string, restoredFrom int) {
	rev, dup, err := s.Store.Put(id, data, arc, base, force, dev, restoredFrom)
	switch {
	case errors.Is(err, ErrConflict):
		writeJSON(w, http.StatusConflict, api.ConflictError{Error: "base revision is not latest", Latest: rev})
	case err != nil:
		s.Logger.Printf("store put %s: %v", id, err)
		writeErr(w, 500, "store failed")
	case dup:
		writeJSON(w, 200, api.UploadResult{Revision: *rev, Duplicate: true})
	default:
		s.Logger.Printf("save %s rev %d from %s (%s, %s)", id, rev.Rev, dev, rev.GameDate, rev.Meta.GameVersion)
		writeJSON(w, 201, api.UploadResult{Revision: *rev})
	}
}

func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rev, err := strconv.Atoi(r.PathValue("rev"))
	if err != nil {
		writeErr(w, 400, "invalid revision")
		return
	}
	p, err := s.Store.ArchivePath(id, rev)
	if err != nil {
		writeErr(w, 404, "revision not found")
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		writeErr(w, 500, "archive missing")
		return
	}
	arc, err := savefmt.ReadArchive(data, id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.put(w, id, data, arc, s.Store.LatestRev(id), true, deviceFrom(r), rev)
}

func (s *Server) wait(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	timeout := api.DefaultWaitTimout
	if t, err := strconv.Atoi(r.URL.Query().Get("timeout")); err == nil && t > 0 {
		timeout = time.Duration(t) * time.Second
	}
	if timeout > api.MaxWaitTimeout {
		timeout = api.MaxWaitTimeout
	}
	ch, ready := s.Store.Wait(id, after)
	if !ready {
		t := time.NewTimer(timeout)
		defer t.Stop()
		select {
		case <-ch:
		case <-t.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
	s.getSave(w, r)
}

func (s *Server) getLock(w http.ResponseWriter, r *http.Request) {
	lk := s.Locks.Get(r.PathValue("id"))
	writeJSON(w, 200, api.LockResponse{Acquired: lk != nil && lk.Holder == deviceFrom(r), Lock: lk})
}

func (s *Server) putLock(w http.ResponseWriter, r *http.Request) {
	var req api.LockRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil && err != io.EOF {
			writeErr(w, 400, "invalid body")
			return
		}
	}
	lk, ok := s.Locks.Acquire(r.PathValue("id"), deviceFrom(r), req.Force, req.GameDate)
	if !ok {
		writeJSON(w, http.StatusLocked, api.LockResponse{Acquired: false, Lock: &lk})
		return
	}
	writeJSON(w, 200, api.LockResponse{Acquired: true, Lock: &lk})
}

func (s *Server) deleteLock(w http.ResponseWriter, r *http.Request) {
	if !s.Locks.Release(r.PathValue("id"), deviceFrom(r)) {
		writeJSON(w, http.StatusLocked, api.LockResponse{Acquired: false, Lock: s.Locks.Get(r.PathValue("id"))})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
