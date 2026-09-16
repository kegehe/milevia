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
use tauri_plugin_updater::UpdaterExt;
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

#[derive(Clone, serde::Serialize)]
#[serde(rename_all = "camelCase")]
struct UpdateInfoRepr {
    app_version: String,
    status: String,
    update: Option<UpdateInfo>,
    error: Option<String>,
}

struct UpdateCheck {
    state: Mutex<UpdateCheckState>,
}

struct UpdateCheckState {
    info: UpdateInfoRepr,
    generation: u64,
    installing: bool,
}

async fn perform_update_check(app: &tauri::AppHandle) -> UpdateInfoRepr {
    let app_version = app.package_info().version.to_string();
    let update_check = app.state::<UpdateCheck>();
    let generation = if let Ok(mut state) = update_check.state.lock() {
        state.generation = state.generation.wrapping_add(1);
        state.info.status = "checking".to_string();
        state.info.error = None;
        state.info.update = None;
        state.generation
    } else {
        0
    };
    let result = async {
        let updater = app
            .updater_builder()
            .timeout(UPDATE_CHECK_TIMEOUT)
            .build()
            .map_err(|error| error.to_string())?;
        let update = updater.check().await.map_err(|error| error.to_string())?;
        Ok::<Option<UpdateInfo>, String>(update.map(|update| UpdateInfo {
            current_version: update.current_version,
            version: update.version,
            notes: update.body.clone(),
        }))
    }
    .await;
    let next = match result {
        Ok(update) => UpdateInfoRepr {
            app_version,
            status: "complete".to_string(),
            update,
            error: None,
        },
        Err(error) => UpdateInfoRepr {
            app_version,
            status: "failed".to_string(),
            update: None,
            error: Some(error),
        },
    };
    if let Ok(mut state) = update_check.state.lock() {
        if state.generation == generation {
            state.info = next.clone();
            return next;
        }
        return state.info.clone();
    }
    next
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

/// 下载并安装新版本，结束后重启应用。期间通过 `updater://progress` 事件回报进度。
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

    let result = install_update_inner(&app).await;
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

async fn install_update_inner(app: &tauri::AppHandle) -> Result<InstallUpdateResult, String> {
    let updater = app
        .updater_builder()
        // The updater client's timeout also applies to the asset download.
        // Keep it generous for large packages on slow networks; the check
        // itself is bounded separately below.
        .timeout(UPDATE_DOWNLOAD_TIMEOUT)
        // 交给安装程序之前先自己停掉子进程。Windows 上 updater 启动安装包后直接
        // `std::process::exit(0)`（tauri-plugin-updater 的 `install_inner`），
        // `RunEvent::ExitRequested` 不会触发，`run()` 回调里的 stop_agent/stop_sidecar
        // 也就没机会执行；而 milevia-control.exe 只能靠 parent-watch 发现自己成了孤儿，
        // 安装程序覆写它时它往往还在运行 —— 于是必弹"无法打开要写入的文件"。
        // 这里优雅停掉：既让安装程序能立刻覆写文件，也让 SQLite 正常收尾
        // （否则升级后首次启动可能卡在"库被锁定"）。
        .on_before_exit({
            let app = app.clone();
            move || {
                stop_agent(&app);
                stop_sidecar(&app);
            }
        })
        .build()
        .map_err(|error| error.to_string())?;
    let _ = app.emit(
        "updater://progress",
        serde_json::json!({ "phase": "checking", "received": 0, "total": null }),
    );
    let update = tokio::time::timeout(UPDATE_CHECK_TIMEOUT, updater.check())
        .await
        .map_err(|_| "检查更新超过 45 秒仍未完成，请检查网络后重试".to_string())?
        .map_err(|error| error.to_string())?;
    let Some(update) = update else {
        let update_check = app.state::<UpdateCheck>();
        if let Ok(mut state) = update_check.state.lock() {
            state.generation = state.generation.wrapping_add(1);
            state.info.status = "complete".to_string();
            state.info.update = None;
            state.info.error = None;
        }
        return Ok(InstallUpdateResult { installed: false });
    };
    let _ = app.emit(
        "updater://progress",
        serde_json::json!({ "phase": "starting", "received": 0, "total": null }),
    );
    let download = tokio::time::timeout(
        UPDATE_DOWNLOAD_TIMEOUT,
        update.download_and_install(
            |received, total| {
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
        Ok(Err(error)) => return Err(error.to_string()),
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
        return Err("agent configuration is missing MILEVIA_CLOUD_URL".into());
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
        return Err(format!("Agent binary not found: {}", binary.display()).into());
    }
    let config = agent_config_path(app)?;
    if !config.exists() {
        return Err(format!("Agent configuration not found: {}", config.display()).into());
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
        Err(error) => Err(format!("failed to start Agent: {error}").into()),
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
    let agent =
        start_agent(&app, &local_agent_token, Some(token)).map_err(|error| error.to_string())?;
    *app.state::<ManagedAgent>()
        .0
        .lock()
        .map_err(|_| "无法保存 Agent 进程状态")? = agent;
    Ok(())
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
  resize: (w, h) => window.__TAURI_INTERNALS__.invoke('set_panel_size', { width: w, height: h }),
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

/// 点击托盘图标（左/右键）时弹出品牌面板。
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

/// 让面板窗口按内容自适应尺寸（前端测量后调用，消除右侧留白）。
/// 尺寸变化后重新按最近一次点击点把面板左下角贴回鼠标位置。
#[tauri::command]
fn set_panel_size(app: tauri::AppHandle, width: f64, height: f64) {
    if let Some(panel) = app.get_webview_window(TRAY_PANEL_LABEL) {
        // 逻辑像素尺寸；加一点安全余量避免贴边裁切
        let new_w = width + 2.0;
        let new_h = height + 2.0;
        let _ = panel.set_size(LogicalSize::new(new_w, new_h));
        // 内容自适应后高度变化会破坏“左下角贴鼠标”，这里用刚请求的尺寸重贴一次
        // （避免读 inner_size() 时 set_size 尚未生效拿到旧值）
        let anchor = app.state::<TrayAnchor>().0.lock().unwrap().clone();
        if let Some(anchor) = anchor {
            position_panel_at_cursor(&panel, &anchor, Some((new_w, new_h)));
        }
    }
}

fn configure_tray(app: &tauri::App) -> tauri::Result<()> {
    // Windows 托盘必须在创建时显式提供图标，否则 Shell_NotifyIconW(NIM_ADD) 只注册一个
    // “无图标”的托盘项，任务栏通知区不会渲染出任何可见图标。
    let tray = TrayIconBuilder::with_id("main-tray")
        // 去掉原生菜单，改由品牌覆盖层面板承载；左/右键都弹面板。
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
            if matches!(button, MouseButton::Left | MouseButton::Right) {
                let app = tray.app_handle();
                open_tray_panel(&app, &position);
            }
        }
    })
    .build(app)?;
    Ok(())
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
            set_panel_size,
            navigate_main,
            show_system_notification,
            enroll_remote_agent,
            get_updater_status,
            check_for_update_now,
            install_update
        ])
        .setup(|app| {
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
                    },
                    generation: 0,
                    installing: false,
                }),
            });
            let local_agent_token = Uuid::new_v4().simple().to_string();
            let sidecar = start_sidecar(&app.handle(), &local_agent_token)
                .map_err(|error| error.to_string())?;
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
