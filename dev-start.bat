@echo off
rem ===========================================================================
rem  Milevia dev-mode launcher (runs latest source code)
rem  ---------------------------------------------------------------------------
rem  Double-click to start. Runs the @milevia/desktop dev script via pnpm:
rem    1. prepare-assets     generate icon assets
rem    2. build-sidecar.mjs  compile Go sidecar (cached; skipped if Go unchanged)
rem    3. tauri dev          start Vite dev server + compile Rust + open window
rem
rem  Frontend hot-reload: edits under apps/web refresh the UI on save.
rem  Rust/Go edits require restarting this script (close window, double-click again).
rem
rem  A console window stays open for logs; closing it exits Milevia.
rem ===========================================================================

setlocal
set "ROOT=%~dp0"
cd /d "%ROOT%"

rem --- check: pnpm ---
where pnpm >nul 2>nul
if errorlevel 1 (
  echo [Milevia] pnpm not found. Install Node.js and pnpm first.
  echo   npm install -g pnpm
  pause
  exit /b 1
)

rem --- check: Rust toolchain ---
where cargo >nul 2>nul
if errorlevel 1 (
  echo [Milevia] cargo not found. Install Rust toolchain first.
  echo   https://rustup.rs
  pause
  exit /b 1
)

rem --- check: Go toolchain (sidecar needs CGO) ---
where go >nul 2>nul
if errorlevel 1 (
  echo [Milevia] go not found. Install Go toolchain first.
  echo   https://go.dev/dl/
  pause
  exit /b 1
)

echo [Milevia] Starting in dev mode...
echo   workdir: %ROOT%
echo   First launch compiles Rust + Go and may take several minutes.
echo   Subsequent launches reuse caches and are much faster.
echo.

rem --- sidecar rebuild policy ------------------------------------------------
rem  The Go sidecar (milevia-control.exe / milevia-approval.exe) uses go-sqlite3
rem  and therefore requires CGO + gcc/mingw-w64. Rebuild it automatically when
rem  the toolchain is available so Go fixes are not silently omitted from dev
rem  launches. Machines without gcc keep reusing the existing binaries.
rem
rem  Override the automatic choice with MILEVIA_SKIP_SIDECAR=1 or =0.
if not defined MILEVIA_SKIP_SIDECAR (
  where gcc >nul 2>nul
  if errorlevel 1 (
    set "MILEVIA_SKIP_SIDECAR=1"
  ) else (
    set "MILEVIA_SKIP_SIDECAR=0"
  )
)

if "%MILEVIA_SKIP_SIDECAR%"=="1" (
  echo [Milevia] Reusing existing sidecar binaries ^(skipping Go rebuild^).
  echo   Edits to Go code under apps/control-server will NOT take effect.
  echo   Install gcc and run with MILEVIA_SKIP_SIDECAR=0 to rebuild the sidecar.
  echo.
  cd /d "%ROOT%apps\desktop"
  call pnpm exec tauri dev
) else (
  echo [Milevia] Full dev: rebuilding sidecar if Go sources changed.
  echo.
  call pnpm --filter @milevia/desktop dev
)

if errorlevel 1 (
  echo.
  echo [Milevia] Startup failed. Check the log above.
  pause
)

endlocal
