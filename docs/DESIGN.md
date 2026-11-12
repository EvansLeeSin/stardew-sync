# Stardew Sync 设计文档

星露谷物语 **PC（解压版 + SMAPI）↔ Android（Root + ReSukiSU）** 单农场存档同步。

## 1. 组件

```
┌──────────── PC (SMAPI) ──────────────────┐
│ CloudSaveSync Mod (C# / net6.0)          │
│  · 游戏启动 / 回到标题 → 拉取云端         │
│  · GameLoop.Saved → 后台上传             │
│  · SaveLoaded → 游玩锁（只提醒不阻止）    │
└───────────────┬──────────────────────────┘
                │ HTTPS (Cloudflare Tunnel) + Bearer Token
        ┌───────▼────────────────────────┐
        │ stardew-sync-server (Go)        │  Docker，仅监听 127.0.0.1:<高位端口>
        │ JSON 索引 + zip 文件存储        │  版本历史 / 回滚 / 长轮询 / 游玩锁
        └───────▲────────────────────────┘
                │
┌───────────────┴──── Android (KSU 模块) ──┐
│ service.sh → stardew-sync daemon (Go arm64 静态) │
│  · 游戏未运行 → 长轮询，云端有新版本立即应用      │
│  · 游戏运行中 → 持锁心跳，存档变化 5 秒后上传     │
│  · 游戏退出   → 上传、释放锁                      │
│  · WebUI：状态、冲突处理、历史版本、回滚          │
└──────────────────────────────────────────┘
```

## 2. 存档格式与哈希

- 存档目录：`Saves/<SaveID>/`，包含 `<SaveID>`（主存档）、`SaveGameInfo`，以及可能存在的 `*_old` 备份。
- 打包：目录下所有普通文件平铺进 zip（不含子目录）。
- **内容哈希**（三端算法必须一致）：
  `hash = hex( sha256( sha256(主存档字节) ‖ sha256(SaveGameInfo字节) ) )`
  不对 zip 算哈希（zip 含时间戳）。
- **元数据**：从 `SaveGameInfo`（根 `<Farmer>` 的直接子元素）解析：
  `name`、`farmName`、`gameVersion`、`millisecondsPlayed`、`yearForSaveGame`、`seasonForSaveGame`(0 春 1 夏 2 秋 3 冬)、`dayOfMonthForSaveGame`。
- **版本兼容**：只比较 `major.minor.patch` 前三段。云端存档版本 > 本机游戏版本时拒绝下载。

## 3. 冲突判定

每台设备本地保存 `state.json`：`base_revision`、`base_hash`（上次同步时的版本号与内容哈希）。

| 本地改动（local_hash ≠ base_hash） | 云端更新（latest.rev > base_revision） | 动作 |
|---|---|---|
| 否 | 否 | 无 |
| 是 | 否 | 上传（base_revision=当前基线） |
| 否 | 是 | 下载（覆盖前先备份） |
| 是 | 是 | 冲突，用户选择保留哪份 |

- 本地哈希与云端最新哈希相同时，直接把基线对齐到云端，不做任何传输。
- 服务端在上传时校验 `base_revision == latest`，不一致返回 **409**（`force=true` 时跳过校验，用于冲突解决“保留本地”）。
- 任何覆盖本地存档的操作前，先把本地存档打包到备份目录（保留最近 10 份）。

## 4. 服务端 API（前缀 `/api/v1`，`Authorization: Bearer <token>`）

设备身份由 Token 决定（一台设备一个 Token，可单独吊销）。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/saves` | 所有存档及最新版本 |
| GET | `/saves/{id}` | 最新版本元数据（无版本时 404） |
| GET | `/saves/{id}/revisions` | 历史版本（新→旧） |
| GET | `/saves/{id}/revisions/{rev}/archive` | 下载 zip |
| POST | `/saves/{id}/revisions` | multipart：`meta`(JSON `{base_revision, force}`) + `archive`(zip) → 201 / 200(内容重复) / 409 |
| POST | `/saves/{id}/restore/{rev}` | 回滚：把旧版本复制为新版本，历史不丢 |
| GET | `/saves/{id}/wait?after=N&timeout=50` | 长轮询：最新版本号 > N 时立即返回 200，否则超时 204 |
| GET/PUT/DELETE | `/saves/{id}/lock` | 游玩锁 |
| GET | `/healthz` | 健康检查（无需鉴权） |

服务端会自行解析 zip 计算哈希与元数据，不信任客户端上报。

**保留策略**：最近 30 个版本 + 每个游戏内季度的最后一个版本。

**存储布局**（无数据库，备份 `data/` 目录即可）：
```
data/devices.json              设备与 Token 的 sha256
data/saves/<id>/index.json     版本索引
data/saves/<id>/<rev>.zip      存档
```

## 5. 游玩锁（只提醒、不阻止）

- `PUT /lock {force, game_date}`：无锁 / 已过期 / 自己持有 → 200 获得或续期；被他人持有 → 423 返回持有者信息；`force=true` 接管。
- TTL 3 分钟，持锁方每 60 秒心跳；崩溃或断网后自动过期。
- 回到标题 / 退出游戏时 `DELETE /lock`（仅持有者可释放）。
- 锁保存在内存中，服务重启后清空（TTL 本来就很短）。
- PC：加载存档时锁被占用 → 对话框“继续（接管）/ 返回标题”。
- Android：游戏启动时锁被占用 → 系统通知提醒。

## 6. Android 模块要点

- 存档路径使用底层路径 `/data/media/0/Android/data/com.chucklefish.stardewvalley/files/Saves`，不走 FUSE。
- 写入流程：下载 zip → 解压到 `files/.stardew-sync-tmp/<id>` → 按 `Saves` 目录的 uid/gid/mode 修正属主、复制 SELinux 上下文 → 备份旧存档 → 旧目录改名移走 → 新目录改名到位 → 删除旧目录。
- 游戏是否运行：扫描 `/proc/*/cmdline`。
- 本机游戏版本：`dumpsys package` 的 `versionName`，取不到则用本地存档的 `gameVersion`。
- 配置/状态/日志/备份：`/data/adb/stardew-sync/`（更新模块不丢失，卸载模块时删除）。

## 7. 部署

- Docker Compose，容器只映射 `127.0.0.1:${SYNC_PORT}`（默认 37821），由宿主机已有的 cloudflared Tunnel 指向 `http://127.0.0.1:37821`。
- Cloudflare：WAF/Bot Fight 对 `/api/` 放行；长轮询单次 50 秒（< CF 100 秒超时）；响应 `Cache-Control: no-store`。

## 8. 构建

GitHub Actions：
- 每次 push / PR：Go 测试、编译服务端与 Android arm64、编译 SMAPI Mod。
- 推送 `v*` tag：发布 `ghcr.io/evansleesin/stardew-sync-server`（amd64+arm64）镜像，Release 附带 `stardew-sync-android.zip`（KSU 模块）与 `CloudSaveSync.zip`（SMAPI Mod）。
- SMAPI Mod 只引用 `StardewModdingAPI.dll`（CI 从 SMAPI 官方安装包提取），游戏类型通过反射访问，无需游戏 DLL。
