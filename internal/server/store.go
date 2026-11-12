package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/EvansLeeSin/stardew-sync/internal/api"
	"github.com/EvansLeeSin/stardew-sync/internal/savefmt"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// Store keeps save revisions on disk:
//
//	<dir>/saves/<id>/index.json
//	<dir>/saves/<id>/<rev>.zip
type Store struct {
	dir       string
	keepLast  int
	mu        sync.Mutex
	saves     map[string]*saveIndex
	listeners map[string]chan struct{}
}

type saveIndex struct {
	NextRev   int            `json:"next_rev"`
	Revisions []api.Revision `json:"revisions"` // ascending by Rev
}

func (s *saveIndex) latest() *api.Revision {
	if len(s.Revisions) == 0 {
		return nil
	}
	r := s.Revisions[len(s.Revisions)-1]
	return &r
}

func NewStore(dir string, keepLast int) (*Store, error) {
	st := &Store{dir: dir, keepLast: keepLast, saves: map[string]*saveIndex{}, listeners: map[string]chan struct{}{}}
	root := filepath.Join(dir, "saves")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !savefmt.ValidSaveID(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name(), "index.json"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		var idx saveIndex
		if err := json.Unmarshal(b, &idx); err != nil {
			return nil, fmt.Errorf("index %s: %w", e.Name(), err)
		}
		st.saves[e.Name()] = &idx
	}
	return st, nil
}

func (s *Store) saveDir(id string) string { return filepath.Join(s.dir, "saves", id) }

func (s *Store) archivePath(id string, rev int) string {
	return filepath.Join(s.saveDir(id), fmt.Sprintf("%d.zip", rev))
}

func (s *Store) writeIndex(id string, idx *saveIndex) error {
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.saveDir(id), "index.json"), b)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// List returns all save IDs with their latest revision.
func (s *Store) List() []api.SaveSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]api.SaveSummary, 0, len(s.saves))
	for id, idx := range s.saves {
		out = append(out, api.SaveSummary{ID: id, Latest: idx.latest()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Latest returns the latest revision of a save.
func (s *Store) Latest(id string) (*api.Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, ok := s.saves[id]
	if !ok || len(idx.Revisions) == 0 {
		return nil, ErrNotFound
	}
	return idx.latest(), nil
}

// LatestRev returns the latest revision number (0 if none).
func (s *Store) LatestRev(id string) int {
	r, err := s.Latest(id)
	if err != nil {
		return 0
	}
	return r.Rev
}

// Revisions returns revisions newest first.
func (s *Store) Revisions(id string) ([]api.Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, ok := s.saves[id]
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]api.Revision, len(idx.Revisions))
	for i, r := range idx.Revisions {
		out[len(idx.Revisions)-1-i] = r
	}
	return out, nil
}

// ArchivePath returns the path of a revision's zip.
func (s *Store) ArchivePath(id string, rev int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, ok := s.saves[id]
	if !ok {
		return "", ErrNotFound
	}
	for _, r := range idx.Revisions {
		if r.Rev == rev {
			return s.archivePath(id, rev), nil
		}
	}
	return "", ErrNotFound
}

// Put stores a new revision. If the content hash equals the latest
// revision, nothing is stored and (latest, true, nil) is returned.
// If base != latest and !force, returns ErrConflict with the latest revision.
func (s *Store) Put(id string, data []byte, arc *savefmt.Archive, base int, force bool, device string, restoredFrom int) (*api.Revision, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, ok := s.saves[id]
	if !ok {
		idx = &saveIndex{NextRev: 1}
	}
	latest := idx.latest()
	if latest != nil && latest.Hash == arc.Hash {
		return latest, true, nil
	}
	latestRev := 0
	if latest != nil {
		latestRev = latest.Rev
	}
	if !force && base != latestRev {
		return latest, false, ErrConflict
	}
	if err := os.MkdirAll(s.saveDir(id), 0o750); err != nil {
		return nil, false, err
	}
	rev := api.Revision{
		Rev:          idx.NextRev,
		BaseRevision: base,
		Device:       device,
		Hash:         arc.Hash,
		Size:         int64(len(data)),
		UploadedAt:   time.Now().UTC(),
		RestoredFrom: restoredFrom,
		Meta:         arc.Meta,
		GameDate:     arc.Meta.GameDate(),
	}
	if err := writeFileAtomic(s.archivePath(id, rev.Rev), data); err != nil {
		return nil, false, err
	}
	newIdx := &saveIndex{NextRev: idx.NextRev + 1, Revisions: append(append([]api.Revision{}, idx.Revisions...), rev)}
	removed := s.prune(newIdx)
	if err := s.writeIndex(id, newIdx); err != nil {
		os.Remove(s.archivePath(id, rev.Rev))
		return nil, false, err
	}
	s.saves[id] = newIdx
	for _, r := range removed {
		os.Remove(s.archivePath(id, r))
	}
	if ch, ok := s.listeners[id]; ok {
		close(ch)
		delete(s.listeners, id)
	}
	return &rev, false, nil
}

// prune applies retention: keep the last keepLast revisions plus the last
// revision of every in-game season. Returns removed revision numbers.
func (s *Store) prune(idx *saveIndex) []int {
	n := len(idx.Revisions)
	if s.keepLast <= 0 || n <= s.keepLast {
		return nil
	}
	keep := map[int]bool{}
	for i := n - s.keepLast; i < n; i++ {
		keep[idx.Revisions[i].Rev] = true
	}
	seasonLast := map[[2]int]int{}
	for _, r := range idx.Revisions {
		if r.Meta.Year > 0 {
			seasonLast[[2]int{r.Meta.Year, r.Meta.Season}] = r.Rev
		}
	}
	for _, rev := range seasonLast {
		keep[rev] = true
	}
	var kept []api.Revision
	var removed []int
	for _, r := range idx.Revisions {
		if keep[r.Rev] {
			kept = append(kept, r)
		} else {
			removed = append(removed, r.Rev)
		}
	}
	idx.Revisions = kept
	return removed
}

// Wait returns a channel closed when a revision newer than after appears.
// The returned bool is true if such a revision already exists.
func (s *Store) Wait(id string, after int) (<-chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx, ok := s.saves[id]; ok {
		if l := idx.latest(); l != nil && l.Rev > after {
			return nil, true
		}
	}
	ch, ok := s.listeners[id]
	if !ok {
		ch = make(chan struct{})
		s.listeners[id] = ch
	}
	return ch, false
}
