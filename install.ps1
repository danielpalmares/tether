#requires -version 5.1
<#
.SYNOPSIS
  Instalador do Tether (host) — baixa o release do GitHub e instala tudo.

.DESCRIPTION
  Feito para rodar com um único comando no PowerShell do usuário final:

    irm https://raw.githubusercontent.com/danielpalmares/tether/main/install.ps1 | iex

  O que faz:
    1. Baixa o último release (tether-host.exe + libs) de github.com/danielpalmares/tether.
    2. Instala em %LOCALAPPDATA%\Programs\Tether.
    3. Instala dependências: FFmpeg (winget/download) e ViGEmBus (driver de gamepad).
    4. Cria atalho no Menu Iniciar e na Área de Trabalho.

  Parâmetros:
    -Version  vX.Y.Z  (default: latest)
    -NoShortcut       não cria atalhos
    -NoDeps           não instala FFmpeg/ViGEmBus (só baixa o app)
#>
[CmdletBinding()]
param(
  [string]$Version = 'latest',
  [switch]$NoShortcut,
  [switch]$NoDeps
)

$ErrorActionPreference = 'Stop'
$repo      = 'danielpalmares/tether'
$installDir = Join-Path $env:LOCALAPPDATA 'Programs\Tether'
$ffmpegUrl  = 'https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-full.7z'

function Info($m) { Write-Host "[*] $m" -ForegroundColor Cyan }
function Ok($m)   { Write-Host "    OK  $m" -ForegroundColor Green }
function Warn($m) { Write-Host "    !   $m" -ForegroundColor Yellow }
function Fail($m) { Write-Host "    X   $m" -ForegroundColor Red }
function Has($c)  { [bool](Get-Command $c -ErrorAction SilentlyContinue) }

# --- download do release -------------------------------------------------------
function Get-ReleaseAssets {
  $api = if ($Version -eq 'latest') {
    "https://api.github.com/repos/$repo/releases/latest"
  } else {
    "https://api.github.com/repos/$repo/releases/tags/$Version"
  }
  Info "consultando release ($Version)..."
  $rel = Invoke-RestMethod -Uri $api -Headers @{ 'User-Agent' = 'tether-installer' }
  Ok "release $($rel.tag_name)"
  return $rel.assets
}

function Download-App {
  New-Item -ItemType Directory -Force -Path $installDir | Out-Null
  $assets = Get-ReleaseAssets

  # Preferência: um zip "tether-host-windows.zip" com tudo. Senão, baixa o .exe solto.
  $zip = $assets | Where-Object { $_.name -match 'windows.*\.zip$' } | Select-Object -First 1
  if ($zip) {
    $tmp = Join-Path $env:TEMP $zip.name
    Info "baixando $($zip.name)..."
    Invoke-WebRequest -Uri $zip.browser_download_url -OutFile $tmp -UseBasicParsing
    Info 'extraindo...'
    Expand-Archive -Path $tmp -DestinationPath $installDir -Force
    Remove-Item $tmp -Force -ErrorAction SilentlyContinue
  } else {
    $exe = $assets | Where-Object { $_.name -eq 'tether-host.exe' } | Select-Object -First 1
    if (-not $exe) { throw 'nenhum asset tether-host.exe nem zip windows encontrado no release.' }
    Info 'baixando tether-host.exe...'
    Invoke-WebRequest -Uri $exe.browser_download_url -OutFile (Join-Path $installDir 'tether-host.exe') -UseBasicParsing
  }
  Ok "app instalado em $installDir"
}

