using CloudSaveSync;

// Simulate a dead system proxy. Must run before the first HttpClient is created
// (HttpClient.DefaultProxy is read once per process).
foreach (var k in new[] { "NO_PROXY", "no_proxy", "ALL_PROXY", "all_proxy", "http_proxy", "https_proxy" })
    Environment.SetEnvironmentVariable(k, null);
Environment.SetEnvironmentVariable("HTTP_PROXY", "http://127.0.0.1:1");
Environment.SetEnvironmentVariable("HTTPS_PROXY", "http://127.0.0.1:1");

int failures = 0;
void Check(bool ok, string what) { Console.WriteLine((ok ? "PASS " : "FAIL ") + what); if (!ok) failures++; }

// Must equal internal/savefmt TestHashVector.
Check(SaveFormat.Hash("main"u8.ToArray(), "info"u8.ToArray()) == "f96b632ae5430d2adb6dd9cf504edf07bb68365fda9403683eee2b8f0918ce5e", "hash vector matches Go");

const string sid = "Sunny_123";
string Info(int day, string ver = "1.6.15") =>
    $"<?xml version=\"1.0\"?><Farmer><name>E</name><items><Item><name>Axe</name></Item></items><farmName>Sunny</farmName><gameVersion>{ver}</gameVersion><millisecondsPlayed>4500000</millisecondsPlayed><yearForSaveGame>1</yearForSaveGame><seasonForSaveGame>1</seasonForSaveGame><dayOfMonthForSaveGame>{day}</dayOfMonthForSaveGame></Farmer>";

var meta = SaveFormat.ParseInfo(System.Text.Encoding.UTF8.GetBytes(Info(15)));
Check(meta.FarmerName == "E" && meta.Day == 15 && meta.Season == 1 && meta.GameVersion == "1.6.15" && meta.GameDate == "第1年 夏 15日" && meta.Played == "1h15m", $"ParseInfo {meta.GameDate} {meta.Played} {meta.FarmerName}");
Check(SaveFormat.CompareVersions("1.6.15", "1.6.14.1") == 1 && SaveFormat.CompareVersions("", "1.6") == 0 && SaveFormat.CompareVersions("1.6.9", "1.6.10") == -1, "CompareVersions");

string root = Path.Combine(Path.GetTempPath(), "css-test-" + Guid.NewGuid().ToString("N"));
void WriteSave(string saves, int day, string ver = "1.6.15")
{
    var d = Path.Combine(saves, sid); Directory.CreateDirectory(d);
    File.WriteAllText(Path.Combine(d, sid), $"<SaveGame day='{day}'/>");
    File.WriteAllText(Path.Combine(d, "SaveGameInfo"), Info(day, ver));
}
int Day(string saves) => SaveFormat.ReadDir(Path.Combine(saves, sid), sid)!.Meta.Day;

