param(
    [Parameter(Mandatory = $true)][string]$CookieFile,
    [string]$Label = 'local-google-account',
    [int]$AuthUser = 0
)
$ErrorActionPreference = 'Stop'
$config = Get-Content (Join-Path $PSScriptRoot 'data/local-config.json') -Raw | ConvertFrom-Json
$baseUrl = "http://127.0.0.1:$($config.port)"
$cookie = (Get-Content -LiteralPath $CookieFile -Raw).Trim()
if ($cookie -notmatch '(^|;\s*)__Secure-1PSID=') {
    throw 'Expected a full Google Cookie header containing __Secure-1PSID. OAuth/CPA JSON and refresh tokens are not supported.'
}
$headers = @{ Authorization = "Bearer $($config.admin_token)" }
$body = @{ label = $Label; cookie = $cookie; authuser = $AuthUser } | ConvertTo-Json
$account = Invoke-RestMethod -Uri "$baseUrl/admin/api/cookies" -Method Post -Headers $headers -ContentType 'application/json' -Body ([Text.Encoding]::UTF8.GetBytes($body))
Write-Output "Imported account ID: $($account.id)"
Invoke-RestMethod -Uri "$baseUrl/admin/api/cookies/$($account.id)/check" -Method Post -Headers $headers | ConvertTo-Json -Depth 5
