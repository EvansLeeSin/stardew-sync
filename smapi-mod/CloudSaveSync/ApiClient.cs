using System.Net;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;

namespace CloudSaveSync;

public sealed class RevisionMeta
{
    [JsonPropertyName("farmer_name")] public string FarmerName { get; set; } = "";
    [JsonPropertyName("farm_name")] public string FarmName { get; set; } = "";
    [JsonPropertyName("game_version")] public string GameVersion { get; set; } = "";
    [JsonPropertyName("played_ms")] public long PlayedMillis { get; set; }
    [JsonPropertyName("year")] public int Year { get; set; }
    [JsonPropertyName("season")] public int Season { get; set; }
    [JsonPropertyName("day")] public int Day { get; set; }

    public string Played { get { long m = PlayedMillis / 60000; return $"{m / 60}h{m % 60:00}m"; } }
}

public sealed class Revision
{
    [JsonPropertyName("rev")] public int Rev { get; set; }
    [JsonPropertyName("base_revision")] public int BaseRevision { get; set; }
    [JsonPropertyName("device")] public string Device { get; set; } = "";
    [JsonPropertyName("hash")] public string Hash { get; set; } = "";
    [JsonPropertyName("size")] public long Size { get; set; }
    [JsonPropertyName("uploaded_at")] public DateTime UploadedAt { get; set; }
    [JsonPropertyName("restored_from")] public int RestoredFrom { get; set; }
    [JsonPropertyName("meta")] public RevisionMeta Meta { get; set; } = new();
    [JsonPropertyName("game_date")] public string GameDate { get; set; } = "";
}

public sealed class PlayLock
{
    [JsonPropertyName("holder")] public string Holder { get; set; } = "";
    [JsonPropertyName("since")] public DateTime Since { get; set; }
    [JsonPropertyName("last_heartbeat")] public DateTime LastHeartbeat { get; set; }
    [JsonPropertyName("expires_at")] public DateTime ExpiresAt { get; set; }
    [JsonPropertyName("game_date")] public string GameDate { get; set; } = "";
}

public sealed class SaveSummary
{
    [JsonPropertyName("id")] public string Id { get; set; } = "";
    [JsonPropertyName("latest")] public Revision? Latest { get; set; }
    [JsonPropertyName("lock")] public PlayLock? Lock { get; set; }
}

internal sealed class UploadResult
{
    [JsonPropertyName("revision")] public Revision Revision { get; set; } = new();
    [JsonPropertyName("duplicate")] public bool Duplicate { get; set; }
}

internal sealed class ConflictBody
{
    [JsonPropertyName("latest")] public Revision? Latest { get; set; }
}

internal sealed class LockResponse
{
    [JsonPropertyName("acquired")] public bool Acquired { get; set; }
    [JsonPropertyName("lock")] public PlayLock? Lock { get; set; }
}

internal sealed class ErrorBody
{
    [JsonPropertyName("error")] public string Error { get; set; } = "";
}

public sealed class ConflictException : Exception
{
    public Revision? Latest { get; }
    public ConflictException(Revision? latest) : base($"conflict: server has rev {latest?.Rev} from {latest?.Device}") => Latest = latest;
}

public sealed class LockResult
{
    public bool Acquired;
    public PlayLock? Lock;
}

/// <summary>HTTP client for the stardew-sync server (see internal/api in Go).</summary>
public sealed class ApiClient : IDisposable
{
    private readonly HttpClient http;          // proxy per config
    private readonly HttpClient? direct;       // no proxy; only in "auto" mode when a proxy applies
    private readonly string baseUrl;
    private readonly Action<string>? log;
    private DateTime preferDirectUntil = DateTime.MinValue;

    /// <summary>Proxy used for the server URL (for diagnostics), or null when connecting directly.</summary>
    public string? ProxyInUse { get; }

