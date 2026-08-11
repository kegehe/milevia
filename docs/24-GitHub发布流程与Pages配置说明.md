# GitHub 发布流程与 Pages 配置说明

> 日期：2026-08-10
> 目标：把 Milevia 桌面端「打包 → 发版 → 在线升级」的完整流程写清楚，并解释 GitHub Release / Pages 等在其中的角色与配置方式，供每次发布新版本（0.1.1、0.2.0 …）时照做。

## 1. 最核心的一条思维：升级靠「版本号升高」，不靠「提交」

用户端是否弹「有新版本」，**完全由 `latest.json` 里的 `version` 决定**，和 git 提交无关。

- 你日常改代码 → 提交、push → **不更新 latest.json、不升版本号** → 用户检测不到任何动静。
- 你**想发版** → 升版本号 + 重建 + 重签 + 更新 latest.json → 用户才看到「发现新版本」，点升级才会行动。

所以发布是一个**显式、独立的动作**，和日常提交彻底分开。

```text
日常开发：改代码 → git add → git commit → git push        （不触碰版本/清单）
发布动作：pnpm release 0.1.1 → 重签 → 更新 latest.json → git tag + push → GitHub Release
```

## 2. GitHub 相关概念（为什么需要它们）

### 2.1 仓库（Repository）、提交（commit）、推送（push）、标签（tag）

| 概念 | 是什么 | 在本项目中的作用 |
|---|---|---|
| **仓库 repo** | 存放代码的地方，`kegehe/milevia` | 托管全部源码 + `latest.json` + `.nojekyll` |
| **commit 提交** | 一次代码变更的快照 | 日常开发痕迹 |
| **push 推送** | 把本地 commit 传到 GitHub | 让远端/别人看到你的代码 |
| **tag 标签** | 指向某个 commit 的命名锚点，如 `v0.1.0` | **标识一个可发布的版本**，是 Release 与 tag 之间的关联点 |

> **tag ≠ 每次提交。** 只有要发布版本时才打 tag。

### 2.2 GitHub Release（发布物）

- **是什么**：一个「版本发布会」，可以带标题、说明（release notes）和**附件文件**（装包 exe、msi、升级清单等）。
- **和 tag 的关系**：Release 必须依附一个 tag（可在网页新建时同时创建 tag）。用户访问 `https://github.com/kegehe/milevia/releases` 就能看到并下载附件。
- **附件下载 URL 是稳定的**：`https://github.com/kegehe/milevia/releases/download/v0.1.0/Milevia_0.1.0_x64-setup.exe` —— 安装包靠这个 URL 被 updater 下载。

### 2.3 GitHub Pages（静态站点，托管 latest.json）

- **是什么**：GitHub 免费提供的静态网页托管，URL 形如 `https://<用户名>.github.io/<仓库名>/`。适合放不依赖后端的静态文件。
- **为什么 latest.json 要放这里**：updater 需要一个**固定、不随版本变化**的地址去查询"最新版本"。如果 endpoint 指向 `releases/download/v0.1.0/latest.json`，那永远只查 0.1.0，升级就废了。而 `https://kegehe.github.io/milevia/latest.json` 是固定地址，内容由每次发布更新——updater 永远查这同一个地址，就能发现新版本。
- **配置位置**：仓库 Settings → Pages → Source 选 **Deploy from a branch** → Branch 选 `main`、目录选 `/`(root)。
- **关键坑：必须加 `.nojekyll`**。GitHub Pages 默认跑 Jekyll，但你的 monorepo 根不是 Jekyll 结构，会导致 build 失败(22 秒 → Failed)。在仓库根放一个**空文件 `.nojekyll`**，告诉 Pages「跳过 Jekyll，直接按静态文件发布」，`latest.json` 就会以原样裸露可访问。
- **代价**：因为 Pages 从 `main` 根目录发布，整个 monorepo 文件都会作为站点内容暴露。由于仓库本来就是开源的，这不泄密；updater 只需 GET `latest.json`，站点根目录乱不乱无所谓。

### 2.4 endpoint（updater 查询地址）与 updater 工作链

- `tauri.conf.json` 里的 `plugins.updater.endpoints` 是 **updater 去拉最新版本的地址**（本项目 = Pages 的 `latest.json`）。
- updater 流程：
  ```
  启动 → 请求 endpoint(latest.json) → 读到 version
        → 若 > 本机版本：下一步从 platforms.windows-x86_64.url 下载安装包
        → 用 signature 校验下载的 exe → 安装 → 重启
  ```
- `signature` 来自 `signer sign` 产生的 `.sig` 文件内容——已被 `release.mjs` 写进 `latest.json`，updater 用它保证安装包未被篡改。

## 3. 发布前置（一次性，首次要配好）

### 3.1 私钥与口令（发版签名用）

- 私钥 `~/.tauri/milevia-updater.key` + 口令文件 `~/.tauri/milevia-updater-password.txt`（都在仓库外，**绝不进 git**，务必离线备份）。
- `release.mjs` 自动读取它们签名，**不需要手动传口令**。

### 3.2 GitHub Pages 已开启 + `.nojekyll` 在仓库根

- 打开 `https://github.com/kegehe/milevia/settings/pages` → Source: Deploy from a branch → `main` / `/`(root) → Save。
- 仓库根有 `.nojekyll` 空文件。
- `latest.json` 放在仓库根目录（跟随每次发布更新、提交、push）。

### 3.3 工具链

