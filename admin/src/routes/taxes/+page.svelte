<script>
    /**
     * Tax rates: rules about a place and a kind of thing.
     *
     * The listing is ordered the way the engine resolves them — most specific
     * first — so an operator wondering which rule wins can read it off the page
     * instead of reproducing the resolution in their head.
     */
    import { api } from "$lib/api.js";
    import { toast } from "$lib/toast.svelte.js";
    import Drawer from "$lib/components/Drawer.svelte";
    import Confirm from "$lib/components/Confirm.svelte";
    import CategoryPicker from "$lib/components/CategoryPicker.svelte";
    import ThemeToggle from "$lib/components/ThemeToggle.svelte";

    let loading = $state(true);
    let rates = $state([]);
    let categories = $state([]);
    let categoriesTruncated = $state(false);

    let open = $state(false);
    let editing = $state(null);
    let form = $state(blank());
    let saving = $state(false);

    let confirmOpen = $state(false);
    let confirmConfig = $state({});

    $effect(() => {
        load();
    });

    async function load() {
        loading = true;
        try {
            const [rateResult, catResult] = await Promise.all([
                api.get("/api/admin/tax-rates"),
                api.get("/api/admin/categories?flat=1"),
            ]);
            rates = rateResult.data ?? [];
            categories = catResult.data ?? [];
            categoriesTruncated = (catResult.meta?.total ?? 0) > categories.length;
        } catch (err) {
            toast.error(err);
        } finally {
            loading = false;
        }
    }

    function blank() {
        return {
            name: "",
            percent: "18",
            country: "",
            state: "",
            category_id: null,
            active: true,
        };
    }

    function openNew() {
        editing = null;
        form = blank();
        open = true;
    }

    function openEdit(r) {
        editing = r;
        form = {
            name: r.name,
            percent: String(r.rate_bp / 100),
            country: r.country ?? "",
            state: r.state ?? "",
            category_id: r.category_id ?? null,
            active: r.active,
        };
        open = true;
    }

    async function save(event) {
        event?.preventDefault();
        saving = true;
        const body = {
            name: form.name.trim(),
            // Typed as people say it, stored as basis points.
            rate_bp: Math.round((parseFloat(form.percent) || 0) * 100),
            country: form.country.trim(),
            state: form.state.trim(),
            category_id: form.category_id,
            active: form.active,
        };
        try {
            if (editing) {
                await api.patch(`/api/admin/tax-rates/${editing.id}`, body);
                toast.success("Rate updated");
            } else {
                await api.post("/api/admin/tax-rates", body);
                toast.success("Rate created");
            }
            open = false;
            await load();
        } catch (err) {
            toast.error(err);
        } finally {
            saving = false;
        }
    }

    function askDelete(r) {
        confirmConfig = {
            title: `Delete ${r.name}?`,
            message:
                "Orders already placed keep what they were charged. Nothing new will be taxed " +
                "under this rule.",
            confirmLabel: "Delete",
            danger: true,
            run: async () => {
                try {
                    await api.delete(`/api/admin/tax-rates/${r.id}`);
                    toast.success("Rate deleted");
                    await load();
                } catch (err) {
                    toast.error(err);
                }
            },
        };
        confirmOpen = true;
    }

    /** Where a rule applies, in the words an operator wrote it in. */
    function where(r) {
        if (!r.country) return "Everywhere";
        return r.state ? `${r.country} · ${r.state}` : r.country;
    }
</script>

