// 发布版（release）以 Windows GUI 子系统运行：不弹黑色 cmd 控制台窗口。
// dev（debug）保留控制台子系统，便于在终端观察日志。
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::{
    env,
    error::Error,
    io::{BufRead, BufReader, Write},
    net::{TcpStream, ToSocketAddrs},
    path::PathBuf,
    process::{Child, Command, Stdio},
    sync::{mpsc, Arc, Mutex},
    thread,
    time::Duration,
};

use tauri::{
    image::Image,
    tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent},
    webview::NewWindowResponse,
    Emitter, LogicalPosition, LogicalSize, Manager, PhysicalPosition, RunEvent, WebviewUrl,
    WebviewWindow, WebviewWindowBuilder, WindowEvent,
};
use tauri_plugin_updater::{Update, UpdaterExt};
use url::Url;
use uuid::Uuid;

#[cfg(windows)]
use std::os::windows::process::CommandExt;

/// 启动 sidecar 时隐藏其控制台窗口：sidecar 是控制台子系统程序，若不传
/// `CREATE_NO_WINDOW`，Windows 会为它分配一个独立黑色 cmd 窗口（伴随主程序弹出）。
const CREATE_NO_WINDOW: u32 = 0x0800_0000;

const DESKTOP_ORIGIN: &str = "https://tauri.localhost";
const DEV_ORIGIN: &str = "http://127.0.0.1:1420";
const SIDECAR_READY_PREFIX: &str = "MILEVIA_READY=";
// 首次打开大型 SQLite 数据库时，控制服务可能需要较长时间完成迁移/恢复。
const SIDECAR_READY_TIMEOUT_SECS: u64 = 60;

struct RunningSidecar {
    child: Child,
    api_base: String,
    session_token: String,
    local_agent_token: String,
}

struct ManagedSidecar(Mutex<Option<RunningSidecar>>);

struct ManagedAgent(Mutex<Option<Child>>);

/// 记住最近一次托盘点击的鼠标位置（物理像素），供面板内容加载后按实际高度重新贴齐。
struct TrayAnchor(Mutex<Option<PhysicalPosition<f64>>>);

/// 向主窗口上报的可序列化升级信息。仅在有新版时存在。
#[derive(Clone, serde::Serialize)]
#[serde(rename_all = "camelCase")]
struct UpdateInfo {
    /// 本机已安装版本
    current_version: String,
    /// 服务器发布的新版本
    version: String,
    /// 更新日志（可能为空）
    notes: Option<String>,
}

const UPDATE_CHECK_TIMEOUT: Duration = Duration::from_secs(45);
// GitHub release assets can be slow on some networks. Keep a generous total
// limit, while still guaranteeing that a stalled download eventually fails.
const UPDATE_DOWNLOAD_TIMEOUT: Duration = Duration::from_secs(45 * 60);
// 单次读取的停滞上限：这么久没读到新数据就判定这条连接已经死了。
//
// reqwest 的 `read_timeout` 是"每次读操作"的超时（读到数据就重置），正是用来
// 对付"连接没断但也不再吐数据"的假死。只有总超时的话，假死要等满
// UPDATE_DOWNLOAD_TIMEOUT（45 分钟）才失败，而这段时间里用户点安装只会被告知
// "更新正在后台下载" —— 等于被锁在门外。挂死后快速失败 → 重试 → 实在不行退回
// 手动路径，最多几分钟就能重新掌握主动权。
const UPDATE_READ_STALL_TIMEOUT: Duration = Duration::from_secs(120);
// 后台静默预下载的总尝试次数与重试间隔。静默下载失败用户是看不见的，
// 所以先自己重试几轮；全部失败才把 download 相位落成 failed，界面退回
// "发现新版本 → 点击后再下载安装"的老路径。
const BACKGROUND_DOWNLOAD_ATTEMPTS: u32 = 3;
const BACKGROUND_DOWNLOAD_RETRY_DELAY: Duration = Duration::from_secs(30);
// "点击安装时预下载还在跑"的等待上限。取得比 UPDATE_READ_STALL_TIMEOUT 略长：
// 这样"连接假死"一定会在等待期内被判失败（phase → failed），用户拿到的是"现场
// 重下"而不是"再等等"。这种点击在现有界面上几乎点不出来（下载中三处入口都不可点），
// 这里只是兜住竞态，绝不因此触发第二次下载。
const PENDING_DOWNLOAD_WAIT_TIMEOUT: Duration = Duration::from_secs(150);

/// 后台预下载的相位快照。前端据此决定"能不能直接安装"。
/// - idle：没有可装的包（尚未开始 / 更新源撤回了版本）
/// - downloading：正在后台静默下载
/// - ready：已下载并校验签名，点击即可安装
/// - failed：静默下载失败，界面退回"点击后再下载安装"
#[derive(Clone, serde::Serialize)]
#[serde(rename_all = "camelCase")]
struct UpdateDownloadRepr {
    phase: String,
    received: u64,
    total: Option<u64>,
    error: Option<String>,
}

impl UpdateDownloadRepr {
    fn idle() -> Self {
        Self {
            phase: "idle".to_string(),
            received: 0,
            total: None,
            error: None,
        }
    }
}

/// 由三份真实下载状态推导出给前端看的相位。
///
/// 做成纯函数是为了可单测：`tauri_plugin_updater::Update` 的字段私有，测试里
/// 造不出实例，所以这里只收版本号与包大小。
///
/// 三份状态都按**当前公告的版本号**守卫：更新源换了版本、或撤回了版本时，
/// 之前下好的包已经没用了，相位一律回到 idle —— 否则界面会提示"已就绪"，
/// 装上去的却是别的版本。
fn download_repr(
    advertised: Option<&str>,
    pending: Option<(&str, u64)>,
    downloading: Option<(&str, u64, Option<u64>)>,
    failed: Option<(&str, &str)>,
) -> UpdateDownloadRepr {
    if let Some((version, size)) = pending {
        if advertised == Some(version) {
            return UpdateDownloadRepr {
                phase: "ready".to_string(),
                received: size,
                total: Some(size),
                error: None,
            };
        }
    }
    if let Some((version, received, total)) = downloading {
        if advertised == Some(version) {
            return UpdateDownloadRepr {
                phase: "downloading".to_string(),
                received,
                total,
                error: None,
            };
        }
    }
    if let Some((version, error)) = failed {
        if advertised == Some(version) {
            return UpdateDownloadRepr {
                phase: "failed".to_string(),
                received: 0,
                total: None,
                error: Some(error.to_string()),
            };
        }
    }
    UpdateDownloadRepr::idle()
}

#[derive(Clone, serde::Serialize)]
#[serde(rename_all = "camelCase")]
struct UpdateInfoRepr {
    app_version: String,
    status: String,
    update: Option<UpdateInfo>,
    error: Option<String>,
    /// 后台静默预下载的进度（见 `UpdateDownloadRepr`）
    download: UpdateDownloadRepr,
}

/// 正在后台静默下载的进度。
struct DownloadProgress {
    version: String,
    received: u64,
    total: Option<u64>,
}

/// 已下载完成、等待安装的整包。
///
/// `update` 是安装所必需的句柄（`install()` 是它的方法），里面还挂着
/// `on_before_exit`（安装前先停 sidecar/agent）—— 见 `build_updater`。
/// `bytes` 约 24MB，只在"有待安装更新"期间常驻内存，安装或换版本即释放。
struct PendingUpdate {
    version: String,
    update: Update,
    bytes: Vec<u8>,
}

/// 取"等待安装的整包"的结果。
enum PendingSlot {
    /// 包已备好，可以直接安装
    Ready(PendingUpdate),
    /// 预下载还在跑，等到超时也没落定
    StillDownloading,
    /// 没有可用的包（还没开始或已失败），交给"现场检查 + 下载"的老路径
    Unavailable,
}

struct UpdateCheck {
    state: Mutex<UpdateCheckState>,
}

struct UpdateCheckState {
    info: UpdateInfoRepr,
    generation: u64,
    installing: bool,
    /// 已下载并验签通过、等待安装的整包
    pending: Option<PendingUpdate>,
    /// 正在后台下载的版本与进度
    downloading: Option<DownloadProgress>,
    /// 后台下载失败的 (版本, 原因)
    download_error: Option<(String, String)>,
}

impl UpdateCheckState {
    /// 丢弃与当前公告版本不符的下载成果 / 任务 / 错误。
    fn prune_download(&mut self) {
        let advertised = self.info.update.as_ref().map(|update| update.version.clone());
        let matches = |version: &str| advertised.as_deref() == Some(version);
        if self
            .pending
            .as_ref()
            .is_some_and(|pending| !matches(&pending.version))
        {
            self.pending = None;
        }
        if self
            .downloading
            .as_ref()
            .is_some_and(|progress| !matches(&progress.version))
        {
            self.downloading = None;
        }
        if self
            .download_error
            .as_ref()
            .is_some_and(|(version, _)| !matches(version))
        {
            self.download_error = None;
        }
    }

    /// 把 `info.download` 与真实的下载状态对齐。所有会改动下载状态的路径都要
    /// 经过这里，前端的相位才不会和事实脱节。
    fn sync_download(&mut self) {
        self.prune_download();
        let repr = {
            let advertised = self.info.update.as_ref().map(|update| update.version.as_str());
            download_repr(
                advertised,
                self.pending
                    .as_ref()
                    .map(|pending| (pending.version.as_str(), pending.bytes.len() as u64)),
                self.downloading.as_ref().map(|progress| {
                    (
                        progress.version.as_str(),
                        progress.received,
                        progress.total,
                    )
                }),
                self.download_error
                    .as_ref()
                    .map(|(version, error)| (version.as_str(), error.as_str())),
            )
        };
        self.info.download = repr;
    }

    /// 同步下载相位之后的对外快照。
    fn snapshot(&mut self) -> UpdateInfoRepr {
        self.sync_download();
        self.info.clone()
    }

    /// 落地一次「检查成功」：写入公告版本、清掉上次的错误，并判断该不该起预下载。
    ///
    /// `announced` 是 (版本号, 对外信息)，None 表示这次没查到新版本。
    /// 返回 true 表示调用方应当在**锁外**为这个版本起一次静默预下载 —— spawn
    /// 不能在持锁时做，但"该不该起"必须和状态落地在同一个锁里，否则并发检查
    /// 会各起一个下载任务。调用方在这之后还要 `snapshot()` 一次。
    fn apply_check_success(
        &mut self,
        app_version: String,
        announced: Option<(String, UpdateInfo)>,
    ) -> bool {
        self.info.app_version = app_version;
        self.info.status = "complete".to_string();
        self.info.error = None;
        let Some((version, info)) = announced else {
            // 服务器说没有新版本：公告清空，之前下好的包也随之作废（prune）。
            self.info.update = None;
            return false;
        };
        self.info.update = Some(info);
        // 同一版本已有成果或任务、或正在安装时不再起第二个。
        if self.installing
            || self
                .pending
                .as_ref()
                .is_some_and(|pending| pending.version == version)
            || self
                .downloading
                .as_ref()
                .is_some_and(|progress| progress.version == version)
        {
            return false;
        }
        self.download_error = None;
        self.downloading = Some(DownloadProgress {
            version,
            received: 0,
            total: None,
        });
        true
    }

    /// 落地一次「检查失败」。
    ///
    /// **刻意只改状态与错误，不动 `info.update` 与下载状态**：一次检查失败
    /// （网络抖动、自建源暂时不可达）跟"更新源撤回了版本"是两回事。清掉
    /// `info.update` 会连带 prune 掉已经下好的整包、并掐掉正在跑的预下载 ——
    /// 用户白等一遍 24MB，明明包已经到手了。下次检查成功时 `info.update` 会被
    /// 重新写一遍，真正该丢的（换版本 / 撤回）自然会被 prune 掉。
    fn apply_check_failure(&mut self, app_version: String, error: String) {
        self.info.app_version = app_version;
        self.info.status = "failed".to_string();
        self.info.error = Some(error);
    }
}

