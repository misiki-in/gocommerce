<script>
    import { api, query } from "$lib/api.js";
    import { stockClass, pluralize } from "$lib/format.js";
    import { toast } from "$lib/toast.svelte.js";
    import Drawer from "$lib/components/Drawer.svelte";

    import ThemeToggle from "$lib/components/ThemeToggle.svelte";
    const PER_PAGE = 25;
    const DEFAULT_THRESHOLD = 5;

    let loading = $state(true);
    let variants = $state([]);
    let meta = $state(null);
    let threshold = $state(DEFAULT_THRESHOLD);
    let draftThreshold = $state(DEFAULT_THRESHOLD);
    let page = $state(1);

    let adjustOpen = $state(false);
    let target = $state(null);
    let mode = $state("adjust"); // adjust | set | move
    let amount = $state("");
    let saving = $state(false);
    let error = $state("");

    // Where the variant in the drawer actually is. Loaded per open rather than
    // with the list: the table is about totals, and one row's breakdown is a
    // question only asked when somebody is about to move something.
    let rows = $state([]);
    let rowsLoading = $state(false);
    let locationID = $state(0);
    let toID = $state(0);

    const multi = $derived(rows.length > 1);
    const here = $derived(rows.find((r) => r.location_id === locationID) ?? null);
    const elsewhere = $derived(rows.filter((r) => r.location_id !== locationID));

    $effect(() => {
        // Re-runs whenever the threshold changes. Page 1 replaces the list; a
        // later page appends, because the table loads more rather than paginating.
        threshold;
        page;
        load();
    });

    async function load() {
        loading = true;
        try {
            const result = await api.get(
                "/api/admin/inventory/low-stock" +
                    query({ threshold, page, limit: PER_PAGE }),
            );
            variants = page === 1 ? result.data : [...variants, ...result.data];
            meta = result.meta;
        } catch (err) {
            toast.error(err);
        } finally {
            loading = false;
        }
    }

    const hasMore = $derived(!!meta && variants.length < meta.total);

    function applyThreshold(e) {
        e.preventDefault();
        // An emptied or negative box is not a threshold; leave the applied one
        // alone rather than asking the server about a count that cannot exist.
        if (draftThreshold === undefined || draftThreshold === null || draftThreshold < 0) return;
        page = 1;
        threshold = draftThreshold;
    }

    function resetThreshold() {
        draftThreshold = DEFAULT_THRESHOLD;
        page = 1;
        threshold = DEFAULT_THRESHOLD;
    }

    function openAdjust(variant, initialMode, event) {
        event?.stopPropagation();
        target = variant;
        mode = initialMode;
        amount = "";
        error = "";
        rows = [];
        locationID = 0;
        toID = 0;
        adjustOpen = true;
        loadRows(variant.id, initialMode);
    }

    async function loadRows(variantID, initialMode) {
        rowsLoading = true;
        try {
            const result = await api.get(`/api/admin/variants/${variantID}/stock`);
            rows = result.data ?? [];
            // Rows arrive in priority order, so the first one holding anything is
            // the shelf an order would come off — the same reading the locations
            // page states in its footer. Falling back to the first row keeps a
            // variant that is nowhere yet pointed at somewhere real.
            const start = rows.find((r) => r.on_hand !== 0) ?? rows[0];
            locationID = start?.location_id ?? 0;
            toID = rows.find((r) => r.location_id !== locationID)?.location_id ?? 0;
            if (initialMode === "set") amount = String(start?.on_hand ?? 0);
        } catch (err) {
            error = err.message;
        } finally {
            rowsLoading = false;
        }
    }

    function pickLocation(id) {
        locationID = id;
        if (mode === "set") amount = String(rows.find((r) => r.location_id === id)?.on_hand ?? 0);
        if (toID === id) toID = elsewhere[0]?.location_id ?? 0;
        error = "";
    }

    async function save(event) {
        event?.preventDefault();
        const value = parseInt(amount, 10);
        if (isNaN(value)) {
            error = "Enter a whole number.";
            return;
        }
        if (mode === "set" && value < 0) {
            error = "Stock cannot be negative.";
            return;
        }
        if (mode === "move" && value <= 0) {
            error = "Move a positive number of units.";
            return;
        }
        if (mode === "move" && !toID) {
            error = "Choose where the units are going.";
            return;
        }

        saving = true;
        error = "";
        try {
            if (mode === "move") {
                await api.post(`/api/admin/variants/${target.id}/stock/transfer`, {
                    from_location_id: locationID,
                    to_location_id: toID,
                    quantity: value,
                });
                toast.success(`Moved ${value} × ${target.sku}`);
            } else {
                const body =
                    mode === "adjust"
                        ? { adjust: value, location_id: locationID }
                        : { set: value, location_id: locationID };
                await api.post(`/api/admin/variants/${target.id}/inventory`, body);
                toast.success(`Updated ${target.sku}`);
            }
            adjustOpen = false;
            page = 1;
            await load();
        } catch (err) {
            // The engine refuses to drop stock below what is reserved for open
            // orders, and explains why — show that rather than a generic error.
            error = err.message;
        } finally {
            saving = false;
        }
    }
