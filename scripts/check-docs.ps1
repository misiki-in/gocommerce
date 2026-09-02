# Keeps the prose honest against the code.
#
#   .\scripts\check-docs.ps1
#
# Two checks, both mechanical:
#
#   1. Every relative Markdown link resolves to a file that exists.
#   2. Every HTTP route, App accessor and service method quoted in skills/
#      exists in the source or the OpenAPI document.
#
# The reason this is worth automating: a guide naming a route that was renamed
# is worse than no guide, because the reader follows it and blames themselves.
# Prose cannot be compiled, so this is the closest thing available.
#
# Exits non-zero on any problem, so CI can gate on it.

param([string]$Repo = (Split-Path -Parent $PSScriptRoot))

$ErrorActionPreference = 'Stop'
$problems = 0

# ------------------------------------------------------------------- links

$docs = Get-ChildItem $Repo -Recurse -Filter *.md |
    Where-Object { $_.FullName -notmatch 'node_modules|admin\\build|\.svelte-kit' }

$links = 0
foreach ($f in $docs) {
    $text = [IO.File]::ReadAllText($f.FullName)
    foreach ($m in [regex]::Matches($text, '\[[^\]]*\]\(([^)\s]+)\)')) {
        $target = $m.Groups[1].Value
        if ($target -match '^(https?:|mailto:|#)') { continue }
        $path = ($target -split '#')[0]
        if (-not $path) { continue }
        $links++
        if (-not (Test-Path (Join-Path $f.DirectoryName $path))) {
            $problems++
            "  BROKEN LINK      {0} -> {1}" -f $f.FullName.Substring($Repo.Length + 1), $target
        }
    }
}

# ------------------------------------------------------------------ skills

$skills = Get-ChildItem "$Repo\skills" -Filter *.md -ErrorAction SilentlyContinue
if ($skills) {
    $goSrc = (Get-ChildItem $Repo -Recurse -Filter *.go |
        Where-Object { $_.FullName -notmatch 'admin\\build' } |
        ForEach-Object { [IO.File]::ReadAllText($_.FullName) }) -join "`n"
    # A missing spec is a finding, not a crash - the run must survive to report
    # everything else it knows.
    $specPath = Join-Path $Repo 'core\openapi.json'
    if (-not (Test-Path $specPath)) {
        "  MISSING SPEC     core\openapi.json (openapi.go embeds it beside itself)"
        $problems++
        $spec = ''
    } else {
        $spec = [IO.File]::ReadAllText($specPath)
    }

    # Good docs use concrete examples ("/api/admin/orders/42", "/api/checkout/cod")
    # where the spec uses placeholders. Collapse both to one shape before
    # comparing, or every worked example reads as a broken route.
    $providerCodes = @('cod', 'stripe', 'razorpay')
    function Normalize([string]$p) {
        $p = [regex]::Replace($p, '\{[A-Za-z0-9_.]+\}', '{X}')
        $p = [regex]::Replace($p, '/\d+', '/{X}')             # /42
        $p = [regex]::Replace($p, '/[A-Z]{2,}-[\w-]+', '/{X}') # /GC-000042
        foreach ($code in $providerCodes) { $p = $p.Replace("/$code", '/{X}') }
        return $p
    }

    $known = @()
    $known += [regex]::Matches($spec, '"(/api/[^"]*)"') | ForEach-Object { Normalize $_.Groups[1].Value }
    $known += [regex]::Matches($goSrc, '"(?:GET|POST|PATCH|PUT|DELETE) (/api/[^"]+)"') |
        ForEach-Object { Normalize $_.Groups[1].Value }

    $routes = @{}; $methods = @{}; $accessors = @{}
    foreach ($f in $skills) {
        $text = [IO.File]::ReadAllText($f.FullName)
        foreach ($m in [regex]::Matches($text, '(?m)\b(GET|POST|PATCH|PUT|DELETE)\s+(/api/[A-Za-z0-9/_{}\-]+)')) {
            $k = $m.Groups[2].Value.TrimEnd('/')
            if (-not $routes.ContainsKey($k)) { $routes[$k] = $f.Name }
        }
        # app.DB() returns a *sql.DB, so what follows is database/sql's surface.
        foreach ($m in [regex]::Matches($text, 'app\.(?!DB\(\))[A-Z][A-Za-z]*\(\)\.([A-Z][A-Za-z0-9]*)\(')) {
            $k = $m.Groups[1].Value
            if (-not $methods.ContainsKey($k)) { $methods[$k] = $f.Name }
        }
        foreach ($m in [regex]::Matches($text, 'app\.([A-Z][A-Za-z]*)\(\)')) {
            $k = $m.Groups[1].Value
            if (-not $accessors.ContainsKey($k)) { $accessors[$k] = $f.Name }
        }
    }

    foreach ($path in ($routes.Keys | Sort-Object)) {
        if ($known -notcontains (Normalize $path)) {
            $problems++
            "  ROUTE NOT FOUND  {0,-44} (cited in {1})" -f $path, $routes[$path]
        }
    }
    foreach ($name in ($methods.Keys | Sort-Object)) {
        if ($goSrc -notmatch "func \([a-z]+ \*[A-Za-z]+\) $name\(") {
            $problems++
            "  METHOD NOT FOUND {0,-44} (cited in {1})" -f $name, $methods[$name]
        }
    }
    foreach ($name in ($accessors.Keys | Sort-Object)) {
        if ($goSrc -notmatch "func \(a \*App\) $name\(") {
            $problems++
            "  ACCESSOR MISSING {0,-44} (cited in {1})" -f $name, $accessors[$name]
        }
    }

    # Every skill must declare what it is for, or it cannot be found by the
    # reader — human or agent — who needs it.
    foreach ($f in ($skills | Where-Object { $_.Name -ne 'README.md' })) {
        $head = Get-Content $f.FullName -Encoding UTF8 -TotalCount 4
        if (($head[0] -ne '---') -or -not ($head -match '^name:') -or -not ($head -match '^description:')) {
            $problems++
            "  NO FRONTMATTER   skills/{0}" -f $f.Name
        }
    }

    "checked {0} links, {1} routes, {2} methods, {3} accessors across {4} skills" -f `
        $links, $routes.Count, $methods.Count, $accessors.Count, $skills.Count
} else {
    # A silent skip here once meant the skills-vs-code checks could vanish
    # without anyone noticing. Their absence is itself a failure.
    "  MISSING          skills/ - the skills-vs-code checks did not run"
    $problems++
}

# D25: the engine lives in core/. A .go file at the repo root is drift, and
# this check is what makes the layout enforced rather than remembered.
$strayGo = Get-ChildItem $Repo -Filter *.go -File -ErrorAction SilentlyContinue
if ($strayGo) {
    foreach ($g in $strayGo) {
        "  STRAY GO FILE    {0} - engine lives in core/ (D25)" -f $g.Name
    }
    $problems += $strayGo.Count
}

if ($problems -gt 0) {
    Write-Host ""
    Write-Host "$problems problem(s)" -ForegroundColor Red
    exit 1
}
Write-Host ""
Write-Host "docs agree with the code" -ForegroundColor Green
