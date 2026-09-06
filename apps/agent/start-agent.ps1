$ErrorActionPreference = "Stop"

$configPath = Join-Path $PSScriptRoot ".env.windows"
if (-not (Test-Path -LiteralPath $configPath)) {
  throw "Missing $configPath"
}

([System.IO.File]::ReadAllLines($configPath)) | ForEach-Object {
  $line = $_.Trim()
  if ($line -and -not $line.StartsWith("#") -and $line -match '^([^=]+)=(.*)$') {
    [Environment]::SetEnvironmentVariable($matches[1].Trim(), $matches[2], "Process")
  }
}

# The desktop control server binds a free loopback port and publishes it here.
# Keep the legacy URL as a fallback for standalone control-server processes.
if (-not $env:MILEVIA_LOCAL_URL_FILE) {
  $env:MILEVIA_LOCAL_URL_FILE = Join-Path $env:LOCALAPPDATA "com.milevia.desktop\milevia.endpoint"
}

Set-Location $PSScriptRoot
go run ./cmd/milevia-agent
