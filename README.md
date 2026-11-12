# Stardew Sync

星露谷物语 **PC（SMAPI）↔ Android（Root + KernelSU/ReSukiSU）** 单农场存档云同步，服务端自建。

- PC 端：SMAPI Mod，回到标题时拉取，睡觉存档后自动上传
- Android 端：KSU 模块，游戏未运行时自动应用云端新存档，游戏退出后上传
- 服务端：Go 单二进制 / Docker，保留版本历史，可回滚
- 冲突检测（双方都改过时不自动覆盖）、游玩锁提醒、覆盖前本地自动备份

设计细节见 [docs/DESIGN.md](docs/DESIGN.md)。

## 构建产物

推送 `v*` tag（如 `v0.1.0`）后，GitHub Actions 会自动：
- 发布 Docker 镜像 `ghcr.io/evansleesin/stardew-sync-server:<版本>` 和 `latest`
- 在 Release 中附带 `stardew-sync-android.zip`（KSU 模块）和 `CloudSaveSync.zip`（SMAPI Mod）

推送到 main 分支会发布 `edge` 镜像；每次构建的产物也可以在 Actions 的 Artifacts 中下载。

> 仓库是公开的，GHCR 镜像第一次发布后默认是私有包。请到 GitHub → Packages → stardew-sync-server → Package settings 改为 Public，或者在服务器上 `docker login ghcr.io`。

---

## 1. 部署服务端（Docker + 已有的 Cloudflare Tunnel）

```bash
sudo mkdir -p /opt/stardew-sync && cd /opt/stardew-sync
# 复制仓库里的 deploy/docker-compose.yml 和 deploy/.env.example 到此目录
cp .env.example .env            # 按需修改 SYNC_PORT（默认 37821）
ss -tlnp | grep 37821           # 确认端口未被占用
mkdir -p data && sudo chown 10001:10001 data   # 容器以 uid 10001 运行
docker compose up -d
curl http://127.0.0.1:37821/healthz            # {"status":"ok"}
```

**生成设备 Token**（每台设备一个，只显示一次）：
```bash
docker compose exec sync stardew-sync-server token add pc
docker compose exec sync stardew-sync-server token add android
docker compose exec sync stardew-sync-server token list
docker compose exec sync stardew-sync-server token revoke pc   # 吊销
```
设备名会显示在版本历史和游玩锁提示里。

**Cloudflare Tunnel**：在 Zero Trust → Networks → Tunnels → 你的隧道 → Public Hostname 新增
`stardew.你的域名` → `HTTP` → `127.0.0.1:37821`。
> 如果 cloudflared 本身也跑在 Docker 里（而不是宿主机上），`127.0.0.1` 指向的是它自己的容器。这种情况下要让 cloudflared 和 sync 加入同一个 Docker 网络，并把地址填成 `http://stardew-sync:8080`。

**Cloudflare 建议设置**
- Security → WAF：新增一条自定义规则，`Hostname equals stardew.你的域名` → **Skip**（跳过所有托管规则、Bot Fight Mode 和 Super Bot Fight Mode）。Mod 和守护进程不是浏览器，会被人机验证拦住；服务端本身有 Token 鉴权。
- Caching → Cache Rules：对该主机名设置 Bypass cache。服务端已经返回 `Cache-Control: no-store`，这一条是保险。

**升级 / 备份 / 卸载**
```bash
docker compose pull && docker compose up -d            # 升级
tar czf stardew-sync-backup.tgz data                   # 备份（只需 data/）
docker compose down --rmi all && cd / && sudo rm -rf /opt/stardew-sync   # 卸载
```

---

## 2. PC 端（SMAPI Mod）

