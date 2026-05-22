$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName = 'C:\Users\ethork\binge-server\binge-server.exe'
$psi.WorkingDirectory = 'C:\Users\ethork\binge-server'
$psi.UseShellExecute = $false
$psi.CreateNoWindow = $true
$psi.RedirectStandardOutput = $true
$psi.RedirectStandardError = $true
$psi.EnvironmentVariables['STASH_URL'] = 'http://localhost:9999'
$psi.EnvironmentVariables['STASH_API_KEY'] = 'eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJ1aWQiOiJldGhvcmsiLCJzdWIiOiJBUElLZXkiLCJpYXQiOjE3Nzc5MjYxOTB9.AjePvgdHp1bfEKt0iAowvxYLipBJyeEpS4sLipAxB8k'
# Paste reddit_session cookie value (or full multi-cookie string) here.
$psi.EnvironmentVariables['REDDIT_SESSION_COOKIE'] = ''
$psi.EnvironmentVariables['REDDIT_USER_AGENT'] = 'binge-server/0.1'
$psi.EnvironmentVariables['BINGE_LISTEN_ADDR'] = '0.0.0.0:7878'
$psi.EnvironmentVariables['BINGE_POLL_INTERVAL'] = '4h'
$psi.EnvironmentVariables['BINGE_PERFORMER_SYNC_INTERVAL'] = '24h'

$logPath = 'C:\Users\ethork\binge-server\binge-server.log'
$p = [System.Diagnostics.Process]::Start($psi)
"PID=$($p.Id) StartTime=$($p.StartTime)" | Out-File -FilePath $logPath -Encoding utf8

Register-ObjectEvent -InputObject $p -EventName OutputDataReceived -Action {
    if ($EventArgs.Data) { Add-Content -Path 'C:\Users\ethork\binge-server\binge-server.log' -Value $EventArgs.Data -Encoding utf8 }
} | Out-Null
Register-ObjectEvent -InputObject $p -EventName ErrorDataReceived -Action {
    if ($EventArgs.Data) { Add-Content -Path 'C:\Users\ethork\binge-server\binge-server.log' -Value "ERR: $($EventArgs.Data)" -Encoding utf8 }
} | Out-Null
$p.BeginOutputReadLine()
$p.BeginErrorReadLine()

Write-Host "STARTED PID=$($p.Id)"
