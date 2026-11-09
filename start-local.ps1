$ErrorActionPreference = 'Stop'
$exe = Join-Path $PSScriptRoot 'gemini-web2api-go.exe'
$config = Get-Content (Join-Path $PSScriptRoot 'data/local-config.json') -Raw | ConvertFrom-Json
$baseUrl = "http://127.0.0.1:$($config.port)"
$running = Get-Process -Name 'gemini-web2api-go' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $exe }
if ($running) {
    Write-Output "Already running: PID $($running.Id). $baseUrl/admin/"
    exit 0
}
$p = Start-Process -FilePath $exe -ArgumentList '--config','data/local-config.json' -WorkingDirectory $PSScriptRoot -WindowStyle Hidden -RedirectStandardOutput (Join-Path $PSScriptRoot 'data/stdout.log') -RedirectStandardError (Join-Path $PSScriptRoot 'data/stderr.log') -PassThru
$p.Id | Set-Content (Join-Path $PSScriptRoot 'data/service.pid')
for ($attempt = 0; $attempt -lt 30; $attempt++) {
    if ($p.HasExited) { throw 'Service exited during startup; inspect data/stderr.log.' }
    try {
        $null = Invoke-WebRequest -Uri "$baseUrl/" -TimeoutSec 2
        Write-Output "Started PID $($p.Id). $baseUrl/admin/"
        exit 0
    } catch { Start-Sleep -Milliseconds 300 }
}
throw 'Service did not become ready; inspect data/stderr.log.'