<div class="page page-taxes">
    <div class="page-content full-height">
        <header class="page-header">
            <nav class="breadcrumbs"><div>Tax rates</div></nav>
            <div class="flex-fill"></div>
            <div class="page-header-primary-btns">
                <button type="button" class="btn" onclick={openNew}>
                    <i class="ri-add-line" aria-hidden="true"></i>
                    <span class="txt">New rate</span>
                </button>
            </div>
        </header>

        <div class="page-table-wrapper">
            <table class="table">
                <thead class="sticky">
                    <tr>
                        <th class="col-field-name-id">Name</th>
                        <th class="col-field-type-text">Applies to</th>
                        <th class="col-field-type-select">Where</th>
                        <th class="col-field-type-number min-width">Rate</th>
                        <th class="col-field-type-select">State</th>
                        <th class="col-meta min-width"></th>
                    </tr>
                </thead>
                <tbody>
                    {#each rates as r (r.id)}
                        <tr class="handle" onclick={() => openEdit(r)}>
                            <td class="col-field-name-id" data-name="Name">
                                <span class="txt-bold">{r.name}</span>
                            </td>
                            <td class="col-field-type-text" data-name="Applies to">
                                {#if r.category_name}
                                    <span class="txt-ellipsis">{r.category_name}</span>
                                    <span class="txt-hint txt-sm">and everything under it</span>
                                {:else}
                                    <span class="txt-hint">Everything</span>
                                {/if}
                            </td>
                            <td class="col-field-type-select" data-name="Where">{where(r)}</td>
                            <td class="col-field-type-number min-width" data-name="Rate">
                                {r.rate_bp / 100}%
                            </td>
                            <td class="col-field-type-select" data-name="State">
                                <span class="label {r.active ? 'success' : ''}">
                                    {r.active ? "on" : "off"}
                                </span>
                            </td>
                            <td class="col-meta min-width">
                                <button
                                    type="button"
                                    class="btn circle sm transparent secondary row-delete"
                                    aria-label="Delete {r.name}"
                                    title="Delete"
                                    onclick={(e) => (e.stopPropagation(), askDelete(r))}
                                >
                                    <i class="ri-delete-bin-7-line" aria-hidden="true"></i>
                                </button>
                                <i class="ri-arrow-right-s-line" aria-hidden="true"></i>
                            </td>
                        </tr>
                    {/each}

                    {#if loading && !rates.length}
                        {#each Array(3) as _, i (i)}
                            <tr><td colspan="6"><span class="skeleton-loader"></span></td></tr>
                        {/each}
                    {/if}

                    {#if !loading && !rates.length}
                        <tr>
                            <td colspan="6" class="txt-center txt-hint p-base">
                                <div class="m-b-10">
                                    <i
                                        class="ri-percent-line"
                                        style="font-size: 32px"
                                        aria-hidden="true"
                                    ></i>
                                </div>
                                No rates yet, so nothing is taxed. Add one for the country you sell
                                into.
                            </td>
                        </tr>
                    {/if}
                </tbody>
            </table>
        </div>

        <footer class="page-footer">
            <span class="txt">
                Rules are read most specific first — a category beats a country, and a deeper
                category beats the one above it.
            </span>
            <ThemeToggle />
        </footer>
    </div>
</div>

<Drawer
    {open}
    size="popup"
    title={editing ? "Edit rate" : "New rate"}
    onclose={() => (open = false)}
>
    <form id="tax-form" onsubmit={save}>
        <div class="fields">
            <div class="field required">
                <label for="t-name">Name</label>
                <input id="t-name" type="text" bind:value={form.name} placeholder="GST 18%" />
            </div>
            <div class="delimiter"></div>
            <div class="field required">
                <label for="t-percent">Rate</label>
                <input
                    id="t-percent"
                    type="number"
                    min="0"
                    max="100"
                    step="0.01"
                    bind:value={form.percent}
                />
            </div>
        </div>
        <div class="field-help">The name is what appears on the invoice.</div>

        <div class="fields m-t-sm">
            <div class="field">
                <label for="t-country">Country</label>
                <input
                    id="t-country"
                    type="text"
                    maxlength="2"
                    bind:value={form.country}
                    placeholder="IN"
                />
            </div>
            <div class="delimiter"></div>
            <div class="field">
                <label for="t-state">State</label>
                <input id="t-state" type="text" bind:value={form.state} placeholder="KA" />
            </div>
        </div>
        <div class="field-help">
            Two-letter country code. Leave both empty for a rule that applies wherever you ship; a
            state needs the country it is in.
        </div>

        <div class="field m-t-sm">
            <label for="t-category">Category</label>
            <CategoryPicker
                id="t-category"
                bind:value={form.category_id}
                {categories}
                remote={categoriesTruncated}
            />
        </div>
        <div class="field-help">
            Leave it empty to tax everything. A category reaches everything beneath it, and a rule
            on a deeper category wins.
        </div>

        <div class="field m-t-sm">
            <input id="t-active" type="checkbox" bind:checked={form.active} />
            <label for="t-active">Active</label>
        </div>
    </form>

    {#snippet footer()}
        <button type="button" class="btn transparent m-r-auto" onclick={() => (open = false)}>
            <span class="txt">Cancel</span>
        </button>
        <button
            type="submit"
            form="tax-form"
            class="btn expanded"
            class:loading={saving}
            disabled={saving}
        >
            <span class="txt">{editing ? "Save changes" : "Create rate"}</span>
        </button>
    {/snippet}
</Drawer>

<Confirm
    bind:open={confirmOpen}
    title={confirmConfig.title}
    message={confirmConfig.message}
    confirmLabel={confirmConfig.confirmLabel}
    danger={confirmConfig.danger}
    onconfirm={() => confirmConfig.run?.()}
/>
