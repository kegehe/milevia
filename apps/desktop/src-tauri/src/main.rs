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
    menu::{Menu, MenuItem},
    tray::TrayIconBuilder,
    webview::NewWindowResponse,
    Manager, RunEvent, WebviewUrl, WebviewWindowBuilder, WindowEvent,
};
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

fn create_main_window(app: &tauri::AppHandle, sidecar: &RunningSidecar) -> tauri::Result<()> {
    let runtime_config = serde_json::json!({
        "apiBase": sidecar.api_base,
        "wsBase": sidecar.api_base.replacen("http", "ws", 1),
        "sessionToken": sidecar.session_token,
    });
    let initialization_script = format!(
        "Object.defineProperty(window, '__MILEVIA_DESKTOP_RUNTIME__', {{ value: {}, writable: false, configurable: false }});",
        serde_json::to_string(&runtime_config).expect("runtime config serializes")
    );
    let window = WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
        .title("Milevia")
        .inner_size(1440.0, 920.0)
        .min_inner_size(1080.0, 720.0)
        .initialization_script(&initialization_script)
        .on_navigation(|url| {
            if cfg!(dev) {
                return url.scheme() == "http"
                    && url.host_str() == Some("127.0.0.1")
                    && url.port_or_known_default() == Some(1420);
            }
            url.scheme() == "tauri"
                || (url.scheme() == "https" && url.host_str() == Some("tauri.localhost"))
        })
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

fn configure_tray(app: &tauri::App) -> tauri::Result<()> {
    let show = MenuItem::with_id(app, "show", "显示 Milevia", true, None::<&str>)?;
    let quit = MenuItem::with_id(app, "quit", "退出", true, None::<&str>)?;
    let menu = Menu::with_items(app, &[&show, &quit])?;
    TrayIconBuilder::new()
        .menu(&menu)
        .on_menu_event(|app, event| match event.id.as_ref() {
            "show" => {
                if let Some(window) = app.get_webview_window("main") {
                    let _ = window.show();
                    let _ = window.set_focus();
                }
            }
            "quit" => app.exit(0),
            _ => {}
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
        .setup(|app| {
            app.manage(ManagedSidecar(Mutex::new(None)));
            let sidecar = start_sidecar(&app.handle()).map_err(|error| error.to_string())?;
            if let Err(error) = create_main_window(&app.handle(), &sidecar) {
                let mut sidecar = sidecar;
                stop_running_sidecar(&mut sidecar);
                return Err(error.into());
            }
            *app.state::<ManagedSidecar>()
                .0
                .lock()
                .expect("sidecar state lock") = Some(sidecar);
            if let Err(error) = configure_tray(app) {
                stop_sidecar(&app.handle());
                return Err(error.into());
            }
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
