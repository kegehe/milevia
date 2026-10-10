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
                                                              └─ 是：后台静默预下载 ╪ 校验签名（全程无提示）
                                                                  下载完成 → 右上角浮"新版本 vX 已就绪 → 立即安装"
                                                                  用户点击 → 本地解包 → 退出应用
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
| 升级触发 | **检查自动、下载自动、安装手动** | 启动时后台静默检查；发现新版立刻后台静默预下载（全程无提示），下载并验签完成才提示"已就绪"，用户点"一键安装"即装。安装仍由用户决定，不打断正在跑的任务 |
| Windows 代码签名 | **暂不（免费方案）** | 用户装包会遇"未知发布者"提示，点"仍要运行"即可；与 Tauri 升级签名相互独立，升级安全仍受保护 |

> **更新（2026-09）**：「升级触发」由「检查自动、安装手动」改为「检查自动、下载自动、安装手动」。
> 原先下载发生在用户点击之后，用户得盯着进度条等整包下完；现在检查到新版本就后台静默预下载，
> 备好（已验签）才提示「新版本已就绪」，点击到重启只剩本地解包 + 拉起安装器。落地方式见 §5.2，
> 降级路径见 §6。

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
- 命令 `get_updater_status` → 返回 `{ appVersion, update: {currentVersion, version, notes} | null, download: {phase, received, total, error} }`，前端启动后轮询。
- **静默预下载**：检查发现新版本后（`apply_check_success` 在锁内判定该不该起，锁外 `spawn_background_download` 起任务）用 `Update::download()` 把整包下到内存（`Vec<u8>`，内置 minisign 验签，约 24MB，仅存在于"有待安装更新"期间），成功存进 `pending`，失败自动重试 2 次后置 `download.phase = "failed"`。**不发 `updater://progress` 事件** —— 那是给"用户点出来的安装"驱动进度条的，静默下载发它就会把横幅点亮；进度只走 `get_updater_status`。
- **检查失败不作废下载**（`apply_check_failure`）：一次检查失败（网络抖动、自建源暂时不可达）跟"更新源撤回了版本"是两回事，因此失败时只改状态与错误，保留公告版本与下载状态；真正该丢的（换版本 / 撤回）由下次检查成功时的 `prune_download` 处理。详见 §6。
- 命令 `install_update` → 有备好的包就 `Update::install()` 直装（不再联网）；没有则退回老路径：重新 `check()` + `download_and_install`，期间通过 `updater://progress` 事件回报 `{received, total}`。两条路径结束后都由插件退出/重启应用。
- 检查与下载共用 `build_updater()`：总超时 45 分钟、**读停滞超时 2 分钟**（`configure_client` 里设 `read_timeout`，对付"连接没断但也不再吐数据"的假死 —— 否则假死要等满 45 分钟才失败，而这段时间里用户点安装只会被告知"正在下载"），且挂着 `on_before_exit`（安装前停 sidecar/agent）。**这个钩子随 `check()` 产出的 `Update` 一起传递**，所以预下载拿到的包在稍后安装时同样会先停子进程 —— 少了它，Windows 上安装程序覆写 `milevia-control.exe` 时会弹"无法打开要写入的文件"。
- 下载状态一律按版本号守卫：换版本 / 撤回版本时旧包与旧任务一并丢弃，任务回写结果前校验版本号仍匹配。
- 在 `tauri::Builder` 注册 `.plugin(tauri_plugin_updater::Builder::new().build())`，命令加入 `generate_handler!`。
- `capabilities/default.json` 增加 `updater:default` 权限。

**React（`apps/web`）**：
- `features/updater/update-view.ts`：升级状态 → 界面文案的纯映射（横幅该不该弹、设置页描述与按钮、检查结果提示语），配 `update-view.test.ts`。
- `features/updater/UpdateBanner.tsx`：`invoke('get_updater_status')` 轮询。**后台预下载期间不渲染任何东西**；`download.phase === "ready"` 才浮"新版本 vX 已就绪 · 立即安装"；预下载失败或未开始时退回"发现新版本 vX · 立即升级"（点击后现场下载）。点击后 `invoke('install_update')`，`listen('updater://progress')` 驱动进度条。
- `pages/SettingsPage.tsx`（关于页）与 `tray/TrayPanel.tsx`：跟随 `download.phase` 显示"正在后台下载（n%）/ 已就绪 / 下载失败"，下载中不给点安装。
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

> **更新（2026-09）**：在线升级主源已从 GitHub 迁到**自建服务器** `keyanjia.info:8443` 的 `/updates/` 静态目录（国内可达，GitHub 在部分网络下不可达会导致"无法检查更新"）。`tauri.conf.json` 的 `plugins.updater.endpoints` 现为：
> 1. `https://keyanjia.info:8443/updates/latest.json`（主，国内可达）；
> 2. `https://kegehe.github.io/milevia/latest.json`（GitHub Pages 兜底）。
>
> `scripts/release.mjs` 生成的清单 `url` 默认指向自建服务器，并产出 `release/updates/`（安装包 + `latest.json`）整目录上传；`--deploy` 可一键 scp 到 `/var/www/milevia/dist/updates/`（服务器需先配置 `infrastructure/nginx-keyanjia-8443.conf.example` 中的 `location /updates/`，缺失文件返回 404 而非 SPA index.html）。GitHub Pages / Releases 仍可同步，仅作归档与兜底。已安装旧版（内置纯 GitHub 端点）的机器需手动安装一次新 exe 后，应用内升级才能连通新源。

