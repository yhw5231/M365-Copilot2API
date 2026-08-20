<#
.SYNOPSIS
    M365-Copilot2API 工作区环境自检脚本

.DESCRIPTION
    验证当前目录是否为有效的项目工作区，检查关键文件、工具链和
    服务运行状态。输出统一格式，供代理或人工判断环境可用性。

    预期输出（成功时）：
      Workspace: <path>
      go.mod: OK
      cmd/server: OK
      internal/web: OK
      web/index.html: OK
      Go: OK
      Node.js: OK
      Server PID: <pid>
      Workspace verification: PASSED

    预期输出（失败时）：
      Workspace: <path>
      go.mod: NOT FOUND
      Workspace verification: FAILED
#>

$ErrorActionPreference = "Stop"
$root = $PWD

Write-Output "Workspace: $root"
Write-Output ""
Write-Output "--- File checks ---"

# 关键项目文件
$checks = @(
    @{Name = "go.mod"; Path = "go.mod" },
    @{Name = "cmd/server"; Path = "cmd/server" },
    @{Name = "internal/web"; Path = "internal/web" },
    @{Name = "web/index.html"; Path = "web/index.html" },
    @{Name = "AGENTS.md"; Path = "AGENTS.md" },
    @{Name = "manage.py"; Path = "manage.py" }
)

$allOk = $true
foreach ($check in $checks) {
    $full = Join-Path $root $check.Path
    if (Test-Path -LiteralPath $full) {
        Write-Output ("  {0}: OK" -f $check.Name)
    }
    else {
        Write-Output ("  {0}: NOT FOUND" -f $check.Name)
        $allOk = $false
    }
}

Write-Output ""
Write-Output "--- Toolchain checks ---"

# Go
try {
    $goVer = go version
    Write-Output "  Go: $goVer"
}
catch {
    Write-Output "  Go: NOT FOUND"
    $allOk = $false
}

# Node.js
try {
    $nodeVer = node --version
    Write-Output "  Node.js: $nodeVer"
}
catch {
    Write-Output "  Node.js: NOT FOUND"
    $allOk = $false
}

# Python (manage.py 需要)
try {
    $pyVer = python --version
    Write-Output "  Python: $pyVer"
}
catch {
    Write-Output "  Python: NOT FOUND"
    $allOk = $false
}

Write-Output ""
Write-Output "--- Server status ---"

# 检查服务进程
$proc = Get-Process -Name 'm365-copilot2api' -ErrorAction SilentlyContinue
if ($proc) {
    Write-Output "  Server PID: $($proc.Id) (running since $($proc.StartTime))"
    try {
        $resp = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:9090/" -TimeoutSec 5
        Write-Output ("  Health check: HTTP {0}" -f $resp.StatusCode)
    }
    catch {
        Write-Output "  Health check: FAILED ($($_.Exception.Message))"
        $allOk = $false
    }
}
else {
    Write-Output "  Server: NOT RUNNING"
}

Write-Output ""
if ($allOk) {
    Write-Output "Workspace verification: PASSED"
}
else {
    Write-Output "Workspace verification: FAILED"
}