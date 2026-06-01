#requires -Version 7
# setup-bridge-windows.ps1
# Builds a fresh 1-bit-bridge install at C:\1-bit-bridge against latest main,
# configures customEndpoints + upscale.variantsDir, mints TLS cert via
# `bridge init`, and prints the magic-DNS + WAN endpoints. Service install
# is skipped -- operator starts via `bin\bridge.exe serve` after this runs.

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'   # mute git/go progress noise

$root      = 'C:\1-bit-bridge'
$src       = Join-Path $root 'src'
$binDir    = Join-Path $root 'bin'
$dataDir   = Join-Path $root 'data'
$cfgFile   = Join-Path $dataDir 'bridge.yaml'
$libRoot   = 'F:\media\music'
$variants  = 'E:\temp'
$wanURL    = 'https://145.224.86.89:7788'
$bridgeNm  = 'home-pc'

Write-Host "=== 1/6 Workspace ==="
New-Item -ItemType Directory -Force -Path $root, $binDir | Out-Null
if (Test-Path (Join-Path $src '.git')) {
  Write-Host "  source clone exists -- fast-forward to origin/main"
  Push-Location $src
  git fetch --quiet origin main
  git reset --hard origin/main | Out-Null
  Pop-Location
} else {
  Write-Host "  fresh clone into $src"
  Push-Location $root
  git clone --depth 1 https://github.com/acoseac/1-bit-bridge.git src 2>&1 | Out-Null
  Pop-Location
}
Push-Location $src
$head = git rev-parse --short HEAD
Pop-Location
Write-Host "  HEAD: $head"

Write-Host "`n=== 2/6 Build bridge.exe ==="
Push-Location $src
$env:CGO_ENABLED = '0'
$ldflags = "-s -w -X github.com/acoseac/1-bit-bridge/internal/version.ServerVersion=$head"
go build -ldflags $ldflags -o (Join-Path $binDir 'bridge.exe') .\cmd\bridge
Pop-Location
$exe = Join-Path $binDir 'bridge.exe'
$bytes = (Get-Item $exe).Length
Write-Host "  built: $exe ($bytes bytes)"

Write-Host "`n=== 3/6 Init config + mint TLS cert (FRESH INSTALL ONLY) ==="
# Cert policy (fixed 2026-06-01): init runs ONLY on a fresh install. On an
# UPDATE we preserve the existing cert + config so a routine binary bump does
# NOT re-mint the cert and does NOT invalidate paired iOS devices. To
# deliberately re-mint, run rotate-cert-windows.ps1 -- never 'init -force'.
if (Test-Path $cfgFile) {
  Write-Host "  config exists -- PRESERVING cert + config (no re-init)."
  Write-Host "  Deliberate re-mint: rotate-cert-windows.ps1 (forces re-pair of every iOS device)."
} else {
  Write-Host "  fresh install -- minting cert via 'bridge init'"
  & $exe init --yes -no-service -dir $dataDir -library $libRoot -name $bridgeNm
  if (-not (Test-Path $cfgFile)) {
    throw "bridge init didn't produce $cfgFile"
  }
}

Write-Host "`n=== 4/6 Inject customEndpoints + upscale.variantsDir ==="
$yaml = Get-Content $cfgFile -Raw

# Add customEndpoints at top level (idempotent -- only append if missing).
if ($yaml -notmatch '(?m)^customEndpoints:') {
  $yaml = $yaml.TrimEnd() + @"


# Operator-added: WAN endpoint reachable via the home router's
# port-forward of 7788 -> this host. Magic-DNS *.ts.net is auto-
# advertised separately when Tailscale is running.
customEndpoints:
  - $wanURL
"@
  Write-Host "  added customEndpoints: $wanURL"
} else {
  Write-Host "  customEndpoints block already present -- leaving as-is"
}

# Add upscale.variantsDir inside the existing upscale block.
# bridge init writes either an `upscale:` block or omits it. We inject either way.
if ($yaml -match '(?m)^upscale:\s*\r?\n') {
  if ($yaml -notmatch '(?m)^\s+variantsDir:') {
    # Append variantsDir as the first child of the existing block.
    $yaml = $yaml -replace '(?m)(^upscale:\s*\r?\n)', "`$1  variantsDir: '$variants'`r`n"
    Write-Host "  added upscale.variantsDir: $variants (inside existing block)"
  } else {
    # Update in-place.
    $yaml = $yaml -replace '(?m)^(\s+)variantsDir:.*$', "`$1variantsDir: '$variants'"
    Write-Host "  updated existing upscale.variantsDir -> $variants"
  }
} else {
  $yaml = $yaml.TrimEnd() + @"


upscale:
  variantsDir: '$variants'
"@
  Write-Host "  added upscale: block with variantsDir: $variants"
}

Set-Content -Path $cfgFile -Value $yaml -Encoding UTF8

Write-Host "`n=== 5/6 Cert info ==="
& $exe cert info -config $cfgFile

Write-Host "`n=== 6/6 Endpoint preview ==="
Write-Host "Run with:"
Write-Host "  $exe serve -config $cfgFile"
Write-Host ""
Write-Host "Once running, expected /v1/health.endpoints:"
Write-Host "  https://192.168.0.208:7788  (LAN)"
Write-Host "  https://home-pc.<tailnet>.ts.net:7788  (magic DNS -- auto)"
Write-Host "  $wanURL  (WAN -- custom)"

Write-Host "`nDone. config: $cfgFile  | binary: $exe"
