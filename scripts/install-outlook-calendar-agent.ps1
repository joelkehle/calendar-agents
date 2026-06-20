param(
  [Parameter(Mandatory = $true)]
  [string]$BinaryPath,

  [Parameter(Mandatory = $true)]
  [string]$AgentSecret,

  [string]$BusUrl = "http://beelink:8081",
  [string]$AgentId = "jk-outlook-calendar-agent",
  [string]$HttpAddr = ":8220",
  [string]$TaskName = "",
  [switch]$IncludePrivateDetails,
  [switch]$RedactPrivateDetails,
  [string]$InstallDir = "$env:LOCALAPPDATA\calendar-agents\outlook-calendar-agent"
)

$ErrorActionPreference = "Stop"

if (-not (Test-Path $BinaryPath)) {
  throw "BinaryPath not found: $BinaryPath"
}

if ($IncludePrivateDetails -and $RedactPrivateDetails) {
  throw "Use only one of -IncludePrivateDetails or -RedactPrivateDetails."
}

if ([string]::IsNullOrWhiteSpace($TaskName)) {
  $TaskName = $AgentId
}

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

$binaryDest = Join-Path $InstallDir "outlook-calendar-agent.exe"
Copy-Item -Force -Path $BinaryPath -Destination $binaryDest

$secretPath = Join-Path $InstallDir "agent-secret.dpapi"
$secureSecret = ConvertTo-SecureString $AgentSecret -AsPlainText -Force
$secureSecret | ConvertFrom-SecureString | Set-Content -NoNewline -Path $secretPath

$launcherPath = Join-Path $InstallDir "run-agent.ps1"
$includePrivate = if ($RedactPrivateDetails) { "false" } else { "true" }
@"
`$ErrorActionPreference = "Stop"
`$secretFile = "$secretPath"
`$secureSecret = Get-Content -Raw -Path `$secretFile | ConvertTo-SecureString
`$bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR(`$secureSecret)
try {
  `$env:OUTLOOK_CALENDAR_AGENT_SECRET = [Runtime.InteropServices.Marshal]::PtrToStringBSTR(`$bstr)
  `$env:BUS_URL = "$BusUrl"
  `$env:OUTLOOK_CALENDAR_AGENT_ID = "$AgentId"
  `$env:OUTLOOK_CALENDAR_HTTP_ADDR = "$HttpAddr"
  `$env:OUTLOOK_CALENDAR_INCLUDE_PRIVATE_DETAILS = "$includePrivate"
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
Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Settings $settings -Description "Registers Joel's Outlook calendar with a Pinakes bus." -Force | Out-Null

Start-ScheduledTask -TaskName $TaskName
Write-Host "Installed and started $TaskName"
Write-Host "Agent: $AgentId"
Write-Host "Bus: $BusUrl"
Write-Host "Health: http://localhost$HttpAddr/health"
