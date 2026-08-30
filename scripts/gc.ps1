# Shared helpers for the gocommerce dev scripts.
#
# Dot-source this, don't run it:
#   . .\scripts\gc.ps1

$script:GCBase  = if ($env:GC_BASE)  { $env:GC_BASE }  else { 'http://127.0.0.1:8080' }
$script:GCToken = if ($env:GC_TOKEN) { $env:GC_TOKEN } else { 'dev-token' }

function Get-GCBase { $script:GCBase }

# Invoke-GC sends one API request and returns the decoded "data" member.
#
# An error envelope is turned into a PowerShell error carrying the engine's own
# message, so a failing script says "only 2 left in stock" rather than
# "response status 409".
function Invoke-GC {
    param(
        [Parameter(Mandatory)][ValidateSet('GET','POST','PATCH','DELETE')][string]$Method,
        [Parameter(Mandatory)][string]$Path,
        $Body,
        [switch]$Admin,
        [hashtable]$Headers = @{},
        [switch]$Raw
    )

    $params = @{
        Method      = $Method
        Uri         = "$script:GCBase$Path"
        UseBasicParsing = $true
        TimeoutSec  = 20
        Headers     = $Headers.Clone()
    }
    if ($Admin) { $params.Headers['Authorization'] = "Bearer $script:GCToken" }
    if ($null -ne $Body) {
        $params.Body = if ($Body -is [string]) { $Body } else { $Body | ConvertTo-Json -Depth 10 -Compress }
        $params.ContentType = 'application/json'
    }

    try {
        $response = Invoke-WebRequest @params
    } catch {
        # Windows PowerShell 5.1 has usually already consumed the response
        # stream by the time the exception surfaces, and puts the body in
        # ErrorDetails.Message instead. Read that first, or the engine's own
        # explanation is lost and every failure reads as a bare status code.
        $detail = $_.ErrorDetails.Message
        if (-not $detail -and $_.Exception.Response) {
            try {
                $reader = New-Object System.IO.StreamReader($_.Exception.Response.GetResponseStream())
                $detail = $reader.ReadToEnd()
            } catch { }
        }

        $code = ''
        if ($_.Exception.Response) { $code = [int]$_.Exception.Response.StatusCode }

        if ($detail) {
            $envelope = $null
            try { $envelope = $detail | ConvertFrom-Json } catch { }
            if ($envelope -and $envelope.error) {
                $message = "$Method $Path -> $code $($envelope.error.code): $($envelope.error.message)"
                if ($envelope.error.details) {
                    $message += " | details: " + ($envelope.error.details | ConvertTo-Json -Depth 5 -Compress)
                }
                throw $message
            }
            throw "$Method $Path -> $code $detail"
        }
        throw "$Method $Path -> $($_.Exception.Message)"
    }

    if ($Raw) { return $response.Content }
    if (-not $response.Content) { return $null }
    $parsed = $response.Content | ConvertFrom-Json
    if ($null -ne $parsed.PSObject.Properties['data']) { return $parsed.data }
    return $parsed
}

# Small assertion helpers so a script reads as a list of expectations.
$script:GCChecks = 0
$script:GCFailures = 0

function Confirm-GC {
    param([Parameter(Mandatory)][string]$What, [Parameter(Mandatory)]$Expected, [Parameter(Mandatory)]$Actual)
    $script:GCChecks++
    if ("$Expected" -eq "$Actual") {
        Write-Host ("  ok    {0}: {1}" -f $What, $Actual) -ForegroundColor DarkGreen
    } else {
        $script:GCFailures++
        Write-Host ("  FAIL  {0}: got '{1}', want '{2}'" -f $What, $Actual, $Expected) -ForegroundColor Red
    }
}

function Confirm-GCTrue {
    param([Parameter(Mandatory)][string]$What, [Parameter(Mandatory)][bool]$Condition)
    Confirm-GC -What $What -Expected $true -Actual $Condition
}

function Write-GCStep { param([string]$Text) Write-Host "`n$Text" -ForegroundColor Cyan }

function Write-GCSummary {
    Write-Host ''
    if ($script:GCFailures -eq 0) {
        Write-Host "All $script:GCChecks checks passed." -ForegroundColor Green
        return 0
    }
    Write-Host "$script:GCFailures of $script:GCChecks checks failed." -ForegroundColor Red
    return 1
}