- Windows 实机 + Node≥22 + pnpm + Go≥1.26 + **MinGW-w64(gcc)** 。
- ⚠️ **必须用 `pnpm release` 发版**，不要直接 `pnpm --filter @milevia/desktop build`——后者不含 CGO 工具链处理，sidecar(go-sqlite3) 会编译失败（gcc 不在 PATH）。`release.mjs` 内置了自动探测并注入 MingW 目录 + `CGO_ENABLED=1`。

## 4. 完整发布流程（每次发版照做）

### 第 0 步：确认要发什么版本、按语义定版本号

- 修小 bug → `0.1.1`；加兼容新功能 → `0.2.0`；破坏性大改 → 提高主版本。
- 版本号严格递增：绝不回退（如从 0.1.0→0.0.1 会让用户检测不到更新）。

### 第 1 步：发版脚本——升版本 + 打包 + 签名 + 生成 latest.json

```powershell
cd D:\projects\Programs\milevia
pnpm release 0.1.1 "修复了…\n新增了…"
```

脚本自动完成（若三个版本文件不一致会先报错阻止）：
1. 将 `tauri.conf.json` / `Cargo.toml` / `apps/desktop/package.json` 三处的版本号同步成 0.1.1；
2. `tauri build`（前端 + sidecar + Rust，产出 NSIS 与 MSI）；
3. `signer sign` 对 NSIS 安装包签名（取私钥 + 口令）；
4. 生成 `release/latest.json`（含新版本号、签名、指向本版本 Release 的下载 URL）；
5. 打印接下来要手动执行的 git 与 `gh` 命令（**不自动 push / 上传**）。

> 变体：`pnpm release:bump 0.1.1` 只同步版本号；`node scripts/release.mjs 0.1.1 --no-build` 不重新出包、签现有安装包。

### 第 2 步：把新的 latest.json 同步到 Pages 根目录并 push

`release.mjs` 生成的清单在 `release/latest.json`，需要**覆盖到仓库根**、提交、推送到 `main`，Pages 才会换成新签名：

```powershell
Copy-Item release\latest.json .\latest.json -Force
git add latest.json apps/desktop/src-tauri/tauri.conf.json
git commit -m "chore: 更新 latest.json 到 v0.1.1"
git push origin main
```

> 若 push 因网络连不上，先设代理：`$env:HTTPS_PROXY="http://127.0.0.1:10808"; $env:HTTP_PROXY="http://127.0.0.1:10808"` 再 push。

### 第 3 步：打 tag 并推送

```powershell
git tag v0.1.1
git push origin v0.1.1
```

### 第 4 步：网页发布 GitHub Release

1. 打开 `https://github.com/kegehe/milevia/releases/new`
2. `Tag`：`v0.1.1`；`Target`：`main`；`Title`：`Milevia v0.1.1`
3. 描述区：粘贴 `release-notes.md`（或点 **Generate release notes**）
4. 拖附件（**务必用本次新出的产物**）：
   - `apps\desktop\src-tauri\target\release\bundle\nsis\Milevia_{version}_x64-setup.exe`（把 `{version}` 换成本次实际版本号）
   - `apps\desktop\src-tauri\target\release\bundle\msi\Milevia_{version}_x64_zh-CN.msi`
   - ⚠️ 别把历史产物（如旧 `en-US.msi`）拖进去
5. 点绿色 **Publish release**

> 注意：**`.sig` 文件不需要上传 Release**，签名内容已写进 latest.json；updater 只读 latest.json 里的 signature。`latest.json` 本身也不用传 Release（它走 Pages）。

### 第 5 步：验证闭环

- `https://kegehe.github.io/milevia/latest.json` → 200，version = 0.1.1
- `https://github.com/kegehe/milevia/releases/download/v0.1.1/Milevia_0.1.1_x64-setup.exe` → 200
- latest.json 里的 signature 与本地生成的 `.sig` 一致（updater 验签才能过）

## 5. 本次发布踩过的坑（务必记住）

1. **直接 `pnpm build` 会因 CGO 失败** → 发布必须走 `pnpm release`（内置 CGO 工具链探测）。
2. **`release.mjs` 的 pnpm/签名/BOM 问题已修好**：Windows 下 pnpm 需 `shell:true`；`signer sign` 用位置参数 + 环境变量传私钥/口令（而非 `-w`）；口令文件要剥离 UTF-8 BOM。
3. **改了配置（如 wix 语言、前端）后必须重签 + 更新 latest.json + 重新传 Pages 根**，否则升级验签失败。
4. **MSI 默认是 `en-US`**，要中文区域需在 `tauri.conf.json` 的 `bundle.windows.wix` 设 `language: "zh-CN"`。
5. **Pages 必须 `.nojekyll`**，否则 monorepo 根 Jekyll build 失败。
6. **安装包附件别拖旧产物**（en-US MSI 等），用本次重新生成的。

## 6. 常见问题

- **Q：为什么不反复建单独 Pages 仓？** 完全可以，但既然主仓开源 + 已开 Pages，`latest.json` 放主仓根最省事。
- **Q：日常提交会不会误发版？** 不会。只要不升版本号、不更新 latest.json、不 push tag、不建 Release，用户端永远安静。
- **Q：换装没 signer 会怎样？** 私钥/口令丢失 = 无法再签名 = 永远无法发布可升级的版本，务必离线备份。
- **Q：用户装包遇到"未知发布者"？** 因为没做 Authenticode 签名（免费方案取舍），点"仍要运行"即可，不影响使用；与 Tauri 升级签名相互独立。
