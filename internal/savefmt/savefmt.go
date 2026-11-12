// Package savefmt handles Stardew Valley save folders: content hashing,
// metadata parsing, zip packing/unpacking and game version comparison.
// The hash algorithm MUST stay identical to the C# implementation in
// smapi-mod/CloudSaveSync/SaveFormat.cs.
package savefmt

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const InfoFile = "SaveGameInfo"

// MaxArchiveSize limits accepted zip size and total uncompressed size.
const MaxArchiveSize = 64 << 20

var saveIDRe = regexp.MustCompile(`^[^/\\:*?"<>|\x00-\x1f]{1,128}$`)

// ValidSaveID reports whether id is a safe save folder name.
func ValidSaveID(id string) bool {
	return saveIDRe.MatchString(id) && id != "." && id != ".." && !strings.HasPrefix(id, ".")
}

// Meta is information extracted from SaveGameInfo.
type Meta struct {
	FarmerName   string `json:"farmer_name"`
	FarmName     string `json:"farm_name"`
	GameVersion  string `json:"game_version"`
	PlayedMillis int64  `json:"played_ms"`
	Year         int    `json:"year"`
	Season       int    `json:"season"` // 0 spring .. 3 winter
	Day          int    `json:"day"`
}

var seasonNames = []string{"春", "夏", "秋", "冬"}

// GameDate returns a human readable in-game date like "第2年 夏 15日".
func (m Meta) GameDate() string {
	if m.Year == 0 {
		return "未知日期"
	}
	s := "?"
	if m.Season >= 0 && m.Season < len(seasonNames) {
		s = seasonNames[m.Season]
	}
	return fmt.Sprintf("第%d年 %s %d日", m.Year, s, m.Day)
}

// Played returns played time like "12h34m".
func (m Meta) Played() string {
	min := m.PlayedMillis / 60000
	return fmt.Sprintf("%dh%02dm", min/60, min%60)
}

// ParseInfo parses SaveGameInfo XML. Only direct children of the root are used.
func ParseInfo(data []byte) (Meta, error) {
	var m Meta
	dec := xml.NewDecoder(bytes.NewReader(data))
	depth := 0
	var cur string
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, fmt.Errorf("parse SaveGameInfo: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 2 {
				cur = t.Name.Local
				text.Reset()
			}
		case xml.CharData:
			if depth == 2 {
				text.Write(t)
			}
		case xml.EndElement:
			if depth == 2 {
				v := strings.TrimSpace(text.String())
				switch cur {
				case "name":
					m.FarmerName = v
				case "farmName":
					m.FarmName = v
				case "gameVersion":
					m.GameVersion = v
				case "millisecondsPlayed":
					m.PlayedMillis, _ = strconv.ParseInt(v, 10, 64)
				case "yearForSaveGame":
					m.Year, _ = strconv.Atoi(v)
				case "seasonForSaveGame":
					m.Season, _ = strconv.Atoi(v)
				case "dayOfMonthForSaveGame":
					m.Day, _ = strconv.Atoi(v)
				}
				cur = ""
			}
			depth--
		}
	}
	return m, nil
}

// Hash computes the content hash from the main save file and SaveGameInfo.
func Hash(main, info []byte) string {
	a := sha256.Sum256(main)
	b := sha256.Sum256(info)
	h := sha256.New()
	h.Write(a[:])
	h.Write(b[:])
	return hex.EncodeToString(h.Sum(nil))
}

// Snapshot is the hashed state of a save.
type Snapshot struct {
	Hash string
	Meta Meta
}

// ReadDir hashes a save folder on disk. Returns os.ErrNotExist when the
// folder or its required files do not exist.
func ReadDir(dir, saveID string) (Snapshot, error) {
	main, err := os.ReadFile(filepath.Join(dir, saveID))
	if err != nil {
		return Snapshot{}, err
	}
	info, err := os.ReadFile(filepath.Join(dir, InfoFile))
	if err != nil {
		return Snapshot{}, err
	}
	meta, err := ParseInfo(info)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Hash: Hash(main, info), Meta: meta}, nil
}

// PackDir zips all regular files directly inside dir.
func PackDir(dir string) ([]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		w, err := zw.Create(n)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Archive is a parsed, validated save zip.
type Archive struct {
	Files map[string][]byte
	Snapshot
}

// ReadArchive validates a save zip for saveID and computes its snapshot.
func ReadArchive(data []byte, saveID string) (*Archive, error) {
	if len(data) > MaxArchiveSize {
		return nil, errors.New("archive too large")
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid zip: %w", err)
	}
	a := &Archive{Files: map[string][]byte{}}
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := f.Name
		if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." || !saveIDRe.MatchString(name) {
			return nil, fmt.Errorf("invalid file name in archive: %q", name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, MaxArchiveSize-total+1))
		rc.Close()
		if err != nil {
			return nil, err
		}
		total += int64(len(b))
		if total > MaxArchiveSize {
			return nil, errors.New("archive uncompressed size too large")
		}
		a.Files[name] = b
	}
	main, ok1 := a.Files[saveID]
	info, ok2 := a.Files[InfoFile]
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("archive must contain %q and %q", saveID, InfoFile)
	}
	meta, err := ParseInfo(info)
	if err != nil {
		return nil, err
	}
	a.Hash = Hash(main, info)
	a.Meta = meta
	return a, nil
}

// WriteFiles writes archive files into dir (which must exist).
func (a *Archive) WriteFiles(dir string) error {
	for name, data := range a.Files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o660); err != nil {
			return err
		}
	}
	return nil
}

// CompareVersions compares the first three numeric components of two
// game versions ("1.6.15" vs "1.6.14.24317"). Returns -1, 0, 1.
// Unparseable or empty versions compare as equal (unknown = don't block).
func CompareVersions(a, b string) int {
	pa, okA := parseVersion(a)
	pb, okB := parseVersion(b)
	if !okA || !okB {
		return 0
	}
	for i := 0; i < 3; i++ {
		if pa[i] < pb[i] {
			return -1
		}
		if pa[i] > pb[i] {
			return 1
		}
	}
	return 0
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimSpace(v)
	if v == "" {
		return out, false
	}
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '+' || r == ' ' })
	n := 0
	for _, p := range parts {
		if n == 3 {
			break
		}
		x, err := strconv.Atoi(p)
		if err != nil {
			break
		}
		out[n] = x
		n++
	}
	return out, n > 0
}
