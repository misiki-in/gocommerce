<script>
    /**
     * Customers: the orders, grouped by who placed them.
     *
     * There is no customer record to open — see customers.go — so a row's only
     * action is to show that person's orders, which the Orders screen already
     * does. The row links there rather than opening a drawer over a thing that
     * does not exist.
     */
    import { base } from "$app/paths";
    import { api, query } from "$lib/api.js";
    import { formatMoney, formatDate, pluralize } from "$lib/format.js";
    import { toast } from "$lib/toast.svelte.js";
    import ThemeToggle from "$lib/components/ThemeToggle.svelte";

    let customers = $state([]);
    let total = $state(0);
    let loading = $state(true);
    let page = $state(1);
    let search = $state("");
    let draftSearch = $state("");

    const limit = 25;

    $effect(() => {
        search;
        page;
        load();
    });

    async function load() {
        loading = true;
        try {
            const res = await api.get(
                "/api/admin/customers" + query({ q: search, limit, offset: (page - 1) * limit }),
            );
            const rows = res.data ?? [];
            customers = page === 1 ? rows : [...customers, ...rows];
            total = res.meta?.total ?? rows.length;
        } catch (err) {
            toast.error(err);
        } finally {
            loading = false;
        }
    }

    function submitSearch(event) {
        event.preventDefault();
        page = 1;
        search = draftSearch.trim();
    }

    function clearSearch() {
        draftSearch = "";
        page = 1;
        search = "";
    }
</script>

<svelte:head><title>Customers · GoCommerce</title></svelte:head>

<div class="page page-customers">
    <div class="page-content full-height">
        <header class="page-header">
            <nav class="breadcrumbs"><div>Customers</div></nav>

            <div class="inline-flex gap-sm">
                <button
                    type="button"
                    class="btn circle transparent secondary"
                    title="Refresh"
                    aria-label="Refresh"
                    onclick={() => ((page = 1), load())}
                >
                    <i class="ri-refresh-line" aria-hidden="true"></i>
                </button>
            </div>

            <form class="fields searchbar" onsubmit={submitSearch}>
                <div class="field">
                    <input
                        type="text"
                        class="p-l-20"
                        placeholder="Search customers by email or name"
                        bind:value={draftSearch}
                    />
                </div>
                {#if draftSearch || search}
                    <div class="field addon p-r-5">
                        {#if draftSearch !== search}
                            <button type="submit" class="btn sm pill warning">Search</button>
                        {/if}
                        <button
                            type="button"
                            class="btn sm pill secondary transparent"
                            onclick={clearSearch}
                        >
                            Clear
                        </button>
                    </div>
                {/if}
            </form>
        </header>

        <div class="page-table-wrapper">
            <table class="table responsive-table">
                <thead class="sticky">
                    <tr>
                        <th class="col-field-name-id">Customer</th>
                        <th class="col-field-type-text">Location</th>
                        <th class="col-field-type-number min-width">Orders</th>
                        <th class="col-field-type-number min-width">Spent</th>
                        <th class="col-field-type-date min-width">Last order</th>
                        <th class="col-meta min-width"></th>
                    </tr>
                </thead>
                <tbody>
                    {#each customers as customer (customer.email)}
                        <!-- Their orders, which is the only thing behind a
                             customer here. -->
                        <tr
                            class="handle"
                            onclick={() =>
                                (window.location.href = `${base}/orders?email=${encodeURIComponent(customer.email)}`)}
                        >
                            <td class="col-field-name-id" data-name="Customer">
                                <div class="row-name">
                                    <span class="txt-bold txt-ellipsis">
                                        {customer.name || customer.email}
                                    </span>
                                    {#if customer.name}
                                        <span class="txt-hint txt-sm row-handle">{customer.email}</span>
                                    {/if}
                                </div>
                            </td>
                            <td class="col-field-type-text txt-hint" data-name="Location">
                                {[customer.address?.city, customer.address?.country]
                                    .filter(Boolean)
                                    .join(", ") || "—"}
                            </td>
                            <td class="col-field-type-number min-width" data-name="Orders">
                                {customer.orders}
                                <span class="txt-hint txt-sm">
                                    {pluralize(customer.orders, "order")}
                                </span>
                            </td>
                            <td class="col-field-type-number min-width txt-bold" data-name="Spent">
                                {formatMoney(customer.spent)}
                            </td>
                            <td class="col-field-type-date min-width txt-hint" data-name="Last order">
                                {formatDate(customer.last_order_at)}
                            </td>
                            <td class="col-meta min-width">
                                <i class="ri-arrow-right-s-line" aria-hidden="true"></i>
                            </td>
                        </tr>
                    {/each}

                    {#if loading && !customers.length}
                        {#each Array(6) as _, i (i)}
                            <tr><td colspan="6"><span class="skeleton-loader"></span></td></tr>
                        {/each}
                    {:else if !customers.length}
                        <tr>
                            <td colspan="6" class="txt-center txt-hint p-base">
                                {search
                                    ? "No customer matches that."
                                    : "Nobody has ordered yet. A customer appears here with their first order."}
                            </td>
                        </tr>
                    {/if}
                </tbody>
            </table>

            {#if customers.length < total}
                <button
                    type="button"
                    class="btn expanded block load-more-btn"
                    class:loading
                    disabled={loading}
                    onclick={() => (page += 1)}
                >
                    <span class="txt">Load more</span>
                </button>
            {/if}
        </div>

        <footer class="page-footer">
            <span class="txt">
                {total}
                {pluralize(total, "customer")}
            </span>
            <ThemeToggle />
        </footer>
    </div>
</div>