/// 构造 updater：检查与下载共用一份配置。
///
/// 下载超时放宽到 45 分钟（GitHub 资源在部分网络下很慢）；检查本身另外用
/// `tokio::time::timeout` 单独限时，所以这个宽超时不会拖慢检查。
///
/// `on_before_exit` 会随 `check()` 产出的 `Update` 一起传下去（插件把它存进
/// `Update`），因此**后台静默预下载拿到的 `Update` 在稍后安装时同样会先停子进程**。
/// 这条不能省：Windows 上 updater 启动安装包后直接 `std::process::exit(0)`，
/// `RunEvent::ExitRequested` 不会触发，`run()` 回调里的 stop_agent/stop_sidecar
/// 也就没机会执行；而 milevia-control.exe 只能靠 parent-watch 发现自己成了孤儿，
/// 安装程序覆写它时它往往还在运行 —— 于是必弹"无法打开要写入的文件"。
/// 顺带也让 SQLite 正常收尾（否则升级后首次启动可能卡在"库被锁定"）。
fn build_updater(
    app: &tauri::AppHandle,
    stage: UpdateStage,
) -> Result<tauri_plugin_updater::Updater, String> {
    app.updater_builder()
        .timeout(UPDATE_DOWNLOAD_TIMEOUT)
        // 连接假死（没断、也不再吐数据）只靠总超时要等满 45 分钟。加一条读停滞
        // 超时，让它在两分钟内失败，交给重试 / 手动路径 —— 详见常量注释。
        .configure_client(|builder| builder.read_timeout(UPDATE_READ_STALL_TIMEOUT))
        .on_before_exit({
            let app = app.clone();
            move || {
                stop_agent(&app);
                stop_sidecar(&app);
            }
        })
        .build()
        .map_err(|error| localize_update_error(stage, error))
}

/// 更新链路上出错的是哪一步 —— 决定「<前缀>失败：<原因>」里的前缀。
///
/// 同一句英文原文（`connection reset`）在两步里要说成不同的话：一次**下载**失败
/// 若被写成"检查更新失败"，用户会去查"为什么连不上更新源"，而其实检查早就成功了，
/// 断的是下载那一跳。这个区分不是修辞——`UpdateDownloadRepr.error` 里放的就是
/// 下载语境的错误，它曾经由一份写死"检查更新失败"的文案产出。
#[derive(Clone, Copy)]
enum UpdateStage {
    /// 问更新源有没有新版本。
    Check,
    /// 把新版本整包下下来并验签。
    Download,
    /// 安装（含现场重新下载的老路径）。
    Install,
}

impl UpdateStage {
    fn prefix(self) -> &'static str {
        match self {
            UpdateStage::Check => "检查更新",
            UpdateStage::Download => "下载更新",
            UpdateStage::Install => "安装更新",
        }
    }
}

/// 把 tauri-plugin-updater 的英文错误转成中文。
///
/// 这个插件是本应用里**唯一一处"纯英文直出界面"**：它的错误经
/// `UpdateInfoRepr.error` / `UpdateDownloadRepr.error` 原样渲染在更新横幅上
/// （`UpdateBanner.tsx` 直接把它塞进 <span>），没有任何兜底。
///
/// 未命中任何一类时也必须套一层中文外壳——「更新失败：<原文>」——不能原样返回英文。
/// 原文一律附在括号里：排障与搜索都要靠它。
fn localize_update_error(stage: UpdateStage, raw: impl std::fmt::Display) -> String {
    let text = raw.to_string();
    let lower = text.to_lowercase();
    let action = stage.prefix();
    // 三类判据都取插件文案里稳定出现的词，不做语义猜测；认不出就走最后那条兜底。
    if lower.contains("timed out") || lower.contains("timeout") || lower.contains("dns")
        || lower.contains("connect") || lower.contains("network")
    {
        return format!("{action}失败：网络不可用（{text}）");
    }
    if lower.contains("404") || lower.contains("not found") {
        return format!("{action}失败：更新源上没有这个版本的文件（{text}）");
    }
    if lower.contains("signature") || lower.contains("verify") || lower.contains("minisign") {
        return format!("更新包校验失败：签名与当前应用不匹配（{text}）");
    }
    format!("更新失败：{text}")
}

/// 与更新无关的本地进程（Agent / 控制服务）启动错误的中文外壳。
///
/// 它们同样是纯英文直出（`std::io::Error`、Tauri 的 Command 错误），需要中文外壳；
/// 但**不能借用更新那套前缀**——"找不到 sidecar 可执行文件"被说成"检查更新失败"
/// 是张冠李戴，而"更新源上没有这个版本的文件"这类判据套到本地进程上更是胡话。
/// 所以这里只套壳、不做语义分类。
fn localize_startup_error(what: &str, raw: impl std::fmt::Display) -> String {
    format!("{what}失败：{raw}")
}

async fn perform_update_check(app: &tauri::AppHandle) -> UpdateInfoRepr {
    let app_version = app.package_info().version.to_string();
    let update_check = app.state::<UpdateCheck>();
    let generation = if let Ok(mut state) = update_check.state.lock() {
        state.generation = state.generation.wrapping_add(1);
        state.info.status = "checking".to_string();
        state.info.error = None;
        // 这里**刻意不动** info.update 与下载状态：正在跑的静默预下载靠
        // info.update.version 做版本守卫，中途清掉它会让每次重新检查（比如打开
        // 托盘面板）都把下到一半的包判成过期、从零重下。status 不是 complete 时
        // 前端本来就不渲染 update，留着不会露旧版本。
        state.generation
    } else {
        0
    };
    let result = async {
        let updater = build_updater(app, UpdateStage::Check)?;
        let update = tokio::time::timeout(UPDATE_CHECK_TIMEOUT, updater.check())
            .await
            .map_err(|_| "检查更新超过 45 秒仍未完成，请检查网络后重试".to_string())?
            .map_err(|error| localize_update_error(UpdateStage::Check, error))?;
        Ok::<Option<Update>, String>(update)
    }
    .await;
    // 只有"最新一代"的检查有权写状态：并发检查里落后的那次沿用先到的结果。
    let mut start_download = None;
    let snapshot = {
        let Ok(mut state) = update_check.state.lock() else {
            return UpdateInfoRepr {
                app_version,
                status: "failed".to_string(),
                update: None,
                error: Some("更新状态锁不可用".to_string()),
                download: UpdateDownloadRepr::idle(),
            };
        };
        if state.generation != generation {
            state.snapshot()
        } else {
            match result {
                Ok(update) => {
                    let announced = update.as_ref().map(|update| {
                        (
                            update.version.clone(),
                            UpdateInfo {
                                current_version: update.current_version.clone(),
                                version: update.version.clone(),
                                notes: update.body.clone(),
                            },
                        )
                    });
                    // 发现新版本就立刻后台静默预下载：等用户点"安装"时包已经在手上了。
                    if state.apply_check_success(app_version, announced) {
                        start_download = update;
                    }
                }
                Err(error) => {
                    state.apply_check_failure(app_version, error);
                }
            }
            state.snapshot()
        }
    };
    if let Some(update) = start_download {
        spawn_background_download(app, update);
    }
    snapshot
}

fn prime_update_check(app: &tauri::AppHandle) {
    let app = app.clone();
    tauri::async_runtime::spawn(async move {
        perform_update_check(&app).await;
    });
}

#[tauri::command]
fn get_updater_status(app: tauri::AppHandle) -> UpdateInfoRepr {
    app.state::<UpdateCheck>()
        .state
        .lock()
        .expect("update state lock")
        .info
        .clone()
}

#[tauri::command]
async fn check_for_update_now(app: tauri::AppHandle) -> UpdateInfoRepr {
    perform_update_check(&app).await
}

/// 起一个后台静默预下载任务。
///
/// 去重与"标记为下载中"由 `UpdateCheckState::apply_check_success` 在锁内一并做完，
/// 这里只负责把任务挂到运行时上。
fn spawn_background_download(app: &tauri::AppHandle, update: Update) {
    let app = app.clone();
    tauri::async_runtime::spawn(async move {
        background_download(app, update).await;
    });
}

/// 后台静默预下载整包到内存（约 24MB），成功后放进 `pending` 等待安装。
///
/// **全程不发 `updater://progress` 事件**：那个事件是给"用户点出来的安装"驱动
/// 进度条的，静默下载往上面发就会把右上角横幅点亮，静默也就无从谈起。进度只走
/// `get_updater_status` 的 `download` 字段，由设置页/托盘按需读。
async fn background_download(app: tauri::AppHandle, update: Update) {
    let version = update.version.clone();
    let mut last_error = String::new();
    for attempt in 1..=BACKGROUND_DOWNLOAD_ATTEMPTS {
        if !background_download_wanted(&app, &version) {
            return; // 已被新检查取代：静默放弃，别把过期结果写回去
        }
        // 重试是从 0 重新下的：先把残留进度清掉，别让上一次的数字挂在界面上。
        record_download_progress(&app, &version, 0, None);
        // 插件的进度回调给的是**本次块大小**（`on_chunk(chunk.len(), content_length)`），
        // 不是累计字节，所以自己累加。计数放在循环体内：重试时天然归零，
        // 否则跨次累加会算出超过 100% 的假进度。
        let mut received: u64 = 0;
        let outcome = tokio::time::timeout(
            UPDATE_DOWNLOAD_TIMEOUT,
            update.download(
                |chunk, total| {
                    received += chunk as u64;
                    record_download_progress(&app, &version, received, total);
                },
                || {},
            ),
        )
        .await;
        match outcome {
            Ok(Ok(bytes)) => {
                store_downloaded(&app, &version, update.clone(), bytes);
                return;
            }
            Ok(Err(error)) => last_error = localize_update_error(UpdateStage::Download, error),
            Err(_) => {
                last_error = format!(
                    "更新下载超过 {} 分钟仍未完成",
                    UPDATE_DOWNLOAD_TIMEOUT.as_secs() / 60
                )
            }
        }
        if attempt < BACKGROUND_DOWNLOAD_ATTEMPTS {
            tokio::time::sleep(BACKGROUND_DOWNLOAD_RETRY_DELAY).await;
        }
    }
    // 重试也全败：把相位落成 failed，界面退回"发现新版本 → 点击后再下载"。
    let update_check = app.state::<UpdateCheck>();
    let Ok(mut state) = update_check.state.lock() else {
        return;
    };
    if !is_downloading(&state, &version) {
        return;
    }
    state.downloading = None;
    state.download_error = Some((version, last_error));
    state.sync_download();
}

/// 这次后台下载是否仍然值得继续：版本没被换掉（换了会被 `prune_download` 清空）。
fn background_download_wanted(app: &tauri::AppHandle, version: &str) -> bool {
    let update_check = app.state::<UpdateCheck>();
    let Ok(state) = update_check.state.lock() else {
        return false;
    };
    is_downloading(&state, version)
}

fn is_downloading(state: &UpdateCheckState, version: &str) -> bool {
    state
        .downloading
        .as_ref()
        .is_some_and(|progress| progress.version == version)
}

/// 记录后台预下载进度。`received` 是**累计**字节数 —— 插件的回调给的是单块大小，
/// 调用方负责累加（见 `background_download` / `install_update_inner`）。
fn record_download_progress(
    app: &tauri::AppHandle,
    version: &str,
    received: u64,
    total: Option<u64>,
) {
    let update_check = app.state::<UpdateCheck>();
    let Ok(mut state) = update_check.state.lock() else {
        return;
    };
    let matched = state.downloading.as_mut().is_some_and(|progress| {
        if progress.version != version {
            return false;
        }
        progress.received = received;
        progress.total = total;
        true
    });
    if matched {
        state.sync_download();
    }
}

/// 下载 + 验签成功后落库。版本在这期间被换掉就丢弃，绝不把旧包当新包用。
fn store_downloaded(app: &tauri::AppHandle, version: &str, update: Update, bytes: Vec<u8>) {
    let update_check = app.state::<UpdateCheck>();
    let Ok(mut state) = update_check.state.lock() else {
        return;
    };
    if !is_downloading(&state, version) {
        return;
    }
    state.downloading = None;
    state.download_error = None;
    state.pending = Some(PendingUpdate {
        version: version.to_string(),
        update,
        bytes,
    });
    state.sync_download();
}

