# Windows 打包与在线升级方案

> 日期：2026-08-10
> 目标：将 Milevia 桌面端打包为 exe 安装程序，支持用户下载安装；后续在新版本发布后，于主界面提醒用户可升级，点击即可在线升级；并提供版本管理与版本显示。整体采用个人免费方案（GitHub Releases + 静态 latest.json 清单），零服务器费用。

## 1. 问题与目标

Milevia 桌面端目前只有开发形态（`pnpm dev` 拉起 Tauri + sidecar），虽然 `tauri build` 已能产出安装包，但用户没有便捷的下载渠道，发布新版本后也**完全没有升级通道**——用户只能卸载重装。目标是把"打包 → 分发 → 在线升级 → 版本展示"这一整条链路打通，且成本为零、维护量最小。

```text
发布方                                                       用户端
----------------------------------------------------   ----------------------------------------------------
tauri build --bundles nsis  ->  Milevia_0.2.0_x64-setup.exe
        │
        ├─ tauri signer sign（用私钥给 exe 生成 signature）
        │
        v  push 到 GitHub Releases
        ├── Milevia_0.2.0_x64-setup.exe（安装包本体）
        └── latest.json（升级清单：版本/下载地址/签名）
            │
            │  GitHub Pages 静态托管（免费）
            v
    启动时后台拉取 latest.json ----------------------------------►
                                                                 v
                                                           server.version > 本地 version ?
                                                              ├─ 否：静默，无提示
                                                              └─ 是：右上角浮"发现新版本 vX → 立即升级"
                                                                  用户点击 → 下载 ╪ 校验签名 → 退出应用
                                                                  → 新版替换旧版 → 重启
```

## 2. 现状盘点

| 项 | 当前状态 | 说明 |
|---|---|---|
| 桌面壳 | Tauri 2.11（`apps/desktop/src-tauri`） | Rust，含托盘、sidecar 托管 |
| 打包 | `bundle.targets = ["nsis","msi"]` | NSIS 配合 `downloadBootstrapper`（详见 §5.1 ③） |
| 前端 | React + Vite（`apps/web`） | 已通过 `window.__TAURI_INTERNALS__.invoke()` 调 Rust，有集成入口 |
| sidecar | 打包时将 `milevia-control.exe` / `milevia-approval.exe` 打进 resources | 需随整包一起替换 |
| 在线升级 | 无 | 本次核心新增 |
| 版本管理 | 无 | 本次补充 |

关键点：`tauri-plugin-updater` 的升级是**整包替换**，sidecar 二进制随新包一起换，天然避免版本不匹配。Tauri 在替换自身 exe 前会要求应用退出，而现有 `ExitRequested` 已会 `stop_sidecar`，动作顺序正确，无需额外处理。

**前置条件**：首个对外可分发版本就必须内置 updater 插件。若某个已分发版本没带 updater，那么装了这个版本的用户永远无法自动升级，只能手动重装（替换安装包）。

## 3. 方案决策（已确认）

| 决策项 | 选择 | 影响 |
|---|---|---|
| 托管位置 | **GitHub Releases** | 大文件走 Releases 免费下载；`latest.json` 用 GitHub Pages 静态托管（主仓根目录，见 §5.5） |
| 升级触发 | **检查自动、安装手动** | 启动时后台静默检查；发现新版只在界面提示，用户点"一键升级"才下载安装，不打断正在跑的任务 |
| Windows 代码签名 | **暂不（免费方案）** | 用户装包会遇"未知发布者"提示，点"仍要运行"即可；与 Tauri 升级签名相互独立，升级安全仍受保护 |

> 备注：GitHub 在国内访问有时偏慢，是免费方案的最大短板。若日后需要加速，把同一份 `latest.json` + `setup.exe` 同步到 Cloudflare R2（全球 CDN + 自定义域名），只需改 `endpoints` 一个 URL，架构不动。

## 4. 技术选型

