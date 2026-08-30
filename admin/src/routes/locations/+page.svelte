<script>
    /**
     * Locations: the places stock physically is.
     *
     * Most stores have exactly one and should be able to ignore this screen
     * entirely, so the listing leads with what each place is *holding* rather
     * than with its settings — the number is the reason to come here.
     */
    import { api } from "$lib/api.js";
    import { pluralize } from "$lib/format.js";
    import { toast } from "$lib/toast.svelte.js";
    import Drawer from "$lib/components/Drawer.svelte";
    import Confirm from "$lib/components/Confirm.svelte";
    import ThemeToggle from "$lib/components/ThemeToggle.svelte";

    let loading = $state(true);
    let locations = $state([]);

    let open = $state(false);
    let editing = $state(null);
    let form = $state(blank());
    let saving = $state(false);
    let error = $state("");

    let confirmOpen = $state(false);
    let confirmConfig = $state({});

    $effect(() => {
        load();
    });

    async function load() {
        loading = true;
        try {
            const result = await api.get("/api/admin/locations");
            locations = result.data ?? [];
        } catch (err) {
            toast.error(err);
        } finally {
            loading = false;
        }
    }

    function blank() {
        return {
            code: "",
            name: "",
            priority: 0,
            active: true,
            line1: "",
            line2: "",
            city: "",
            state: "",
            postal_code: "",
            country: "",
        };
    }

    function openNew() {
        editing = null;
        form = blank();
        error = "";
        open = true;
    }

    function openEdit(l) {
        editing = l;
        const a = l.address ?? {};
        form = {
            code: l.code,
            name: l.name,
            priority: l.priority,
            active: l.active,
            line1: a.line1 ?? "",
            line2: a.line2 ?? "",
            city: a.city ?? "",
            state: a.state ?? "",
            postal_code: a.postal_code ?? "",
            country: a.country ?? "",
        };
        error = "";
        open = true;
    }

    /** An address is sent only when something was typed into it. */
    function address() {
        const a = {
            line1: form.line1.trim(),
            line2: form.line2.trim(),
            city: form.city.trim(),
            state: form.state.trim(),
            postal_code: form.postal_code.trim(),
            country: form.country.trim().toUpperCase(),
        };
        return Object.values(a).some(Boolean) ? a : null;
    }

    async function save(event) {
        event?.preventDefault();
        saving = true;
        error = "";
        const body = {
            name: form.name.trim(),
            priority: Number(form.priority) || 0,
            active: form.active,
            address: address(),
        };
        try {
            if (editing) {
                await api.patch(`/api/admin/locations/${editing.id}`, body);
                toast.success("Location updated");
            } else {
                await api.post("/api/admin/locations", {
                    ...body,
                    code: form.code.trim().toLowerCase(),
                });
                toast.success("Location opened");
            }
            open = false;
            await load();
        } catch (err) {
            // Deactivating a location that still holds stock is refused, and
            // the message names how many units are stranded. That is the answer
            // the operator needs, so show it in place rather than as a toast
            // that scrolls away.
            error = err.message;
        } finally {
            saving = false;
        }
    }

    async function makeDefault(l, event) {
        event?.stopPropagation();
        try {
            await api.post(`/api/admin/locations/${l.id}/default`, {});
            toast.success(`${l.name} is now the default`);
            await load();
        } catch (err) {
            toast.error(err);
        }
    }

    function askDelete(l, event) {
        event?.stopPropagation();
        confirmConfig = {
            title: `Close ${l.name}?`,
            message:
                "Orders already filled from here keep reading exactly as they do now. " +
                "New orders will be filled from somewhere else.",
            confirmLabel: "Close",
            danger: true,
            run: async () => {
                try {
                    await api.delete(`/api/admin/locations/${l.id}`);
                    toast.success("Location closed");
                    await load();
                } catch (err) {
                    toast.error(err);
                }
            },
        };
        confirmOpen = true;
    }

    function where(l) {
        if (!l.address) return "";
        return [l.address.city, l.address.country].filter(Boolean).join(", ");
    }

    const totalUnits = $derived(locations.reduce((sum, l) => sum + l.on_hand, 0));
</script>

