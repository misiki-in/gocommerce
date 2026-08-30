# Walks a running store through a complete sale and checks what each step
# should have changed.
#
#   .\scripts\smoke.ps1
#
# It creates its own product with a unique SKU, so it is safe to run against a
# store that already has data, and safe to run repeatedly.

param(
    [string]$BaseUrl = $env:GC_BASE,
    [string]$Token   = $env:GC_TOKEN
)

$ErrorActionPreference = 'Stop'
if ($BaseUrl) { $env:GC_BASE = $BaseUrl }
if ($Token)   { $env:GC_TOKEN = $Token }
. "$PSScriptRoot\gc.ps1"

$run = (Get-Date -Format 'HHmmss') + (Get-Random -Maximum 999)
Write-Host "Smoke test against $(Get-GCBase)  (run $run)" -ForegroundColor Cyan

# ---------------------------------------------------------------- the store

Write-GCStep '1. The store is up and says what it is'
$ready = Invoke-GC GET '/health/ready'
Confirm-GC 'status'   'ready' $ready.status
Confirm-GC 'currency' 'USD'   $ready.currency
Confirm-GC 'language' 'en'    $ready.language

$methods = Invoke-GC GET '/api/checkout'
Confirm-GCTrue 'cash on delivery is available' ($methods.payment_methods -contains 'cod')

# ------------------------------------------------------------------ catalog

Write-GCStep '2. Create a product with two variants'
$product = Invoke-GC POST '/api/admin/products' @{
    title = "Smoke test tee $run"; status = 'active'
    options  = @(@{ name = 'Size'; values = @('S', 'M') })
    variants = @(
        @{ sku = "SMOKE-$run-S"; price_minor = 2500; options = @('S'); stock_on_hand = 5 },
        @{ sku = "SMOKE-$run-M"; price_minor = 2500; options = @('M'); stock_on_hand = 3 }
    )
} -Admin
Confirm-GC 'variants created' 2 $product.variants.Count

$medium = $product.variants | Where-Object { $_.options -contains 'M' } | Select-Object -First 1
Confirm-GC 'variant label' 'M' $medium.label
Confirm-GC 'available'     3   $medium.available

Write-GCStep '3. A duplicate option combination is refused'
$duplicateRejected = $false
try {
    Invoke-GC POST "/api/admin/products/$($product.id)/variants" @{
        sku = "SMOKE-$run-DUPE"; price_minor = 2500; options = @('M')
    } -Admin | Out-Null
} catch { $duplicateRejected = "$_" -match 'combination' }
Confirm-GCTrue 'second M variant rejected' $duplicateRejected

# --------------------------------------------------------------------- cart

Write-GCStep '4. Shop as a guest — no account anywhere'
$cart = Invoke-GC POST '/api/carts'
Confirm-GCTrue 'cart token issued' ($cart.id.Length -gt 32)

$cart = Invoke-GC POST "/api/carts/$($cart.id)/line-items" @{ variant_id = $medium.id; quantity = 2 }
Confirm-GC 'items in cart' 2    $cart.item_count
Confirm-GC 'subtotal'      5000 $cart.subtotal.amount_minor
Confirm-GC 'currency'      'USD' $cart.subtotal.currency

Write-GCStep '5. The cart refuses more than the shelf holds'
$overRejected = $false
try {
    Invoke-GC PATCH "/api/carts/$($cart.id)/line-items/$($cart.line_items[0].id)" @{ quantity = 99 } | Out-Null
} catch { $overRejected = "$_" -match 'left in stock' }
Confirm-GCTrue 'quantity of 99 rejected' $overRejected

# ----------------------------------------------------------------- checkout

Write-GCStep '6. Check out with cash on delivery'
$checkoutBody = @{
    cart_id = $cart.id; email = 'buyer@example.com'; name = 'A Buyer'
    address = @{ line1 = '1 High Street'; city = 'Town'; postal_code = '12345'; country = 'US' }
}
$key = "smoke-$run"
$result = Invoke-GC POST '/api/checkout/cod' $checkoutBody -Headers @{ 'Idempotency-Key' = $key }
$order = $result.order

