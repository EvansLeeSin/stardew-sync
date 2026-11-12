namespace CloudSaveSync;

public sealed class ModConfig
{
    /// <summary>服务器地址，例如 https://stardew.example.com</summary>
    public string ServerUrl { get; set; } = "";

    /// <summary>设备 Token（stardew-sync-server token add pc 生成）。</summary>
    public string Token { get; set; } = "";

    /// <summary>要同步的存档文件夹名，例如 Farm_123456789。留空时：只有一个存档则自动使用；否则使用第一次加载的存档。</summary>
    public string SaveId { get; set; } = "";

    /// <summary>在标题界面时自动下载云端更新。</summary>
    public bool AutoDownload { get; set; } = true;

    /// <summary>
    /// 代理："auto"（默认：用系统代理，代理连不上时自动直连）、"system"（只走系统代理）、
    /// "none"（始终直连）、或指定地址如 "http://127.0.0.1:7890"。
    /// </summary>
    public string Proxy { get; set; } = "auto";

    /// <summary>启用游玩锁提示。</summary>
    public bool PlayLock { get; set; } = true;
}