- **`tauri-plugin-updater`（v2，官方维护，Rust + JS 双端）**：负责检查、下载、签名校验、进度回调。
- **`latest.json` 静态清单**：updater 只拉一个静态 JSON，天然适合 GitHub Pages / 任何静态托管，零后端。
- **Tauri `signer` CLI（内置于 `@tauri-apps/cli`）**：生成密钥对、给安装包签名。
- **项目侧 API**：`@tauri-apps/api`（`getVersion` 取当前版本）、`@tauri-apps/api/process`（`relaunch` 重启）、`@tauri-apps/plugin-updater`（`check` / `downloadAndInstall`）。

## 5. 实施步骤

### 5.1 一次性准备（第一版发布前只做一次）

**① 生成更新签名密钥对**（须在 Windows 本机，用项目里的 tauri CLI，不要在 WSL）：

```powershell
# 用强随机口令（存到本机口令文件，避免口令进聊天/进命令记录）
$chars = 'abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789!@#$%^&*'
$rng = New-Object System.Security.Cryptography.RNGCryptoServiceProvider
$bytes = New-Object byte[] 28; $rng.GetBytes($bytes)
$pw = -join (($bytes | ForEach-Object { $chars[$_ % $chars.Length] }))
Set-Content "$env:USERPROFILE\.tauri\milevia-updater-password.txt" -Value $pw -NoNewline -Encoding utf8

cd apps/desktop
pnpm tauri signer generate -w "$env:USERPROFILE\.tauri\milevia-updater.key" -p $pw --force
```

- `generate` 直接打印**公钥（一行字符串）**，把它原样填进 `tauri.conf.json` 的 `plugins.updater.pubkey`（那是公钥本体，不是 base64 编码）。
- 私钥 `~/.tauri/milevia-updater.key` 与口令文件 `~/.tauri/milevia-updater-password.txt` **都在仓库外、绝不能进 git**；两者一起离线备份（U 盘 / 密码管理器）。丢失任一个 = 永远无法再升级。

**② 安装并注册 updater 插件**

- Rust 依赖：`apps/desktop/src-tauri/Cargo.toml` 增加 `tauri-plugin-updater`。
- JS 依赖：`apps/web` 增加 `@tauri-apps/plugin-updater`（前端 `import { check } from '@tauri-apps/plugin-updater'`）——注意它是 JS sidecar 包，需 `pnpm add`。`@tauri-apps/api` 同样装在 `apps/web`（非 `apps/desktop`）。
- 注册命令：`apps/desktop/src-tauri/src/main.rs` 的 `tauri::Builder` 增加 `.plugin(tauri_plugin_updater::Builder::new().build())`。
- `apps/desktop/src-tauri/tauri.conf.json` 增加：

```jsonc
{
  "plugins": {
    "updater": {
      "pubkey": "<signer generate 输出的公钥，原样粘贴>",
      "endpoints": ["https://<user>.github.io/milevia-update/latest.json"]
    }
  }
}
```

**③ WebView2 安装模式的取舍（updater 兼容关键点）**

Tauri updater 依赖 NSIS installer 的静默更新时间（`/UPDATE`），而 WebView2 安装方式会叠加在这条链路上，两种模式各有坑：

| 模式 | 首装体验 | 升级体验 | 建议 |
|---|---|---|---|
| `downloadBootstrapper`（现状） | 安装包小，首装联网下 WebView2 | 升级时 NSIS 会**再次触发 WebView2 引导逻辑**，部分环境下会重复下载/校验，甚至因安装器被占用需重试 | 先用现状验证；若升级反复出问题再改 |
| `offlineInstaller` | 安装包大（内含 WebView2 引导） | 升级时不再走 WebView2 引导，更稳定 | 对个人免费分发**更省心**，推荐优先级更高 |

> 建议：本阶段先保持 `downloadBootstrapper` 跑通 MVP；若实测升级阶段出现 WebView2 重复引导或安装器占用问题，就把 `webviewInstallMode.type` 改为 `offlineInstaller` 重新出包。此改动不影响升级逻辑本身。

### 5.2 接入实现（Rust 驱动，主窗不走 IPC 改为命令 + 事件）

因主窗口 webview 完全不经 Tauri IPC、只走 sidecar HTTP/WS，更新插件不依赖前端 JS 绑定，改为 **Rust 驱动 + 前端轮询/监听**，与现有架构一致：

