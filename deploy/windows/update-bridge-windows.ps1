#requires -Version 7
# update-bridge-windows.ps1 — routine binary update for home-pc.
#
# Fast-forwards the source to origin/main, rebuilds bridge.exe, and restarts
# via the scheduled task. Does NOT run `bridge init`, so the TLS cert (and
# every paired iOS device's pin) and the config are PRESERVED. Use this for
# every code update; use setup-bridge-windows.ps1 only for a fresh install,
# and rotate-cert-windows.ps1 only to deliberately re-mint the cert.
#
# Stops the task before building because Windows locks the running exe.

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'
$root     = 'C:\1-bit-bridge'
$src      = Join-Path $root 'src'
$exe      = Join-Path $root 'bin\bridge.exe'
$taskName = '1-bit-bridge (home-pc)'

Write-Host "=== 1/4 Fast-forward source to origin/main ==="
git -C $src fetch --quiet origin main
git -C $src reset --hard origin/main | Out-Null
$head = (git -C $src describe --tags --always).Trim()
Write-Host "  HEAD: $head"

Write-Host "`n=== 2/4 Stop task + free the exe lock ==="
try { Stop-ScheduledTask -TaskName $taskName } catch { Write-Host "  (task was not running)" }
Get-Process -Name bridge -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 1

Write-Host "`n=== 3/4 Rebuild bridge.exe (no init -> cert + config preserved) ==="
Push-Location $src
$env:CGO_ENABLED = '0'
$ldflags = "-s -w -X github.com/acoseac/1-bit-bridge/internal/version.ServerVersion=$head"
go build -ldflags $ldflags -o $exe .\cmd\bridge
Pop-Location
Write-Host "  built: $exe ($((Get-Item $exe).Length) bytes)"

Write-Host "`n=== 4/4 Restart via scheduled task (SSH-safe) ==="
Start-ScheduledTask -TaskName $taskName
Start-Sleep -Seconds 4

$proc = Get-Process -Name bridge -ErrorAction SilentlyContinue
if ($proc) {
  Write-Host "bridge running ($head):"
  $proc | Format-Table -AutoSize Id, ProcessName, StartTime
} else {
  Write-Host "ERROR: bridge did NOT start — check Task Scheduler history"
  Get-ScheduledTaskInfo -TaskName $taskName | Format-List LastRunTime, LastTaskResult
  exit 1
}
