using System.Text.Json;
using System.Text.Json.Serialization;

namespace CloudSaveSync;

public sealed class ConflictInfo
{
    public DateTime DetectedAt { get; set; }
    public string LocalHash { get; set; } = "";
    public string LocalGameDate { get; set; } = "";
    public string LocalPlayed { get; set; } = "";
    public int RemoteRev { get; set; }
    public string RemoteDevice { get; set; } = "";
    public string RemoteGameDate { get; set; } = "";
    public string RemotePlayed { get; set; } = "";
}

public sealed class SyncState
{
    public string SaveId { get; set; } = "";
    public int BaseRevision { get; set; }
    public string BaseHash { get; set; } = "";
    public DateTime LastSync { get; set; }
    public string LastAction { get; set; } = "";
    public string LastError { get; set; } = "";
    public ConflictInfo? Conflict { get; set; }
    public int PendingRemote { get; set; }
}

public enum SyncAction { None, InSync, Uploaded, Downloaded, Conflict, Pending }

public sealed record SyncResult(SyncAction Action, string Message, int Rev = 0);

/// <summary>Environment callbacks provided by the mod.</summary>
public interface ISyncHost
{
    string SavesPath { get; }
    string GameVersion { get; }
    /// <summary>Run apply (replacing the save folder) only if no save is loaded / loading. Returns false if refused.</summary>
    bool TryApply(Action apply);
    void Log(string message, bool warn = false);
}

/// <summary>
/// Sync decision logic. Mirrors internal/agent/engine.go; see docs/DESIGN.md §3.
/// All public methods are serialized by a semaphore and may run on a background thread.
/// </summary>
public sealed class SyncEngine
{
    private const int BackupKeep = 10;
    private readonly ISyncHost host;
    private readonly ApiClient api;
    private readonly string dataDir;
    private readonly SemaphoreSlim gate = new(1, 1);
    private static readonly JsonSerializerOptions JsonOpts = new() { WriteIndented = true, DefaultIgnoreCondition = JsonIgnoreCondition.WhenWritingNull };

    public string SaveId { get; }
    public SyncState State { get; private set; }

    public SyncEngine(ISyncHost host, ApiClient api, string dataDir, string saveId)
    {
        this.host = host;
        this.api = api;
        this.dataDir = dataDir;
        this.SaveId = saveId;
        this.State = this.LoadState();
    }

    public ApiClient Api => this.api;
    private string LocalDir => Path.Combine(this.host.SavesPath, this.SaveId);
    private string StatePath => Path.Combine(this.dataDir, "state.json");

    private SyncState LoadState()
    {
        try
        {
            if (File.Exists(this.StatePath))
            {
                var s = JsonSerializer.Deserialize<SyncState>(File.ReadAllText(this.StatePath));
                if (s != null && s.SaveId == this.SaveId) return s;
            }
        }
        catch (Exception ex) { this.host.Log($"读取 state.json 失败，重置: {ex.Message}", true); }
        return new SyncState { SaveId = this.SaveId };
    }

    private void SaveStateFile()
    {
        Directory.CreateDirectory(this.dataDir);
        string tmp = this.StatePath + ".tmp";
        File.WriteAllText(tmp, JsonSerializer.Serialize(this.State, JsonOpts));
        File.Move(tmp, this.StatePath, true);
    }

    public SaveFormat.Snapshot? LocalSnapshot() => SaveFormat.ReadDir(this.LocalDir, this.SaveId);

    private async Task<SyncResult> Locked(Func<Task<SyncResult>> fn, CancellationToken ct)
    {
        await this.gate.WaitAsync(ct).ConfigureAwait(false);
        try
        {
            this.State = this.LoadState();
            SyncResult res;
            try
            {
                res = await fn().ConfigureAwait(false);
                this.State.LastError = "";
                if (res.Action is not (SyncAction.None or SyncAction.InSync))
                    this.State.LastAction = $"{DateTime.Now:MM-dd HH:mm} {res.Message}";
            }
            catch (Exception ex) when (ex is not OperationCanceledException)
            {
                this.State.LastError = ex.Message;
                throw;
            }
            finally
            {
                this.State.LastSync = DateTime.UtcNow;
                try { this.SaveStateFile(); } catch (Exception ex) { this.host.Log($"写入 state.json 失败: {ex.Message}", true); }
            }
            return res;
        }
        finally { this.gate.Release(); }
    }