**Rust（`apps/desktop/src-tauri/src/main.rs`）**：
- `setup()` 内 `app.manage(UpdateCheck(...))` 并调 `prime_update_check(&app.handle())`，后台静默 `check()`，结果缓存，错误吞掉不阻断启动。
- 命令 `get_updater_status` → 返回 `{ appVersion, update: {currentVersion, version, notes} | null }`，前端启动后轮询一次。
- 命令 `install_update` → 重新 `check()`，`download_and_install` 期间通过 `updater://progress` 事件回报 `{received, total}`，结束后 `app.restart()`。
- 在 `tauri::Builder` 注册 `.plugin(tauri_plugin_updater::Builder::new().build())`，命令加入 `generate_handler!`。
- `capabilities/default.json` 增加 `updater:default` 权限。

**React（`apps/web`）**：
- `features/updater/UpdateBanner.tsx`：启动后 `invoke('get_updater_status')`；有新版时右上角浮"发现新版本 vX · 立即升级"；点击后 `invoke('install_update')`，`listen('updater://progress')` 驱动进度条。
- `features/updater/AppVersionTag.tsx`：`getVersion()` 显示当前版本，挂在 Dashboard 主界面右上角。
- `App.tsx` 挂 `<UpdateBanner />`（`isDesktop()` 守卫，浏览器环境不渲染）；`DashboardPage.tsx` 的 `.dashboard-actions` 挂 `<AppVersionTag />`。

> 新增前端依赖：`@tauri-apps/api`（`invoke`/`listen`/`getVersion`），装在 `apps/web`（桌面 WebView 复用 web 前端），仅桌面端使用，浏览器环境 no-op。

### 5.3 发版流水线（每次发版重复）

已封装进 `scripts/release.mjs`（命令：`pnpm release`），日常 commit 完全不碰它：

```text
pnpm release 0.2.0 "这次更新的说明"
   ├─ 1. 校验三处版本一致（tauri.conf.json / Cargo.toml / desktop/package.json）
   ├─ 2. 三处一起升到 0.2.0
   ├─ 3. pnpm --filter @milevia/desktop build
   │        -> target/release/bundle/nsis/Milevia_0.2.0_x64-setup.exe
   ├─ 4. tauri signer sign（口令自动读 ~/.tauri/milevia-updater-password.txt，不进命令行）
   └─ 5. 生成 release/latest.json（version / notes / pub_date / platforms，见 §5.4）

发布脚本只做到本地出包+签名+清单，不自动 push/upload。随后手动确认：
   6. git tag v0.2.0 && git push origin v0.2.0
   7. gh release create v0.2.0 "<setup.exe>" "release/latest.json"
   8. 把 release/latest.json 同步到 GitHub Pages 根目录
```

> 变体：`pnpm release:bump 0.2.0`（只同步三处版本号不打包）、`node scripts/release.mjs 0.2.0 --no-build`（不重新出包、签现有安装包）。
> release/ 目录已加入 `.gitignore`（生成物重新生成即可），`latest.json` 需手动同步到 Pages。

### 5.4 latest.json 清单格式

`tauri-plugin-updater` 拉取的就是这个静态 JSON：

```jsonc
{
  "version": "0.2.0",                              // 与 tauri.conf.json version 一致
  "notes": "修复…\n新增…",                            // 更新日志，可含换行
  "pub_date": "2026-08-10T12:00:00Z",              // ISO 8601 UTC，必须带 Z
  "platforms": {
    "windows-x86_64": {
      "signature": "<tauri signer sign 输出的签名>",
      "url": "https://github.com/<user>/<repo>/releases/download/v0.2.0/Milevia_0.2.0_x64-setup.exe"
    }
  }
}
```

- `signature` 由 `tauri signer sign` 输出，应逐字符复制，不可手改。
- `url` 的安装包文件名必须与第 5.3 步 `tauri build` 实际产出一致（注意带 `_x64`）。

### 5.5 latest.json 托管选择

> **更新**：本节原推荐"独立公开仓库 `milevia-update`"（因当时主仓为 private）。主仓现已改为 **public**，实际采用**当前仓库根目录**方案，`latest.json` 直接放主仓根，GitHub Pages 指向 main 分支根目录，URL 为 `https://kegehe.github.io/milevia/latest.json`。详见 `docs/24-GitHub发布流程与Pages配置说明.md`。下方保留原两种方案对比作参考。