</script>

<div class="page page-inventory">
    <div class="page-content full-height">
        <header class="page-header">
            <nav class="breadcrumbs"><div>Inventory</div></nav>

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

            <form class="fields searchbar" onsubmit={applyThreshold}>
                <div class="field addon">
                    <label class="txt-nowrap" for="threshold">Available at or below</label>
                </div>
                <div class="field">
                    <input id="threshold" type="number" min="0" bind:value={draftThreshold} />
                </div>
                {#if draftThreshold !== threshold || threshold !== DEFAULT_THRESHOLD}
                    <div class="field addon p-r-5">
                        {#if draftThreshold !== threshold}
                            <button type="submit" class="btn sm pill warning">Apply</button>
                        {/if}
                        <button
                            type="button"
                            class="btn sm pill secondary transparent"
                            onclick={resetThreshold}
                        >
                            Reset
                        </button>
                    </div>
                {/if}
            </form>
        </header>

        <div class="alert info m-b-sm">
            <p>
                <i class="ri-information-line" aria-hidden="true"></i>
                <strong>Available</strong> is on hand minus what is reserved for orders in flight.
                Stock moves as a delta or an absolute count, never as a blind overwrite — so a sale
                that lands mid-edit cannot be lost.
            </p>
        </div>

        <div class="page-table-wrapper">
            <table class="table responsive-table" class:optimize={variants.length > 60}>
                <thead class="sticky">
                    <tr>
                        <th class="col-field-name-id">SKU</th>
                        <th class="col-field-type-text">Variant</th>
                        <th class="col-field-type-number min-width">On hand</th>
                        <th class="col-field-type-number min-width">Reserved</th>
                        <th class="col-field-type-number min-width">Available</th>
                        <th class="col-meta min-width"></th>
                    </tr>
                </thead>
                <tbody>
                    {#each variants as variant (variant.id)}
                        <tr class="handle" onclick={() => openAdjust(variant, "adjust")}>
                            <td class="col-field-name-id" data-name="SKU">
                                <span class="txt-bold txt-code">{variant.sku}</span>
                            </td>
                            <td class="col-field-type-text txt-hint" data-name="Variant">
                                {variant.label || "—"}
                            </td>
                            <td class="col-field-type-number min-width" data-name="On hand">
                                {variant.stock_on_hand}
                            </td>
                            <td
                                class="col-field-type-number min-width txt-hint"
                                data-name="Reserved"
                            >
                                {variant.stock_reserved}
                            </td>
                            <td
                                class="col-field-type-number min-width txt-bold {stockClass(
                                    variant.available,
                                )}"
                                data-name="Available"
                            >
                                {variant.available}
                            </td>
                            <td class="col-meta min-width">
                                <button
                                    type="button"
                                    class="btn circle sm transparent secondary"
                                    aria-label="Stock take for {variant.sku}"
                                    title="Stock take"
                                    onclick={(e) => openAdjust(variant, "set", e)}
                                >
                                    <i class="ri-list-check-2" aria-hidden="true"></i>
                                </button>
                                <i class="ri-arrow-right-s-line" aria-hidden="true"></i>
                            </td>
                        </tr>
                    {/each}

                    {#if loading && !variants.length}
                        {#each Array(6) as _, i (i)}
                            <tr>
                                <td colspan="6"><span class="skeleton-loader"></span></td>
                            </tr>
                        {/each}
                    {/if}

                    {#if !loading && !variants.length}
                        <tr>
                            <td colspan="6" class="txt-center txt-hint p-base">
                                <div class="m-b-10">
                                    <i
                                        class="ri-checkbox-circle-line"
                                        style="font-size: 32px"
                                        aria-hidden="true"
                                    ></i>
                                </div>
                                Nothing is running low. Every tracked variant has more than
                                {threshold} available.
                            </td>
                        </tr>
                    {/if}
                </tbody>
            </table>

            {#if hasMore}
                <button
                    type="button"
                    class="btn expanded block load-more-btn"
                    class:loading
                    disabled={loading}
                    onclick={() => (page += 1)}
                >
                    <i class="ri-arrow-down-s-line" aria-hidden="true"></i>
                    <span class="txt">Load more</span>
                </button>
            {/if}
        </div>

        <footer class="page-footer">
            <span class="txt">
                {#if meta}
                    Showing {variants.length} of {meta.total}
                    {pluralize(meta.total, "variant")} at or below {threshold}
                {:else}
                    …
                {/if}
            </span>
            <ThemeToggle />
        </footer>
    </div>
</div>

<Drawer
    open={adjustOpen}
    size="sm"
    title={target
        ? mode === "adjust"
            ? `Receive stock — ${target.sku}`
            : mode === "move"
              ? `Move stock — ${target.sku}`
              : `Stock take — ${target.sku}`
        : ""}
    onclose={() => (adjustOpen = false)}
>
    {#if target}
        <form id="stock-form" onsubmit={save}>
            {#if multi}
                <!--
                    With more than one location the totals are a sum, and every
                    movement below acts on exactly one row — so the row has to be
                    the thing being pointed at, not a setting buried in a select.
                -->
                <table class="table stock-locations m-b-base">
                    <thead>
                        <tr>
                            <th>Location</th>
                            <th class="txt-right">On hand</th>
                            <th class="txt-right">Reserved</th>
                            <th class="txt-right">Available</th>
                        </tr>
                    </thead>
                    <tbody>
                        {#each rows as r (r.location_id)}
                            <tr
                                class="handle"
                                class:selected={r.location_id === locationID}
                                onclick={() => pickLocation(r.location_id)}
                            >
                                <td>
                                    <span class="txt-bold">{r.location_name}</span>
                                    {#if !r.active}
                                        <span class="label">closed</span>
                                    {/if}
                                </td>
                                <td class="txt-right">{r.on_hand}</td>
                                <td class="txt-right txt-hint">{r.reserved}</td>
                                <td class="txt-right txt-bold {stockClass(r.available)}">
                                    {r.available}
                                </td>
                            </tr>
                        {/each}
                    </tbody>
                </table>
            {:else}
                <div class="flex m-b-sm">
                    <span class="txt-hint">On hand</span>
                    <div class="flex-fill"></div>
                    <strong>{target.stock_on_hand}</strong>
                </div>
                <div class="flex m-b-base">
                    <span class="txt-hint">Reserved for open orders</span>
                    <div class="flex-fill"></div>
                    <strong>{target.stock_reserved}</strong>
                </div>
            {/if}

            {#if mode === "move"}
                <div class="field required">
                    <label for="move-to">Move to</label>
                    <select id="move-to" bind:value={toID}>
                        {#each elsewhere as r (r.location_id)}
                            <option value={r.location_id}>{r.location_name}</option>
                        {/each}
                    </select>
                </div>
            {/if}

            <div class="field required" class:error={!!error}>
                <label for="amount">
                    {mode === "adjust"
                        ? "Add (or subtract) this many"
                        : mode === "move"
                          ? "How many units"
                          : "Set the count to"}
                </label>
                <!-- svelte-ignore a11y_autofocus -->
                <input
                    id="amount"
                    type="number"
                    autofocus
                    bind:value={amount}
                    oninput={() => (error = "")}
                />
            </div>
            {#if error}<div class="field-help error">{error}</div>{/if}
            <div class="field-help">
                {#if mode === "adjust"}
                    A delta: 25 receives a case, -1 writes one off.
                    {#if multi && here}Applied at {here.location_name}.{/if}
                {:else if mode === "move"}
                    Reserved units stay where they are — they are promised to orders that will be
                    picked from {here?.location_name ?? "here"}.
                {:else}
                    Cannot go below the {multi
                        ? (here?.reserved ?? 0)
                        : target.stock_reserved} already promised to open orders{multi && here
                        ? ` at ${here.location_name}`
                        : ""}.
                {/if}
            </div>
        </form>
    {/if}

    {#snippet footer()}
        <button type="button" class="btn transparent m-r-auto" onclick={() => (adjustOpen = false)}>
            <span class="txt">Cancel</span>
        </button>
        {#if multi && mode !== "move"}
            <button
                type="button"
                class="btn secondary"
                onclick={() => ((mode = "move"), (amount = ""), (error = ""))}
            >
                <i class="ri-arrow-left-right-line" aria-hidden="true"></i>
                <span class="txt">Move</span>
            </button>
        {/if}
        <button
            type="submit"
            form="stock-form"
            class="btn"
            class:loading={saving || rowsLoading}
            disabled={saving || rowsLoading}
        >
            <span class="txt">{mode === "move" ? "Move" : "Apply"}</span>
        </button>
    {/snippet}
</Drawer>