> **更新（历史）**：本节原推荐"独立公开仓库 `milevia-update`"（因当时主仓为 private）。主仓现已改为 **public**，实际采用**当前仓库根目录**方案，`latest.json` 直接放主仓根，GitHub Pages 指向 main 分支根目录，URL 为 `https://kegehe.github.io/milevia/latest.json`。详见 `docs/24-GitHub发布流程与Pages配置说明.md`。下方保留原两种方案对比作参考。

`latest.json` 只有一个文件，两种放法：

- **独立公开仓库（原推荐，因当前仓库曾为 private）**：新建公开仓库 `milevia-update` 并开启 GitHub Pages，把 `latest.json` 放仓库根，得到稳定 URL `https://<user>.github.io/milevia-update/latest.json`。大文件 `setup.exe` 仍走 Releases，不占 Pages 配额。
- **当前仓库根目录（✅ 历史实际采用，现为自建服务器主源的 GitHub 兜底）**：主仓已 public，`latest.json` 放仓库根，Pages 源为 main 分支根目录，URL `https://kegehe.github.io/milevia/latest.json`。

> Pages 源建议用「分支源 / 根目录静态推送」，避免引入每次发版的 GitHub Actions 构建步骤；`latest.json` 本身就是确定性的小文件，直接静态提交最简单。

### 5.6 若接入 GitHub Actions 自动发版（可选）

`tauri-apps/tauri-action` 打 tag 自动 build + 生成 `latest.json` + 上传 Releases。**安全关键点**：

- updater 签名依赖于私钥，Actions 环境里必须把 `~/.tauri/milevia-updater.key`（及其加密口令）配为 **GitHub Actions Secret**，在工作流中解密后供 `tauri signer sign` 使用。
- 切勿把私钥明文提交进仓库或 workflow 文件；泄露 = 攻击者可签发"合法"升级包。

## 6. 升级流程（用户体验）

```text
启动 → 后台数秒静默检查
  → 无新版：无提示
  → 有新版：立刻后台静默预下载整包（约 24MB，全程无提示，不打断任何操作）
     ├─ 下载 + 验签成功：主界面右上角浮"新版本 v0.2.0 已就绪 · 立即安装"
     │    用户点击 → 本地解包（约 1 秒）→ 应用自动退出
     │    → 旧版卸载/新版安装 → 自动重启 → 新版启动
     └─ 下载失败：自动重试 2 次；仍失败则退回
          "发现新版本 v0.2.0 · 立即升级" → 点击后现场下载安装
          （静默下载失败不会让用户失去升级能力）
```

> 静默下载的进度在**设置页 → 关于 → 当前版本**与**托盘面板**可见（"正在后台下载 vX（42%）"），
> 只有主界面横幅在下载期间保持沉默。下载中这两处都不给点安装，避免"点了没反应"。
>
> **失败要说原因**：`download.error`（Rust 侧已按**下载**语境本地化，见
> `localize_update_error(UpdateStage::Download)`）由三处显示出来 —— 设置页描述、横幅
> （那一行是 nowrap + 省略号，另挂 `title` 兜住全文）、托盘面板副标题。判据是前端的
> `downloadFailureReason()`，只在 `phase === "failed"` 时取值；老版本 Rust 不带这个字段
> 或字段为空时照旧只说"后台下载未完成"。**别把它再放回"没人读"的状态** —— 静默下载
> 失败时界面只剩"点击后重新下载"，用户拿不到任何原因，只能瞎点。
>
> **检查失败不作废下载**：一次检查失败（网络抖动、自建源暂时不可达）与"更新源撤回了版本"
> 是两回事，因此失败时**只改状态与错误，不动公告版本与下载状态** —— 否则会连带丢弃已经
> 下好的整包、并掐掉正在跑的预下载，用户白等一遍 24MB。真正该丢的（换版本 / 撤回）由下次
> 检查成功时的版本守卫（`prune_download`）处理。界面上同理：**已备好（`download.phase ===
> "ready"`）的包不受"最近一次检查失败"影响**，横幅照常给「立即安装」、设置页照常给按钮
> （描述里同时写明那次失败，不假装检查是好的）—— 包已经过签名校验，不联网也能装，
> 藏起来只会白下载一场。
>
> 代价与取舍：整包只存在内存里，**不落盘** —— 缓存文件在下次启动时无法重新验签
> （`verify_signature` 是 `tauri-plugin-updater` 的私有实现），落盘就等于放弃"只信任签名过的包"
> 这条保证。因此用户一直不安装时，每次启动都会重新下一遍。若日后要省这份流量，可引入
> `minisign-verify` 直接依赖自行验签后再做磁盘缓存。

## 7. 范围与非目标

### 7.1 本阶段实现
- `tauri-plugin-updater` 接入，Rust 后台执行升级检查与静默预下载（`get_updater_status` / `install_update`）；
- 「就绪」横幅 + 一键安装 + 重启（`UpdateBanner`，下载期间静默）；预下载失败退回"点击后再下载安装"；
- 主界面版本号显示（`AppVersionTag`）；
- 发版脚本 / GitHub Actions 自动发版（可选）；
- 文档、版本管理流程。

### 7.2 非目标（本阶段不做）
- Windows Authenticode 代码签名（付费消除"未知发布者"提示）；
- 私有云下载加速 / 国内 CDN；
- Linux / macOS 安装包（仅 Windows）；
- 手机端（Android）的"点击直接安装"：需要 `REQUEST_INSTALL_PACKAGES` 权限加一段原生插件才能拉起系统安装器，仍走浏览器下载安装（见 `apps/web/src/features/updater/mobile-update.ts`）。

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