# --- FFmpeg --------------------------------------------------------------------
function Ensure-FFmpeg {
  Info 'FFmpeg'
  if (Has 'ffmpeg') { Ok 'já presente'; return }
  if (Has 'winget') {
    try {
      Warn 'instalando via winget...'
      winget install --id=Gyan.FFmpeg -e --accept-source-agreements --accept-package-agreements --silent | Out-Null
      Refresh-Path
      if (Has 'ffmpeg') { Ok 'instalado (winget)'; return }
    } catch { Warn "winget falhou: $($_.Exception.Message)" }
  }
  # fallback: download + extração para a pasta do app, e PATH do usuário.
  try {
    Warn 'baixando FFmpeg de gyan.dev...'
    $tmp = Join-Path $env:TEMP 'tether-ffmpeg.7z'
    Invoke-WebRequest -Uri $ffmpegUrl -OutFile $tmp -UseBasicParsing
    $sevenZip = Resolve-SevenZip
    if (-not $sevenZip) { Fail 'sem 7-Zip para extrair; instale o FFmpeg manualmente.'; return }
    $dest = Join-Path $installDir 'ffmpeg'
    & $sevenZip x $tmp "-o$dest" -y | Out-Null
    $exe = Get-ChildItem $dest -Recurse -Filter 'ffmpeg.exe' | Select-Object -First 1
    if ($exe) { Add-UserPath (Split-Path $exe.FullName); Refresh-Path; Ok 'instalado (download)' }
    Remove-Item $tmp -Force -ErrorAction SilentlyContinue
  } catch { Fail "não foi possível instalar o FFmpeg: $($_.Exception.Message)" }
}

function Resolve-SevenZip {
  foreach ($c in @('7z','7za')) { if (Has $c) { return (Get-Command $c).Source } }
  $p = "$env:ProgramFiles\7-Zip\7z.exe"; if (Test-Path $p) { return $p }
  if (Has 'winget') {
    try { winget install --id=7zip.7zip -e --silent --accept-source-agreements --accept-package-agreements | Out-Null } catch {}
    if (Test-Path $p) { return $p }
  }
  return $null
}

function Add-UserPath($dir) {
  $u = [Environment]::GetEnvironmentVariable('Path','User')
  if ($u -notlike "*$dir*") { [Environment]::SetEnvironmentVariable('Path', "$u;$dir", 'User'); Ok "PATH += $dir" }
}
function Refresh-Path {
  $env:Path = [Environment]::GetEnvironmentVariable('Path','Machine') + ';' + [Environment]::GetEnvironmentVariable('Path','User')
}

# --- ViGEmBus ------------------------------------------------------------------
function Ensure-ViGEm {
  Info 'ViGEmBus (gamepad virtual)'
  if (Get-Service -Name 'ViGEmBus' -ErrorAction SilentlyContinue) { Ok 'driver presente'; return }
  $inst = Get-ChildItem $installDir -Recurse -Filter 'ViGEmBus_*.exe' -ErrorAction SilentlyContinue | Select-Object -First 1
  if (-not $inst) { Warn 'instalador não veio no release; gamepad desativado. https://github.com/ViGEm/ViGEmBus/releases'; return }
  try {
    Warn 'instalando ViGEmBus (UAC)...'
    Start-Process -FilePath $inst.FullName -ArgumentList '/qn' -Verb RunAs -Wait
    Ok 'ViGEmBus instalado'
  } catch { Warn "instalação cancelada — vídeo segue funcionando." }
}

# --- atalhos -------------------------------------------------------------------
function New-Shortcuts {
  $exe = Join-Path $installDir 'tether-host.exe'
  if (-not (Test-Path $exe)) { return }
  $ws = New-Object -ComObject WScript.Shell
  $targets = @(
    (Join-Path ([Environment]::GetFolderPath('Programs')) 'Tether.lnk'),
    (Join-Path ([Environment]::GetFolderPath('Desktop'))  'Tether.lnk')
  )
  foreach ($lnk in $targets) {
    $s = $ws.CreateShortcut($lnk)
    $s.TargetPath = $exe
    $s.WorkingDirectory = $installDir
    $s.Description = 'Tether · Game Streaming host'
    $s.Save()
  }
  Ok 'atalhos criados (Menu Iniciar + Área de Trabalho)'
}

# --- fluxo ---------------------------------------------------------------------
Write-Host '========================================' -ForegroundColor White
Write-Host '   TETHER · instalador'                    -ForegroundColor White
Write-Host '========================================' -ForegroundColor White

Download-App
if (-not $NoDeps) { Ensure-FFmpeg; Ensure-ViGEm }
if (-not $NoShortcut) { New-Shortcuts }

Write-Host ''
Ok 'Instalação concluída.'
Write-Host "    App: $installDir\tether-host.exe" -ForegroundColor Gray
Write-Host "    Abra pelo atalho 'Tether' ou rode o exe; o painel sobe em http://localhost:8787" -ForegroundColor Gray
