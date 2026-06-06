#requires -version 5.1
<#
.SYNOPSIS
  Launcher do host Tether: valida e instala as dependências do sistema, depois
  sobe o servidor.

.DESCRIPTION
  Verifica (e instala quando ausente):
    - FFmpeg  : via winget; fallback para download do build gyan.dev + PATH do usuário.
    - ViGEmBus: driver de gamepad virtual (libs/ViGEmBus_*.exe), instalação com UAC.
    - GPU     : aviso se não houver NVIDIA (NVENC) — o host ainda tenta o fallback CPU.
    - Binário : usa tether-host.exe se existir; senão compila uma vez (precisa de Go).

  Uso:
    powershell -ExecutionPolicy Bypass -File .\launch.ps1
    .\launch.ps1 -SkipChecks      # pula a validação e sobe o host direto
    .\launch.ps1 -Reinstall       # força reinstalar FFmpeg mesmo se presente
#>
[CmdletBinding()]
param(
  [switch]$SkipChecks,
  [switch]$Reinstall
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$libs = Join-Path $root 'libs'
$ffmpegUrl = 'https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-full.7z'

function Write-Step($msg)  { Write-Host "`n[*] $msg" -ForegroundColor Cyan }
function Write-Ok($msg)    { Write-Host "    OK  $msg" -ForegroundColor Green }
function Write-Warn2($msg) { Write-Host "    !   $msg" -ForegroundColor Yellow }
function Write-Err2($msg)  { Write-Host "    X   $msg" -ForegroundColor Red }

function Test-Command($name) {
  return [bool](Get-Command $name -ErrorAction SilentlyContinue)
}

# --- FFmpeg --------------------------------------------------------------------
function Install-FFmpegViaWinget {
  if (-not (Test-Command 'winget')) { return $false }
  Write-Warn2 'instalando FFmpeg via winget (pode pedir confirmação)...'
  try {
    winget install --id=Gyan.FFmpeg -e --accept-source-agreements --accept-package-agreements --silent | Out-Null
    # winget atualiza o PATH da máquina; recarrega no processo atual.
    Update-SessionPath
    return (Test-Command 'ffmpeg')
  } catch {
    Write-Warn2 "winget falhou: $($_.Exception.Message)"
    return $false
  }
}

function Install-FFmpegFromArchive {
  Write-Warn2 'baixando FFmpeg de gyan.dev (build .7z)...'
  $tmp7z = Join-Path $env:TEMP 'tether-ffmpeg.7z'
  $dest  = Join-Path $env:LOCALAPPDATA 'Tether\ffmpeg'

  Invoke-WebRequest -Uri $ffmpegUrl -OutFile $tmp7z -UseBasicParsing

  $sevenZip = Get-SevenZip
  if (-not $sevenZip) {
    Write-Err2 'arquivo .7z baixado, mas nenhum extrator 7-Zip encontrado.'
    Write-Err2 "Extraia manualmente $tmp7z e coloque ffmpeg.exe no PATH, ou instale o 7-Zip / winget."
    return $false
  }

  if (Test-Path $dest) { Remove-Item $dest -Recurse -Force }
  New-Item -ItemType Directory -Force -Path $dest | Out-Null
  Write-Warn2 'extraindo...'
  & $sevenZip x $tmp7z "-o$dest" -y | Out-Null

  # O build vem em uma subpasta ffmpeg-*/bin/ffmpeg.exe — localiza o exe.
  $exe = Get-ChildItem -Path $dest -Recurse -Filter 'ffmpeg.exe' | Select-Object -First 1
  if (-not $exe) { Write-Err2 'ffmpeg.exe não encontrado após extração.'; return $false }

  $binDir = Split-Path -Parent $exe.FullName
  Add-ToUserPath $binDir
  Update-SessionPath
  Remove-Item $tmp7z -Force -ErrorAction SilentlyContinue
  return (Test-Command 'ffmpeg')
}

function Get-SevenZip {
  foreach ($c in @('7z', '7za')) { if (Test-Command $c) { return (Get-Command $c).Source } }
  foreach ($p in @(
      "$env:ProgramFiles\7-Zip\7z.exe",
      "${env:ProgramFiles(x86)}\7-Zip\7z.exe")) {
    if (Test-Path $p) { return $p }
  }
  # tenta instalar 7-Zip via winget para conseguir extrair
  if (Test-Command 'winget') {
    try {
      winget install --id=7zip.7zip -e --accept-source-agreements --accept-package-agreements --silent | Out-Null
      $p = "$env:ProgramFiles\7-Zip\7z.exe"
      if (Test-Path $p) { return $p }
    } catch {}
  }
  return $null
}

function Add-ToUserPath($dir) {
  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  if ($userPath -notlike "*$dir*") {
    [Environment]::SetEnvironmentVariable('Path', "$userPath;$dir", 'User')
    Write-Ok "adicionado ao PATH do usuário: $dir"
  }
}

function Update-SessionPath {
  $machine = [Environment]::GetEnvironmentVariable('Path', 'Machine')
  $user    = [Environment]::GetEnvironmentVariable('Path', 'User')
  $env:Path = "$machine;$user"
}

function Ensure-FFmpeg {
  Write-Step 'FFmpeg (captura + encode)'
  if (-not $Reinstall -and (Test-Command 'ffmpeg')) {
    $ver = (& ffmpeg -version 2>$null | Select-Object -First 1)
    Write-Ok "presente — $ver"
    return $true
  }
  if (Install-FFmpegViaWinget) { Write-Ok 'FFmpeg instalado (winget)'; return $true }
  if (Install-FFmpegFromArchive) { Write-Ok 'FFmpeg instalado (download)'; return $true }
  Write-Err2 'não foi possível instalar o FFmpeg automaticamente.'
  return $false
}

# --- ViGEmBus (gamepad virtual) ------------------------------------------------
function Test-ViGEmInstalled {
  # O driver registra o serviço/dispositivo ViGEmBus.
  $svc = Get-Service -Name 'ViGEmBus' -ErrorAction SilentlyContinue
  if ($svc) { return $true }
  $reg = Get-ChildItem 'HKLM:\SYSTEM\CurrentControlSet\Services' -ErrorAction SilentlyContinue |
         Where-Object { $_.PSChildName -like 'ViGEmBus*' }
  return [bool]$reg
}

function Ensure-ViGEmBus {
  Write-Step 'ViGEmBus (gamepad virtual)'
  if (Test-ViGEmInstalled) { Write-Ok 'driver presente'; return }
  $installer = Get-ChildItem -Path $libs -Filter 'ViGEmBus_*.exe' -ErrorAction SilentlyContinue |
               Select-Object -First 1
  if (-not $installer) {
    Write-Warn2 'instalador não encontrado em libs/ — gamepad ficará desativado.'
    Write-Warn2 'baixe em https://github.com/ViGEm/ViGEmBus/releases'
    return
  }
  Write-Warn2 "instalando $($installer.Name) (UAC vai aparecer)..."
  try {
    # /qn = silencioso; o instalador (Inno/MSI) dispara o UAC sozinho.
    $p = Start-Process -FilePath $installer.FullName -ArgumentList '/qn' -Verb RunAs -PassThru -Wait
    if (Test-ViGEmInstalled) { Write-Ok 'ViGEmBus instalado' }
    else { Write-Warn2 "instalador encerrou (código $($p.ExitCode)); reinicie se o gamepad não funcionar." }
  } catch {
    Write-Warn2 "instalação cancelada/falhou: $($_.Exception.Message) — vídeo segue funcionando."
  }
}

# --- GPU / NVENC ---------------------------------------------------------------
function Check-GPU {
  Write-Step 'GPU (NVENC)'
  $gpu = Get-CimInstance Win32_VideoController -ErrorAction SilentlyContinue |
         Where-Object { $_.Name -match 'NVIDIA' } | Select-Object -First 1
  if ($gpu) { Write-Ok "NVIDIA detectada — $($gpu.Name)" }
  else { Write-Warn2 'sem GPU NVIDIA — h264_nvenc pode falhar; o host tentará o fallback CPU.' }
}

# --- Binário do host -----------------------------------------------------------
function Ensure-Binary {
  Write-Step 'binário do host'
  $exe = Join-Path $root 'tether-host.exe'
  if (Test-Path $exe) { Write-Ok 'tether-host.exe presente'; return $exe }
  Write-Warn2 'tether-host.exe ausente — compilando (precisa de Go)...'
  if (-not (Test-Command 'go')) {
    Write-Err2 'Go não encontrado. Instale o Go ou copie um tether-host.exe pré-compilado.'
    return $null
  }
  Push-Location $root
  try { & go build -o tether-host.exe ./cmd/host; Write-Ok 'compilado' }
  finally { Pop-Location }
  if (Test-Path $exe) { return $exe }
  Write-Err2 'falha na compilação.'
  return $null
}

# --- Fluxo principal -----------------------------------------------------------
Write-Host '======================================' -ForegroundColor White
Write-Host '   TETHER · launcher de host'           -ForegroundColor White
Write-Host '======================================' -ForegroundColor White

if (-not $SkipChecks) {
  $ffmpegOk = Ensure-FFmpeg
  Ensure-ViGEmBus
  Check-GPU
  if (-not $ffmpegOk) {
    Write-Err2 'FFmpeg é obrigatório para o vídeo. Abortando.'
    exit 1
  }
} else {
  Write-Warn2 'validação de dependências pulada (-SkipChecks)'
  Update-SessionPath
}

$exe = Ensure-Binary
if (-not $exe) { exit 1 }

Write-Step 'subindo o host...'
& $exe