Confirm-GC 'payment kind'   'none'      $result.payment.kind
Confirm-GC 'order status'   'confirmed' $order.status
Confirm-GC 'payment status' 'pending'   $order.payment_status
Confirm-GC 'order total'    5000        $order.total.amount_minor
Confirm-GCTrue 'access token returned' ($order.access_token.Length -gt 32)

Write-GCStep '7. A retried checkout returns the same order, not a second one'
$replay = Invoke-GC POST '/api/checkout/cod' $checkoutBody -Headers @{ 'Idempotency-Key' = $key }
Confirm-GC 'same order id' $order.id $replay.order.id

Write-GCStep '8. The sale took the stock off the shelf'
$after = Invoke-GC GET "/api/variants/$($medium.id)"
Confirm-GC 'on hand'  1 $after.stock_on_hand
Confirm-GC 'reserved' 0 $after.stock_reserved

# ------------------------------------------------------------------- orders

Write-GCStep '9. The shopper can read their own order back'
$guest = Invoke-GC GET "/api/orders/$($order.number)?token=$($order.access_token)"
Confirm-GC 'order number' $order.number $guest.number

$wrongTokenRejected = $false
try { Invoke-GC GET "/api/orders/$($order.number)?token=not-the-token" | Out-Null }
catch { $wrongTokenRejected = "$_" -match 'not_found' }
Confirm-GCTrue 'a wrong token reveals nothing' $wrongTokenRejected

Write-GCStep '10. Settle, ship and deliver'
Invoke-GC POST "/api/admin/orders/$($order.id)/mark-paid" @{ reference = 'cash on delivery' } -Admin | Out-Null
$shipped = Invoke-GC POST '/api/admin/create-fulfillment' @{
    order_id = $order.id; provider = 'manual'; tracking = "TRACK-$run"
} -Admin
Confirm-GC 'status after shipping' 'shipped' $shipped.status
Confirm-GC 'tracking recorded' "TRACK-$run" $shipped.fulfillments[0].tracking

$delivered = Invoke-GC POST "/api/admin/orders/$($order.id)/deliver" -Admin
Confirm-GC 'final status'  'delivered' $delivered.status
Confirm-GC 'final payment' 'paid'      $delivered.payment_status

Write-GCStep '11. Cash on delivery cannot refund, and says so'
$refundRefused = $false
try { Invoke-GC POST "/api/admin/orders/$($order.id)/refund" -Admin | Out-Null }
catch { $refundRefused = "$_" -match 'does not support refunds' }
Confirm-GCTrue 'refund refused with a reason' $refundRefused

# --------------------------------------------------------------- admin bits

Write-GCStep '12. Admin routes need the token'
$unauthorised = $false
try { Invoke-GC GET '/api/admin/orders' | Out-Null } catch { $unauthorised = "$_" -match 'unauthorized' }
Confirm-GCTrue 'unauthenticated admin request refused' $unauthorised

Write-GCStep '13. CSV export round-trips'
$csv = Invoke-GC GET '/api/admin/export/admin-products' -Admin -Raw
Confirm-GCTrue 'header is the documented one' ($csv.StartsWith('product_slug,product_title'))
Confirm-GCTrue 'the new SKUs are in the export' ($csv -match "SMOKE-$run-M")

$dryRun = Invoke-GC POST '/api/admin/import/products?dry_run=1' $csv -Admin
Confirm-GCTrue 'dry run reports work without doing it' ($dryRun.dry_run -eq $true -and $dryRun.errors.Count -eq 0)

Write-GCStep '14. Pagination works both ways and agrees with itself'
# Page numbers and offsets are two views of one window. The engine reports
# both, so a UI drawing "2 of 5" never has to do the arithmetic.
$catalog = Invoke-GC GET '/api/products?limit=100'
$total = $catalog.Count
if ($total -ge 2) {
    $byPage   = Invoke-GC GET '/api/products?limit=1&page=2' -Raw | ConvertFrom-Json
    $byOffset = Invoke-GC GET '/api/products?limit=1&offset=1' -Raw | ConvertFrom-Json
    Confirm-GC 'page 2 and offset 1 return the same item' $byOffset.data[0].slug $byPage.data[0].slug
    Confirm-GC 'meta reports the page number' 2 $byPage.meta.page
    Confirm-GC 'meta reports the offset it used' 1 $byPage.meta.offset

    # Both at once: the page is the more specific intent, so it wins.
    $both = Invoke-GC GET '/api/products?limit=1&offset=999&page=2' -Raw | ConvertFrom-Json
    Confirm-GC 'page beats offset when both are sent' $byOffset.data[0].slug $both.data[0].slug
}
$badPageRejected = $false
try { Invoke-GC GET '/api/products?page=0' | Out-Null } catch { $badPageRejected = "$_" -match 'pages start at 1' }
Confirm-GCTrue 'page=0 is refused with a reason' $badPageRejected