    public Task<SyncResult> Sync(bool allowUpload, bool allowDownload, CancellationToken ct)
        => this.Locked(() => this.Decide(allowUpload, allowDownload, ct), ct);

    private async Task<SyncResult> Decide(bool allowUpload, bool allowDownload, CancellationToken ct)
    {
        var st = this.State;
        var local = this.LocalSnapshot();
        var remote = (await this.api.Latest(this.SaveId, ct).ConfigureAwait(false))?.Latest;

        if (local != null && remote != null && local.Hash == remote.Hash)
        {
            st.BaseRevision = remote.Rev; st.BaseHash = remote.Hash; st.Conflict = null; st.PendingRemote = 0;
            return new SyncResult(SyncAction.InSync, "已是最新", remote.Rev);
        }
        bool localChanged = local != null && local.Hash != st.BaseHash;
        bool remoteChanged = remote != null && remote.Rev > st.BaseRevision;

        if (localChanged && remoteChanged)
        {
            st.Conflict = new ConflictInfo
            {
                DetectedAt = DateTime.UtcNow,
                LocalHash = local!.Hash, LocalGameDate = local.Meta.GameDate, LocalPlayed = local.Meta.Played,
                RemoteRev = remote!.Rev, RemoteDevice = remote.Device, RemoteGameDate = remote.GameDate, RemotePlayed = remote.Meta.Played,
            };
            return new SyncResult(SyncAction.Conflict, $"冲突：本机 {local.Meta.GameDate} / 云端 {remote.GameDate}（来自 {remote.Device}）");
        }
        if (localChanged)
            return allowUpload ? await this.Upload(false, ct).ConfigureAwait(false) : new SyncResult(SyncAction.None, "");
        if (remoteChanged)
            return await this.Download(remote!, allowDownload, ct).ConfigureAwait(false);
        return new SyncResult(SyncAction.None, "无变化");
    }

    private async Task<SyncResult> Upload(bool force, CancellationToken ct)
    {
        var zip = SaveFormat.PackDir(this.LocalDir);
        var arc = SaveFormat.ReadArchive(zip, this.SaveId);
        try
        {
            var rev = await this.api.Upload(this.SaveId, zip, this.State.BaseRevision, force, ct).ConfigureAwait(false);
            this.State.BaseRevision = rev.Rev; this.State.BaseHash = arc.Hash; this.State.Conflict = null;
            string msg = $"已上传 #{rev.Rev}（{arc.Meta.GameDate}）";
            this.host.Log(msg);
            return new SyncResult(SyncAction.Uploaded, msg, rev.Rev);
        }
        catch (ConflictException ex)
        {
            return new SyncResult(SyncAction.Pending, ex.Message);
        }
    }

    private async Task<SyncResult> Download(Revision remote, bool allow, CancellationToken ct)
    {
        if (!allow)
        {
            this.State.PendingRemote = remote.Rev;
            return new SyncResult(SyncAction.Pending, $"云端 #{remote.Rev}（{remote.GameDate}）待应用");
        }
        string lv = this.host.GameVersion;
        if (SaveFormat.CompareVersions(remote.Meta.GameVersion, lv) > 0)
        {
            this.State.PendingRemote = remote.Rev;
            return new SyncResult(SyncAction.Pending, $"云端存档版本 {remote.Meta.GameVersion} 高于本机游戏 {lv}，请先更新游戏");
        }
        var zip = await this.api.Download(this.SaveId, remote.Rev, ct).ConfigureAwait(false);
        var arc = SaveFormat.ReadArchive(zip, this.SaveId);
        if (arc.Hash != remote.Hash) throw new InvalidDataException("下载内容哈希不匹配");

        bool applied = this.host.TryApply(() =>
        {
            this.Backup("before-download");
            ApplyReplace(this.host.SavesPath, this.SaveId, arc);
        });
        if (!applied)
        {
            this.State.PendingRemote = remote.Rev;
            return new SyncResult(SyncAction.Pending, "存档正在加载/游玩中，暂不覆盖");
        }
        this.State.BaseRevision = remote.Rev; this.State.BaseHash = remote.Hash; this.State.Conflict = null; this.State.PendingRemote = 0;
        string msg = $"已同步云端 #{remote.Rev}（{remote.GameDate}，来自 {remote.Device}）";
        this.host.Log(msg);
        return new SyncResult(SyncAction.Downloaded, msg, remote.Rev);
    }

