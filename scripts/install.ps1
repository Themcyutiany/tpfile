# tpfile Windows 安装脚本（无需管理员权限）
# 用法（在 tpfile-windows-amd64.exe 所在目录运行）：
#   powershell -ExecutionPolicy Bypass -File install.ps1
#   powershell -ExecutionPolicy Bypass -File install.ps1 tpfile-windows-amd64.exe
# 功能：
#   1. 把 tpfile-windows-amd64.exe 复制为 %LOCALAPPDATA%\tpfile\tpfile.exe
#   2. 把 %LOCALAPPDATA%\tpfile 加入用户 PATH（只加一次，保留原有格式）
#   3. 重新打开终端后，任意目录都能直接输入 tpfile

param(
  [string]$Source = ''
)

$ErrorActionPreference = 'Stop'

# 1. 定位要安装的可执行文件
if (-not $Source) {
  foreach ($cand in @('tpfile-windows-amd64.exe', 'tpfile-windows-arm64.exe', 'tpfile.exe')) {
    if (Test-Path -LiteralPath $cand) { $Source = (Resolve-Path -LiteralPath $cand).Path; break }
  }
}
if (-not $Source -or -not (Test-Path -LiteralPath $Source)) {
  Write-Host '错误：找不到 tpfile 可执行文件。' -ForegroundColor Red
  Write-Host '请先进入 tpfile-windows-amd64.exe 所在目录，然后运行：'
  Write-Host '  powershell -ExecutionPolicy Bypass -File install.ps1 tpfile-windows-amd64.exe'
  exit 1
}

# 2. 复制到用户目录（无需管理员权限）
$destDir = Join-Path $env:LOCALAPPDATA 'tpfile'
New-Item -ItemType Directory -Force -Path $destDir | Out-Null
$dest = Join-Path $destDir 'tpfile.exe'
Copy-Item -LiteralPath $Source -Destination $dest -Force
Write-Host "OK - 已安装到：$dest"

# 3. 把安装目录加入用户 PATH（保留 REG_EXPAND_SZ 类型，避免破坏 %VAR% 形式的原有条目）
$regKey = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
if ($null -eq $regKey) { throw '无法打开 HKCU\Environment' }
try {
  $userPath = $regKey.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
  $kind = if ($regKey.GetValueNames() -contains 'Path') { $regKey.GetValueKind('Path') } else { [Microsoft.Win32.RegistryValueKind]::ExpandString }
  $paths = @($userPath -split ';' | Where-Object { $_ -ne '' })
  if ($paths -contains $destDir) {
    Write-Host 'OK - 用户 PATH 已包含安装目录，无需重复添加。'
  } else {
    $newPath = ($paths + $destDir) -join ';'
    $regKey.SetValue('Path', $newPath, $kind)
    Write-Host "OK - 已把 $destDir 加入用户 PATH"
  }
} finally {
  $regKey.Close()
}

# 4. 广播环境变量变更，让新打开的终端立即生效
Add-Type -Namespace TpfileInstall -Name Native -MemberDefinition '[DllImport("user32.dll", SetLastError=true, CharSet=CharSet.Auto)] public static extern IntPtr SendMessageTimeout(IntPtr hWnd, uint Msg, UIntPtr wParam, string lParam, uint fuFlags, uint uTimeout, out UIntPtr lpdwResult);' -ErrorAction SilentlyContinue
$wmResult = [UIntPtr]::Zero
$null = [TpfileInstall.Native]::SendMessageTimeout([IntPtr]0xFFFF, 0x1A, [UIntPtr]::Zero, 'Environment', 2, 5000, [ref]$wmResult)

# 5. 验证
& $dest --version
Write-Host ''
Write-Host 'OK - 安装完成！请重新打开一个终端（或新开窗口），然后在任意目录输入 tpfile 即可。'