    /// <param name="proxy">
    /// "auto" (default): system proxy (HTTPS_PROXY env / Windows settings), falling back to a direct
    /// connection when the proxy is unreachable. "system": system proxy only. "none": always direct.
    /// "http://host:port": explicit proxy.
    /// </param>
    public ApiClient(string baseUrl, string token, string? proxy = "auto", Action<string>? log = null)
    {
        this.baseUrl = baseUrl.TrimEnd('/');
        this.log = log;
        string mode = (proxy ?? "").Trim();
        if (mode.Length == 0) mode = "auto";

        IWebProxy? webProxy = mode.ToLowerInvariant() switch
        {
            "none" => null,
            "auto" or "system" => HttpClient.DefaultProxy,
            _ => new WebProxy(mode),
        };
        this.ProxyInUse = DescribeProxy(webProxy, new Uri(this.baseUrl));
        this.http = Create(token, this.ProxyInUse != null ? webProxy : null);
        if (mode.Equals("auto", StringComparison.OrdinalIgnoreCase) && this.ProxyInUse != null)
            this.direct = Create(token, null);
    }

    private static HttpClient Create(string token, IWebProxy? proxy)
    {
        var handler = new SocketsHttpHandler
        {
            UseProxy = proxy != null,
            Proxy = proxy,
            ConnectTimeout = TimeSpan.FromSeconds(15),
            PooledConnectionLifetime = TimeSpan.FromMinutes(5),
        };
        var c = new HttpClient(handler) { Timeout = TimeSpan.FromMinutes(5) };
        c.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("Bearer", token);
        c.DefaultRequestHeaders.UserAgent.ParseAdd("CloudSaveSync-SMAPI/0.1");
        return c;
    }

    private static string? DescribeProxy(IWebProxy? proxy, Uri target)
    {
        if (proxy == null) return null;
        try
        {
            if (proxy.IsBypassed(target)) return null;
            var p = proxy.GetProxy(target);
            return p == null || p == target ? null : p.Authority;
        }
        catch { return null; }
    }

    public void Dispose()
    {
        this.http.Dispose();
        this.direct?.Dispose();
    }

    /// <summary>True when the failure happened while connecting (proxy down / refused / unreachable).</summary>
    private static bool IsConnectFailure(HttpRequestException ex)
    {
        for (Exception? e = ex; e != null; e = e.InnerException)
            if (e is System.Net.Sockets.SocketException) return true;
        return false;
    }

    /// <summary>Send a request; in auto mode retry directly when the proxy cannot be reached.</summary>
    private async Task<HttpResponseMessage> Send(Func<HttpRequestMessage> make, CancellationToken ct)
    {
        if (this.direct != null && DateTime.UtcNow < this.preferDirectUntil)
            return await this.direct.SendAsync(make(), ct).ConfigureAwait(false);
        try
        {
            return await this.http.SendAsync(make(), ct).ConfigureAwait(false);
        }
        catch (HttpRequestException ex) when (this.direct != null && IsConnectFailure(ex) && !ct.IsCancellationRequested)
        {
            this.log?.Invoke($"代理 {this.ProxyInUse} 不可用（{ex.Message}），改为直连。");
            var resp = await this.direct.SendAsync(make(), ct).ConfigureAwait(false);
            this.preferDirectUntil = DateTime.UtcNow.AddMinutes(10);
            return resp;
        }
        catch (HttpRequestException ex) when (this.ProxyInUse != null && IsConnectFailure(ex))
        {
            throw new HttpRequestException($"{ex.Message}（经由代理 {this.ProxyInUse}；可在 config.json 设置 Proxy 为 none 直连）", ex);
        }
    }

    private Task<HttpResponseMessage> Get(string url, CancellationToken ct)
        => this.Send(() => new HttpRequestMessage(HttpMethod.Get, url), ct);

    private string Url(string id, string suffix = "") => $"{this.baseUrl}/api/v1/saves/{Uri.EscapeDataString(id)}{suffix}";

    private static async Task<Exception> Error(HttpResponseMessage resp)
    {
        string body = await resp.Content.ReadAsStringAsync();
        try
        {
            var e = JsonSerializer.Deserialize<ErrorBody>(body);
            if (!string.IsNullOrEmpty(e?.Error)) return new HttpRequestException($"server {(int)resp.StatusCode}: {e.Error}");
        }
        catch (JsonException) { }
        if (body.Length > 200) body = body[..200];
        return new HttpRequestException($"server {(int)resp.StatusCode}: {body}");
    }

    private static async Task<T> Read<T>(HttpResponseMessage resp)
        => JsonSerializer.Deserialize<T>(await resp.Content.ReadAsStringAsync()) ?? throw new InvalidDataException("empty response");