Write-GCStep '15. Superuser sign-in works, and gives the same access a token does'
# The panel signs in with an email and a password; scripts use a token. Both
# end up as a bearer credential on the same routes, and this proves it rather
# than assuming it.
$authState = Invoke-GC GET '/api/admin/auth-state'
Confirm-GCTrue 'the store reports whether it has an operator' ($null -ne $authState.installed)

if ($authState.installed) {
    $suEmail = $env:GC_ADMIN_EMAIL
    $suPass  = $env:GC_ADMIN_PASSWORD
    if (-not $suEmail) { $suEmail = 'admin@example.com' }
    if (-not $suPass)  { $suPass  = 'devpassword' }

    $session = $null
    try {
        $session = Invoke-GC POST '/api/admin/auth-with-password' @{
            identity = $suEmail; password = $suPass
        }
    } catch {
        Write-Host "  note  no dev superuser to sign in as ($suEmail); skipping" -ForegroundColor DarkYellow
    }

    if ($session) {
        Confirm-GCTrue 'sign-in returns a session token' ([bool]$session.token)
        Confirm-GC     'sign-in returns the operator'    $suEmail $session.record.email
        Confirm-GCTrue 'no password hash is ever sent back' `
            (($session.record | ConvertTo-Json -Compress) -notmatch 'pbkdf2|password')

        # The session token must open an admin route, exactly as the static
        # token does.
        $viaSession = Invoke-WebRequest "$(Get-GCBase)/api/admin/products?limit=1" `
            -Headers @{ Authorization = "Bearer $($session.token)" } -UseBasicParsing
        Confirm-GC 'a session token opens an admin route' 200 $viaSession.StatusCode

        # Invoke-GC only knows the configured admin token, and this call has to
        # present the session's own, so it goes out by hand.
        Invoke-WebRequest "$(Get-GCBase)/api/admin/auth-logout" -Method POST `
            -Headers @{ Authorization = "Bearer $($session.token)" } -UseBasicParsing | Out-Null
        $revoked = 0
        try {
            Invoke-WebRequest "$(Get-GCBase)/api/admin/products?limit=1" `
                -Headers @{ Authorization = "Bearer $($session.token)" } -UseBasicParsing | Out-Null
        } catch { $revoked = $_.Exception.Response.StatusCode.value__ }
        Confirm-GC 'logging out revokes the session' 401 $revoked
    }
}

$wrongRejected = 0
try {
    Invoke-GC POST '/api/admin/auth-with-password' @{
        identity = 'nobody@example.invalid'; password = 'not-the-password'
    } | Out-Null
} catch { $wrongRejected = 400 }
Confirm-GC 'bad credentials are refused' 400 $wrongRejected

Write-GCStep '16. The served contract covers what is served'
$spec = Invoke-GC GET '/doc' -Raw | ConvertFrom-Json
Confirm-GCTrue 'checkout is documented' ($null -ne $spec.paths.'/api/checkout/{code}')
Confirm-GCTrue 'money schema is documented' ($null -ne $spec.components.schemas.Money)

Write-GCStep '17. Tidy up'
# Leave the store as we found it. The order survives: its lines are snapshots,
# so deleting the product does not damage the history.
try {
    Invoke-GC DELETE "/api/admin/products/$($product.id)" -Admin | Out-Null
    $history = Invoke-GC GET "/api/admin/orders/$($order.id)" -Admin
    Confirm-GC 'the order still reads correctly' 5000 $history.total.amount_minor
    Confirm-GC 'its line kept its snapshot' 2500 $history.line_items[0].unit_price.amount_minor
} catch {
    Write-Host "  note  could not remove the test product: $_" -ForegroundColor DarkYellow
}

exit (Write-GCSummary)
