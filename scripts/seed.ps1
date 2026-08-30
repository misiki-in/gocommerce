# Seeds a dev store with a small catalog, so the API has something to browse.
#
#   .\scripts\seed.ps1
#
# Safe to re-run: products are matched by slug and skipped if they already
# exist, rather than piling up duplicates.

param(
    [string]$BaseUrl = $env:GC_BASE,
    [string]$Token   = $env:GC_TOKEN
)

$ErrorActionPreference = 'Stop'
if ($BaseUrl) { $env:GC_BASE = $BaseUrl }
if ($Token)   { $env:GC_TOKEN = $Token }
. "$PSScriptRoot\gc.ps1"

Write-Host "Seeding $(Get-GCBase)" -ForegroundColor Cyan

$catalog = @(
    @{
        slug = 'cotton-tee'; title = 'Cotton tee'; status = 'active'
        description = 'A plain cotton t-shirt. The variant is what you actually buy.'
        options = @(
            @{ name = 'Size';  values = @('S', 'M', 'L') },
            @{ name = 'Colour'; values = @('Black', 'White') }
        )
        variants = @(
            @{ sku = 'TEE-S-BLK'; price_minor = 2500; options = @('S', 'Black'); stock_on_hand = 12; weight_grams = 180 },
            @{ sku = 'TEE-M-BLK'; price_minor = 2500; options = @('M', 'Black'); stock_on_hand = 8;  weight_grams = 190 },
            @{ sku = 'TEE-L-BLK'; price_minor = 2500; options = @('L', 'Black'); stock_on_hand = 3;  weight_grams = 200 },
            @{ sku = 'TEE-M-WHT'; price_minor = 2500; options = @('M', 'White'); stock_on_hand = 5;  weight_grams = 190 },
            # Deliberately out of stock, so checkout's conflict path is easy to try.
            @{ sku = 'TEE-L-WHT'; price_minor = 2500; options = @('L', 'White'); stock_on_hand = 0;  weight_grams = 200 }
        )
    },
    @{
        slug = 'enamel-mug'; title = 'Enamel mug'; status = 'active'
        description = 'One size, so it has a single default variant and no options.'
        sku = 'MUG-001'; price_minor = 1400; stock = 40
    },
    @{
        slug = 'gift-card'; title = 'Gift card'; status = 'active'
        description = 'Never runs out: track_inventory is false.'
        variants = @(
            @{ sku = 'GIFT-25'; price_minor = 2500; track_inventory = $false },
            @{ sku = 'GIFT-50'; price_minor = 5000; track_inventory = $false }
        )
        options = @(@{ name = 'Value'; values = @('25', '50') })
    },
    @{
        slug = 'winter-scarf'; title = 'Winter scarf'; status = 'draft'
        description = 'A draft: visible to admin, invisible to shoppers.'
        sku = 'SCARF-001'; price_minor = 3200; stock = 6
    }
)

# The gift card variants need their option values; fill them in.
$catalog[2].variants[0].options = @('25')
$catalog[2].variants[1].options = @('50')

$created = 0
$skipped = 0
foreach ($product in $catalog) {
    try {
        $existing = Invoke-GC GET "/api/products/slug/$($product.slug)"
        if ($existing) { $skipped++; Write-Host "  skip  $($product.slug) (already exists)"; continue }
    } catch {
        # Not found is the expected case on a fresh database. A draft product is
        # also invisible here, so fall through and let the create report a
        # conflict if it really is there.
    }

    try {
        $result = Invoke-GC POST '/api/admin/products' $product -Admin
        $created++
        Write-Host ("  new   {0}  ({1} variant(s), id {2})" -f $product.slug, $result.variants.Count, $result.id) -ForegroundColor DarkGreen
    } catch {
        if ("$_" -match 'conflict') { $skipped++; Write-Host "  skip  $($product.slug) (already exists)" }
        else { throw }
    }
}

Write-Host ''
Write-Host "Seeded: $created created, $skipped already present." -ForegroundColor Green
Write-Host "Browse the catalog:  $(Get-GCBase)/api/products"
Write-Host "Browse the API docs: $(Get-GCBase)/docs"
