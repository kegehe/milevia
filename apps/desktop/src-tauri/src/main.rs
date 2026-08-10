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

const DESKTOP_ORIGIN: &str = "https://tauri.localhost";
const DEV_ORIGIN: &str = "http://127.0.0.1:1420";
const SIDECAR_READY_PREFIX: &str = "MILEVIA_READY=";

struct RunningSidecar {
    child: Child,
    api_base: String,
    session_token: String,
}

struct ManagedSidecar(Mutex<Option<RunningSidecar>>);

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

/// 启动时后台查询到的升级结果缓存；`None` 表示暂无可升级信息。
struct UpdateCheck(Mutex<Option<UpdateInfo>>);

/// 静默拉取升级清单：成功且有新版则缓存信息，任何错误都吞掉（不阻断启动）。
fn prime_update_check(app: &tauri::AppHandle) {
    let app = app.clone();
    tauri::async_runtime::spawn(async move {
        let Ok(updater) = app.updater() else {
            return;
        };
        let Ok(Some(update)) = updater.check().await else {
            return;
        };
        let info = UpdateInfo {
            current_version: update.current_version,
            version: update.version,
            notes: update.body.clone(),
        };
        *app.state::<UpdateCheck>().0.lock().expect("update state lock") = Some(info);
    });
}

/// 查询升级状态：返回本机版本号 + 是否发现新版本。前端启动时轮询一次。
#[tauri::command]
fn get_updater_status(app: tauri::AppHandle) -> UpdateInfoRepr {
    let cached = app.state::<UpdateCheck>().0.lock().expect("update state lock").clone();
    UpdateInfoRepr {
        app_version: app.package_info().version.to_string(),
        update: cached,
    }
}

#[derive(serde::Serialize)]
#[serde(rename_all = "camelCase")]
struct UpdateInfoRepr {
    app_version: String,
    update: Option<UpdateInfo>,
}

/// 下载并安装新版本，结束后重启应用。期间通过 `updater://progress` 事件回报进度。
#[tauri::command]
async fn install_update(app: tauri::AppHandle) -> Result<(), String> {
    let updater = app.updater().map_err(|error| error.to_string())?;
    let Some(update) = updater.check().await.map_err(|error| error.to_string())? else {
        return Ok(());
    };
    update
        .download_and_install(
            |received, total| {
                let _ = app.emit(
                    "updater://progress",
                    serde_json::json!({ "received": received, "total": total }),
                );
            },
            || {},
        )
        .await
        .map_err(|error| error.to_string())?;
    app.restart();
    #[allow(unreachable_code)]
    Ok(())
}

const TRAY_PANEL_LABEL: &str = "tray-panel";
/// 面板初始宽度/高度（仅作建窗时的初始值，前端随后按内容自适应覆盖）。
const TRAY_PANEL_WIDTH: f64 = 220.0;
const TRAY_PANEL_HEIGHT: f64 = 224.0;

fn sidecar_binary(app: &tauri::AppHandle) -> Result<PathBuf, Box<dyn Error>> {
    if let Ok(path) = env::var("MILEVIA_CONTROL_BINARY") {
        return Ok(PathBuf::from(path));
    }
    Ok(app.path().resource_dir()?.join("milevia-control.exe"))
}

fn approval_binary(app: &tauri::AppHandle) -> Result<PathBuf, Box<dyn Error>> {
    if let Ok(path) = env::var("MILEVIA_APPROVAL_BINARY") {
        return Ok(PathBuf::from(path));
    }
    Ok(app.path().resource_dir()?.join("milevia-approval.exe"))
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
) -> Result<String, Box<dyn Error>> {
    let (sender, receiver) = mpsc::sync_channel(1);
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

    match receiver.recv_timeout(Duration::from_secs(12)) {
        Ok(Ok(url)) => Ok(url),
        Ok(Err(error)) => Err(error.into()),
        Err(mpsc::RecvTimeoutError::Timeout) => Err(format!(
            "控制服务启动超时（12 秒内未就绪）。\n程序：{}\nstderr:\n{}",
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

fn start_sidecar(app: &tauri::AppHandle) -> Result<RunningSidecar, Box<dyn Error>> {
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
    let mut child = Command::new(&sidecar_path)
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
        ])
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|e| {
            format!(
                "无法启动控制服务。\n程序：{}\n原因：{}\n请确认程序未被占用，且 CGO/SQLite 编译工具链正常。",
                sidecar_path.display(),
                e
            )
        })?;
    let stdout = child.stdout.take().ok_or("无法获取控制服务 stdout 管道")?;
    let stderr = child.stderr.take().ok_or("无法获取控制服务 stderr 管道")?;
    match wait_for_ready(stdout, stderr, sidecar_path.clone()) {
        Ok(api_base) => {
            let sidecar = RunningSidecar {
                child,
                api_base,
                session_token,
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
}, writable: false, configurable: false });"#,
        );
    }
    script
}

fn create_main_window(app: &tauri::AppHandle, sidecar: &RunningSidecar) -> tauri::Result<()> {
    let initialization_script = runtime_init_script(&sidecar.api_base, &sidecar.session_token, "app");
    let window = WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
        .title("Milevia")
        .inner_size(1440.0, 920.0)
        .min_inner_size(1080.0, 720.0)
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
    let window = WebviewWindowBuilder::new(
        app,
        TRAY_PANEL_LABEL,
        WebviewUrl::App("index.html".into()),
    )
    .title("Milevia")
    .inner_size(TRAY_PANEL_WIDTH, TRAY_PANEL_HEIGHT)
    .decorations(false)
    .transparent(true)
    .always_on_top(true)
    .skip_taskbar(true)
    .resizable(false)
    .shadow(false)
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
        .icon(app.default_window_icon().map(Clone::clone).unwrap_or_else(|| {
            Image::from_bytes(include_bytes!("../icons/icon.ico"))
                .expect("内置图标必须可解码")
        }))
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
            get_updater_status,
            install_update
        ])
        .setup(|app| {
            app.manage(ManagedSidecar(Mutex::new(None)));
            app.manage(TrayAnchor(Mutex::new(None)));
            app.manage(UpdateCheck(Mutex::new(None)));
            let sidecar = start_sidecar(&app.handle()).map_err(|error| error.to_string())?;
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
            if let Err(error) = configure_tray(app) {
                stop_sidecar(&app.handle());
                return Err(error.into());
            }
            // 预建隐藏的品牌面板窗口：首次点击即可直接显示，避免首点延迟/空白。
            if let Err(error) = restore_or_create_panel(&app.handle(), &panel_api_base, &panel_session_token) {
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
            stop_sidecar(app_handle);
        }
    });
}
