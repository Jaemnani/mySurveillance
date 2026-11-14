# Rebuilds mySurveillance and restarts the copy that serves http://localhost:8787
# with the new build. Cameras, recordings and the password are kept.
#
#   powershell -ExecutionPolicy Bypass -File scripts\redeploy.ps1
$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
$exe = Join-Path $root 'dist\mysurveillance.exe'
$new = Join-Path $root 'dist\mysurveillance.build.exe'

$go = Join-Path $env:LOCALAPPDATA 'Programs\go\bin'
if (Test-Path $go) { $env:Path = "$go;$env:Path" }

Push-Location $root
try {
    go build -tags embedffmpeg -o $new .
    if ($LASTEXITCODE -ne 0) { throw '빌드 실패 - 실행 중인 프로그램은 그대로 둡니다.' }
} finally { Pop-Location }

# Only the copy started from this folder is replaced.
Get-CimInstance Win32_Process -Filter "name='mysurveillance.exe'" |
    Where-Object { $_.ExecutablePath -eq $exe } |
    ForEach-Object { Stop-Process -Id $_.ProcessId -Force }
Start-Sleep -Seconds 2
Move-Item $new $exe -Force

# Started through WMI so it keeps running after this window closes.
$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{
    CommandLine      = "`"$exe`" run --background"
    CurrentDirectory = (Split-Path $exe)
}
if ($r.ReturnValue -ne 0) { throw "실행 실패 (코드 $($r.ReturnValue))" }

foreach ($i in 1..20) {
    Start-Sleep -Seconds 1
    try {
        if ((Invoke-WebRequest 'http://127.0.0.1:8787/' -UseBasicParsing -TimeoutSec 2).Content -match '<title>mySurveillance') {
            Write-Host "업데이트 완료: http://localhost:8787 (pid $($r.ProcessId))"
            exit 0
        }
    } catch {}
}
throw '새 빌드가 8787 포트에서 응답하지 않습니다. 다른 프로그램이 포트를 쓰고 있는지 확인하세요.'
