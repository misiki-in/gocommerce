# Starts a gocommerce dev server, creating its database if needed.
#
#   .\scripts\dev.ps1              # foreground, Ctrl+C to stop
#   .\scripts\dev.ps1 -Seed        # ...and load a demo catalog first
#   .\scripts\dev.ps1 -Reset       # ...starting from an empty database
#
# The dev store runs on its own database so nothing here can touch your test
# database or anything else on the cluster.
#
# Two credentials exist and they are for different things. The admin token is
# for scripts (seed.ps1, smoke.ps1, curl). The email and password are for the
# panel, because a person signing in to a browser should not be pasting a
# shared secret. The server accepts either on the same routes.

param(
    [int]$Port = 8080,
    [string]$Database = 'gocommerce_dev',
    [string]$Token = 'dev-token',
    [string]$Email = 'admin@example.com',
    [string]$Password = 'devpassword',
    [string]$MediaDir = '',
    [string]$PgHost = '127.0.0.1',
    [int]$PgPort = 5433,
    [string]$PgUser = 'gocommerce',
    [switch]$Seed,
    [switch]$Reset
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot

# Find the PostgreSQL client tools.
$pgbin = @(
    'C:\Program Files\PostgreSQL\18\bin',
    'C:\Program Files\PostgreSQL\17\bin',
    'C:\Program Files\PostgreSQL\16\bin'
) | Where-Object { Test-Path (Join-Path $_ 'createdb.exe') } | Select-Object -First 1

if (-not $pgbin) {
    Write-Host 'Could not find createdb.exe. Create the database yourself and set DATABASE_URL.' -ForegroundColor Yellow
} else {
    if ($Reset) {
        Write-Host "Dropping $Database" -ForegroundColor DarkYellow
        & "$pgbin\dropdb.exe" -h $PgHost -p $PgPort -U $PgUser --if-exists $Database 2>&1 | Out-Null
    }
    & "$pgbin\createdb.exe" -h $PgHost -p $PgPort -U $PgUser $Database 2>&1 | Out-Null
}

# Go is installed outside the machine PATH on this box.
$goBin = 'C:\Users\LENOVO\go-sdk\go\bin'
if ((Test-Path $goBin) -and ($env:Path -notlike "*$goBin*")) { $env:Path += ";$goBin" }

Write-Host 'Building...' -ForegroundColor DarkGray
Push-Location $repo
try {
    & go build -o "$repo\gocommerce.exe" ./cmd/gocommerce
    if ($LASTEXITCODE -ne 0) { throw 'build failed' }
} finally { Pop-Location }

$env:DATABASE_URL = "postgres://$PgUser@${PgHost}:$PgPort/$Database?sslmode=disable"
$base = "http://127.0.0.1:$Port"

# Create the panel operator. Bootstrap only acts when there is none, so this is
# safe to run against an existing database on every start.
$env:GOCOMMERCE_ADMIN_EMAIL = $Email
$env:GOCOMMERCE_ADMIN_PASSWORD = $Password

# Somewhere to put uploads. Kept beside the repo rather than inside it, so a
# stray 40MB video never lands in a commit.
if (-not $MediaDir) { $MediaDir = Join-Path (Split-Path -Parent $repo) 'gocommerce-media' }
New-Item -ItemType Directory -Force -Path $MediaDir | Out-Null
$env:GOCOMMERCE_MEDIA_DIR = $MediaDir

function Show-Banner {
    Write-Host ''
    Write-Host "Serving on $base" -ForegroundColor Green
    Write-Host "  panel:    $base/"
    Write-Host "  docs:     $base/docs"
    Write-Host "  contract: $base/doc"
    Write-Host ''
    Write-Host "  sign in:  $Email / $Password" -ForegroundColor Cyan
    Write-Host "  token:    $Token   (for scripts and curl)"
    Write-Host "  database: $Database on $PgHost`:$PgPort"
    Write-Host "  media:    $MediaDir"
    Write-Host ''
}

if ($Seed) {
    # Seeding needs the server up, so start it, seed, and leave it running.
    $server = Start-Process -FilePath "$repo\gocommerce.exe" `
        -ArgumentList '-addr', "127.0.0.1:$Port", '-admin-token', $Token, 'serve' `
        -PassThru -NoNewWindow

    foreach ($i in 1..40) {
        try { Invoke-WebRequest "$base/health" -UseBasicParsing -TimeoutSec 2 | Out-Null; break }
        catch { Start-Sleep -Milliseconds 400 }
    }

    $env:GC_BASE = $base
    $env:GC_TOKEN = $Token
    & "$PSScriptRoot\seed.ps1"

    Show-Banner
    Write-Host 'Ctrl+C to stop' -ForegroundColor DarkGray
    Wait-Process -Id $server.Id
    return
}

Show-Banner

& "$repo\gocommerce.exe" -addr "127.0.0.1:$Port" -admin-token $Token serve
