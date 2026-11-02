# Ucompute installer for Windows.
#
#   irm https://raw.githubusercontent.com/nealrau/Ucompute/main/install.ps1 | iex
#
# Installs ucompute.exe into %LOCALAPPDATA%\Programs\ucompute and adds it to your PATH.
$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$repo = 'nealrau/Ucompute'
if (-not [Environment]::Is64BitOperatingSystem) { throw 'ucompute needs 64-bit Windows' }
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64' -or $env:PROCESSOR_ARCHITEW6432 -eq 'ARM64') { 'arm64' } else { 'amd64' }
$asset = "ucompute_windows_$arch.exe"
$base = "https://github.com/$repo/releases/latest/download"

$dir = Join-Path $env:LOCALAPPDATA 'Programs\ucompute'
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$dest = Join-Path $dir 'ucompute.exe'
$tmp = "$dest.download"
$sums = "$dest.checksums"

Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $tmp
Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile $sums
$line = Get-Content $sums | Where-Object { $_ -match "\s$([regex]::Escape($asset))$" } | Select-Object -First 1
Remove-Item $sums
if ($line) {
  $expected = ($line -split '\s+')[0].ToLower()
  $actual = (Get-FileHash $tmp -Algorithm SHA256).Hash.ToLower()
  if ($expected -ne $actual) { Remove-Item $tmp; throw "checksum mismatch for $asset; not installing" }
}

# A running ucompute.exe (the background worker) can be renamed but not overwritten.
if (Test-Path $dest) { Move-Item -Force $dest "$dest.old" }
Move-Item -Force $tmp $dest

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not $userPath) { $userPath = '' }
if (-not (($userPath -split ';') -contains $dir)) {
  [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ";$dir").TrimStart(';'), 'User')
  $env:Path += ";$dir"
  Write-Host "Added $dir to your PATH (new terminals pick it up automatically)."
}

$version = & $dest version
Write-Host "ucompute $version installed to $dest"
if ((& $dest service status) -match '^installed') {
  & $dest service restart | Out-Null
  Write-Host 'background worker restarted'
}
Write-Host ''
Write-Host 'Next: ucompute setup            (on the machine you work on)'
Write-Host '      ucompute setup --worker   (on the machine that should run the jobs)'
