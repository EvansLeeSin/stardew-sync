using System.Collections.Concurrent;
using StardewModdingAPI;
using StardewModdingAPI.Enums;
using StardewModdingAPI.Events;

namespace CloudSaveSync;

public sealed class ModEntry : Mod, ISyncHost
{
    private ModConfig config = new();
    private SyncEngine? engine;
    private readonly ConcurrentQueue<Action> mainThread = new();
    private readonly List<(Func<bool> ready, Action show)> pendingDialogs = new();
    private bool lockHeld;
    private int heartbeatCounter;
    private string? deviceName;
    private volatile LoadStage loadStage = LoadStage.None;

    /// <summary>A save is loaded or being loaded/created.</summary>
    private bool SaveActive => this.loadStage != LoadStage.None;

    // ISyncHost
    public string SavesPath => Constants.SavesPath;
    public string GameVersion => GameUi.GameVersion();
    void ISyncHost.Log(string message, bool warn) => this.Monitor.Log(message, warn ? LogLevel.Warn : LogLevel.Info);

    public override void Entry(IModHelper helper)
    {
        this.config = helper.ReadConfig<ModConfig>();
        if (string.IsNullOrWhiteSpace(this.config.ServerUrl) || string.IsNullOrWhiteSpace(this.config.Token))
        {
            helper.WriteConfig(this.config);
            this.Monitor.Log($"未配置服务器。请编辑 {Path.Combine(helper.DirectoryPath, "config.json")} 填写 ServerUrl 与 Token。", LogLevel.Warn);
        }

        helper.Events.GameLoop.GameLaunched += (_, _) => this.SyncInBackground("启动");
        helper.Events.GameLoop.ReturnedToTitle += this.OnReturnedToTitle;
        helper.Events.GameLoop.SaveLoaded += this.OnSaveLoaded;
        helper.Events.GameLoop.Saved += this.OnSaved;
        helper.Events.GameLoop.UpdateTicked += this.OnUpdateTicked;
        helper.Events.GameLoop.OneSecondUpdateTicked += this.OnOneSecond;
        helper.Events.Specialized.LoadStageChanged += (_, e) => this.loadStage = e.NewStage;

        helper.ConsoleCommands.Add("cloudsync",
            "云存档同步。\n  cloudsync status | sync | push(保留本机) | pull(保留云端) | revisions | restore <rev> | test",
            this.OnCommand);
    }

    // ---------- engine ----------

    private bool Configured => !string.IsNullOrWhiteSpace(this.config.ServerUrl) && !string.IsNullOrWhiteSpace(this.config.Token);

    private SyncEngine? Engine()
    {
        if (this.engine != null || !this.Configured) return this.engine;
        string id = this.config.SaveId;
        if (string.IsNullOrWhiteSpace(id))
        {
            if (this.SaveActive && !string.IsNullOrEmpty(Constants.SaveFolderName))
                id = Constants.SaveFolderName!;
            else if (Directory.Exists(this.SavesPath))
            {
                var dirs = Directory.GetDirectories(this.SavesPath)
                    .Where(d => File.Exists(Path.Combine(d, SaveFormat.InfoFile)))
                    .Select(d => Path.GetFileName(d)!).ToList();
                if (dirs.Count == 1) id = dirs[0];
            }
            if (string.IsNullOrWhiteSpace(id)) return null;
            this.config.SaveId = id;
            this.Helper.WriteConfig(this.config);
            this.Monitor.Log($"已自动选择同步存档: {id}（可在 config.json 的 SaveId 修改）", LogLevel.Info);
        }
        var api = new ApiClient(this.config.ServerUrl, this.config.Token);
        this.engine = new SyncEngine(this, api, Path.Combine(this.Helper.DirectoryPath, "data"), id);
        return this.engine;
    }

    private bool IsSyncedSave => this.engine != null && this.SaveActive && Constants.SaveFolderName == this.engine.SaveId;

