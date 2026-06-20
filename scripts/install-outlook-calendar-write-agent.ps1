param(
  [Parameter(Mandatory = $true)]
  [string]$BinaryPath,

  [Parameter(Mandatory = $true)]
  [string]$AgentSecret,

  [string]$BusUrl = "http://beelink:8080",
  [string]$AgentId = "ucla-tdg-outlook-calendar-write-agent",
  [string]$HttpAddr = ":8219",
  [string]$TaskName = "",
  [string]$DryRun = "true",
  [string]$HoldRequesters = "",
  [string]$InstallDir = "$env:LOCALAPPDATA\calendar-agents\outlook-calendar-write-agent"
)

$ErrorActionPreference = "Stop"

if (-not (Test-Path $BinaryPath)) {
  throw "BinaryPath not found: $BinaryPath"
}

if ([string]::IsNullOrWhiteSpace($TaskName)) {
  $TaskName = $AgentId
}

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

$binaryDest = Join-Path $InstallDir "outlook-calendar-write-agent.exe"
Copy-Item -Force -Path $BinaryPath -Destination $binaryDest

$secretPath = Join-Path $InstallDir "agent-secret.dpapi"
$secureSecret = ConvertTo-SecureString $AgentSecret -AsPlainText -Force
$secureSecret | ConvertFrom-SecureString | Set-Content -NoNewline -Path $secretPath

$launcherPath = Join-Path $InstallDir "run-agent.ps1"
@"
`$ErrorActionPreference = "Stop"
`$secretFile = "$secretPath"
`$secureSecret = Get-Content -Raw -Path `$secretFile | ConvertTo-SecureString
`$bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR(`$secureSecret)
try {
  `$env:OUTLOOK_CALENDAR_WRITE_AGENT_SECRET = [Runtime.InteropServices.Marshal]::PtrToStringBSTR(`$bstr)
  `$env:BUS_URL = "$BusUrl"
  `$env:OUTLOOK_CALENDAR_WRITE_AGENT_ID = "$AgentId"
  `$env:OUTLOOK_CALENDAR_WRITE_HTTP_ADDR = "$HttpAddr"
  `$env:OUTLOOK_CALENDAR_WRITE_DRY_RUN = "$DryRun"
  `$env:OUTLOOK_CALENDAR_WRITE_HOLD_REQUESTERS = "$HoldRequesters"
  & "$binaryDest"
} finally {
  if (`$bstr -ne [IntPtr]::Zero) {
    [Runtime.InteropServices.Marshal]::ZeroFreeBSTR(`$bstr)
  }
}
"@ | Set-Content -Path $launcherPath

try {
  $acl = Get-Acl $InstallDir
  $acl.SetAccessRuleProtection($true, $false)
  $rule = New-Object System.Security.AccessControl.FileSystemAccessRule($env:USERNAME, "FullControl", "ContainerInherit,ObjectInherit", "None", "Allow")
  $acl.SetAccessRule($rule)
  Set-Acl -Path $InstallDir -AclObject $acl
} catch {
  Write-Warning "Set-Acl hardening failed on ${InstallDir}: $($_.Exception.Message)"
  $icacls = Join-Path $env:SystemRoot "System32\icacls.exe"
  if (Test-Path $icacls) {
    & $icacls $InstallDir /inheritance:r /grant:r "${env:USERNAME}:F" /T /C | Out-Host
  } else {
    Write-Warning "icacls.exe not found; leaving default profile ACLs in place."
  }
}

$action = New-ScheduledTaskAction -Execute "powershell.exe" -Argument "-NoProfile -ExecutionPolicy Bypass -File `"$launcherPath`""
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$settings = New-ScheduledTaskSettingsSet -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -AllowStartIfOnBatteries
Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Settings $settings -Description "Registers Joel's writable Outlook calendar guard-block agent with a Pinakes bus." -Force | Out-Null

Start-ScheduledTask -TaskName $TaskName
Write-Host "Installed and started $TaskName"
Write-Host "Agent: $AgentId"
Write-Host "Bus: $BusUrl"
Write-Host "DryRun: $DryRun"
Write-Host "HoldRequesters: $HoldRequesters"
Write-Host "Health: http://localhost$HttpAddr/health"
