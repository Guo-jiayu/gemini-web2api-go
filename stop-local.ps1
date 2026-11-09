$ErrorActionPreference = 'Stop'
$exe = Join-Path $PSScriptRoot 'gemini-web2api-go.exe'
$running = Get-Process -Name 'gemini-web2api-go' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $exe }
foreach ($process in $running) {
    Stop-Process -Id $process.Id
    $process.WaitForExit()
}
Write-Output 'Local service stopped.'