    /// <summary>Apply a download on the main thread only when no save is loaded.</summary>
    bool ISyncHost.TryApply(Action apply)
    {
        var tcs = new TaskCompletionSource<bool>();
        int state = 0; // 0 pending, 1 started, 2 abandoned
        this.mainThread.Enqueue(() =>
        {
            if (Interlocked.CompareExchange(ref state, 1, 0) != 0) return; // caller gave up
            if (this.SaveActive) { tcs.TrySetResult(false); return; }
            try
            {
                apply();
                GameUi.CloseTitleSubMenu(); // the load menu caches slot info; reopen to refresh
                tcs.TrySetResult(true);
            }
            catch (Exception ex) { tcs.TrySetException(ex); }
        });
        if (!tcs.Task.Wait(TimeSpan.FromSeconds(30)) && Interlocked.CompareExchange(ref state, 2, 0) == 0)
            return false; // never started; it will be skipped
        return tcs.Task.GetAwaiter().GetResult();
    }

    private void SyncInBackground(string reason)
    {
        var eng = this.Engine();
        if (eng == null)
        {
            if (this.Configured) this.Monitor.Log("未找到要同步的存档（有多个存档时请在 config.json 设置 SaveId）。", LogLevel.Warn);
            return;
        }
        Task.Run(async () =>
        {
            try
            {
                var res = await eng.Sync(allowUpload: true, allowDownload: this.config.AutoDownload, CancellationToken.None);
                this.Monitor.Log($"[{reason}] {res.Message}", res.Action == SyncAction.Conflict ? LogLevel.Warn : LogLevel.Info);
                if (res.Action == SyncAction.Conflict) this.mainThread.Enqueue(() => this.QueueConflictDialog());
            }
            catch (Exception ex) { this.Monitor.Log($"[{reason}] 同步失败: {ex.Message}", LogLevel.Warn); }
        });
    }

    // ---------- events ----------

    private void OnUpdateTicked(object? sender, UpdateTickedEventArgs e)
    {
        while (this.mainThread.TryDequeue(out var a))
        {
            try { a(); } catch (Exception ex) { this.Monitor.Log(ex.ToString(), LogLevel.Error); }
        }
        for (int i = 0; i < this.pendingDialogs.Count; i++)
        {
            var (ready, show) = this.pendingDialogs[i];
            if (ready())
            {
                this.pendingDialogs.RemoveAt(i);
                show();
                break; // one dialog at a time
            }
        }
    }

    private void OnOneSecond(object? sender, OneSecondUpdateTickedEventArgs e)
    {
        if (!this.config.PlayLock || !this.lockHeld || !this.IsSyncedSave) return;
        if (++this.heartbeatCounter < 60) return;
        this.heartbeatCounter = 0;
        var eng = this.engine!;
        string date = eng.LocalSnapshot()?.Meta.GameDate ?? "";
        Task.Run(async () =>
        {
            try
            {
                var lr = await eng.Api.Lock(eng.SaveId, false, date, CancellationToken.None);
                if (!lr.Acquired && lr.Lock != null)
                {
                    this.lockHeld = false;
                    this.mainThread.Enqueue(() => GameUi.Hud($"⚠ {lr.Lock.Holder} 接管了此存档的游玩锁", true));
                }
            }
            catch (Exception ex) { this.Monitor.Log($"心跳失败: {ex.Message}", LogLevel.Trace); }
        });
    }

