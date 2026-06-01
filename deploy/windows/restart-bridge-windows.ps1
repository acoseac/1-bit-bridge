#requires -Version 7
# Restart the bridge via its scheduled task. Safe to call from SSH:
# Task Scheduler owns the bridge process so the new instance survives
# the SSH session ending.
#
# - Stop-ScheduledTask terminates the task's process tree (the bridge.exe).
# - The defensive Stop-Process below is belt-and-braces in case the
#   process orphaned from the task (e.g. if the task was deleted mid-run).
# - Start-ScheduledTask spawns a fresh instance, detached as usual.

$ErrorActionPreference = 'Stop'
$taskName = '1-bit-bridge (home-pc)'

Write-Host "stopping task $taskName ..."
try { Stop-ScheduledTask -TaskName $taskName } catch { Write-Host "  (was not running)" }

# Belt-and-braces: kill any orphan bridge.exe even if the task was
# unregistered mid-run or the process detached from the task.
$orphans = Get-Process -Name bridge -ErrorAction SilentlyContinue
if ($orphans) {
  Write-Host "killing $($orphans.Count) orphan bridge process(es)"
  $orphans | Stop-Process -Force
}

Start-Sleep -Seconds 1

Write-Host "starting task ..."
Start-ScheduledTask -TaskName $taskName
Start-Sleep -Seconds 2

$proc = Get-Process -Name bridge -ErrorAction SilentlyContinue
if ($proc) {
  Write-Host "bridge running:"
  $proc | Format-Table -AutoSize Id, ProcessName, StartTime
} else {
  Write-Host "ERROR: bridge did NOT start — check Task Scheduler history"
  Get-ScheduledTaskInfo -TaskName $taskName |
      Format-List LastRunTime, LastTaskResult, NextRunTime
  exit 1
}