/// 取"等待安装的整包"。若预下载还在跑就先等它落定（有界）—— 一次"点击安装"
/// 不该触发第二次下载。
async fn take_pending_update(app: &tauri::AppHandle) -> PendingSlot {
    let deadline = tokio::time::Instant::now() + PENDING_DOWNLOAD_WAIT_TIMEOUT;
    loop {
        let waiting = {
            let update_check = app.state::<UpdateCheck>();
            let Ok(mut state) = update_check.state.lock() else {
                return PendingSlot::Unavailable;
            };
            if let Some(pending) = state.pending.take() {
                state.sync_download();
                return PendingSlot::Ready(pending);
            }
            // 预下载还在跑就再等等；没有在跑的（还没开始 / 已失败）交给老路径现场下载。
            state.downloading.is_some()
        };
        if !waiting {
            return PendingSlot::Unavailable;
        }
        if tokio::time::Instant::now() >= deadline {
            return PendingSlot::StillDownloading;
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

/// 用内存里备好的整包安装（不再联网）。失败时把包放回状态，用户还能再点一次。
fn install_pending(app: &tauri::AppHandle, pending: PendingUpdate) -> Result<(), String> {
    // 安装前先停 sidecar/agent 的钩子挂在这个 `Update` 上（随 check() 一起传下来的）。
    let installed = pending.update.install(&pending.bytes);
    if let Err(error) = installed {
        let update_check = app.state::<UpdateCheck>();
        if let Ok(mut state) = update_check.state.lock() {
            state.pending = Some(pending);
            state.sync_download();
        }
        return Err(localize_update_error(UpdateStage::Install, error));
    }
    // Windows 上 install_inner 内部已 `std::process::exit(0)`，走不到这里；
    // 其它平台兜底重启一次。
    app.restart();
    #[allow(unreachable_code)]
    Ok(())
}

/// 安装新版本，结束后重启应用。期间通过 `updater://progress` 事件回报进度。
///
/// 两条路径：后台静默预下载已备好包时直接装（快，且不再联网）；否则现场
/// 检查 + 下载 + 安装（老路径，也是静默下载失败时的兜底）。
#[derive(serde::Serialize)]
struct InstallUpdateResult {
    installed: bool,
}

#[tauri::command]
async fn install_update(app: tauri::AppHandle) -> Result<InstallUpdateResult, String> {
    {
        let update_check = app.state::<UpdateCheck>();
        let mut state = update_check
            .state
            .lock()
            .map_err(|_| "更新状态锁不可用".to_string())?;
        if state.installing {
            return Err("已有更新正在进行，请等待当前操作结束".to_string());
        }
        state.installing = true;
    }

    let result = match take_pending_update(&app).await {
        PendingSlot::Ready(pending) => {
            install_pending(&app, pending).map(|()| InstallUpdateResult { installed: true })
        }
        PendingSlot::StillDownloading => {
            Err("更新正在后台下载，下载完成后再点安装即可".to_string())
        }
        PendingSlot::Unavailable => install_update_inner(&app).await,
    };
    if let Ok(mut state) = app.state::<UpdateCheck>().state.lock() {
        state.installing = false;
    }
    match result {
        Ok(installed) => Ok(installed),
        Err(error) => {
            let _ = app.emit(
                "updater://progress",
                serde_json::json!({
                    "phase": "failed",
                    "received": 0,
                    "total": null,
                    "error": error,
                }),
            );
            Err(error)
        }
    }
}

/// 老路径：现场检查一次，有新版就下载并安装。
async fn install_update_inner(app: &tauri::AppHandle) -> Result<InstallUpdateResult, String> {
    let updater = build_updater(app, UpdateStage::Install)?;
    let _ = app.emit(
        "updater://progress",
        serde_json::json!({ "phase": "checking", "received": 0, "total": null }),
    );
    let update = tokio::time::timeout(UPDATE_CHECK_TIMEOUT, updater.check())
        .await
        .map_err(|_| "检查更新超过 45 秒仍未完成，请检查网络后重试".to_string())?
        .map_err(|error| localize_update_error(UpdateStage::Check, error))?;
    let Some(update) = update else {
        let update_check = app.state::<UpdateCheck>();
        if let Ok(mut state) = update_check.state.lock() {
            state.generation = state.generation.wrapping_add(1);
            state.info.status = "complete".to_string();
            state.info.update = None;
            state.info.error = None;
            // 更新源撤回了版本：之前下好的包（若有）也要一起作废。
            state.sync_download();
        }
        return Ok(InstallUpdateResult { installed: false });
    };
    let _ = app.emit(
        "updater://progress",
        serde_json::json!({ "phase": "starting", "received": 0, "total": null }),
    );
    // 插件的进度回调给的是**单块大小**而不是累计字节（见 `background_download` 里的
    // 说明），这里同样要自己累加 —— 否则横幅上的进度条会一直停在 0% 附近。
    let mut received: u64 = 0;
    let download = tokio::time::timeout(
        UPDATE_DOWNLOAD_TIMEOUT,
        update.download_and_install(
            |chunk, total| {
                received += chunk as u64;
                let _ = app.emit(
                    "updater://progress",
                    serde_json::json!({ "phase": "downloading", "received": received, "total": total }),
                );
            },
            || {
                let _ = app.emit(
                    "updater://progress",
                    serde_json::json!({ "phase": "installing", "received": 0, "total": null }),
                );
            },
        ),
    )
    .await;
    match download {
        Ok(Ok(())) => {}
        Ok(Err(error)) => return Err(localize_update_error(UpdateStage::Download, error)),
        Err(_) => {
            return Err(format!(
                "更新下载超过 {} 分钟仍未完成，请检查网络后重试",
                UPDATE_DOWNLOAD_TIMEOUT.as_secs() / 60
            ));
        }
    }
    app.restart();
    #[allow(unreachable_code)]
    Ok(InstallUpdateResult { installed: true })
}

const TRAY_PANEL_LABEL: &str = "tray-panel";
/// 面板初始宽度/高度（仅作建窗时的初始值，前端随后按内容自适应覆盖）。
const TRAY_PANEL_WIDTH: f64 = 220.0;
const TRAY_PANEL_HEIGHT: f64 = 224.0;

fn sidecar_binary(_app: &tauri::AppHandle) -> Result<PathBuf, Box<dyn Error>> {
    if let Ok(path) = env::var("MILEVIA_CONTROL_BINARY") {
        return Ok(PathBuf::from(path));
    }
    // Tauri copies resources into target/debug only when it rebuilds the host.
    // In development the Go sidecar can be rebuilt independently, so use the
    // freshly generated binary instead of a stale copied resource.
    #[cfg(debug_assertions)]
    {
        return Ok(PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("..")
            .join("binaries")
            .join("milevia-control.exe"));
    }
    #[cfg(not(debug_assertions))]
    Ok(_app.path().resource_dir()?.join("milevia-control.exe"))
}

fn approval_binary(_app: &tauri::AppHandle) -> Result<PathBuf, Box<dyn Error>> {
    if let Ok(path) = env::var("MILEVIA_APPROVAL_BINARY") {
        return Ok(PathBuf::from(path));
    }
    #[cfg(debug_assertions)]
    {
        return Ok(PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("..")
            .join("binaries")
            .join("milevia-approval.exe"));
    }
    #[cfg(not(debug_assertions))]
    Ok(_app.path().resource_dir()?.join("milevia-approval.exe"))
}

fn agent_binary(_app: &tauri::AppHandle) -> Result<PathBuf, Box<dyn Error>> {
    if let Ok(path) = env::var("MILEVIA_AGENT_BINARY") {
        return Ok(PathBuf::from(path));
    }
    #[cfg(debug_assertions)]
    {
        return Ok(PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("..")
            .join("binaries")
            .join("milevia-agent.exe"));
    }
    #[cfg(not(debug_assertions))]
    Ok(_app.path().resource_dir()?.join("milevia-agent.exe"))
}

fn agent_config_path(app: &tauri::AppHandle) -> Result<PathBuf, Box<dyn Error>> {
    let data_dir = app.path().app_local_data_dir()?;
    let local = data_dir.join("milevia-agent.env");
    if local.exists() {
        return Ok(local);
    }
    #[cfg(debug_assertions)]
    {
        let source = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("..")
            .join("..")
            .join("agent")
            .join(".env.windows");
        if source.exists() {
            return Ok(source);
        }
    }
    let bundled = app.path().resource_dir()?.join("milevia-agent.env");
    Ok(bundled)
}

fn load_agent_env(
    command: &mut Command,
    config_path: &PathBuf,
    endpoint_path: &PathBuf,
    local_agent_token: &str,
    enrollment_token: Option<&str>,
) -> Result<(), Box<dyn Error>> {
    let contents = std::fs::read_to_string(config_path)?;
    let mut required = std::collections::HashSet::new();
    for line in contents.lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let Some((key, value)) = line.split_once('=') else {
            continue;
        };
        let key = key.trim();
        if matches!(
            key,
            "MILEVIA_INSTANCE_ID"
                | "MILEVIA_CLOUD_URL"
                | "MILEVIA_CLOUD_AGENT_TOKEN"
                | "MILEVIA_LOCAL_URL"
        ) {
            command.env(key, value.trim());
            if matches!(
                key,
                "MILEVIA_INSTANCE_ID" | "MILEVIA_CLOUD_URL" | "MILEVIA_CLOUD_AGENT_TOKEN"
            ) && !value.trim().is_empty()
            {
                required.insert(key);
            }
        }
    }
    if !required.contains("MILEVIA_CLOUD_URL")
        && env::var("MILEVIA_CLOUD_URL")
            .map(|value| value.trim().is_empty())
            .unwrap_or(true)
    {
        return Err("Agent 配置缺少 MILEVIA_CLOUD_URL，无法注册到云端".into());
    }
    let credential_path = endpoint_path
        .parent()
        .unwrap_or(endpoint_path)
        .join("agent-credentials.bin");
    let has_existing_credentials = (required.contains("MILEVIA_INSTANCE_ID")
        || env::var("MILEVIA_INSTANCE_ID")
            .map(|value| !value.trim().is_empty())
            .unwrap_or(false))
        && (required.contains("MILEVIA_CLOUD_AGENT_TOKEN")
            || env::var("MILEVIA_CLOUD_AGENT_TOKEN")
                .map(|value| !value.trim().is_empty())
                .unwrap_or(false));
    let has_enrollment_token = enrollment_token
        .map(|value| !value.trim().is_empty())
        .unwrap_or(false)
        || env::var("MILEVIA_AGENT_ENROLLMENT_TOKEN")
            .map(|value| !value.trim().is_empty())
            .unwrap_or(false);
    if !has_existing_credentials && !credential_path.exists() && !has_enrollment_token {
        return Err(
            "Agent 尚未注册。请由管理员仅为本次启动提供 MILEVIA_AGENT_ENROLLMENT_TOKEN，注册成功后该令牌不再需要"
                .into(),
        );
    }
    command.env("MILEVIA_LOCAL_URL_FILE", endpoint_path);
    command.env("MILEVIA_AGENT_CREDENTIAL_FILE", credential_path);
    // 此令牌仅在本次桌面进程生命周期内存在；必须同时传给 sidecar 与 Agent。
    // 它绝不写入配置、日志或安装包资源。
    command.env("AUTO_REMOTE_AGENT_TOKEN", local_agent_token);
    // 宿主自己的 PID：Agent 靠它盯住父进程，父进程一消失就自行退出。
    // 没有这个参数时，宿主被强杀 / 崩溃 / 升级覆写留下的 Agent 会变成孤儿，带着**上一次会话**
    // 的本地令牌继续跑 —— 那个令牌对新 control-server 必然无效，而云端仍会把手机的命令投给它，
    // 结果是一律 401 invalid agent token（见 apps/agent/internal/agent/parent_watch_windows.go）。
    // control-server 一直有这个参数（见 start_sidecar），Agent 这边过去漏了。
    command.arg("--parent-pid").arg(std::process::id().to_string());
    if let Some(token) = enrollment_token.filter(|token| !token.trim().is_empty()) {
        // 仅注入这个新建 Agent 子进程；不得写入磁盘或继承到 sidecar。
        command.env("MILEVIA_AGENT_ENROLLMENT_TOKEN", token.trim());
    }
    Ok(())
}

fn start_agent(
    app: &tauri::AppHandle,
    local_agent_token: &str,
    enrollment_token: Option<&str>,
) -> Result<Option<Child>, Box<dyn Error>> {
    let binary = agent_binary(app)?;
    if !binary.exists() {
        return Err(format!("找不到 Agent 可执行文件：{}", binary.display()).into());
    }
    let config = agent_config_path(app)?;
    if !config.exists() {
        return Err(format!("找不到 Agent 配置文件：{}", config.display()).into());
    }
    let data_dir = app.path().app_local_data_dir()?;
    let endpoint = data_dir.join("milevia.endpoint");
    let agent_log = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(data_dir.join("milevia-agent.log"))?;
    let agent_error_log = agent_log.try_clone()?;
    let mut command = Command::new(binary);
    command
        .stdin(Stdio::null())
        .stdout(Stdio::from(agent_log))
        .stderr(Stdio::from(agent_error_log));
    load_agent_env(
        &mut command,
        &config,
        &endpoint,
        local_agent_token,
        enrollment_token,
    )?;
    #[cfg(windows)]
    command.creation_flags(CREATE_NO_WINDOW);
    match command.spawn() {
        Ok(child) => Ok(Some(child)),
        Err(error) => Err(format!("启动 Agent 失败：{error}").into()),
    }
}

/// 启动一次首次注册。令牌只进入本次 Agent 子进程环境；注册成功后 Agent 会把
/// 机器凭据存为 DPAPI 加密文件，令牌不会落盘，也不会包含在后续启动环境中。
#[tauri::command]
fn enroll_remote_agent(app: tauri::AppHandle, enrollment_token: String) -> Result<(), String> {
    let token = enrollment_token.trim();
    if token.is_empty() || token.len() > 4096 {
        return Err("请输入有效的管理员注册令牌".into());
    }
    stop_agent(&app);
    let local_agent_token = app
        .state::<ManagedSidecar>()
        .0
        .lock()
        .map_err(|_| "无法读取桌面服务状态")?
        .as_ref()
        .map(|sidecar| sidecar.local_agent_token.clone())
        .ok_or("本地控制服务尚未启动")?;
    let agent = start_agent(&app, &local_agent_token, Some(token))
        .map_err(|error| localize_startup_error("启动 Agent", error))?;
    *app.state::<ManagedAgent>()
        .0
        .lock()
        .map_err(|_| "无法保存 Agent 进程状态")? = agent;
    Ok(())
}

/// 在系统文件管理器里打开应用数据目录。
///
/// 排障要看 `milevia-agent.log`，它就落在 `app_local_data_dir()` 下 —— 让用户自己从
/// `%LOCALAPPDATA%` 一路点进去不现实，所以给一个入口。设置页的「数据」一节与远程控制页的
/// 服务卡片都用它（前端两处的 `invoke("open_app_data_directory")` 是一致的）。
///
/// ⚠️ 这个命令**前端一直在调、宿主却从没注册过**（2026-09-17 复查发现）：两处按钮点下去
/// 只会拿到 Tauri 的 "command not found" 回绝，用户看到的是 `setError` 那条红字。
/// 教训：加前端 `invoke("…")` 时，`invoke_handler` 那张表必须一起改 —— 两侧没有共享的类型，
/// 编译期抓不到，只有真机点下去才知道。
#[tauri::command]
fn open_app_data_directory(app: tauri::AppHandle) -> Result<(), String> {
    let dir = app
        .path()
        .app_local_data_dir()
        .map_err(|error| format!("定位应用数据目录失败：{error}"))?;
    // 目录可能还没被建出来（全新安装、Agent 一次都没跑过）。先建再开，否则文件管理器
    // 会弹一句"找不到路径"——那时候用户只会以为按钮坏了。
    std::fs::create_dir_all(&dir).map_err(|error| format!("创建应用数据目录失败：{error}"))?;
    open_directory(&dir)
}

/// 把 URL 交给系统协议处理器打开（http/https → 默认浏览器，mailto: → 邮件客户端）。
///
/// ⚠️ 这个命令**前端一直在调、宿主却从没注册过**（2026-09-29 复查发现）：`@milevia/sdk`
/// 的 `openExternal()` 会 `invoke("open_external")`，失败后自己退回 `window.open`；
/// 而本应用两个窗口都 `.on_new_window(|_, _| NewWindowResponse::Deny)`、`on_navigation`
/// 只放行本地源 —— 于是**桌面端所有外链都打不开**：MCP 的 OAuth 授权页（还会先弹一句
/// "已打开授权页面"然后死等 flow）、CLI 工具登录的授权链接、运行日志与消息里的链接。
/// 与 open_app_data_directory 是同一个教训：`invoke("…")` 必须与 `invoke_handler` 那张表
/// 一起改，两侧没有共享类型，编译期抓不到。
///
/// 只放行 http/https/mailto：这个参数会交给系统的协议处理器，放行 `file:`/`javascript:` 等于把
/// 前端能拿到的任意字符串变成一次"用系统默认程序打开它"。`mailto:` 是同一天补的（此前桌面端
/// 点 AI 回复里的邮箱毫无反应）：它没有代码执行能力、去处就是系统邮件客户端，与 Web/手机端
/// 浏览器的原生行为一致，所以放进来；`tel:` 桌面端没有去处、`ftp:` 在所有现代浏览器里都已失效
/// （Chrome 88 起移除 FTP），都不放。SDK 侧（`openExternal`）有一道同样的判据，这里必须自己
/// 再判一次 —— 命令是可以被直接 invoke 的，不能把安全性寄托在调用方身上。
#[tauri::command]
fn open_external(url: String) -> Result<(), String> {
    let parsed = Url::parse(url.trim()).map_err(|_| "链接格式不正确。".to_string())?;
    match parsed.scheme() {
        "http" | "https" | "mailto" => open_url_in_browser(parsed.as_str()),
        _ => Err("只支持打开 http/https/mailto 链接。".to_string()),
    }
}

/// 把 URL 交给系统协议处理器（http/https 落到默认浏览器，mailto: 落到邮件客户端）。
///
/// Windows 走 `rundll32 url.dll,FileProtocolHandler`（ShellExecute 的官方 shim）：不进
/// shell、URL 走独立 argv，所以 `&`/`|` 这类元字符不会被重解释（与 `explorer` 那条同一
/// 个理由）；它也不像 `cmd /C start` 那样会弹出控制台窗口。同 `open_directory`，
/// **不等退出状态**：这类进程的退出码不代表成功与否。
#[cfg(windows)]
fn open_url_in_browser(url: &str) -> Result<(), String> {
    Command::new("rundll32.exe")
        .args(["url.dll,FileProtocolHandler", url])
        .spawn()
        .map(|_| ())
        .map_err(|error| format!("打开链接失败：{error}"))
}

/// 其它平台目前没有发布产物，明确回一句而不是假装成功（同 `open_directory`）。
#[cfg(not(windows))]
fn open_url_in_browser(_url: &str) -> Result<(), String> {
    Err("当前平台暂不支持打开外部链接。".to_string())
}

/// 用系统文件管理器打开一个目录。
///
/// Windows 走 `explorer`：它**成功时也返回非 0 退出码**，所以这里只看 `spawn` 有没有失败，
/// 不能去等退出状态（等了就会把成功当失败，反过来报一个假错误）。
#[cfg(windows)]
fn open_directory(dir: &std::path::Path) -> Result<(), String> {
    Command::new("explorer")
        .arg(dir)
        .spawn()
        .map(|_| ())
        .map_err(|error| format!("打开目录失败：{error}"))
}

/// 其它平台目前没有发布产物，明确回一句而不是假装成功。
#[cfg(not(windows))]
fn open_directory(dir: &std::path::Path) -> Result<(), String> {
    Err(format!("当前平台暂不支持打开目录：{}", dir.display()))
}

fn stop_agent(app: &tauri::AppHandle) {
    let state = app.state::<ManagedAgent>();
    let Ok(mut guard) = state.0.lock() else {
        return;
    };
    let Some(mut child) = guard.take() else {
        return;
    };
    let _ = child.kill();
    let _ = child.wait();
}

fn log_agent_startup_error(app: &tauri::AppHandle, error: &dyn Error) {
    let Ok(data_dir) = app.path().app_local_data_dir() else {
        eprintln!("[agent] startup configuration failed: {error}");
        return;
    };
    if let Ok(mut log) = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(data_dir.join("milevia-agent.log"))
    {
        let _ = writeln!(log, "[desktop] Agent startup configuration failed: {error}");
    }
    eprintln!("[agent] startup configuration failed: {error}");
}

fn page_origin() -> &'static str {
    if cfg!(dev) {
        DEV_ORIGIN
    } else {
        DESKTOP_ORIGIN
    }
}

fn wait_for_ready(
    stdout: impl std::io::Read + Send + 'static,
    stderr: impl std::io::Read + Send + 'static,
    binary_path: std::path::PathBuf,
    endpoint_path: std::path::PathBuf,
    previous_endpoint_modified: Option<std::time::SystemTime>,
) -> Result<String, Box<dyn Error>> {
    let (sender, receiver) = mpsc::sync_channel(1);
    let endpoint_sender = sender.clone();
    let stderr_lines = Arc::new(Mutex::new(String::new()));

    // Collect stderr into a buffer for diagnostics, while also echoing
    // to the terminal so real-time errors are visible.
    {
        let stderr_lines = Arc::clone(&stderr_lines);
        thread::spawn(move || {
            for line in BufReader::new(stderr).lines() {
                if let Ok(line) = line {
                    eprintln!("[control-server] {line}");
                    let mut buf = stderr_lines.lock().unwrap();
                    if buf.len() < 4096 {
                        buf.push_str(&line);
                        buf.push('\n');
                    }
                }
            }
        });
    }

    let stderr_snapshot = Arc::clone(&stderr_lines);
    let path_snapshot = binary_path.clone();
    thread::spawn(move || {
        let mut sent = false;
        for line in BufReader::new(stdout).lines() {
            match line {
                Ok(line) if line.starts_with(SIDECAR_READY_PREFIX) => {
                    if !sent {
                        let _ = sender.send(Ok(line[SIDECAR_READY_PREFIX.len()..].to_string()));
                        sent = true;
                    }
                }
                Ok(line) => eprintln!("[control-server] {line}"),
                Err(error) => {
                    if !sent {
                        let _ = sender.send(Err(error.to_string()));
                    }
                    return;
                }
            }
        }
        if !sent {
            let _ = sender.send(Err(format!(
                "控制服务在发出就绪信号前退出。\n程序：{}\nstderr:\n{}",
                path_snapshot.display(),
                stderr_snapshot.lock().unwrap()
            )));
        }
    });

    // stdout is the fast path, while the endpoint file is a durable fallback.
    // The health check that follows still authenticates the endpoint with the
    // per-start session token, so a stale file cannot be accepted as ready.
    thread::spawn(move || {
        let deadline = std::time::Instant::now() + Duration::from_secs(SIDECAR_READY_TIMEOUT_SECS);
        while std::time::Instant::now() < deadline {
            let modified = std::fs::metadata(&endpoint_path)
                .and_then(|metadata| metadata.modified())
                .ok();
            if modified.is_some() && modified <= previous_endpoint_modified {
                thread::sleep(Duration::from_millis(100));
                continue;
            }
            if let Ok(contents) = std::fs::read_to_string(&endpoint_path) {
                if let Some(url) = contents
                    .lines()
                    .map(str::trim)
                    .find(|line| line.starts_with("http://") || line.starts_with("https://"))
                {
                    let _ = endpoint_sender.send(Ok(url.to_string()));
                    return;
                }
            }
            thread::sleep(Duration::from_millis(100));
        }
    });

    match receiver.recv_timeout(Duration::from_secs(SIDECAR_READY_TIMEOUT_SECS)) {
        Ok(Ok(url)) => Ok(url),
        Ok(Err(error)) => Err(error.into()),
        Err(mpsc::RecvTimeoutError::Timeout) => Err(format!(
            "控制服务启动超时（{} 秒内未就绪）。\n程序：{}\nstderr:\n{}",
            SIDECAR_READY_TIMEOUT_SECS,
            binary_path.display(),
            stderr_lines.lock().unwrap()
        )
        .into()),
        Err(mpsc::RecvTimeoutError::Disconnected) => Err(format!(
            "控制服务管道意外关闭。\n程序：{}\nstderr:\n{}",
            binary_path.display(),
            stderr_lines.lock().unwrap()
        )
        .into()),
    }
}

fn wait_for_health(sidecar: &RunningSidecar) -> Result<(), Box<dyn Error>> {
    let url = Url::parse(&sidecar.api_base)?;
    let host = url.host_str().ok_or("控制服务地址缺少主机名")?;
    let port = url.port_or_known_default().ok_or("控制服务地址缺少端口")?;
    let address = format!("{host}:{port}");
    let socket_address = address
        .to_socket_addrs()?
        .next()
        .ok_or("无法解析控制服务地址")?;
    let deadline = std::time::Instant::now() + Duration::from_secs(12);
    loop {
        if let Ok(mut stream) =
            TcpStream::connect_timeout(&socket_address, Duration::from_millis(250))
        {
            let request = format!(
                "GET /api/health HTTP/1.1\r\nHost: {host}\r\nX-Milevia-Session: {}\r\nConnection: close\r\n\r\n",
                sidecar.session_token
            );
            if stream.write_all(request.as_bytes()).is_ok() {
                let mut status = String::new();
                if BufReader::new(stream).read_line(&mut status).is_ok()
                    && status.starts_with("HTTP/")
                    && status.contains(" 200 ")
                {
                    return Ok(());
                }
            }
        }
        if std::time::Instant::now() >= deadline {
            return Err(format!(
                "控制服务健康检查超时（{}: 12 秒内无 200 响应）\n请确认控制服务可正常访问。",
                sidecar.api_base
            )
            .into());
        }
        thread::sleep(Duration::from_millis(100));
    }
}

/// Directly launched debug binaries do not pass through `scripts/dev.mjs`.
/// Load the local Agent env as a fallback so pairing still has Cloud Control
/// settings, while preserving explicitly configured process variables.
fn apply_debug_remote_env(command: &mut Command) {
    #[cfg(debug_assertions)]
    {
        let env_path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("..")
            .join("..")
            .join("agent")
            .join(".env.windows");
        let Ok(contents) = std::fs::read_to_string(env_path) else {
            return;
        };
        let mut values = std::collections::HashMap::new();
        for line in contents.lines() {
            let line = line.trim();
            if line.is_empty() || line.starts_with('#') {
                continue;
            }
            let Some((key, value)) = line.split_once('=') else {
                continue;
            };
            values.insert(key.trim(), value.trim());
        }
        for (target, source) in [
            ("AUTO_REMOTE_CLOUD_URL", "MILEVIA_CLOUD_URL"),
            ("AUTO_REMOTE_CLOUD_TOKEN", "MILEVIA_CLOUD_AGENT_TOKEN"),
            ("AUTO_REMOTE_INSTANCE_ID", "MILEVIA_INSTANCE_ID"),
        ] {
            if env::var(target)
                .map(|value| !value.trim().is_empty())
                .unwrap_or(false)
            {
                continue;
            }
            if let Some(value) = values.get(source).filter(|value| !value.is_empty()) {
                command.env(target, value);
            }
        }
    }
}

fn start_sidecar(
    app: &tauri::AppHandle,
    local_agent_token: &str,
) -> Result<RunningSidecar, Box<dyn Error>> {
    let data_dir = app.path().app_local_data_dir()?;
    std::fs::create_dir_all(&data_dir)?;
    let data_dir_arg = data_dir.to_string_lossy().to_string();

    let sidecar_path = sidecar_binary(app)?;
    let approval_path = approval_binary(app)?;

    // ── 预检：二进制文件是否存在 ──
    if !sidecar_path.exists() {
        return Err(format!(
            "找不到 Milevia 控制服务程序。\n预期位置：{}\n请确认 Milevia 已正确安装，或运行 pnpm --filter @milevia/desktop dev 重新编译。",
            sidecar_path.display()
        )
        .into());
    }
    if !approval_path.exists() {
        return Err(format!(
            "找不到 Milevia 审批辅助程序。\n预期位置：{}",
            approval_path.display()
        )
        .into());
    }

    let approval_binary_arg = approval_path.to_string_lossy().to_string();
    let session_token = Uuid::new_v4().simple().to_string();
    let parent_pid = std::process::id();
    let mut command = Command::new(&sidecar_path);
    command
        .args([
            "--mode",
            "desktop-api",
            "--addr",
            "127.0.0.1:0",
            "--data-dir",
            &data_dir_arg,
            "--session-token",
            &session_token,
            "--allowed-origin",
            page_origin(),
            "--approval-hook",
            &approval_binary_arg,
            "--native-approval-hook",
            "--parent-pid",
            &parent_pid.to_string(),
        ])
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        // 隐藏 sidecar 控制台窗口（见 CREATE_NO_WINDOW）。
        .creation_flags(CREATE_NO_WINDOW);
    // 与 Agent 共享的进程内随机令牌；桌面模式不允许 loopback 无令牌回退。
    command.env("AUTO_REMOTE_AGENT_TOKEN", local_agent_token);
    // 首次注册令牌只属于 Agent；即使桌面宿主由带该环境变量的管理员命令启动，
    // 也不能让 sidecar 继承它。
    command.env_remove("MILEVIA_AGENT_ENROLLMENT_TOKEN");
    apply_debug_remote_env(&mut command);
    let endpoint_path = data_dir.join("milevia.endpoint");
    let previous_endpoint_modified = std::fs::metadata(&endpoint_path)
        .and_then(|metadata| metadata.modified())
        .ok();
    let mut child = command.spawn()
        .map_err(|e| {
            format!(
                "无法启动控制服务。\n程序：{}\n原因：{}\n请确认程序未被占用，且 CGO/SQLite 编译工具链正常。",
                sidecar_path.display(),
                e
            )
        })?;
    let stdout = child.stdout.take().ok_or("无法获取控制服务 stdout 管道")?;
    let stderr = child.stderr.take().ok_or("无法获取控制服务 stderr 管道")?;
    match wait_for_ready(
        stdout,
        stderr,
        sidecar_path.clone(),
        endpoint_path,
        previous_endpoint_modified,
    ) {
        Ok(api_base) => {
            let sidecar = RunningSidecar {
                child,
                api_base,
                session_token,
                local_agent_token: local_agent_token.to_owned(),
            };
            if let Err(error) = wait_for_health(&sidecar) {
                let mut sidecar = sidecar;
                let _ = sidecar.child.kill();
                let _ = sidecar.child.wait();
                return Err(error);
            }
            Ok(sidecar)
        }
        Err(error) => {
            let _ = child.kill();
            let _ = child.wait();
            Err(error)
        }
    }
}

fn request_graceful_shutdown(sidecar: &RunningSidecar) {
    let Ok(url) = Url::parse(&sidecar.api_base) else {
        return;
    };
    let Some(host) = url.host_str() else { return };
    let Some(port) = url.port_or_known_default() else {
        return;
    };
    let Ok(address) = format!("{host}:{port}").to_socket_addrs() else {
        return;
    };
    let Some(address) = address.into_iter().next() else {
        return;
    };
    let Ok(mut stream) = TcpStream::connect_timeout(&address, Duration::from_secs(1)) else {
        return;
    };
    let _ = stream.set_read_timeout(Some(Duration::from_secs(2)));
    let request = format!(
        "POST /api/internal/shutdown HTTP/1.1\r\nHost: {host}\r\nX-Milevia-Session: {}\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
        sidecar.session_token
    );
    let _ = stream.write_all(request.as_bytes());
}

fn stop_sidecar(app: &tauri::AppHandle) {
    let state = app.state::<ManagedSidecar>();
    let Ok(mut sidecar) = state.0.lock() else {
        return;
    };
    let Some(mut sidecar) = sidecar.take() else {
        return;
    };
    stop_running_sidecar(&mut sidecar);
}

fn stop_running_sidecar(sidecar: &mut RunningSidecar) {
    request_graceful_shutdown(sidecar);
    for _ in 0..50 {
        if matches!(sidecar.child.try_wait(), Ok(Some(_))) {
            return;
        }
        thread::sleep(Duration::from_millis(100));
    }
    let _ = sidecar.child.kill();
    let _ = sidecar.child.wait();
}

/// 主窗口与托盘面板窗口共用的导航白名单：只放行本地前端源。
fn navigation_allowed(url: &Url) -> bool {
    if cfg!(dev) {
        return url.scheme() == "http"
            && url.host_str() == Some("127.0.0.1")
            && url.port_or_known_default() == Some(1420);
    }
    url.scheme() == "tauri"
        || (url.scheme() == "https" && url.host_str() == Some("tauri.localhost"))
}

/// 生成注入前端 `window.__MILEVIA_DESKTOP_RUNTIME__` 的初始化脚本。
/// 主窗口用 `mode:"app"`，托盘面板用 `mode:"tray"`（前端据此分流渲染）。
/// 托盘面板额外注入 `window.__MILEVIA_TRAY_ACTIONS__`，经 `__TAURI_INTERNALS__.invoke`
/// 调用 Rust command（免去在 web 包引入 @tauri-apps/api 依赖）。
fn runtime_init_script(api_base: &str, session_token: &str, mode: &str) -> String {
    let runtime_config = serde_json::json!({
        "apiBase": api_base,
        "wsBase": api_base.replacen("http", "ws", 1),
        "sessionToken": session_token,
        "mode": mode,
    });
    let runtime_define = format!(
        "Object.defineProperty(window, '__MILEVIA_DESKTOP_RUNTIME__', {{ value: {}, writable: false, configurable: false }});",
        serde_json::to_string(&runtime_config).expect("runtime config serializes")
    );
    let mut script = runtime_define;
    if mode == "tray" {
        script.push_str(
            r#"Object.defineProperty(window, '__MILEVIA_TRAY_ACTIONS__', { value: {
  showMain: () => window.__TAURI_INTERNALS__.invoke('show_main_window'),
  close: () => window.__TAURI_INTERNALS__.invoke('close_panel'),
  quit: () => window.__TAURI_INTERNALS__.invoke('quit_app'),
  restart: () => window.__TAURI_INTERNALS__.invoke('restart_app'),
  resize: (w, h) => window.__TAURI_INTERNALS__.invoke('set_panel_size', { width: w, height: h }),
  resizeKeep: (w, h) => window.__TAURI_INTERNALS__.invoke('set_panel_size', { width: w, height: h, relocate: false }),
  resizeExpand: (w, h, shift) => window.__TAURI_INTERNALS__.invoke('set_panel_size', { width: w, height: h, relocate: false, shiftLeft: shift }),
  resizeUp: (w, h, up) => window.__TAURI_INTERNALS__.invoke('set_panel_size', { width: w, height: h, relocate: false, shiftUp: up }),
  navigateMain: (path) => window.__TAURI_INTERNALS__.invoke('navigate_main', { path }),
  getUpdaterStatus: () => window.__TAURI_INTERNALS__.invoke('get_updater_status'),
  checkForUpdate: () => window.__TAURI_INTERNALS__.invoke('check_for_update_now'),
  installUpdate: () => window.__TAURI_INTERNALS__.invoke('install_update'),
}, writable: false, configurable: false });"#,
        );
    }
    script
}

