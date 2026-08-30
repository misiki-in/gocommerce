# Builds the single executable: the API and the admin panel in one file.
#
#   .\scripts\build.ps1                    # build for this machine
#   .\scripts\build.ps1 -SkipPanel         # reuse the committed panel build
#   .\scripts\build.ps1 -NoPanel           # API only (-tags no_admin)
#   .\scripts\build.ps1 -All               # cross-compile for every platform
#
# The panel is a SvelteKit app compiled to static files and embedded with
# go:embed. Once built, the result needs no Node.js, no separate web server and
# no configuration beyond a database URL.
#
# This file is deliberately plain ASCII: Windows PowerShell 5.1 reads .ps1
# files as ANSI unless they carry a BOM, and a stray typographic dash is enough
# to make the whole script fail to parse.

param(
    [string]$Output = "gocommerce.exe",
    [switch]$SkipPanel,
    [switch]$NoPanel,
    [switch]$All,
    [string]$Version = "dev"
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
Push-Location $repo

try {
    # Go lives outside the machine PATH on this box.
    $goBin = "C:\Users\LENOVO\go-sdk\go\bin"
    if ((Test-Path $goBin) -and ($env:Path -notlike "*$goBin*")) { $env:Path += ";$goBin" }

    # ---------------------------------------------------------------- panel

    if ($NoPanel) {
        Write-Host "Skipping the admin panel entirely (-tags no_admin)." -ForegroundColor DarkYellow
    } elseif ($SkipPanel) {
        if (-not (Test-Path "$repo\admin\build\index.html")) {
            throw "admin\build\index.html is missing. Run without -SkipPanel first."
        }
        Write-Host "Reusing the existing panel build." -ForegroundColor DarkGray
    } else {
        Write-Host "Building the admin panel..." -ForegroundColor Cyan
        Push-Location "$repo\admin"
        try {
            if (-not (Test-Path "node_modules")) {
                Write-Host "  installing dependencies..." -ForegroundColor DarkGray
                & npm install --no-fund --no-audit
                if ($LASTEXITCODE -ne 0) { throw "npm install failed" }
            }
            & npm run build
            if ($LASTEXITCODE -ne 0) { throw "the panel build failed" }
        } finally { Pop-Location }
    }

    if (-not $NoPanel) {
        $files = Get-ChildItem "$repo\admin\build" -Recurse -File
        $bytes = ($files | Measure-Object Length -Sum).Sum
        Write-Host ("  panel: {0} files, {1:N0} bytes" -f $files.Count, $bytes) -ForegroundColor DarkGray
    }

    # ------------------------------------------------------------- binaries

    $tags = if ($NoPanel) { @("-tags", "no_admin") } else { @() }
    $ldflags = "-s -w -X main.version=$Version"

    function Build-One($goos, $goarch, $out) {
        $env:GOOS = $goos
        $env:GOARCH = $goarch
        $env:CGO_ENABLED = "0"
        & go build @tags -ldflags $ldflags -o $out ./cmd/gocommerce
        $code = $LASTEXITCODE
        Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
        if ($code -ne 0) { throw "build failed for $goos/$goarch" }
        Write-Host ("  {0,-40} {1,12:N0} bytes" -f $out, (Get-Item $out).Length) -ForegroundColor DarkGreen
    }

    Write-Host "Building the binary..." -ForegroundColor Cyan

    if ($All) {
        New-Item -ItemType Directory -Force "$repo\dist" | Out-Null
        # Pure Go with CGO off, so every target cross-compiles from any host.
        Build-One "windows" "amd64" "dist\gocommerce-windows-amd64.exe"
        Build-One "windows" "arm64" "dist\gocommerce-windows-arm64.exe"
        Build-One "linux"   "amd64" "dist\gocommerce-linux-amd64"
        Build-One "linux"   "arm64" "dist\gocommerce-linux-arm64"
        Build-One "darwin"  "amd64" "dist\gocommerce-darwin-amd64"
        Build-One "darwin"  "arm64" "dist\gocommerce-darwin-arm64"
    } else {
        & go build @tags -ldflags $ldflags -o $Output ./cmd/gocommerce
        if ($LASTEXITCODE -ne 0) { throw "build failed" }
        Write-Host ("  {0,-40} {1,12:N0} bytes" -f $Output, (Get-Item $Output).Length) -ForegroundColor DarkGreen
    }

    Write-Host ""
    Write-Host "Done. One file, containing the API and the admin panel." -ForegroundColor Green
    if (-not $NoPanel) {
        Write-Host "  Run it:   .\$Output -db <database-url> -admin-token <token> serve"
        Write-Host "  Panel:    http://127.0.0.1:8080/"
        Write-Host "  API docs: http://127.0.0.1:8080/docs"
    }
} finally {
    Pop-Location
}