    private void OnSaveLoaded(object? sender, SaveLoadedEventArgs e)
    {
        var eng = this.Engine();
        if (eng == null || !this.IsSyncedSave) return;
        this.heartbeatCounter = 0;
        var local = eng.LocalSnapshot();
        string date = local?.Meta.GameDate ?? "";
        Task.Run(async () =>
        {
            var warnings = new List<string>();
            try
            {
                var sum = await eng.Api.Latest(eng.SaveId, CancellationToken.None);
                var st = eng.State;
                if (sum?.Latest is { } r && local != null && r.Hash != local.Hash && r.Rev > st.BaseRevision)
                    warnings.Add($"云端有更新的进度：#{r.Rev} {r.GameDate}（来自 {r.Device}）\n当前加载的是：{local.Meta.GameDate}");
                if (this.config.PlayLock)
                {
                    var lr = await eng.Api.Lock(eng.SaveId, false, date, CancellationToken.None);
                    this.lockHeld = lr.Acquired;
                    if (!lr.Acquired && lr.Lock != null)
                    {
                        int mins = (int)Math.Max(0, (DateTime.UtcNow - lr.Lock.LastHeartbeat.ToUniversalTime()).TotalMinutes);
                        warnings.Add($"{lr.Lock.Holder} 正在游玩此存档（{lr.Lock.GameDate}，{mins} 分钟前活动）");
                    }
                }
            }
            catch (Exception ex)
            {
                this.Monitor.Log($"加载检查失败（离线？）: {ex.Message}", LogLevel.Warn);
                return;
            }
            if (warnings.Count == 0) return;
            string msg = "⚠ " + string.Join("\n\n", warnings) + "\n\n继续游玩可能产生冲突。是否继续？\n（选“否”返回标题）";
            this.mainThread.Enqueue(() => this.pendingDialogs.Add((() => Context.IsPlayerFree, (Action)(() =>
            {
                bool shown = GameUi.Confirm(msg,
                    onYes: () => Task.Run(async () =>
                    {
                        if (!this.config.PlayLock) return;
                        try { this.lockHeld = (await eng.Api.Lock(eng.SaveId, true, date, CancellationToken.None)).Acquired; }
                        catch (Exception ex) { this.Monitor.Log($"接管锁失败: {ex.Message}", LogLevel.Warn); }
                    }),
                    onNo: () => GameUi.ExitToTitle());
                if (!shown) this.Monitor.Log(msg, LogLevel.Warn);
            }))));
        });
    }

    private void OnSaved(object? sender, SavedEventArgs e)
    {
        var eng = this.Engine();
        if (eng == null || !this.IsSyncedSave) return;
        Task.Run(async () =>
        {
            try
            {
                var res = await eng.Sync(allowUpload: true, allowDownload: false, CancellationToken.None);
                this.Monitor.Log($"[存档] {res.Message}", LogLevel.Info);
                this.mainThread.Enqueue(() =>
                {
                    switch (res.Action)
                    {
                        case SyncAction.Uploaded: GameUi.Hud($"☁ 存档已上传 #{res.Rev}"); break;
                        case SyncAction.Conflict:
                        case SyncAction.Pending: GameUi.Hud("⚠ 云存档冲突，回到标题界面时处理", true); break;
                    }
                });
            }
            catch (Exception ex)
            {
                this.Monitor.Log($"上传失败: {ex.Message}", LogLevel.Warn);
                this.mainThread.Enqueue(() => GameUi.Hud("⚠ 云存档上传失败（下次存档时重试）", true));
            }
        });
    }

    private void OnReturnedToTitle(object? sender, ReturnedToTitleEventArgs e)
    {
        var eng = this.engine;
        if (eng != null && this.lockHeld)
        {
            this.lockHeld = false;
            Task.Run(() => eng.Api.Unlock(eng.SaveId, CancellationToken.None));
        }
        this.SyncInBackground("回到标题");
    }

    // ---------- conflict UI ----------

    private void QueueConflictDialog()
    {
        var eng = this.engine;
        var c = eng?.State.Conflict;
        if (eng == null || c == null) return;
        this.pendingDialogs.Add((() => GameUi.OnTitleMainScreen() && !this.SaveActive, (Action)(() =>
        {
            string head = $"⚠ 云存档冲突\n本机：{c.LocalGameDate}（{c.LocalPlayed}）\n云端：#{c.RemoteRev} {c.RemoteGameDate}（{c.RemotePlayed}，来自 {c.RemoteDevice}）\n\n";
            bool shown = GameUi.Confirm(head + "保留【云端】进度？（本机存档会先备份）",
                onYes: () => this.RunResolve(eng, "remote"),
                onNo: () => this.mainThread.Enqueue(() =>
                    GameUi.Confirm(head + "保留【本机】进度并覆盖云端？\n（选“否”暂不处理）",
                        onYes: () => this.RunResolve(eng, "local"),
                        onNo: () => this.Monitor.Log("冲突未处理，可用控制台命令 cloudsync push / pull 处理。", LogLevel.Warn))));
            if (!shown) this.Monitor.Log("检测到冲突，请使用控制台命令 cloudsync push（保留本机）或 cloudsync pull（保留云端）。", LogLevel.Warn);
        })));
    }