`latest.json` 只有一个文件，两种放法：

- **独立公开仓库（原推荐，因当前仓库曾为 private）**：新建公开仓库 `milevia-update` 并开启 GitHub Pages，把 `latest.json` 放仓库根，得到稳定 URL `https://<user>.github.io/milevia-update/latest.json`。大文件 `setup.exe` 仍走 Releases，不占 Pages 配额。
- **当前仓库根目录（✅ 实际采用）**：主仓已 public，`latest.json` 放仓库根，Pages 源为 main 分支根目录，URL `https://kegehe.github.io/milevia/latest.json`。

> Pages 源建议用「分支源 / 根目录静态推送」，避免引入每次发版的 GitHub Actions 构建步骤；`latest.json` 本身就是确定性的小文件，直接静态提交最简单。

### 5.6 若接入 GitHub Actions 自动发版（可选）

`tauri-apps/tauri-action` 打 tag 自动 build + 生成 `latest.json` + 上传 Releases。**安全关键点**：

- updater 签名依赖于私钥，Actions 环境里必须把 `~/.tauri/milevia-updater.key`（及其加密口令）配为 **GitHub Actions Secret**，在工作流中解密后供 `tauri signer sign` 使用。
- 切勿把私钥明文提交进仓库或 workflow 文件；泄露 = 攻击者可签发"合法"升级包。

## 6. 升级流程（用户体验）

```text
启动 → 后台数秒静默检查
  → 无新版：无提示
  → 有新版：主界面右上角浮"发现新版本 v0.2.0 · 立即升级"
     用户点击 → 显示下载进度 → 下载完应用自动退出
     → 旧版卸载/新版安装 → 自动重启 → 新版启动
```

## 7. 范围与非目标

### 7.1 本阶段实现
- `tauri-plugin-updater` 接入，Rust 后台执行升级检查（`get_updater_status` / `install_update`）；
- 主界面"有新版本"横幅 + 下载进度 + 重启（`UpdateBanner`）；
- 主界面版本号显示（`AppVersionTag`）；
- 发版脚本 / GitHub Actions 自动发版（可选）；
- 文档、版本管理流程。

### 7.2 非目标（本阶段不做）
- Windows Authenticode 代码签名（付费消除"未知发布者"提示）；
- 私有云下载加速 / 国内 CDN；
- Linux / macOS 安装包（仅 Windows）。

## 8. 关键要点与风险

1. **签名独立性**：Tauri 升级签名（必须，防篡改，`signer` 密钥）≠ Windows Authenticode（本次不做，防"是否信任"弹窗），两者互不依赖。
2. **私钥安全**：丢失 = 永久失去升级能力；私钥绝不入库；若接 CI，必须用 GitHub Actions Secret 注入（§5.6）。
3. **应用退出时机**：Tauri 替换 exe 前要求应用退出，现有 `ExitRequested` 已停 sidecar，动作顺序天然正确。
4. **WebView2 安装模式**：`downloadBootstrapper` 下升级可能触发 WebView2 重复引导，必要时切 `offlineInstaller`（§5.1 ③）。
5. **版本号三处一致**：package.json / tauri.conf.json / Cargo.toml，遗漏会导致 build 告警或版本错乱（§5.3）。
6. **国内网络**：GitHub 访问偏慢是免费方案主要代价；需要加速时迁 Cloudflare R2，仅改 `endpoints` URL 即可。

## 9. 落地方式

本方案涉及 Rust、React、构建脚本、GitHub 配置多端改动，可按阶段推进：

1. **本地可验证先行**：安装依赖 → 注册插件 → 加 Rust 命令 → 前端版本号 / 横幅骨架 → 本地手动触发升级做通（含签名、`latest.json`、本地 endpoint）。
2. **接入 GitHub 发版**：tag + 上传 Releases + Pages 托管 `latest.json`，走通真实远程升级。
3. **自动化（可选）**：接入 GitHub Actions 自动发版（配好私钥 Secret）。