    /// <summary>Resolve a conflict: keep "local" (force upload) or "remote" (download, local backed up).</summary>
    public Task<SyncResult> Resolve(string keep, CancellationToken ct) => this.Locked(async () =>
    {
        var remote = (await this.api.Latest(this.SaveId, ct).ConfigureAwait(false))?.Latest;
        SyncResult res;
        if (keep == "local")
        {
            if (this.LocalSnapshot() == null) throw new InvalidOperationException("本机没有存档");
            if (remote != null) this.State.BaseRevision = remote.Rev;
            res = await this.Upload(true, ct).ConfigureAwait(false);
        }
        else
        {
            if (remote == null) throw new InvalidOperationException("云端没有存档");
            this.State.BaseHash = ""; this.State.BaseRevision = 0;
            res = await this.Download(remote, true, ct).ConfigureAwait(false);
            if (res.Action != SyncAction.Downloaded) throw new InvalidOperationException(res.Message);
        }
        this.State.Conflict = null;
        return res;
    }, ct);

    public async Task<SyncResult> Restore(int rev, CancellationToken ct)
    {
        await this.api.Restore(this.SaveId, rev, ct).ConfigureAwait(false);
        return await this.Resolve("remote", ct).ConfigureAwait(false);
    }

    private void Backup(string reason)
    {
        if (!Directory.Exists(this.LocalDir)) return;
        string dir = Path.Combine(this.dataDir, "backups");
        Directory.CreateDirectory(dir);
        File.WriteAllBytes(Path.Combine(dir, $"{this.SaveId}-{DateTime.Now:yyyyMMdd-HHmmss-fff}-{reason}.zip"), SaveFormat.PackDir(this.LocalDir));
        var files = Directory.GetFiles(dir, "*.zip").OrderBy(f => f, StringComparer.Ordinal).ToList();
        while (files.Count > BackupKeep) { File.Delete(files[0]); files.RemoveAt(0); }
    }

    /// <summary>Write archive to a temp folder next to Saves, then swap with renames.</summary>
    public static void ApplyReplace(string savesPath, string saveId, SaveFormat.Archive arc)
    {
        Directory.CreateDirectory(savesPath);
        string tmpRoot = Path.Combine(Path.GetDirectoryName(Path.GetFullPath(savesPath).TrimEnd(Path.DirectorySeparatorChar))!, ".cloudsavesync-tmp");
        if (Directory.Exists(tmpRoot)) Directory.Delete(tmpRoot, true);
        Directory.CreateDirectory(tmpRoot);
        try
        {
            string newDir = Path.Combine(tmpRoot, saveId);
            Directory.CreateDirectory(newDir);
            foreach (var (name, data) in arc.Files) File.WriteAllBytes(Path.Combine(newDir, name), data);
            string target = Path.Combine(savesPath, saveId);
            string oldDir = Path.Combine(tmpRoot, saveId + ".old");
            bool hadOld = false;
            if (Directory.Exists(target)) { Directory.Move(target, oldDir); hadOld = true; }
            try { Directory.Move(newDir, target); }
            catch
            {
                if (hadOld) Directory.Move(oldDir, target);
                throw;
            }
        }
        finally
        {
            try { Directory.Delete(tmpRoot, true); } catch { /* best effort */ }
        }
    }
}