    private void RunResolve(SyncEngine eng, string keep)
    {
        Task.Run(async () =>
        {
            try { this.Monitor.Log((await eng.Resolve(keep, CancellationToken.None)).Message, LogLevel.Info); }
            catch (Exception ex) { this.Monitor.Log($"处理冲突失败: {ex.Message}", LogLevel.Error); }
        });
    }

    // ---------- console ----------

    private void OnCommand(string name, string[] args)
    {
        var eng = this.Engine();
        if (eng == null) { this.Monitor.Log("未配置或未找到存档。", LogLevel.Error); return; }
        string sub = args.Length > 0 ? args[0] : "status";
        Task.Run(async () =>
        {
            try
            {
                var ct = CancellationToken.None;
                switch (sub)
                {
                    case "status":
                        var st = eng.State;
                        var local = eng.LocalSnapshot();
                        var remote = await eng.Api.Latest(eng.SaveId, ct);
                        this.Monitor.Log(
                            $"存档 {eng.SaveId}\n  本机: {local?.Meta.GameDate ?? "无"}{(local != null && local.Hash != st.BaseHash ? "（未上传）" : "")}\n" +
                            $"  云端: {(remote?.Latest is { } r ? $"#{r.Rev} {r.GameDate} {r.Device} {r.UploadedAt.ToLocalTime():g}" : "无")}\n" +
                            $"  基线: #{st.BaseRevision}  锁: {(remote?.Lock is { } l ? l.Holder : "无")}\n" +
                            $"  最近: {st.LastAction}  错误: {st.LastError}" +
                            (st.Conflict != null ? $"\n  ⚠ 冲突：本机 {st.Conflict.LocalGameDate} / 云端 #{st.Conflict.RemoteRev} {st.Conflict.RemoteGameDate}" : ""),
                            LogLevel.Info);
                        break;
                    case "sync":
                        this.Monitor.Log((await eng.Sync(true, !this.SaveActive, ct)).Message, LogLevel.Info);
                        break;
                    case "push":
                        this.Monitor.Log((await eng.Resolve("local", ct)).Message, LogLevel.Info);
                        break;
                    case "pull":
                        if (this.SaveActive) { this.Monitor.Log("请先回到标题界面。", LogLevel.Error); return; }
                        this.Monitor.Log((await eng.Resolve("remote", ct)).Message, LogLevel.Info);
                        break;
                    case "revisions":
                        foreach (var v in await eng.Api.Revisions(eng.SaveId, ct))
                            this.Monitor.Log($"#{v.Rev,-4} {v.GameDate,-14} {v.Meta.Played,-8} {v.Device,-10} {v.UploadedAt.ToLocalTime():g}{(v.RestoredFrom > 0 ? $" ↺{v.RestoredFrom}" : "")}", LogLevel.Info);
                        break;
                    case "restore" when args.Length == 2 && int.TryParse(args[1], out int rev):
                        if (this.SaveActive) { this.Monitor.Log("请先回到标题界面。", LogLevel.Error); return; }
                        this.Monitor.Log((await eng.Restore(rev, ct)).Message, LogLevel.Info);
                        break;
                    case "test":
                        this.deviceName = await eng.Api.WhoAmI(ct);
                        this.Monitor.Log($"连接正常，设备名: {this.deviceName}", LogLevel.Info);
                        break;
                    default:
                        this.Monitor.Log("用法: cloudsync status | sync | push | pull | revisions | restore <rev> | test", LogLevel.Info);
                        break;
                }
            }
            catch (Exception ex) { this.Monitor.Log($"失败: {ex.Message}", LogLevel.Error); }
        });
    }
}