fn create_main_window(app: &tauri::AppHandle, sidecar: &RunningSidecar) -> tauri::Result<()> {
    let initialization_script =
        runtime_init_script(&sidecar.api_base, &sidecar.session_token, "app");
    let window = WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
        .title("Milevia")
        .inner_size(1440.0, 920.0)
        .min_inner_size(1080.0, 720.0)
        // 必须禁用 Tauri 原生文件拖放处理器：它会 RevokeDragDrop 掉 WebView2 子窗口自带的
        // OLE IDropTarget 并注册一个只转发文件拖放的替代者，导致窗口内 HTML5 拖拽
        // （任务队列/看板卡片重排、常用提示词/命令排序）在 Windows 上收不到 dragstart/
        // dragover/drop。应用本身不依赖 Tauri 的文件拖放事件，禁用后 WebView2 原生
        // 接管拖放，HTML5 拖拽恢复正常。参考 tauri 2.11 `disable_drag_drop_handler` 文档。
        .disable_drag_drop_handler()
        // Windows 上统一走 https://tauri.localhost 标准 scheme：WebView2 会把 http://
        // 升级为 https://，而 wry 的资源拦截只注册 http，升级后无人拦截导致
        // ERR_CONNECTION_REFUSED → 空白黑屏。打开 https 让导航/拦截/过滤三者一致。
        .use_https_scheme(true)
        .initialization_script(&initialization_script)
        .on_navigation(navigation_allowed)
        .on_new_window(|_, _| NewWindowResponse::Deny)
        .build()?;
    let w = window.clone();
    window.on_window_event(move |event| {
        if let WindowEvent::CloseRequested { api, .. } = event {
            api.prevent_close();
            let _ = w.hide();
        }
    });
    Ok(())
}