<div class="page page-locations">
    <div class="page-content full-height">
        <header class="page-header">
            <nav class="breadcrumbs"><div>Locations</div></nav>
            <div class="flex-fill"></div>
            <div class="page-header-primary-btns">
                <button type="button" class="btn" onclick={openNew}>
                    <i class="ri-add-line" aria-hidden="true"></i>
                    <span class="txt">New location</span>
                </button>
            </div>
        </header>

        <div class="page-table-wrapper">
            <table class="table">
                <thead class="sticky">
                    <tr>
                        <th class="col-field-name-id">Name</th>
                        <th class="col-field-type-text">Where</th>
                        <th class="col-field-type-number min-width">On hand</th>
                        <th class="col-field-type-number min-width">Reserved</th>
                        <th class="col-field-type-number min-width">SKUs</th>
                        <th class="col-field-type-select">State</th>
                        <th class="col-meta min-width"></th>
                    </tr>
                </thead>
                <tbody>
                    {#each locations as l (l.id)}
                        <tr class="handle" onclick={() => openEdit(l)}>
                            <td class="col-field-name-id" data-name="Name">
                                <div class="txt-bold">{l.name}</div>
                                <div class="txt-hint txt-sm txt-code">{l.code}</div>
                            </td>
                            <td class="col-field-type-text txt-hint" data-name="Where">
                                {where(l) || "—"}
                            </td>
                            <td class="col-field-type-number min-width" data-name="On hand">
                                {l.on_hand}
                            </td>
                            <td
                                class="col-field-type-number min-width txt-hint"
                                data-name="Reserved"
                            >
                                {l.reserved}
                            </td>
                            <td
                                class="col-field-type-number min-width txt-hint"
                                data-name="SKUs"
                            >
                                {l.skus}
                            </td>
                            <td class="col-field-type-select" data-name="State">
                                {#if l.is_default}
                                    <span class="label success">default</span>
                                {:else if !l.active}
                                    <span class="label">closed</span>
                                {:else}
                                    <span class="label">open</span>
                                {/if}
                            </td>
                            <td class="col-meta min-width">
                                {#if !l.is_default}
                                    <button
                                        type="button"
                                        class="btn circle sm transparent secondary"
                                        aria-label="Make {l.name} the default"
                                        title="Make default"
                                        onclick={(e) => makeDefault(l, e)}
                                    >
                                        <i class="ri-pushpin-line" aria-hidden="true"></i>
                                    </button>
                                    <button
                                        type="button"
                                        class="btn circle sm transparent secondary row-delete"
                                        aria-label="Close {l.name}"
                                        title="Close"
                                        onclick={(e) => askDelete(l, e)}
                                    >
                                        <i class="ri-delete-bin-7-line" aria-hidden="true"></i>
                                    </button>
                                {/if}
                                <i class="ri-arrow-right-s-line" aria-hidden="true"></i>
                            </td>
                        </tr>
                    {/each}

                    {#if loading && !locations.length}
                        {#each Array(2) as _, i (i)}
                            <tr><td colspan="7"><span class="skeleton-loader"></span></td></tr>
                        {/each}
                    {/if}
                </tbody>
            </table>
        </div>

        <footer class="page-footer">
            <span class="txt">
                {locations.length}
                {pluralize(locations.length, "location")} holding {totalUnits}
                {pluralize(totalUnits, "unit")}. Orders are filled from the first one, top to
                bottom, that can cover the line.
            </span>
            <ThemeToggle />
        </footer>
    </div>
</div>

<Drawer
    {open}
    size="popup"
    title={editing ? `Edit ${editing.name}` : "New location"}
    onclose={() => (open = false)}
>
    <form id="location-form" onsubmit={save}>
        <div class="fields">
            <div class="field required">
                <label for="l-name">Name</label>
                <input id="l-name" type="text" bind:value={form.name} placeholder="North warehouse" />
            </div>
            <div class="delimiter"></div>
            <div class="field required">
                <label for="l-code">Code</label>
                <input
                    id="l-code"
                    type="text"
                    bind:value={form.code}
                    disabled={!!editing}
                    placeholder="warehouse"
                />
            </div>
        </div>
        <div class="field-help">
            The code is what an import or a script refers to, so it does not change once orders
            have been filled from here.
        </div>

        <div class="fields m-t-sm">
            <div class="field">
                <label for="l-priority">Priority</label>
                <input id="l-priority" type="number" bind:value={form.priority} />
            </div>
            <div class="delimiter"></div>
            <div class="field">
                <span class="label-block"></span>
                <div class="inline-flex">
                    <input id="l-active" type="checkbox" bind:checked={form.active} />
                    <label for="l-active">Open for new orders</label>
                </div>
            </div>
        </div>
        <div class="field-help">
            Lower is tried first. A location has to be empty before it can be closed — otherwise
            its units would be counted as in stock while nothing could reserve them.
        </div>

        <div class="field m-t-base">
            <label for="l-line1">Address</label>
            <input id="l-line1" type="text" bind:value={form.line1} placeholder="Line 1" />
        </div>
        <div class="field m-t-5">
            <input type="text" bind:value={form.line2} placeholder="Line 2" aria-label="Line 2" />
        </div>
        <div class="fields m-t-5">
            <div class="field">
                <input type="text" bind:value={form.city} placeholder="City" aria-label="City" />
            </div>
            <div class="delimiter"></div>
            <div class="field">
                <input
                    type="text"
                    bind:value={form.state}
                    placeholder="State"
                    aria-label="State"
                />
            </div>
        </div>
        <div class="fields m-t-5">
            <div class="field">
                <input
                    type="text"
                    bind:value={form.postal_code}
                    placeholder="Postal code"
                    aria-label="Postal code"
                />
            </div>
            <div class="delimiter"></div>
            <div class="field">
                <input
                    type="text"
                    maxlength="2"
                    bind:value={form.country}
                    placeholder="Country"
                    aria-label="Country"
                />
            </div>
        </div>
        <div class="field-help">
            Only needed where it is used: a pickup point a shopper is sent to, or the origin on a
            customs form.
        </div>

        {#if error}<div class="field-help error m-t-sm">{error}</div>{/if}
    </form>

    {#snippet footer()}
        <button type="button" class="btn transparent m-r-auto" onclick={() => (open = false)}>
            <span class="txt">Cancel</span>
        </button>
        <button
            type="submit"
            form="location-form"
            class="btn expanded"
            class:loading={saving}
            disabled={saving}
        >
            <span class="txt">{editing ? "Save changes" : "Open location"}</span>
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
