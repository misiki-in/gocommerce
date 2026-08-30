<script>
    import { base } from "$app/paths";
    import { goto } from "$app/navigation";
    import { api, query } from "$lib/api.js";
    import {
        formatMoney,
        relativeTime,
        orderStatusClass,
        paymentStatusClass,
        stockClass,
    } from "$lib/format.js";
    import { toast } from "$lib/toast.svelte.js";
    import ThemeToggle from "$lib/components/ThemeToggle.svelte";

    let loading = $state(true);
    let recent = $state([]);
    let lowStock = $state([]);
    let counts = $state({ orders: 0, products: 0, pending: 0, unpaid: 0 });
    let revenue = $state(null);

    $effect(() => {
        load();
    });

    async function load() {
        loading = true;
        try {
            // Five small reads rather than one bespoke dashboard endpoint: the
            // panel uses the same API everything else does, and an endpoint
            // that exists only for this screen would be one more thing to keep
            // in step with it.
            const [orders, products, pending, unpaid, stock] = await Promise.all([
                api.get("/api/admin/orders" + query({ limit: 8 })),
                api.get("/api/admin/products" + query({ limit: 1 })),
                api.get("/api/admin/orders" + query({ status: "confirmed", limit: 1 })),
                api.get("/api/admin/orders" + query({ payment_status: "pending", limit: 1 })),
                api.get("/api/admin/inventory/low-stock" + query({ threshold: 5, limit: 6 })),
            ]);

            recent = orders.data;
            counts = {
                orders: orders.meta.total,
                products: products.meta.total,
                pending: pending.meta.total,
                unpaid: unpaid.meta.total,
            };
            lowStock = stock.data;

            // Revenue over what came back, clearly labelled as such rather than
            // presented as an all-time figure it is not.
            const paid = orders.data.filter((o) => o.payment_status === "paid");
            revenue = paid.length
                ? {
                      amount_minor: paid.reduce((sum, o) => sum + o.total.amount_minor, 0),
                      currency: paid[0].total.currency,
                  }
                : null;
        } catch (err) {
            toast.error(err);
        } finally {
            loading = false;
        }
    }

    const cards = $derived([
        {
            icon: "ri-shopping-bag-3-line",
            label: "Orders",
            value: counts.orders,
            hint: "All time",
            href: "/orders",
        },
        {
            icon: "ri-truck-line",
            label: "Awaiting shipment",
            value: counts.pending,
            hint: "Confirmed, not yet shipped",
            href: "/orders?status=confirmed",
        },
        {
            icon: "ri-money-dollar-circle-line",
            label: "Awaiting payment",
            value: counts.unpaid,
            hint: "Cash on delivery, or unpaid",
            href: "/orders?payment_status=pending",
        },
        {
            icon: "ri-price-tag-3-line",
            label: "Products",
            value: counts.products,
            hint: "Every status",
            href: "/products",
        },
    ]);
</script>