/// 克隆侧边进程的注入所需字段（api_base / session_token），避免持锁建窗。
fn sidecar_snapshot(app: &tauri::AppHandle) -> Option<(String, String)> {
    let state = app.state::<ManagedSidecar>();
    let guard = state.0.lock().ok()?;
    guard
        .as_ref()
        .map(|s| (s.api_base.clone(), s.session_token.clone()))
}

/// 惰性创建并返回托盘品牌面板窗口（已存在则复用）。
fn restore_or_create_panel(
    app: &tauri::AppHandle,
    api_base: &str,
    session_token: &str,
) -> tauri::Result<WebviewWindow> {
    if let Some(window) = app.get_webview_window(TRAY_PANEL_LABEL) {
        return Ok(window);
    }
    let initialization_script = runtime_init_script(api_base, session_token, "tray");
    let window =
        WebviewWindowBuilder::new(app, TRAY_PANEL_LABEL, WebviewUrl::App("index.html".into()))
            .title("Milevia")
            .inner_size(TRAY_PANEL_WIDTH, TRAY_PANEL_HEIGHT)
            .decorations(false)
            .transparent(true)
            .always_on_top(true)
            .skip_taskbar(true)
            .resizable(false)
            .shadow(false)
            // 同上：统一 https scheme，避免 WebView2 将 http 升级后 wry 拦截失效导致黑屏。
            .use_https_scheme(true)
            .visible(false) // 先隐藏，待定位后再 show，避免在错误坐标闪一下
            .initialization_script(&initialization_script)
            .on_navigation(navigation_allowed)
            .on_new_window(|_, _| NewWindowResponse::Deny)
            .build()?;
    let w = window.clone();
    window.on_window_event(move |event| {
        match event {
            WindowEvent::Focused(false) => {
                // 失焦自动隐藏
                let _ = w.hide();
            }
            WindowEvent::CloseRequested { api, .. } => {
                api.prevent_close();
                let _ = w.hide();
            }
            _ => {}
        }
    });
    Ok(window)
}