1. 安装 [SMAPI](https://smapi.io)：运行安装器，游戏目录选择解压包中 `Stardew Valley.exe` 所在的文件夹。以后通过 `StardewModdingAPI.exe` 启动游戏。
2. 把 `CloudSaveSync.zip` 解压到游戏目录的 `Mods/` 下，得到 `Mods/CloudSaveSync/`。
3. 启动一次游戏后退出，然后编辑 `Mods/CloudSaveSync/config.json`：
   ```json
   {
     "ServerUrl": "https://stardew.你的域名",
     "Token": "sds_...（pc 的 token）",
     "SaveId": "农场名_123456789",
     "AutoDownload": true,
     "PlayLock": true
   }
   ```
   `SaveId` 是 `%AppData%\StardewValley\Saves\` 下的文件夹名。只有一个存档时可以留空，会自动识别。
4. 在 SMAPI 控制台输入 `cloudsync test`，确认连接正常。

**行为**
| 时机 | 动作 |
|---|---|
| 游戏启动 / 回到标题 | 检查云端，有新版本就下载（先备份本地到 `Mods/CloudSaveSync/data/backups/`） |
| 加载存档 | 申请游玩锁。另一端正在玩，或云端有更新的进度时弹窗提醒，可以选择继续或返回标题 |
| 每晚睡觉存档后 | 后台上传，HUD 显示“☁ 存档已上传 #N” |
| 冲突 | 回到标题时弹窗，选择保留云端或保留本机 |

**控制台命令**：`cloudsync status | sync | push（保留本机）| pull（保留云端）| revisions | restore <版本号> | test`

---

## 3. Android 端（KSU 模块）

1. 在 KernelSU/ReSukiSU 管理器中刷入 `stardew-sync-android.zip`，然后重启。
2. 在管理器的模块列表里打开 **Stardew Sync** 的 WebUI：
   - 填写服务器地址、android 的 Token 和存档 ID（与 PC 相同的文件夹名），保存
   - 点击“测试连接”
3. 守护进程会在开机后自动运行，WebUI 里也可以手动启动或停止。

**行为**
- 游戏**未运行**时：通过长轮询实时获知云端更新，立即应用到本机（先备份）。PC 上睡觉存档后几秒，手机里就是最新进度。
- 游戏**启动**时：申请游玩锁。如果 PC 正在玩，会发系统通知提醒。
- 游戏**运行中**：存档文件写入并稳定 5 秒后上传。云端有更新时只发通知，不覆盖本机。
- 游戏**退出**后：再检查一次并上传，然后释放锁。
- 冲突：发通知，在 WebUI 中选择“保留本机”或“保留云端”。
- WebUI 还提供历史版本、回滚和日志查看。模块卡片上的“执行”按钮会立即同步一次。

数据目录是 `/data/adb/stardew-sync/`，存放配置、状态、日志和最近 10 份备份。卸载模块时会一并删除。

命令行（root shell）：
```sh
B=/data/adb/modules/stardew-sync/bin/stardew-sync
$B status | $B sync | $B push | $B pull | $B revisions | $B restore 12
$B config set server_url https://stardew.你的域名
```

---

## 首次使用顺序

1. 部署服务端，生成两个 Token。
2. 在**进度最新**的那一端先完成配置。它会把本地存档作为 #1 上传。
3. 再配置另一端。它的本地存档和云端不同、又没有同步基线，会被判定为**冲突**，这时选择“保留云端”即可。另一端本来没有这个存档时，会直接下载。

## 注意

- 两端游戏版本最好一致。云端存档版本高于本机游戏版本时会拒绝覆盖，并提示先更新游戏。
- 两端都不要装会往存档里写数据的 Mod，否则另一端可能读不了存档。
- 回滚 = 把旧版本复制成一个新的最新版本，历史记录不会丢。

## 开发

```bash
go test ./...                                   # 服务端 + 同步引擎
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./cmd/agent
# SMAPI Mod：把 SMAPI 安装包 internal/windows/install.dat（zip）解压到 smapi-mod/lib/
dotnet build smapi-mod/CloudSaveSync/CloudSaveSync.csproj -c Release
# C# 引擎与 Go 服务端的联调测试（见 .github/workflows/build.yml）
dotnet run --project smapi-mod/CloudSaveSync.Tests -- http://127.0.0.1:37999 <tokenA> <tokenB>
```