<div class="page page-dashboard">
    <div class="page-content">
        <header class="page-header">
            <nav class="breadcrumbs"><div>Dashboard</div></nav>
            <div class="page-header-primary-btns">
                <button type="button" class="btn secondary" class:loading disabled={loading} onclick={load}>
                    <i class="ri-refresh-line" aria-hidden="true"></i>
                    <span class="txt">Refresh</span>
                </button>
            </div>
        </header>

        <div class="grid m-b-base">
            {#each cards as card (card.label)}
                <div class="col-3">
                    <a class="stat-card" href="{base}{card.href}">
                        <span class="stat-label">
                            <i class={card.icon} aria-hidden="true"></i>
                            {card.label}
                        </span>
                        {#if loading}
                            <span class="skeleton-loader" style="height: 26px"></span>
                        {:else}
                            <span class="stat-value">{card.value}</span>
                        {/if}
                        <span class="stat-hint">{card.hint}</span>
                    </a>
                </div>
            {/each}
        </div>

        <h6 class="section-title">
            <i class="ri-history-line" aria-hidden="true"></i>
            Recent orders
            {#if revenue}
                <span class="txt-hint txt-sm">· {formatMoney(revenue)} paid across these</span>
            {/if}
            <a href="{base}/orders" class="btn sm transparent secondary">
                <span class="txt">All orders</span>
                <i class="ri-arrow-right-line" aria-hidden="true"></i>
            </a>
        </h6>

        <table class="table responsive-table">
            <thead>
                <tr>
                    <th class="col-field-name-id">Order</th>
                    <th>Customer</th>
                    <th class="col-field-type-select">Status</th>
                    <th class="col-field-type-select">Payment</th>
                    <th class="col-field-type-number min-width">Total</th>
                    <th class="col-field-type-date min-width">Placed</th>
                </tr>
            </thead>
            <tbody>
                {#each recent as order (order.id)}
                    <tr class="handle" onclick={() => goto(`${base}/orders?q=${order.number}`)}>
                        <td class="col-field-name-id txt-code txt-sm" data-name="Order">
                            {order.number}
                        </td>
                        <!-- The ellipsis goes on a span, never the cell: overflow
                             on a <td> takes it out of the table's border model
                             and the row's rules stop meeting. -->
                        <td class="col-field-type-text" data-name="Customer">
                            <span class="txt-ellipsis">{order.name || order.email}</span>
                        </td>
                        <td class="col-field-type-select" data-name="Status">
                            <span class="label {orderStatusClass(order.status)}">{order.status}</span>
                        </td>
                        <td class="col-field-type-select" data-name="Payment">
                            <span class="label {paymentStatusClass(order.payment_status)}">
                                {order.payment_status}
                            </span>
                        </td>
                        <td class="col-field-type-number min-width" data-name="Total">
                            {formatMoney(order.total)}
                        </td>
                        <td class="col-field-type-date min-width txt-hint txt-sm" data-name="Placed">
                            {relativeTime(order.created_at)}
                        </td>
                    </tr>
                {/each}

                {#if loading && !recent.length}
                    {#each Array(4) as _, i (i)}
                        <tr><td colspan="6"><span class="skeleton-loader"></span></td></tr>
                    {/each}
                {/if}

                {#if !loading && !recent.length}
                    <tr>
                        <td colspan="6" class="txt-center txt-hint p-base">
                            No orders yet. They will appear here as soon as somebody buys something.
                        </td>
                    </tr>
                {/if}
            </tbody>
        </table>

        <h6 class="section-title">
            <i class="ri-alert-line" aria-hidden="true"></i>
            Running low
            <a href="{base}/inventory" class="btn sm transparent secondary">
                <span class="txt">Inventory</span>
                <i class="ri-arrow-right-line" aria-hidden="true"></i>
            </a>
        </h6>

        <table class="table responsive-table">
            <thead>
                <tr>
                    <th class="col-field-name-id">SKU</th>
                    <th>Variant</th>
                    <th class="col-field-type-number min-width">On hand</th>
                    <th class="col-field-type-number min-width">Reserved</th>
                    <th class="col-field-type-number min-width">Available</th>
                </tr>
            </thead>
            <tbody>
                {#each lowStock as variant (variant.id)}
                    <tr>
                        <td class="col-field-name-id txt-code txt-sm" data-name="SKU">{variant.sku}</td>
                        <td class="txt-hint" data-name="Variant">{variant.label || "—"}</td>
                        <td class="col-field-type-number min-width" data-name="On hand">
                            {variant.stock_on_hand}
                        </td>
                        <td class="col-field-type-number min-width txt-hint" data-name="Reserved">
                            {variant.stock_reserved}
                        </td>
                        <td
                            class="col-field-type-number min-width txt-bold {stockClass(variant.available)}"
                            data-name="Available"
                        >
                            {variant.available}
                        </td>
                    </tr>
                {/each}

                {#if loading && !lowStock.length}
                    <tr><td colspan="5"><span class="skeleton-loader"></span></td></tr>
                {/if}

                {#if !loading && !lowStock.length}
                    <tr>
                        <td colspan="5" class="txt-center txt-hint p-base">
                            Nothing is running out — every tracked variant has more than five
                            available.
                        </td>
                    </tr>
                {/if}
            </tbody>
        </table>

        <footer class="page-footer">
            <span class="txt">Live counts, read straight from the API</span>
            <ThemeToggle />
        </footer>
    </div>
</div>