/// 将品牌面板定位到鼠标右键点：面板**左下角**贴住点击点（向左上展开），
/// 仅在超出显示器边界时平移收进屏幕。
/// - `size_logical: Some((w,h))` 使用调用方提供的逻辑尺寸（如 `set_panel_size`
///   传入刚请求的尺寸，避免 `inner_size()` 读到 set_size 前的旧值）。
/// - `None` 时读面板当前实际 inner 尺寸。
fn position_panel_at_cursor(
    panel: &WebviewWindow,
    click_physical: &PhysicalPosition<f64>,
    size_logical: Option<(f64, f64)>,
) {
    let Ok(scale) = panel.scale_factor() else {
        return;
    };
    let (pw, ph) = match size_logical {
        Some((w, h)) => (w, h),
        None => {
            let Ok(inner) = panel.inner_size() else {
                return;
            };
            (inner.width as f64 / scale, inner.height as f64 / scale)
        }
    };
    // 点击点 → 逻辑像素
    let mx = click_physical.x / scale;
    let my = click_physical.y / scale;

    // 面板期望：左下角贴住点击点 → 顶缘 = 鼠标y - 高度，左缘 = 鼠标x
    let mut x = mx;
    let mut y = my - ph;

    // 收进当前显示器完整范围（含任务栏），避免超出上/右界被裁
    if let Ok(Some(monitor)) = panel.current_monitor() {
        let pos = monitor.position();
        let size = monitor.size();
        let wl = pos.x as f64 / scale;
        let wt = pos.y as f64 / scale;
        let wr = (pos.x + size.width as i32) as f64 / scale;
        let wb = (pos.y + size.height as i32) as f64 / scale;
        if x + pw > wr {
            x = wr - pw;
        }
        if y + ph > wb {
            y = wb - ph;
        }
        if x < wl {
            x = wl;
        }
        if y < wt {
            y = wt;
        }
    }
    let _ = panel.set_position(LogicalPosition::new(x, y));
    // 记录锚点，供内容自适应 resize 后重新贴齐
    *panel.app_handle().state::<TrayAnchor>().0.lock().unwrap() = Some(*click_physical);
}

/// 右键点击托盘图标时弹出品牌面板。
fn open_tray_panel(app: &tauri::AppHandle, click_position: &PhysicalPosition<f64>) {
    let Some((api_base, session_token)) = sidecar_snapshot(app) else {
        return;
    };
    let panel = match restore_or_create_panel(app, &api_base, &session_token) {
        Ok(panel) => panel,
        Err(error) => {
            eprintln!("[tray-panel] 创建品牌面板失败: {error}");
            return;
        }
    };
    position_panel_at_cursor(&panel, click_position, None);
    let _ = panel.set_always_on_top(true);
    let _ = panel.show();
    let _ = panel.set_focus();
    // 通知面板前端"本次已打开"：品牌面板窗口常驻复用（关闭=隐藏、不会重挂载），
    // 需靠这个事件让前端在每次打开时自动重跑一次更新检查（前端驱动 check）。
    let _ = app.emit_to(TRAY_PANEL_LABEL, "tray://panel-opened", ());
}

/// 显示并聚焦主窗口（先隐藏托盘面板，避免焦点竞争导致面板误关）。
#[tauri::command]
fn show_main_window(app: tauri::AppHandle) {
    if let Some(panel) = app.get_webview_window(TRAY_PANEL_LABEL) {
        let _ = panel.hide();
    }
    if let Some(window) = app.get_webview_window("main") {
        let _ = window.show();
        let _ = window.unminimize();
        let _ = window.set_focus();
    }
}

/// 隐藏品牌托盘面板。
#[tauri::command]
fn close_panel(app: tauri::AppHandle) {
    if let Some(panel) = app.get_webview_window(TRAY_PANEL_LABEL) {
        let _ = panel.hide();
    }
}

/// 左键点击托盘图标：切换主窗口显示/隐藏。
/// 主窗口可见且未最小化 → 隐藏；隐藏或最小化 → 显示并聚焦。
fn toggle_main_window(app: &tauri::AppHandle) {
    // 先隐藏托盘面板，避免焦点竞争导致面板误关
    if let Some(panel) = app.get_webview_window(TRAY_PANEL_LABEL) {
        let _ = panel.hide();
    }
    if let Some(window) = app.get_webview_window("main") {
        let visible = window.is_visible().unwrap_or(false);
        let minimized = window.is_minimized().unwrap_or(false);
        if visible && !minimized {
            let _ = window.hide();
        } else {
            let _ = window.unminimize();
            let _ = window.show();
            let _ = window.set_focus();
        }
    }
}

/// 让主窗口导航到指定路径（相对路径，基于主窗口自身 origin 解析）。
/// 先显示并聚焦主窗口，再让前端以客户端路由跳转（避免整页刷新丢状态）。
#[tauri::command]
fn navigate_main(app: tauri::AppHandle, path: String) {
    if let Some(window) = app.get_webview_window("main") {
        let _ = window.show();
        let _ = window.unminimize();
        let _ = window.set_focus();
        let _ = window.eval(&format!(
            "window.__mileviaNavigate && window.__mileviaNavigate({path:?})"
        ));
    }
}

/// 在 Windows 系统右下角弹一条系统通知。前端只在用户开启“Windows 弹窗通知”
/// 且收到任务完成类通知时调用。
///
/// - 内容刻意不包含具体项目/任务名，统一显示“有任务完成”，避免在通知中心泄露细节。
/// - `Duration::Short` 让系统按短时展示（会在一段时间后自动消失）。
/// - 用户点击弹窗时（前台激活）调用回调：`on_activated` 闭包直接捕获本次 `path`，
///   因此**每条弹窗点击都只跳到它对应的那条任务的项目**（无需中心的共享状态，
///   也不存在多条弹窗互相覆盖导航目标的问题）。跳转复用托盘“跳转主窗口”同款实现。
#[tauri::command]
fn show_system_notification(app: tauri::AppHandle, path: String) {
    let app_id = app.config().identifier.clone();
    let callback_app = app.clone();
    // 把本条弹窗自己的导航目标放进闭包，随 Toast 一起保存（每条调用独享一份）。
    let target = path.clone();
    let result = tauri_winrt_notification::Toast::new(app_id.as_str())
        .title("Milevia")
        .text1("有任务完成")
        .duration(tauri_winrt_notification::Duration::Short)
        .on_activated(move |_args| {
            // WebView 必须在主线程操作，跳到该条弹窗对应的项目。
            let app = callback_app.clone();
            let target = target.clone();
            let _ = callback_app.run_on_main_thread(move || {
                if let Some(window) = app.get_webview_window("main") {
                    let _ = window.show();
                    let _ = window.unminimize();
                    let _ = window.set_focus();
                    let _ = window.eval(&format!(
                        "window.__mileviaNavigate && window.__mileviaNavigate({target:?})"
                    ));
                }
            });
            Ok(())
        })
        .show();
    if let Err(error) = result {
        eprintln!("[notification] 显示系统通知失败: {error}");
    }
}

/// 真正退出应用（触发 ExitRequested → 优雅停掉 sidecar）。
#[tauri::command]
fn quit_app(app: tauri::AppHandle) {
    app.exit(0);
}

/// 重启应用：先触发 ExitRequested（优雅停掉 sidecar）再拉起新实例。
/// 与 `quit_app` 同一条退出路径（`AppHandle::exit` 也是走 `request_exit`），配合
/// single-instance 插件避免双开。对照：更新安装那条路是**另一套**——安装器在 Windows 上
/// 直接 `std::process::exit(0)`，ExitRequested 根本没机会跑，所以它靠 `build_updater`
/// 的 `on_before_exit` 停子进程。
///
/// ⚠️ 这里**必须**用 `request_restart()` 而不是 `restart()`。`AppHandle::restart()` 分区
/// 判断调用线程（tauri 的 app.rs）：主线程上只做 `cleanup_before_exit()` + 立即
/// `process::restart()`——**根本不发 `RunEvent::ExitRequested`**，于是 `run()` 回调里的
/// `stop_agent` / `stop_sidecar` 一次都不会执行。而本命令是**同步** command：wry 的
/// WebMessageReceived 回调是内联调用 ipc handler 的（WebView2 事件全在 UI 线程上抛），
/// 所以它恰好落在主线程那条分支上 —— 注释写着"优雅停掉 sidecar"，实际却是硬重启：
/// 旧 control-server 还开着同一个 data-dir 的 SQLite，新实例的 sidecar 就上去开库了
/// （正是 build_updater 注释里担心的"库被锁定"）。
///
/// `request_restart()` 走请求退出那条路：事件循环先派发 ExitRequested（我们的回调在这里
/// 停子进程），随后 Exit 事件里发现 restart_on_exit 已置位再拉起新实例。注意**不要**在
/// ExitRequested 分支里调 `api.prevent_exit()`，那会把这个流程永久挂住。
#[tauri::command]
fn restart_app(app: tauri::AppHandle) {
    app.request_restart();
}

/// 让面板窗口按内容自适应尺寸（前端测量后调用，消除右侧留白）。
/// - `relocate`（默认 true）为 true 时，尺寸变化后重新按最近一次点击点把面板左下角
///   贴回鼠标位置（内容自适应时保持贴齐）。
/// - `relocate` 为 false 且 `shift_left` 为 Some>0 时：只改尺寸并整体向左平移窗口
///   `shift_left`（逻辑像素），用于"二级子菜单自动判定：右侧放不下就放左侧"——
///   向左让出一段空间给子菜单。一级随整体一次到位、不抖动。
/// - `relocate` 为 false 且 `shift_left` 为 None/0 时：只改尺寸、不重新定位
///   （右侧子菜单弹出场景，一级保持原位）。
#[tauri::command]
fn set_panel_size(
    app: tauri::AppHandle,
    width: f64,
    height: f64,
    relocate: Option<bool>,
    shift_left: Option<f64>,
    shift_up: Option<f64>,
) {
    if let Some(panel) = app.get_webview_window(TRAY_PANEL_LABEL) {
        // 逻辑像素尺寸；加一点安全余量避免贴边裁切
        let new_w = width + 2.0;
        let new_h = height + 2.0;
        let _ = panel.set_size(LogicalSize::new(new_w, new_h));
        if relocate.unwrap_or(true) {
            // 内容自适应后高度变化会破坏“左下角贴鼠标”，这里用刚请求的尺寸重贴一次
            // （避免读 inner_size() 时 set_size 尚未生效拿到旧值）
            let anchor = app.state::<TrayAnchor>().0.lock().unwrap().clone();
            if let Some(anchor) = anchor {
                position_panel_at_cursor(&panel, &anchor, Some((new_w, new_h)));
            }
        } else {
            let Ok(pos) = panel.outer_position() else { return };
            let Ok(scale) = panel.scale_factor() else { return };
            let mut lx = pos.x as f64 / scale;
            let mut ly = pos.y as f64 / scale;
            if let Some(shift) = shift_left {
                if shift > 0.0 {
                    // 向左平移整窗：右侧放不下、改为左侧时，把窗口整体左移让出空间
                    lx -= shift;
                }
            }
            if let Some(up) = shift_up {
                if up > 0.0 {
                    // 向上平移整窗（底部保持锚点）：hover 子菜单需要更高时，让额外高度
                    // 向上生长，避免多出的高度向下越出屏幕而被裁断。
                    ly -= up;
                }
            }
            let _ = panel.set_position(LogicalPosition::new(lx, ly));
        }
    }
}

