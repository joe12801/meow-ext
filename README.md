# meow-ext · 飞牛音乐 Meow 音源扩展

把 [MeowMusicServer](https://github.com/joe12801/MeowMusicServer-multisource) 的多源音乐接进**官方飞牛音乐**：继续用 fnOS 自带的 Web 与 App，搜索框里除了本地曲库，还会多出在线曲目，点开就能听。

单二进制、无容器、无 Python、无 Node。安装包 3MB 上下。

> 面向 fnOS 官方音乐应用 `trim.music` 的社区扩展，非飞牛官方产品。通过可逆的 socket 接管增加能力，**不修改官方二进制、不修改 nginx 配置、不写官方数据库**；在应用中心停止本应用即恢复官方直连。

## 它做什么

```text
飞牛音乐 Web / App
        │
        ▼
官方 nginx（配置不动）
        │  /var/run/trim_music.socket
        ▼
   meow-ext（单进程，systemd 托管）
        ├── 本地曲库 / 播放列表 / 设置 等 ──► 官方后端（原样透传）
        ├── 搜索          ──► 官方结果 + Meow /api/search 合并
        ├── 播放 / 拖动进度 ──► Meow /stream_live（含 Range）
        ├── 歌词          ──► Meow /api/lyric
        └── 管理页         ──► /app/meow-ext/（桌面图标进入）
                                     │
                                     ▼
                          MeowMusicServer（外部服务）
                          kuwo / netease / kugou / bilibili
                          ximalaya / 5sing / 荔枝 / 汽水 / ext
```

**只做三件事**：搜索合并、取流转发、歌词与封面。收藏、歌单、播放历史、转码落库这些仍由官方与 Meow 各自负责——扩展不掺和，也就不会因版本更新而与官方业务语义打架。

## 两种安装方式

### 1. 应用中心（推荐）

1. 应用中心里已安装并**启动**官方「飞牛音乐」。
2. 从 Releases 下载 `meow-ext-<版本>-arm64.fpk`（或 `-amd64`），应用中心 → 手动安装。
3. 向导里填 Meow 地址（可以留空，装完再填）。
4. 从飞牛桌面打开「Meow 音源扩展」，测试连接、选音源、保存。

### 2. 命令行

```bash
# 编译（需要 go，或本机有 docker 会自动用容器编译）
./packaging/build.sh

sudo ./scripts/install.sh --meow-url http://192.168.2.220:2233
```

卸载：

```bash
sudo ./scripts/install.sh --uninstall
```

## 配置

配置文件默认 `/etc/meow-ext/config.json`（应用中心安装时在应用数据目录下），也可以在管理页里改。环境变量优先级最高，便于临时排障。

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `meow_url` | `http://127.0.0.1:2233` | MeowMusicServer 地址，含 `http://` |
| `meow_token` | 空 | 上游启用鉴权时填写 |
| `sources` | 空 | 启用的音源键名；**留空 = 由 Meow 决定** |
| `search_limit` | 30 | 单次从 Meow 拉的候选条数 |
| `online_enabled` | true | 关掉后退化为纯透传 |
| `search_timeout_s` | 5 | 搜索超时；超时只是不给在线结果，官方结果照常 |
| `stream_timeout_s` | 300 | 取流超时；Meow 首次点歌要转码，给足时间 |
| `lyric_timeout_s` | 8 | 歌词超时 |
| `target_socket` | `/var/run/trim_music.socket` | 官方入口，接管目标 |
| `upstream_socket` | `/var/run/trim_music_upstream.socket` | 官方后端被移到这里 |

环境变量：`MEOW_EXT_URL`、`MEOW_EXT_TOKEN`、`MEOW_EXT_SOURCES`、`MEOW_EXT_ONLINE`、`MEOW_EXT_CONFIG`、`MEOW_EXT_DATA_DIR`、`MEOW_EXT_STATE_DIR`、`MEOW_EXT_GATEWAY_SOCKET`。

## 命令

```bash
meow-ext run       # 接管入口并常驻（systemd 用的就是这个）
meow-ext restore   # 把入口还给官方音乐（幂等，可随时执行）
meow-ext status    # 两端 socket 的实际状态（JSON）
meow-ext check     # 安装前自检：官方在不在、Meow 通不通
meow-ext version
```

## 接管是怎么做的

飞牛音乐的 nginx 固定连 `/var/run/trim_music.socket`。扩展要增强它，就得顶替这个路径，但**官方进程仍持有那个 socket 的 inode**——把文件改名不影响监听，官方照常服务，扩展再把请求转回去。

顺序上有一处绝不能错：Go 的 `net.Listen("unix", p)` 会先 unlink 已存在的 `p`。直接 listen 在入口上，等于把官方 socket 无声删掉。所以实际顺序是：

1. 在私有目录 `listen.sock` 建监听（不影响官方）
2. 把入口 `rename` 到 `trim_music_upstream.socket`
3. 把自己的监听器 `rename` 到入口位置

第 2、3 步之间只有两条 `rename`，窗口在毫秒级；任一步失败都会把官方入口挪回去。进程被 `kill -9` 时来不及回滚，此时入口是"无人监听的残留文件"，下次启动会识别并清理，`systemctl stop` 也会由 `ExecStopPost` 兜底还原。

## 为什么 id 要伪装成 32 位十六进制

飞牛 App 会按 id 形态过滤条目：非 32-hex 的 id 会被整条丢弃（症状是收藏、播放历史列表一片空白）。所以扩展下发的每个在线曲目 id 都是 `md5("meow-ext::" + 真实id)`，并在本地维护双向映射——客户端拿这个 id 回来取流时再解回。

映射表落盘保存（默认 `guid-map.json`），因为进程重启后客户端手里还握着旧 id。丢了它，历史里的歌点开就是 404。

真实 id 本身是 `online:meow:<base64>`，里面打包了歌名、歌手、`xid`、`bv` 与来源。**这几项不能省**：喜马拉雅（`xid=trackId`）、5sing（`xid=fs:<songId>:<type>`）、B 站（`bv=BV1…`）只能按 id 精确取流，只靠"歌名+歌手"会解析成同名的另一条音频——用户点的是这集有声书，响的却是同名的歌。

## 已知限制

这些是**上游**的限制，不是扩展的问题，管理页里也标了出来：

| 音源 | 情况 |
| --- | --- |
| QQ 音乐 | 上游常返回空结果 |
| 喜马拉雅 | 热门内容多为 VIP，需要上游账号有会员 |
| 5sing | 游客每日 20 首，用满后返回 429 |
| 荔枝 FM | 上游解析不稳定 |

**Meow 自己的游客额度是全局的**（`GUEST_DAILY_SONGS`，默认 20 首/天，按来源 IP 计），不分音源。用满后 `/stream_live` 一律返回：

```
HTTP/1.1 429 Too Many Requests
Retry-After: 3600
X-Guest-Quota: exhausted

游客每日试听 20 首已用完，登录后不限
```

扩展**不会**把它翻译成自己的错误，而是连状态码、`Retry-After`、`X-Guest-Quota` 和那句原文一起透传给客户端——用户需要知道该等一小时还是去登录 Meow。在 Meow 里登录（或给扩展配 `meow_token`）即可解除；把 Meow 的 `GUEST_DAILY_SONGS` 设成 `0` 也能关闭限额。

两个容易踩的坑：

- **HEAD 与"中段 Range"不计额度**（Meow 只把 `Range` 缺失或以 `bytes=0-` 开头的请求算作新的一首）。所以探测长度用 HEAD、拖动进度条都不消耗额度——扩展也必须把 HEAD 原样发出去，否则每探一次就白烧一首。
- **官方搜索接口需要登录凭据**。扩展只是透传客户端带来的 `cookie` / `authorization` / `x-trim-music-temp-token`，自己不做登录；因此裸 curl 打 `/music/api/v1/search/track` 会得到 `INVALID TOKEN`，这不是扩展坏了。

另外两件事得先说明：

- **本扩展不含音乐源**。它只是适配器，需要一个已经在跑的 MeowMusicServer。
- **收藏与播放历史**里的在线曲目由官方接口处理，扩展不接管这部分写入。能听、能拖进度、有歌词；是否进收藏列表取决于官方对未知 id 的处理。

## 开发

```bash
go test ./...                    # 单元与集成测试（假官方 socket + 假 Meow）
python tools/make_icons.py       # 重新生成图标
VERSION=0.1.0 ./packaging/build.sh
```

代码结构：

| 文件 | 职责 |
| --- | --- |
| `main.go` | 子命令、生命周期、信号处理 |
| `takeover.go` | socket 接管与回滚 |
| `server.go` | 路由装配、健康检查 |
| `search.go` | 搜索合并、曲目 VO 构造 |
| `stream.go` | 取流、歌词、封面 |
| `meow.go` | Meow 上游客户端 |
| `guid.go` | id 编解码与 32-hex 伪装映射 |
| `admin.go` + `ui/index.html` | 管理页 |
| `upstream.go` | 官方后端反代 |

## 许可

MIT