    private static CancellationTokenSource Timeout(CancellationToken ct, TimeSpan t)
    {
        var cts = CancellationTokenSource.CreateLinkedTokenSource(ct);
        cts.CancelAfter(t);
        return cts;
    }

    public async Task<string> WhoAmI(CancellationToken ct)
    {
        using var cts = Timeout(ct, TimeSpan.FromSeconds(20));
        using var resp = await this.Get($"{this.baseUrl}/api/v1/whoami", cts.Token);
        if (!resp.IsSuccessStatusCode) throw await Error(resp);
        var d = await Read<Dictionary<string, string>>(resp);
        return d.TryGetValue("device", out var v) ? v : "";
    }

    /// <summary>Latest revision, or null when the server has none.</summary>
    public async Task<SaveSummary?> Latest(string id, CancellationToken ct)
    {
        using var cts = Timeout(ct, TimeSpan.FromSeconds(30));
        using var resp = await this.Get(this.Url(id), cts.Token);
        if (resp.StatusCode == HttpStatusCode.NotFound) return null;
        if (!resp.IsSuccessStatusCode) throw await Error(resp);
        return await Read<SaveSummary>(resp);
    }

    public async Task<List<Revision>> Revisions(string id, CancellationToken ct)
    {
        using var cts = Timeout(ct, TimeSpan.FromSeconds(30));
        using var resp = await this.Get(this.Url(id, "/revisions"), cts.Token);
        if (!resp.IsSuccessStatusCode) throw await Error(resp);
        return await Read<List<Revision>>(resp);
    }

    public async Task<byte[]> Download(string id, int rev, CancellationToken ct)
    {
        using var resp = await this.Get(this.Url(id, $"/revisions/{rev}/archive"), ct);
        if (!resp.IsSuccessStatusCode) throw await Error(resp);
        return await resp.Content.ReadAsByteArrayAsync(ct);
    }

    public async Task<Revision> Upload(string id, byte[] zip, int baseRev, bool force, CancellationToken ct)
    {
        string meta = JsonSerializer.Serialize(new { base_revision = baseRev, force });
        using var resp = await this.Send(() =>
        {
            var form = new MultipartFormDataContent();
            form.Add(new StringContent(meta, Encoding.UTF8), "meta");
            var file = new ByteArrayContent(zip);
            file.Headers.ContentType = new MediaTypeHeaderValue("application/zip");
            form.Add(file, "archive", id + ".zip");
            return new HttpRequestMessage(HttpMethod.Post, this.Url(id, "/revisions")) { Content = form };
        }, ct);
        if (resp.StatusCode == HttpStatusCode.Conflict)
            throw new ConflictException((await Read<ConflictBody>(resp)).Latest);
        if (!resp.IsSuccessStatusCode) throw await Error(resp);
        return (await Read<UploadResult>(resp)).Revision;
    }

    public async Task<Revision> Restore(string id, int rev, CancellationToken ct)
    {
        using var resp = await this.Send(() => new HttpRequestMessage(HttpMethod.Post, this.Url(id, $"/restore/{rev}")), ct);
        if (!resp.IsSuccessStatusCode) throw await Error(resp);
        return (await Read<UploadResult>(resp)).Revision;
    }

    public async Task<LockResult> Lock(string id, bool force, string gameDate, CancellationToken ct)
    {
        using var cts = Timeout(ct, TimeSpan.FromSeconds(20));
        string body = JsonSerializer.Serialize(new { force, game_date = gameDate });
        using var resp = await this.Send(() => new HttpRequestMessage(HttpMethod.Put, this.Url(id, "/lock"))
        {
            Content = new StringContent(body, Encoding.UTF8, "application/json"),
        }, cts.Token);
        if (resp.StatusCode != HttpStatusCode.OK && (int)resp.StatusCode != 423) throw await Error(resp);
        var lr = await Read<LockResponse>(resp);
        return new LockResult { Acquired = lr.Acquired, Lock = lr.Lock };
    }

    public async Task Unlock(string id, CancellationToken ct)
    {
        using var cts = Timeout(ct, TimeSpan.FromSeconds(10));
        using var resp = await this.Send(() => new HttpRequestMessage(HttpMethod.Delete, this.Url(id, "/lock")), cts.Token);
    }
}
