package savefmt

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleInfo = `<?xml version="1.0" encoding="utf-8"?>
<Farmer xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
  <name>Evans</name>
  <items><Item><name>Axe</name></Item></items>
  <farmName>Sunny</farmName>
  <gameVersion>1.6.15</gameVersion>
  <millisecondsPlayed>4500000</millisecondsPlayed>
  <yearForSaveGame>2</yearForSaveGame>
  <seasonForSaveGame>1</seasonForSaveGame>
  <dayOfMonthForSaveGame>15</dayOfMonthForSaveGame>
</Farmer>`

func TestParseInfo(t *testing.T) {
	m, err := ParseInfo([]byte(sampleInfo))
	if err != nil {
		t.Fatal(err)
	}
	if m.FarmerName != "Evans" || m.FarmName != "Sunny" || m.GameVersion != "1.6.15" || m.Year != 2 || m.Season != 1 || m.Day != 15 || m.PlayedMillis != 4500000 {
		t.Fatalf("bad meta %+v", m)
	}
	if m.GameDate() != "第2年 夏 15日" || m.Played() != "1h15m" {
		t.Fatalf("format %q %q", m.GameDate(), m.Played())
	}
}

// Known vector shared with the C# implementation (see SaveFormat.cs).
func TestHashVector(t *testing.T) {
	got := Hash([]byte("main"), []byte("info"))
	const want = "f96b632ae5430d2adb6dd9cf504edf07bb68365fda9403683eee2b8f0918ce5e"
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestPackRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id := "Sunny_123"
	os.WriteFile(filepath.Join(dir, id), []byte("<SaveGame/>"), 0o644)
	os.WriteFile(filepath.Join(dir, InfoFile), []byte(sampleInfo), 0o644)
	os.WriteFile(filepath.Join(dir, id+"_old"), []byte("old"), 0o644)
	snap, err := ReadDir(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := PackDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	arc, err := ReadArchive(data, id)
	if err != nil {
		t.Fatal(err)
	}
	if arc.Hash != snap.Hash || len(arc.Files) != 3 {
		t.Fatalf("mismatch %s %s %d", arc.Hash, snap.Hash, len(arc.Files))
	}
	if _, err := ReadArchive(data, "Other_1"); err == nil {
		t.Fatal("expected error for wrong save id")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.6.15", "1.6.15", 0},
		{"1.6.15", "1.6.14.24317", 1},
		{"1.6.9", "1.6.10", -1},
		{"1.6", "1.6.0", 0},
		{"", "1.6.0", 0},
		{"1.7.0", "1.6.15", 1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("%s vs %s = %d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestValidSaveID(t *testing.T) {
	for _, ok := range []string{"Farm_123", "中文农场_1"} {
		if !ValidSaveID(ok) {
			t.Error(ok)
		}
	}
	for _, bad := range []string{"", "..", "a/b", `a\b`, ".hidden"} {
		if ValidSaveID(bad) {
			t.Error(bad)
		}
	}
}
