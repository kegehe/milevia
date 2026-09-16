; Milevia —— 自定义 NSIS Hook
; tauri 通过 `nsis.installerHooks` 载入本文件，hook 到安装与卸载流程。

; ── 为什么安装/卸载前必须结束随包进程 ──────────────────────────────
; `milevia-control.exe`、`milevia-agent.exe`、`milevia-approval.exe`、
; `milevia-terminal-bridge.exe` 都是主程序在运行时拉起的**独立进程**（不是主程序本身）。
; tauri 内置的 `CheckIfAppIsRunning` 只处理主程序 ${MAINBINARYNAME}.exe，不会碰它们，
; 而 `File` 会把它们逐个覆写回 $INSTDIR：
;   - 目标文件正被自己的进程当映像占用时，NSIS 覆写失败，弹
;     "无法打开要写入的文件: ...\milevia-control.exe"（Abort/Retry/Ignore）；
;   - 点「忽略」会把该文件留在旧版本，出现新宿主 + 旧 sidecar 的混杂安装。
; 所以这里在复制文件之前逐个结束它们；结束不了就明确报错中止，
; 而不是让 `File` 抛那条难懂的报错。

; 结束一个随包进程。
; 判据用 `KillProcessCurrentUser` 的返回码，而不是"再 Find 一次看还在不在"：
; 进程被结束后，它的**进程对象**在句柄全部关闭前仍会出现在进程列表里（宿主、杀软、
; 拉起它的 shell 都可能还握着句柄），拿"还能枚举到"当"没杀掉"会误判并中断安装。
; 返回码约定与 tauri 内置宏一致：0 = 已结束，2 = 进程本来就不在；其它值 = 结束失败。
;
; 必须整段放在宏里：`!addplugindir` 在本文件被 include 之后才声明，
; 顶层直接写 `nsis_tauri_utils::` 会在插件路径注册前解析而编译失败。
!macro MileviaStopBundledProcess processName
  nsis_tauri_utils::FindProcessCurrentUser "${processName}"
  Pop $R0
  ${If} $R0 = 0
    nsis_tauri_utils::KillProcessCurrentUser "${processName}"
    Pop $R1
    ; 失败时再试一次：可能是进程正在启动/退出途中的瞬时状态。
    ${If} $R1 <> 0
    ${AndIf} $R1 <> 2
      Sleep 500
      nsis_tauri_utils::KillProcessCurrentUser "${processName}"
      Pop $R1
    ${EndIf}
    ${If} $R1 <> 0
    ${AndIf} $R1 <> 2
      DetailPrint "无法结束 Milevia 的后台进程 ${processName}（返回码 $R1）。"
      ${If} ${Silent}
        ; 安静安装不能弹窗等点击：日志已记下，直接中止。
      ${Else}
        MessageBox MB_OK|MB_ICONSTOP \
          "Milevia 的后台进程 ${processName} 仍在运行，安装无法继续。$\r$\n\
          $\r$\n\
          请在任务管理器中结束该进程，然后重新运行安装程序。"
      ${EndIf}
      Abort
    ${EndIf}
    ; 进程已结束：给内核/杀软一点时间释放映像文件句柄，再让 NSIS 覆写该 exe
    ; （tauri 内置宏结束主程序后同样是 Sleep 500）。
    Sleep 600
  ${EndIf}
  ; 插件调用可能留下错误标志，清干净再回到安装流程。
  ClearErrors
!macroend

; 安装前：先结束主程序（hook 位置在内置检查之前，这里先做一次以保证顺序），
; 再结束全部随包进程，最后才轮到 NSIS 复制文件。
!macro NSIS_HOOK_PREINSTALL
  !insertmacro CheckIfAppIsRunning "${MAINBINARYNAME}.exe" "${PRODUCTNAME}"
  !insertmacro MileviaStopBundledProcess "milevia-control.exe"
  !insertmacro MileviaStopBundledProcess "milevia-agent.exe"
  !insertmacro MileviaStopBundledProcess "milevia-approval.exe"
  !insertmacro MileviaStopBundledProcess "milevia-terminal-bridge.exe"
!macroend

; 卸载前同理：否则 `Delete "$INSTDIR\milevia-control.exe"` 会静默失败，留下残留文件。
!macro NSIS_HOOK_PREUNINSTALL
  !insertmacro CheckIfAppIsRunning "${MAINBINARYNAME}.exe" "${PRODUCTNAME}"
  !insertmacro MileviaStopBundledProcess "milevia-control.exe"
  !insertmacro MileviaStopBundledProcess "milevia-agent.exe"
  !insertmacro MileviaStopBundledProcess "milevia-approval.exe"
  !insertmacro MileviaStopBundledProcess "milevia-terminal-bridge.exe"
!macroend

; 卸载完成后弹窗询问是否删除应用数据。
; 数据目录 = %LOCALAPPDATA%\com.milevia.desktop（与 desktop 侧 `app_local_data_dir` 一致）。
!macro NSIS_HOOK_POSTUNINSTALL
  ; 这里在主程序和残留 sidecar 都已退出后执行，删除数据目录最干净。
  ClearErrors
  MessageBox MB_YESNO|MB_ICONQUESTION|MB_DEFBUTTON2 \
    "是否同时删除 Milevia 的应用数据（项目、会话历史与设置）？$\r$\n\
    $\r$\n\
    【是】删除全部数据，下次全新安装时从空白开始。$\r$\n\
    【否】保留数据，重装后项目与会话仍可恢复。" \
    IDYES +2
  Goto done
  ; 用户选择「是」→ 删除数据目录
  RMDir /r "$LOCALAPPDATA\com.milevia.desktop"
  done:
!macroend