if (args.Length == 3)
{
    var hostA = new Host(Path.Combine(root, "a", "Saves"));
    var hostB = new Host(Path.Combine(root, "b", "Saves"));
    var a = new SyncEngine(hostA, new ApiClient(args[0], args[1]), Path.Combine(root, "a", "data"), sid);
    var b = new SyncEngine(hostB, new ApiClient(args[0], args[2]), Path.Combine(root, "b", "data"), sid);
    var ct = CancellationToken.None;

    WriteSave(hostA.SavesPath, 1);
    var r = await a.Sync(true, true, ct); Check(r.Action == SyncAction.Uploaded, "A upload: " + r.Message);
    r = await b.Sync(true, true, ct); Check(r.Action == SyncAction.Downloaded && Day(hostB.SavesPath) == 1, "B download: " + r.Message);
    r = await b.Sync(true, true, ct); Check(r.Action == SyncAction.InSync, "B in sync");
    WriteSave(hostB.SavesPath, 2);
    r = await b.Sync(true, true, ct); Check(r.Action == SyncAction.Uploaded, "B upload");
    hostA.Refuse = true;
    r = await a.Sync(true, true, ct); Check(r.Action == SyncAction.Pending && Day(hostA.SavesPath) == 1, "A refuse apply while loaded: " + r.Message);
    hostA.Refuse = false;
    r = await a.Sync(true, true, ct); Check(r.Action == SyncAction.Downloaded && Day(hostA.SavesPath) == 2, "A download");
    WriteSave(hostA.SavesPath, 3); WriteSave(hostB.SavesPath, 4);
    await b.Sync(true, true, ct);
    r = await a.Sync(true, true, ct); Check(r.Action == SyncAction.Conflict && a.State.Conflict != null, "conflict: " + r.Message);
    r = await a.Resolve("remote", ct); Check(r.Action == SyncAction.Downloaded && Day(hostA.SavesPath) == 4, "resolve remote");
    Check(Directory.GetFiles(Path.Combine(root, "a", "data", "backups")).Length == 2, "backups created");
    r = await a.Restore(1, ct); Check(r.Action == SyncAction.Downloaded && Day(hostA.SavesPath) == 1, "restore #1");
    WriteSave(hostB.SavesPath, 9, "1.7.0");
    r = await b.Sync(true, true, ct);
    Check(r.Action == SyncAction.Conflict, "B conflict after restore (local changed)");
    r = await b.Resolve("local", ct); Check(r.Action == SyncAction.Uploaded, "resolve local");
    r = await a.Sync(true, true, ct); Check(r.Action == SyncAction.Pending && Day(hostA.SavesPath) == 1, "version guard: " + r.Message);
    // Proxy handling: a configured-but-dead system proxy (e.g. Clash closed); env set at program start.
    var fallbackLogs = new List<string>();
    // Proxies always bypass loopback addresses, so reach the server via the machine host name
    // (the server must listen on all interfaces for this, see build.yml).
    string viaHost = new UriBuilder(args[0]) { Host = System.Net.Dns.GetHostName() }.Uri.ToString();
    var auto = new ApiClient(viaHost, args[1], "auto", fallbackLogs.Add);
    Check(auto.ProxyInUse == "127.0.0.1:1", "auto picks up env proxy: " + auto.ProxyInUse);
    var who = await auto.WhoAmI(ct);
    Check(who == "pc" && fallbackLogs.Count == 1, "auto falls back to direct when proxy is down: " + string.Join(";", fallbackLogs));
    Check(await auto.Latest(sid, ct) != null && fallbackLogs.Count == 1, "auto keeps using direct afterwards");
    var none = new ApiClient(args[0], args[1], "none");
    Check(none.ProxyInUse == null && await none.WhoAmI(ct) == "pc", "none = direct");
    try { await new ApiClient(args[0], args[1], "http://127.0.0.1:1").WhoAmI(ct); Check(false, "explicit dead proxy should fail"); }
    catch (HttpRequestException ex) { Check(ex.Message.Contains("127.0.0.1:1") && ex.Message.Contains("none"), "explicit dead proxy error: " + ex.Message); }

    var l1 = await a.Api.Lock(sid, false, "x", ct); var l2 = await b.Api.Lock(sid, false, "", ct);
    Check(l1.Acquired && !l2.Acquired && l2.Lock?.Holder != null, "lock: holder=" + l2.Lock?.Holder);
}
try { Directory.Delete(root, true); } catch { }
Console.WriteLine(failures == 0 ? "ALL PASSED" : $"{failures} FAILED");
return failures == 0 ? 0 : 1;

sealed class Host : ISyncHost
{
    public Host(string saves) { this.SavesPath = saves; Directory.CreateDirectory(saves); }
    public string SavesPath { get; }
    public string GameVersion => "1.6.15";
    public bool Refuse;
    public bool TryApply(Action apply) { if (this.Refuse) return false; apply(); return true; }
    public void Log(string message, bool warn = false) { }
}
