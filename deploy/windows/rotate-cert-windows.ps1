#requires -Version 7
# Rotate the bridge's TLS cert (e.g. after a customEndpoints edit so the
# SANs cover the new endpoints) and restart via the scheduled task.
#
# WARNING: rotating the cert changes its SHA-256 fingerprint, so EVERY
# paired iOS device must re-pair. Only run this deliberately — a routine
# binary update must NOT rotate the cert (see setup-bridge-windows.ps1,
# which preserves the cert on an existing install).
#
# SSH-safe: restarts via the `1-bit-bridge (home-pc)` scheduled task, NOT
# `Start-Process -RedirectStandardOutput` — the latter makes the bridge a
# child of the SSH-launched pwsh and it dies on disconnect (the documented
# footgun in ops/deployment-runbook.md).

$ErrorActionPreference = 'Stop'
$exe      = 'C:\1-bit-bridge\bin\bridge.exe'
$cfg      = 'C:\1-bit-bridge\data\bridge.yaml'
$taskName = '1-bit-bridge (home-pc)'

Write-Host "--- 1. Stop the bridge (scheduled task + any orphan) ---"
try { Stop-ScheduledTask -TaskName $taskName } catch { Write-Host "  (task was not running)" }
$orphans = Get-Process -Name bridge -ErrorAction SilentlyContinue
if ($orphans) {
  Write-Host "  killing $($orphans.Count) orphan bridge process(es)"
  $orphans | Stop-Process -Force
}
Start-Sleep -Seconds 1

Write-Host "`n--- 2. Rotate cert (picks up current customEndpoints in the SANs) ---"
& $exe cert rotate --config $cfg --yes

Write-Host "`n--- 3. Cert info ---"
& $exe cert info --config $cfg

Write-Host "`n--- 4. Restart via scheduled task (SSH-safe, detached at the OS level) ---"
Start-ScheduledTask -TaskName $taskName
Start-Sleep -Seconds 3

$proc = Get-Process -Name bridge -ErrorAction SilentlyContinue
if ($proc) {
  Write-Host "bridge running:"
  $proc | Format-Table -AutoSize Id, ProcessName, StartTime
  Write-Host "`nNOTE: cert fingerprint changed — re-pair every iOS device."
} else {
  Write-Host "ERROR: bridge did NOT start — check Task Scheduler history"
  Get-ScheduledTaskInfo -TaskName $taskName | Format-List LastRunTime, LastTaskResult
  exit 1
}
