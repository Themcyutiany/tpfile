# tpfile 一键安装脚本（Windows）
# 用法：irm https://raw.githubusercontent.com/Themcyutiany/tpfile/main/install.ps1 | iex
# 说明：不需要管理员权限；自动获取最新版本，按 CPU 架构下载对应的安装包，
#       安装到 %LOCALAPPDATA%\tpfile，并把该目录加入用户 PATH。

$ErrorActionPreference = 'Stop'

# 兼容 Windows PowerShell 5.1：GitHub 需要 TLS 1.2
try { [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12 } catch {}

# 关闭下载进度条，下载更快
$ProgressPreference = 'SilentlyContinue'

$Repo = 'Themcyutiany/tpfile'

# 1. 获取最新版本号（优先用 GitHub API，失败时回退到固定版本）
$Tag = ''
try {
  $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -Headers @{ 'User-Agent' = 'tpfile-installer' } -TimeoutSec 15
  $Tag = [string]$rel.tag_name
} catch {}
if (-not $Tag) { $Tag = '1.00' }

# 2. 根据 CPU 架构选择安装包
$arch = $env:PROCESSOR_ARCHITECTURE
$suffix = 'amd64'
if ($arch -eq 'ARM64') { $suffix = 'arm64' }
$asset = "tpfile-windows-$suffix.zip"
$url = "https://github.com/$Repo/releases/download/$Tag/$asset"

# 3. 下载到临时目录并解压
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ('tpfile-install-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
$zipPath = Join-Path $tmp $asset
Write-Host "正在下载 $asset（版本 $Tag）..."
Invoke-WebRequest -Uri $url -OutFile $zipPath
Expand-Archive -Path $zipPath -DestinationPath $tmp -Force
$exe = Get-ChildItem -Path $tmp -Filter 'tpfile-windows-*.exe' -Recurse | Select-Object -First 1
if (-not $exe) { throw '下载的压缩包中没有找到 tpfile 程序文件。' }

# 4. 安装到用户目录（无需管理员权限）
$destDir = Join-Path $env:LOCALAPPDATA 'tpfile'
New-Item -ItemType Directory -Force -Path $destDir | Out-Null
$dest = Join-Path $destDir 'tpfile.exe'
Copy-Item -LiteralPath $exe.FullName -Destination $dest -Force
Write-Host "已安装到：$dest"

# 5. 把安装目录加入用户 PATH（只加一次，保留原有格式）
$regKey = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
if ($null -eq $regKey) { throw '无法打开 HKCU\Environment' }
try {
  $userPath = $regKey.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
  $kind = if ($regKey.GetValueNames() -contains 'Path') { $regKey.GetValueKind('Path') } else { [Microsoft.Win32.RegistryValueKind]::ExpandString }
  $paths = @($userPath -split ';' | Where-Object { $_ -ne '' })
  if ($paths -contains $destDir) {
    Write-Host '用户 PATH 已包含安装目录，无需重复添加。'
  } else {
    $regKey.SetValue('Path', ($paths + $destDir) -join ';', $kind)
    Write-Host "已把 $destDir 加入用户 PATH"
  }
} finally {
  $regKey.Close()
}

# 6. 广播环境变量变更，让新打开的终端立即生效
Add-Type -Namespace TpfileInstall -Name Native -MemberDefinition '[DllImport("user32.dll", SetLastError=true, CharSet=CharSet.Auto)] public static extern IntPtr SendMessageTimeout(IntPtr hWnd, uint Msg, UIntPtr wParam, string lParam, uint fuFlags, uint uTimeout, out UIntPtr lpdwResult);' -ErrorAction SilentlyContinue
$wmResult = [UIntPtr]::Zero
$null = [TpfileInstall.Native]::SendMessageTimeout([IntPtr]0xFFFF, 0x1A, [UIntPtr]::Zero, 'Environment', 2, 5000, [ref]$wmResult)

# 7. 清理临时文件并验证
Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
& $dest --version
Write-Host ''
Write-Host '安装完成！请重新打开一个终端（或新开窗口），然后在任意目录输入 tpfile 即可。'
