#requires -Version 7
# Add a Windows Defender Firewall inbound rule for the new bridge install
# at C:\1-bit-bridge\bin\bridge.exe. Self-elevates via UAC if not already
# admin (UAC popup appears on the Windows desktop — operator clicks Yes).

$ErrorActionPreference = 'Stop'
$exePath = 'C:\1-bit-bridge\bin\bridge.exe'
$ruleName = '1-bit-bridge (C:\1-bit-bridge)'

function Test-Admin {
  $id = [Security.Principal.WindowsIdentity]::GetCurrent()
  $p  = [Security.Principal.WindowsPrincipal]$id
  return $p.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

if (-not (Test-Admin)) {
  Write-Host "Not running as admin - elevating via UAC (check the Windows desktop for the popup)..."
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName  = 'pwsh.exe'
  $psi.Arguments = "-NoLogo -NoProfile -ExecutionPolicy Bypass -File `"$PSCommandPath`""
  $psi.Verb      = 'RunAs'
  $psi.UseShellExecute = $true
  try {
    $proc = [Diagnostics.Process]::Start($psi)
    $proc.WaitForExit()
    if ($proc.ExitCode -ne 0) {
      Write-Host "Elevated session exited with code $($proc.ExitCode)"
      exit $proc.ExitCode
    }
    Write-Host "Elevated session completed."
    exit 0
  } catch {
    Write-Host "Elevation failed: $_"
    exit 1
  }
}

Write-Host "Running as admin. Adding firewall rules for $exePath ..."

# Remove any prior rules for this exact path (idempotent re-run).
Get-NetFirewallRule -DisplayName $ruleName -ErrorAction SilentlyContinue |
    Remove-NetFirewallRule

# Inbound TCP allow on 7788 (bridge serve port). Scoped to the exe so
# only the bridge can accept on that port — not a port-wide open.
New-NetFirewallRule `
    -DisplayName $ruleName `
    -Description "1-bit-bridge HTTPS listener (PR #276 fresh install)" `
    -Direction Inbound `
    -Action Allow `
    -Protocol TCP `
    -LocalPort 7788 `
    -Program $exePath `
    -Profile Domain,Private,Public `
    -Enabled True | Out-Null

# Also inbound TCP on 7789 for the loopback-only admin console — needed
# even though it binds to 127.0.0.1 because some firewall configs
# require any inbound listening port to have a rule. Belt-and-braces.
New-NetFirewallRule `
    -DisplayName ($ruleName + ' admin') `
    -Description "1-bit-bridge admin console (loopback)" `
    -Direction Inbound `
    -Action Allow `
    -Protocol TCP `
    -LocalPort 7789 `
    -Program $exePath `
    -Profile Domain,Private,Public `
    -Enabled True | Out-Null

# UDP for HTTP/3 / QUIC support — the bridge ships HTTP/3 on the LAN
# endpoint (per CLAUDE.md QUIC PR #271).
New-NetFirewallRule `
    -DisplayName ($ruleName + ' UDP') `
    -Description "1-bit-bridge HTTP/3 QUIC listener" `
    -Direction Inbound `
    -Action Allow `
    -Protocol UDP `
    -LocalPort 7788 `
    -Program $exePath `
    -Profile Domain,Private,Public `
    -Enabled True | Out-Null

Write-Host "Rules added. Verify with:"
Get-NetFirewallRule -DisplayName "$ruleName*" | Format-Table -AutoSize DisplayName,Enabled,Direction,Action

Write-Host "`nDone."
