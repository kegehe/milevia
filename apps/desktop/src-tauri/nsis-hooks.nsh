; Milevia —— 自定义 NSIS 卸载 Hook
; 提供「卸载时是否同时删除应用数据」的选项。
; tauri 通过 `nsis.installerHooks` 载入本文件，hook 到卸载流程。

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