fn configure_tray(app: &tauri::App) -> tauri::Result<()> {
    // Windows 托盘必须在创建时显式提供图标，否则 Shell_NotifyIconW(NIM_ADD) 只注册一个
    // “无图标”的托盘项，任务栏通知区不会渲染出任何可见图标。
    let tray = TrayIconBuilder::with_id("main-tray")
        // 去掉原生左键菜单；右键仍由品牌覆盖层面板承载，左键切换主窗口显隐。
        .icon(
            app.default_window_icon()
                .map(Clone::clone)
                .unwrap_or_else(|| {
                    Image::from_bytes(include_bytes!("../icons/icon.ico"))
                        .expect("内置图标必须可解码")
                }),
        )
        .show_menu_on_left_click(false);

    tray.on_tray_icon_event(|tray, event| {
        if let TrayIconEvent::Click {
            button_state: MouseButtonState::Up,
            button,
            position,
            ..
        } = event
        {
            let app = tray.app_handle();
            match button {
                // 左键：切换主窗口显示/隐藏
                MouseButton::Left => toggle_main_window(app),
                // 右键：弹出品牌托盘面板
                MouseButton::Right => open_tray_panel(app, &position),
                _ => {}
            }
        }
    })
    .build(app)?;
    Ok(())
}

// ── 升级后首启白屏：宿主层清理 ──────────────────────────────────────────────
//
// 症状：升级安装后被自动拉起的第一次启动是白屏，手动重启一次就恢复。
//
// 链路（真机上逐条验证过）：
//   1. 旧版本（≤ v0.1.6）的桌面端在 `https://tauri.localhost` 上注册过 Service Worker，
//      把当时的应用外壳 index.html 连同 `/assets/index-<旧hash>.js` 一起写进了
//      `EBWebView/Default/Service Worker/CacheStorage`；
//   2. 升级时安装程序覆盖文件，首启的导航由那份仍然注册着的旧 SW 接管；它的
//      network-first `fetch` 在宿主/浏览器进程交接的那一刻打不通，于是拿缓存里的
//      **旧外壳**兜底；
//   3. 旧外壳引用的 `/assets/index-<旧hash>.js` 在新版本里已不存在，而 Tauri 的资源
//      处理器对**任何找不到的路径**都兜底返回 index.html + `text/html`；
//   4. `<script type="module">` 拿到 HTML → MIME 校验失败 → React 不挂载 → 白屏。
//
// 为什么只在前端修不住：出白屏的那一次跑起来的正是**旧包**，新包里的清理代码
// （前端 `purgeServiceWorkerState`）没有机会执行，只能救「新包已经跑起来」之后的机器。
// 所以必须在新宿主建窗之前，从宿主侧把这些会跨版本残留的缓存目录删掉。
const WEBVIEW_CACHE_DIRS: [&str; 3] = ["Service Worker", "Cache", "Code Cache"];
/// 版本标记文件，放在应用数据目录根下（不属于 WebView2 自己的目录）。
const WEBVIEW_CACHE_MARKER: &str = ".webview-cache-version";

/// 在新宿主创建任何 webview 之前清掉上一版本残留的 WebView2 缓存。
fn purge_stale_webview_cache(app: &tauri::AppHandle) {
    let Ok(local_data) = app.path().app_local_data_dir() else {
        return;
    };
    if !local_data.is_dir() {
        return;
    }
    if purge_webview_cache(&local_data, &app.package_info().version.to_string()) {
        return;
    }
    // 发布版是 GUI 子系统，没有控制台：这条线索必须落到日志文件里，
    // 否则线上再出白屏时我们手上什么都没有。
    const MESSAGE: &str = "未能清理上一版本残留的 WebView2 缓存（可能仍被上个进程占用），下次启动重试";
    eprintln!("[webview-cache] {MESSAGE}");
    if let Ok(mut log) = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(local_data.join("milevia-agent.log"))
    {
        let _ = writeln!(log, "[desktop] webview-cache: {MESSAGE}");
    }
}

/// 清理重试的上界。上一版宿主的 WebView2 浏览器进程可能还没退干净，目录会短暂被占用，
/// 所以删不掉时多试几轮；但总等待时间有上界，且不随目录个数放大（否则最坏情况会把启动卡住好几秒）。
const PURGE_ROUNDS: u32 = 10;
const PURGE_RETRY_INTERVAL: Duration = Duration::from_millis(200);

/// 按版本号清理一次。返回值表示「本次没有需要上报的失败」：
/// - 版本一致 → 无需清理；
/// - 删干净了 → 已落版本标记；
/// - 还没有 WebView2 配置目录（全新安装的首次启动）→ 无可清理，且**故意不落标记**，
///   等下次启动目录已经建出来，再真正检查一遍。
///
/// 抽成纯函数便于测试。标记只在**真正检查过 profile** 之后才落：写早了会漏掉一次清理
/// （下次升级又白屏），漏写则只是多检查一次，代价小得多。
fn purge_webview_cache(local_data: &std::path::Path, version: &str) -> bool {
    let marker = local_data.join(WEBVIEW_CACHE_MARKER);
    if std::fs::read_to_string(&marker).is_ok_and(|saved| saved.trim() == version) {
        return true;
    }
    let default_dir = local_data.join("EBWebView").join("Default");
    if !default_dir.is_dir() {
        // 全新安装时 WebView2 还没建过配置目录（要等第一次建窗才建出来），现在没有可清的东西。
        // 若将来 WebView2 换了目录层级，这条日志会每次启动都出现 —— 这正是要的效果：
        // 宁可吵，也不要让「清理」悄悄变成空操作而没人发现。
        eprintln!(
            "[webview-cache] 未发现 WebView2 配置目录，本次跳过（下次启动再检查）：{}",
            default_dir.display()
        );
        return true;
    }
    // 用 `is_dir` 而不是 `exists` 筛选：万一真有同名文件挡在那里，`remove_dir_all` 会永远失败，
    // 那样每次启动都要白等一整轮重试。这种情况直接跳过。
    let mut pending: Vec<PathBuf> = WEBVIEW_CACHE_DIRS
        .iter()
        .map(|name| default_dir.join(name))
        .filter(|dir| dir.is_dir())
        .collect();
    for round in 1..=PURGE_ROUNDS {
        let mut still_there = Vec::new();
        for dir in pending {
            if std::fs::remove_dir_all(&dir).is_err() {
                still_there.push(dir);
            }
        }
        pending = still_there;
        if pending.is_empty() {
            break;
        }
        if round < PURGE_ROUNDS {
            thread::sleep(PURGE_RETRY_INTERVAL);
        }
    }
    if !pending.is_empty() {
        return false;
    }
    std::fs::write(&marker, version).is_ok()
}

