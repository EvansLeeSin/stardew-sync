using System.IO.Compression;
using System.Security.Cryptography;
using System.Xml;

namespace CloudSaveSync;

/// <summary>Save folder hashing / packing. MUST match internal/savefmt (Go).</summary>
public static class SaveFormat
{
    public const string InfoFile = "SaveGameInfo";

    public sealed class Meta
    {
        public string FarmerName = "", FarmName = "", GameVersion = "";
        public long PlayedMillis;
        public int Year, Season, Day;

        private static readonly string[] Seasons = { "春", "夏", "秋", "冬" };

        public string GameDate => Year == 0 ? "未知日期"
            : $"第{Year}年 {(Season >= 0 && Season < 4 ? Seasons[Season] : "?")} {Day}日";

        public string Played
        {
            get { long m = PlayedMillis / 60000; return $"{m / 60}h{m % 60:00}m"; }
        }
    }

    public sealed record Snapshot(string Hash, Meta Meta);

    public static string Hash(byte[] main, byte[] info)
    {
        var a = SHA256.HashData(main);
        var b = SHA256.HashData(info);
        var all = new byte[64];
        a.CopyTo(all, 0);
        b.CopyTo(all, 32);
        return Convert.ToHexString(SHA256.HashData(all)).ToLowerInvariant();
    }

    private static readonly HashSet<string> Fields = new()
    {
        "name", "farmName", "gameVersion", "millisecondsPlayed", "yearForSaveGame", "seasonForSaveGame", "dayOfMonthForSaveGame",
    };

    /// <summary>Parse direct children of the SaveGameInfo root element.</summary>
    public static Meta ParseInfo(byte[] data)
    {
        var m = new Meta();
        using var ms = new MemoryStream(data);
        using var r = XmlReader.Create(ms, new XmlReaderSettings { DtdProcessing = DtdProcessing.Prohibit, IgnoreComments = true });
        r.MoveToContent();
        int d = r.Depth;
        if (r.IsEmptyElement) return m;
        r.Read();
        while (!r.EOF)
        {
            if (r.NodeType == XmlNodeType.EndElement && r.Depth == d) break;
            if (r.NodeType == XmlNodeType.Element && r.Depth == d + 1)
            {
                string name = r.LocalName;
                if (!Fields.Contains(name)) { r.Skip(); continue; }
                string v = r.ReadElementContentAsString().Trim();
                switch (name)
                {
                    case "name": m.FarmerName = v; break;
                    case "farmName": m.FarmName = v; break;
                    case "gameVersion": m.GameVersion = v; break;
                    case "millisecondsPlayed": long.TryParse(v, out m.PlayedMillis); break;
                    case "yearForSaveGame": int.TryParse(v, out m.Year); break;
                    case "seasonForSaveGame": int.TryParse(v, out m.Season); break;
                    case "dayOfMonthForSaveGame": int.TryParse(v, out m.Day); break;
                }
                continue;
            }
            r.Read();
        }
        return m;
    }

    /// <summary>Returns null if the save folder or required files do not exist.</summary>
    public static Snapshot? ReadDir(string dir, string saveId)
    {
        string mainPath = Path.Combine(dir, saveId), infoPath = Path.Combine(dir, InfoFile);
        if (!File.Exists(mainPath) || !File.Exists(infoPath)) return null;
        var main = File.ReadAllBytes(mainPath);
        var info = File.ReadAllBytes(infoPath);
        return new Snapshot(Hash(main, info), ParseInfo(info));
    }

    public static byte[] PackDir(string dir)
    {
        using var ms = new MemoryStream();
        using (var zip = new ZipArchive(ms, ZipArchiveMode.Create, true))
        {
            foreach (var f in Directory.GetFiles(dir).OrderBy(p => Path.GetFileName(p), StringComparer.Ordinal))
            {
                var e = zip.CreateEntry(Path.GetFileName(f), CompressionLevel.Optimal);
                using var s = e.Open();
                var bytes = File.ReadAllBytes(f);
                s.Write(bytes, 0, bytes.Length);
            }
        }
        return ms.ToArray();
    }

    public sealed class Archive
    {
        public Dictionary<string, byte[]> Files = new();
        public string Hash = "";
        public Meta Meta = new();
    }

    public static Archive ReadArchive(byte[] data, string saveId)
    {
        const long max = 64L << 20;
        var a = new Archive();
        long total = 0;
        using var zip = new ZipArchive(new MemoryStream(data), ZipArchiveMode.Read);
        foreach (var e in zip.Entries)
        {
            if (e.FullName.EndsWith("/")) continue;
            string n = e.FullName;
            if (n.Length == 0 || n.Contains('/') || n.Contains('\\') || n == "." || n == ".." || n.IndexOfAny(Path.GetInvalidFileNameChars()) >= 0)
                throw new InvalidDataException($"invalid file name in archive: {n}");
            using var s = e.Open();
            using var ms = new MemoryStream();
            s.CopyTo(ms);
            total += ms.Length;
            if (total > max) throw new InvalidDataException("archive too large");
            a.Files[n] = ms.ToArray();
        }
        if (!a.Files.TryGetValue(saveId, out var main) || !a.Files.TryGetValue(InfoFile, out var info))
            throw new InvalidDataException($"archive must contain {saveId} and {InfoFile}");
        a.Hash = Hash(main, info);
        a.Meta = ParseInfo(info);
        return a;
    }

    /// <summary>Compare first three numeric parts. Unknown versions compare equal.</summary>
    public static int CompareVersions(string a, string b)
    {
        var pa = Parse(a); var pb = Parse(b);
        if (pa == null || pb == null) return 0;
        for (int i = 0; i < 3; i++)
            if (pa[i] != pb[i]) return pa[i] < pb[i] ? -1 : 1;
        return 0;
    }

    private static int[]? Parse(string v)
    {
        if (string.IsNullOrWhiteSpace(v)) return null;
        var outv = new int[3];
        int n = 0;
        foreach (var p in v.Trim().Split('.', '-', '+', ' '))
        {
            if (n == 3 || !int.TryParse(p, out var x)) break;
            outv[n++] = x;
        }
        return n > 0 ? outv : null;
    }
}
