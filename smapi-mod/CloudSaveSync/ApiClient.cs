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
    private readonly HttpClient http;
    private readonly string baseUrl;

    public ApiClient(string baseUrl, string token)
    {
        this.baseUrl = baseUrl.TrimEnd('/');
        this.http = new HttpClient { Timeout = TimeSpan.FromMinutes(5) };
        this.http.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("Bearer", token);
        this.http.DefaultRequestHeaders.UserAgent.ParseAdd("CloudSaveSync-SMAPI/0.1");
    }

    public void Dispose() => this.http.Dispose();

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
        using var resp = await this.http.GetAsync($"{this.baseUrl}/api/v1/whoami", cts.Token);
        if (!resp.IsSuccessStatusCode) throw await Error(resp);
        var d = await Read<Dictionary<string, string>>(resp);
        return d.TryGetValue("device", out var v) ? v : "";
    }

    /// <summary>Latest revision, or null when the server has none.</summary>
    public async Task<SaveSummary?> Latest(string id, CancellationToken ct)
    {
        using var cts = Timeout(ct, TimeSpan.FromSeconds(30));
        using var resp = await this.http.GetAsync(this.Url(id), cts.Token);
        if (resp.StatusCode == HttpStatusCode.NotFound) return null;
        if (!resp.IsSuccessStatusCode) throw await Error(resp);
        return await Read<SaveSummary>(resp);
    }

    public async Task<List<Revision>> Revisions(string id, CancellationToken ct)
    {
        using var cts = Timeout(ct, TimeSpan.FromSeconds(30));
        using var resp = await this.http.GetAsync(this.Url(id, "/revisions"), cts.Token);
        if (!resp.IsSuccessStatusCode) throw await Error(resp);
        return await Read<List<Revision>>(resp);
    }

    public async Task<byte[]> Download(string id, int rev, CancellationToken ct)
    {
        using var resp = await this.http.GetAsync(this.Url(id, $"/revisions/{rev}/archive"), ct);
        if (!resp.IsSuccessStatusCode) throw await Error(resp);
        return await resp.Content.ReadAsByteArrayAsync(ct);
    }

    public async Task<Revision> Upload(string id, byte[] zip, int baseRev, bool force, CancellationToken ct)
    {
        using var form = new MultipartFormDataContent();
        form.Add(new StringContent(JsonSerializer.Serialize(new { base_revision = baseRev, force }), Encoding.UTF8), "meta");
        var file = new ByteArrayContent(zip);
        file.Headers.ContentType = new MediaTypeHeaderValue("application/zip");
        form.Add(file, "archive", id + ".zip");
        using var resp = await this.http.PostAsync(this.Url(id, "/revisions"), form, ct);
        if (resp.StatusCode == HttpStatusCode.Conflict)
            throw new ConflictException((await Read<ConflictBody>(resp)).Latest);
        if (!resp.IsSuccessStatusCode) throw await Error(resp);
        return (await Read<UploadResult>(resp)).Revision;
    }

    public async Task<Revision> Restore(string id, int rev, CancellationToken ct)
    {
        using var resp = await this.http.PostAsync(this.Url(id, $"/restore/{rev}"), null, ct);
        if (!resp.IsSuccessStatusCode) throw await Error(resp);
        return (await Read<UploadResult>(resp)).Revision;
    }

    public async Task<LockResult> Lock(string id, bool force, string gameDate, CancellationToken ct)
    {
        using var cts = Timeout(ct, TimeSpan.FromSeconds(20));
        var body = new StringContent(JsonSerializer.Serialize(new { force, game_date = gameDate }), Encoding.UTF8, "application/json");
        using var resp = await this.http.PutAsync(this.Url(id, "/lock"), body, cts.Token);
        if (resp.StatusCode != HttpStatusCode.OK && (int)resp.StatusCode != 423) throw await Error(resp);
        var lr = await Read<LockResponse>(resp);
        return new LockResult { Acquired = lr.Acquired, Lock = lr.Lock };
    }

    public async Task Unlock(string id, CancellationToken ct)
    {
        using var cts = Timeout(ct, TimeSpan.FromSeconds(10));
        using var resp = await this.http.DeleteAsync(this.Url(id, "/lock"), cts.Token);
    }
}