fn main() {
    let app = tauri::Builder::default()
        .plugin(tauri_plugin_single_instance::init(|app, _, _| {
            if let Some(window) = app.get_webview_window("main") {
                let _ = window.show();
                let _ = window.set_focus();
            }
        }))
        .plugin(tauri_plugin_updater::Builder::new().build())
        .invoke_handler(tauri::generate_handler![
            show_main_window,
            close_panel,
            quit_app,
            restart_app,
            set_panel_size,
            navigate_main,
            show_system_notification,
            enroll_remote_agent,
            open_app_data_directory,
            open_external,
            get_updater_status,
            check_for_update_now,
            install_update
        ])
        .setup(|app| {
            // 必须排在所有 webview 创建之前：升级后首启的白屏正是上一版残留的
            // Service Worker 外壳喂出来的，建窗之后再清就已经晚了。
            purge_stale_webview_cache(&app.handle());
            app.manage(ManagedSidecar(Mutex::new(None)));
            app.manage(ManagedAgent(Mutex::new(None)));
            app.manage(TrayAnchor(Mutex::new(None)));
            app.manage(UpdateCheck {
                state: Mutex::new(UpdateCheckState {
                    info: UpdateInfoRepr {
                        app_version: app.package_info().version.to_string(),
                        status: "checking".to_string(),
                        update: None,
                        error: None,
                        download: UpdateDownloadRepr::idle(),
                    },
                    generation: 0,
                    installing: false,
                    pending: None,
                    downloading: None,
                    download_error: None,
                }),
            });
            let local_agent_token = Uuid::new_v4().simple().to_string();
            let sidecar = start_sidecar(&app.handle(), &local_agent_token)
                .map_err(|error| localize_startup_error("启动本地控制服务", error))?;
            if let Err(error) = create_main_window(&app.handle(), &sidecar) {
                let mut sidecar = sidecar;
                stop_running_sidecar(&mut sidecar);
                return Err(error.into());
            }
            // 提前保存注入字段，供预建面板使用（sidecar 随后移入状态）。
            let panel_api_base = sidecar.api_base.clone();
            let panel_session_token = sidecar.session_token.clone();
            *app.state::<ManagedSidecar>()
                .0
                .lock()
                .expect("sidecar state lock") = Some(sidecar);
            match start_agent(&app.handle(), &local_agent_token, None) {
                Ok(agent) => {
                    *app.state::<ManagedAgent>()
                        .0
                        .lock()
                        .expect("agent state lock") = agent;
                }
                Err(error) => log_agent_startup_error(&app.handle(), error.as_ref()),
            }
            if let Err(error) = configure_tray(app) {
                stop_agent(&app.handle());
                stop_sidecar(&app.handle());
                return Err(error.into());
            }
            // 预建隐藏的品牌面板窗口：首次点击即可直接显示，避免首点延迟/空白。
            if let Err(error) =
                restore_or_create_panel(&app.handle(), &panel_api_base, &panel_session_token)
            {
                eprintln!("[tray-panel] 预建面板失败（首次点击时将重建）: {error}");
            }
            // 后台静默检查更新，结果供主窗 `get_updater_status` 查询。
            prime_update_check(&app.handle());
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("failed to build Milevia desktop host");
    app.run(|app_handle, event| {
        if let RunEvent::ExitRequested { .. } = event {
            stop_agent(app_handle);
            stop_sidecar(app_handle);
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 造一个「老版本残留」的假数据目录：三块缓存目录各放一个文件，
    /// 外加一块必须被保住的应用数据（Local Storage 里是用户的界面偏好）。
    fn fake_local_data(version_marker: Option<&str>) -> PathBuf {
        let root = env::temp_dir().join(format!("milevia-cache-test-{}", Uuid::new_v4()));
        let default = root.join("EBWebView").join("Default");
        for name in WEBVIEW_CACHE_DIRS {
            std::fs::create_dir_all(default.join(name)).unwrap();
            std::fs::write(default.join(name).join("stale-entry"), b"old").unwrap();
        }
        std::fs::create_dir_all(default.join("Local Storage")).unwrap();
        std::fs::write(default.join("Local Storage").join("keep"), b"user-pref").unwrap();
        if let Some(version) = version_marker {
            std::fs::write(root.join(WEBVIEW_CACHE_MARKER), version).unwrap();
        }
        root
    }

    #[test]
    fn purges_previous_version_cache_and_keeps_app_data() {
        let root = fake_local_data(None);
        let default = root.join("EBWebView").join("Default");

        assert!(purge_webview_cache(&root, "0.1.8"));

        for name in WEBVIEW_CACHE_DIRS {
            assert!(!default.join(name).exists(), "{name} 应当被清掉");
        }
        assert!(default.join("Local Storage").join("keep").exists(), "应用数据不能被动");
        assert_eq!(
            std::fs::read_to_string(root.join(WEBVIEW_CACHE_MARKER)).unwrap(),
            "0.1.8"
        );
        std::fs::remove_dir_all(&root).ok();
    }

    #[test]
    fn skips_purge_when_version_is_unchanged() {
        let root = fake_local_data(Some("0.1.8"));

        assert!(purge_webview_cache(&root, "0.1.8"));

        let default = root.join("EBWebView").join("Default");
        assert!(
            default.join("Cache").join("stale-entry").exists(),
            "版本没变就不该动缓存"
        );
        std::fs::remove_dir_all(&root).ok();
    }

    #[test]
    fn purges_again_when_version_changed() {
        let root = fake_local_data(Some("0.1.7"));
        let default = root.join("EBWebView").join("Default");

        assert!(purge_webview_cache(&root, "0.1.8"));

        assert!(!default.join("Service Worker").exists(), "换版本必须重新清一次");
        std::fs::remove_dir_all(&root).ok();
    }

    #[test]
    fn tolerates_a_file_where_a_cache_dir_is_expected() {
        // WebView2 的目录层级将来若变化（比如某一块变成文件），清理不能卡住启动：
        // 跳过它、照常写版本标记，而不是每次启动都白等一轮重试。
        let root = env::temp_dir().join(format!("milevia-cache-test-{}", Uuid::new_v4()));
        let default = root.join("EBWebView").join("Default");
        std::fs::create_dir_all(&default).unwrap();
        std::fs::write(default.join("Cache"), b"not a dir").unwrap();

        assert!(purge_webview_cache(&root, "0.1.8"));

        assert!(default.join("Cache").is_file(), "同名文件不该被动");
        assert_eq!(
            std::fs::read_to_string(root.join(WEBVIEW_CACHE_MARKER)).unwrap(),
            "0.1.8"
        );
        std::fs::remove_dir_all(&root).ok();
    }

    #[test]
    fn fresh_install_defers_until_profile_exists() {
        let root = env::temp_dir().join(format!("milevia-cache-test-{}", Uuid::new_v4()));
        std::fs::create_dir_all(&root).unwrap();

        // 首次启动：WebView2 还没建过配置目录 → 无可清理，也不能「没检查就宣告干净」
        assert!(purge_webview_cache(&root, "0.1.8"));
        assert!(
            !root.join(WEBVIEW_CACHE_MARKER).exists(),
            "没看过 profile 就不该落版本标记"
        );

        // 建过窗之后目录存在了：这次才真正检查并落标记
        let default = root.join("EBWebView").join("Default");
        std::fs::create_dir_all(default.join("Cache")).unwrap();
        assert!(purge_webview_cache(&root, "0.1.8"));
        assert_eq!(
            std::fs::read_to_string(root.join(WEBVIEW_CACHE_MARKER)).unwrap(),
            "0.1.8"
        );
        std::fs::remove_dir_all(&root).ok();
    }

    /// 造一份「检查已完成、公告了 advertised 版本」的下载状态。
    /// `pending` 留空：`PendingUpdate` 里的 `Update` 字段私有，测试里造不出实例，
    /// 所以"已就绪"那一相由 `download_repr` 直接覆盖。
    fn update_state(advertised: Option<&str>) -> UpdateCheckState {
        UpdateCheckState {
            info: UpdateInfoRepr {
                app_version: "0.1.7".to_string(),
                status: "complete".to_string(),
                update: advertised.map(update_info),
                error: None,
                download: UpdateDownloadRepr::idle(),
            },
            generation: 1,
            installing: false,
            pending: None,
            downloading: None,
            download_error: None,
        }
    }

    fn update_info(version: &str) -> UpdateInfo {
        UpdateInfo {
            current_version: "0.1.7".to_string(),
            version: version.to_string(),
            notes: None,
        }
    }

    /// 同一条英文原文，说法必须跟着**出错的那一步**走。
    ///
    /// 回归的是：`background_download` 重试全败时把错误写进 `download.error`，
    /// 而它当时借的是检查语境的文案 —— 一次下载失败被说成"检查更新失败"，
    /// 用户会去查"为什么连不上更新源"，可检查其实早就成功了。
    #[test]
    fn update_error_wording_follows_the_stage() {
        let network = "error sending request: connection reset by peer";
        // 每一步都要说自己的话：一次下载失败绝不能被说成"检查更新失败"。
        let check = localize_update_error(UpdateStage::Check, network);
        let download = localize_update_error(UpdateStage::Download, network);
        let install = localize_update_error(UpdateStage::Install, network);
        assert!(check.starts_with("检查更新失败：网络不可用"), "{check}");
        assert!(download.starts_with("下载更新失败：网络不可用"), "{download}");
        assert!(install.starts_with("安装更新失败：网络不可用"), "{install}");
        assert!(
            !download.contains("检查"),
            "下载失败不能说成检查失败：{download}"
        );
        // 原文一律留在括号里：排障与搜索都靠它。
        assert!(download.contains(network));

        // 404 跟着步骤走；签名不匹配与步骤无关（只有下载会验签），说法唯一。
        let missing = localize_update_error(UpdateStage::Download, "404 Not Found");
        assert!(missing.starts_with("下载更新失败：更新源上没有这个版本的文件"));
        for stage in [UpdateStage::Check, UpdateStage::Download] {
            let text = localize_update_error(stage, "signature verification failed");
            assert!(text.starts_with("更新包校验失败：签名与当前应用不匹配"), "{text}");
        }

        // 认不出的原文也必须套中文外壳，绝不能原样英文直出。
        assert_eq!(
            localize_update_error(UpdateStage::Download, "unexpected end of file"),
            "更新失败：unexpected end of file"
        );
    }

    /// 本地进程启动错误不能借用更新那套前缀（"找不到 sidecar 可执行文件"说成
    /// "检查更新失败"是张冠李戴，套上"更新源上没有这个版本的文件"更是胡话）。
    #[test]
    fn startup_errors_do_not_borrow_the_update_wording() {
        let text = localize_startup_error("启动本地控制服务", "failed to spawn sidecar: not found");
        assert_eq!(
            text,
            "启动本地控制服务失败：failed to spawn sidecar: not found"
        );
        assert!(!text.contains("更新"));
    }

    #[test]
    fn download_repr_reports_each_phase() {
        let ready = download_repr(Some("0.1.8"), Some(("0.1.8", 1024)), None, None);
        assert_eq!(ready.phase, "ready");
        assert_eq!(ready.received, 1024);
        assert_eq!(ready.total, Some(1024));

        let downloading = download_repr(Some("0.1.8"), None, Some(("0.1.8", 512, Some(2048))), None);
        assert_eq!(downloading.phase, "downloading");
        assert_eq!(downloading.received, 512);
        assert_eq!(downloading.total, Some(2048));

        let failed = download_repr(Some("0.1.8"), None, None, Some(("0.1.8", "网络中断")));
        assert_eq!(failed.phase, "failed");
        assert_eq!(failed.error.as_deref(), Some("网络中断"));

        assert_eq!(download_repr(Some("0.1.8"), None, None, None).phase, "idle");
    }

    #[test]
    fn download_repr_ignores_state_from_another_version() {
        // 更新源换了版本、或整个撤回了版本时，旧包的成果/任务/错误一律不算数：
        // 否则界面会提示"已就绪"，装上去的却是别的版本。
        assert_eq!(
            download_repr(Some("0.1.9"), Some(("0.1.8", 1024)), None, None).phase,
            "idle"
        );
        assert_eq!(
            download_repr(Some("0.1.9"), None, Some(("0.1.8", 512, None)), None).phase,
            "idle"
        );
        assert_eq!(
            download_repr(Some("0.1.9"), None, None, Some(("0.1.8", "网络中断"))).phase,
            "idle"
        );
        assert_eq!(
            download_repr(None, None, Some(("0.1.8", 512, None)), None).phase,
            "idle"
        );
    }

    #[test]
    fn sync_download_drops_progress_of_a_replaced_version() {
        let mut state = update_state(Some("0.1.8"));
        state.downloading = Some(DownloadProgress {
            version: "0.1.8".to_string(),
            received: 900,
            total: Some(1000),
        });
        state.sync_download();
        assert_eq!(state.info.download.phase, "downloading");
        assert_eq!(state.info.download.received, 900);

        // 重新检查发现服务器已经换到 0.1.9：旧任务的进度必须真的丢掉，
        // 而不只是不显示 —— 否则它下次回写会把 0.1.8 的包当成 0.1.9 的。
        state.info.update = Some(UpdateInfo {
            current_version: "0.1.7".to_string(),
            version: "0.1.9".to_string(),
            notes: None,
        });
        state.sync_download();
        assert_eq!(state.info.download.phase, "idle");
        assert!(state.downloading.is_none(), "换版本要真的把旧任务丢掉");
    }

    #[test]
    fn sync_download_keeps_failure_only_for_the_announced_version() {
        let mut state = update_state(Some("0.1.8"));
        state.download_error = Some(("0.1.8".to_string(), "网络中断".to_string()));
        state.sync_download();
        assert_eq!(state.info.download.phase, "failed");
        assert_eq!(state.info.download.error.as_deref(), Some("网络中断"));

        // 版本被撤回：失败态也要一起清掉，界面回到"无更新"。
        state.info.update = None;
        state.sync_download();
        assert_eq!(state.info.download.phase, "idle");
        assert!(state.download_error.is_none());
    }

    #[test]
    fn check_failure_keeps_the_download_it_knows_nothing_about() {
        // 一次检查失败（网络抖动、自建源暂时不可达）跟"更新源撤回了版本"是两回事：
        // 不能因此掐掉正在跑的预下载 —— 那会让用户白等一遍 24MB。
        // 公告版本也必须留着，它正是下载任务的版本守卫。
        let mut state = update_state(Some("0.1.8"));
        state.downloading = Some(DownloadProgress {
            version: "0.1.8".to_string(),
            received: 300,
            total: Some(1000),
        });

        state.apply_check_failure("0.1.7".to_string(), "网络不可达".to_string());
        let snapshot = state.snapshot();

        assert_eq!(snapshot.status, "failed");
        assert_eq!(snapshot.error.as_deref(), Some("网络不可达"));
        assert_eq!(
            snapshot.update.as_ref().map(|update| update.version.as_str()),
            Some("0.1.8"),
        );
        assert!(state.downloading.is_some(), "正在跑的预下载不该被掐掉");
        assert_eq!(snapshot.download.phase, "downloading");
        assert_eq!(snapshot.download.received, 300, "进度不该被清零");
    }

    #[test]
    fn check_success_announces_and_starts_exactly_one_download() {
        let mut state = update_state(None);

        let start = state.apply_check_success(
            "0.1.7".to_string(),
            Some(("0.1.8".to_string(), update_info("0.1.8"))),
        );
        state.snapshot();

        assert!(start, "查到新版本就该起一次静默预下载");
        assert_eq!(state.info.status, "complete");
        assert_eq!(state.info.download.phase, "downloading");
        assert_eq!(
            state.downloading.as_ref().map(|progress| progress.version.as_str()),
            Some("0.1.8"),
        );
    }

    #[test]
    fn check_success_does_not_restart_a_running_download() {
        // 打开托盘面板会重新检查一次：已经在下的包不能被从零重下。
        let mut state = update_state(Some("0.1.8"));
        state.downloading = Some(DownloadProgress {
            version: "0.1.8".to_string(),
            received: 300,
            total: Some(1000),
        });

        let start = state.apply_check_success(
            "0.1.7".to_string(),
            Some(("0.1.8".to_string(), update_info("0.1.8"))),
        );
        state.snapshot();

        assert!(!start);
        assert_eq!(
            state.downloading.as_ref().map(|progress| progress.received),
            Some(300),
            "进度不该被清零",
        );
    }

    #[test]
    fn check_success_switches_download_when_the_version_changed() {
        let mut state = update_state(Some("0.1.8"));
        state.downloading = Some(DownloadProgress {
            version: "0.1.8".to_string(),
            received: 300,
            total: Some(1000),
        });

        let start = state.apply_check_success(
            "0.1.7".to_string(),
            Some(("0.1.9".to_string(), update_info("0.1.9"))),
        );
        state.snapshot();

        assert!(start, "换版本要重下");
        assert_eq!(
            state.downloading.as_ref().map(|progress| progress.version.as_str()),
            Some("0.1.9"),
        );
    }

    #[test]
    fn check_success_without_update_clears_the_announcement() {
        let mut state = update_state(Some("0.1.8"));
        state.download_error = Some(("0.1.8".to_string(), "网络中断".to_string()));

        let start = state.apply_check_success("0.1.7".to_string(), None);
        state.snapshot();

        assert!(!start);
        assert!(state.info.update.is_none());
        assert_eq!(state.info.download.phase, "idle");
        assert!(
            state.download_error.is_none(),
            "版本撤回了，旧包的失败态也该作废",
        );
    }

    #[test]
    fn check_success_does_not_start_a_download_while_installing() {
        let mut state = update_state(None);
        state.installing = true;

        let start = state.apply_check_success(
            "0.1.7".to_string(),
            Some(("0.1.8".to_string(), update_info("0.1.8"))),
        );
        state.snapshot();

        assert!(!start, "正在安装时不该再起下载");
        assert!(state.downloading.is_none());
    }
}
