<script>
    /**
     * The options editor and the grouped variant table.
     *
     * Everything in here writes through its own route the moment it is applied,
     * rather than riding the page's save bar. That is not a shortcut: options,
     * variants and stock each have an endpoint of their own, and the option
     * matrix in particular is a single transaction that reconciles the variants
     * with it. Folding those into one "Save" would mean either firing four
     * unrelated requests behind one button or pretending the reconciliation had
     * not already happened.
     *
     * Two details are worth knowing before reading further.
     *
     * An axis's `id` is carried through the editor and sent back on save. The
     * engine matches axes by id, not by name — omitting it means "this one is
     * new", which strips every variant of that axis. Renaming "Size" without
     * the id would delete every size.
     *
     * And a variant's `options` is a list of values, not a map. The engine
     * refuses to let one value appear on two axes precisely so that a value can
     * be resolved back to its axis, which is what `valueOn` relies on.
     */
    import { api, request } from "$lib/api.js";
    import { toast } from "$lib/toast.svelte.js";
    import { toMinor, fromMinor, stockClass, pluralize, currencySymbol } from "$lib/format.js";
    import Select from "$lib/components/Select.svelte";
    import Drawer from "$lib/components/Drawer.svelte";
    import Confirm from "$lib/components/Confirm.svelte";

    let { product = $bindable(), media = [], onchange } = $props();

    // The variant whose image is being chosen; null while the picker is shut.
    let imageFor = $state(null);

    /**
     * A variant nominates one of the *product's* images rather than owning a
     * file, so the picker offers exactly what the product already shows. That
     * is also why it can be empty: with no media on the product there is
     * nothing to nominate, and the honest answer is to say so rather than to
     * open an upload behind a variant's back.
     */
    const imageChoices = $derived(media.filter((m) => m.kind === "image"));

    async function setVariantImage(variant, mediaID) {
        working = true;
        try {
            await request("PUT", `/api/admin/variants/${variant.id}/media`, {
                body: { media_id: mediaID },
            });
            imageFor = null;
        } catch (err) {
            toast.error(err);
        } finally {
            await refresh().catch((err) => toast.error(err));
            working = false;
        }
    }

    /**
     * Weight goes as a value plus its unit, never as grams — resolveWeight() in
     * the engine owns the conversion, and a second copy of the factors on this
     * side is how the two come to disagree about a pound.
     */
    async function commitWeight(variant, raw) {
        const value = parseFloat(raw);
        if (isNaN(value) || value < 0) return;
        const shown = parseFloat(String(variant.weight ?? ""));
        if (isFinite(shown) && shown === value) return;
        working = true;
        try {
            await api.patch(`/api/admin/variants/${variant.id}`, {
                weight: value,
                weight_unit: variant.weight_unit || "g",
            });
        } catch (err) {
            toast.error(err);
        } finally {
            await refresh().catch((err) => toast.error(err));
            working = false;
        }
    }

    /** The number to show in a variant's weight box, in its own unit. */
    function weightValue(variant) {
        const shown = parseFloat(String(variant.weight ?? ""));
        return isFinite(shown) ? shown : (variant.weight_grams ?? 0);
    }

    const axes = $derived(product?.options ?? []);
    const variants = $derived(product?.variants ?? []);
    const currency = $derived(variants[0]?.price?.currency ?? product?.currency ?? "USD");

    // --------------------------------------------------------------- options

    /** @type {{id: number|null, name: string, values: string[]}[]} */
    let draftAxes = $state([]);
    let axesDirty = $state(false);
    let savingAxes = $state(false);
    let axesError = $state("");
    /**
     * Adding an option value creates the variants that value implies.
     *
     * Shopify has no switch for this — typing "XL" gives you the XL variants —
     * and defaulting it off is what produced the reports of a variant table
     * that would not drill down: an axis whose values no variant used rendered
     * a row of groups that each expanded to nothing. The switch stays, because
     * an operator importing a partial matrix has a real reason to turn it off.
     */
    let generate = $state(true);
    let generatePrice = $state("");
    let lastChange = $state(null);

    let confirmAxesOpen = $state(false);

    $effect(() => {
        const source = product?.options ?? [];
        // Re-seeding on top of an edit in progress would delete what is being
        // typed, so the server's copy only wins while nothing is pending.
        if (axesDirty) return;
        draftAxes = source.map((axis) => ({
            id: axis.id,
            name: axis.name,
            // Shopify's trailing blank: the row that becomes real when typed
            // into, so adding a value is never a separate button press.
            values: [...axis.values.map((v) => v.value), ""],
        }));
    });

    /**
     * Which axis is open for editing, by index; null while all are collapsed.
     *
     * One at a time, because opening two turns the section back into the wall
     * of boxes that collapsing it was meant to fix.
     */
    let expandedAxis = $state(null);

    let axisDrag = $state(-1);
    let axisOver = $state(-1);

    function addAxis() {
        axesDirty = true;
        draftAxes = [...draftAxes, { id: null, name: "", values: [""] }];
        // A new axis has nothing to show collapsed, so it opens itself.
        expandedAxis = draftAxes.length - 1;
    }

    function removeAxis(index) {
        axesDirty = true;
        draftAxes = draftAxes.filter((_, i) => i !== index);
        expandedAxis = null;
    }

    /**
     * Axis order is the order the options are sent in, and the engine assigns
     * each one's position from its index — so dragging a row here really does
     * reorder the axes rather than only the display.
     *
     * Only collapsed rows are draggable: a row with focus in a text box should
     * not start a drag when somebody selects the text in it.
     */
    function startAxisDrag(event, index) {
        if (expandedAxis !== null) return;
        axisDrag = index;
        // Firefox will not begin a drag unless the payload is set.
        event.dataTransfer?.setData("text/plain", String(index));
        if (event.dataTransfer) event.dataTransfer.effectAllowed = "move";
    }

    function dragOverAxis(event, index) {
        if (axisDrag < 0) return;
        event.preventDefault();
        axisOver = index;
    }

    function dropAxis(event, index) {
        if (axisDrag < 0) return;
        event.preventDefault();
        const from = axisDrag;
        axisDrag = -1;
        axisOver = -1;
        if (from === index) return;
        const next = [...draftAxes];
        const [moved] = next.splice(from, 1);
        next.splice(index, 0, moved);
        draftAxes = next;
        axesDirty = true;
    }

    function endAxisDrag() {
        axisDrag = -1;
        axisOver = -1;
    }

    function onValueInput(axisIndex, valueIndex) {
        axesDirty = true;
        const axis = draftAxes[axisIndex];
        if (valueIndex === axis.values.length - 1 && axis.values[valueIndex].trim() !== "") {
            axis.values.push("");
        }
    }

    function removeValue(axisIndex, valueIndex) {
        axesDirty = true;
        const axis = draftAxes[axisIndex];
        axis.values.splice(valueIndex, 1);
        if (!axis.values.length || axis.values[axis.values.length - 1].trim() !== "") {
            axis.values.push("");
        }
    }

    function cleanedAxes() {
        return draftAxes
            .map((axis) => ({
                id: axis.id ?? undefined,
                name: axis.name.trim(),
                values: axis.values.map((v) => v.trim()).filter(Boolean),
            }))
            .filter((axis) => axis.name || axis.values.length);
    }

    /** valueOn returns the value a variant holds on one axis, or "". */
    function valueOn(variant, axis) {
        const offered = new Set(axis.values.map((v) => v.value));
        return (variant.options ?? []).find((value) => offered.has(value)) ?? "";
    }

    /**
     * doomed lists the variants this draft would delete.
     *
     * It mirrors SetOptions in options.go, which kills a variant two ways. One:
     * a value it holds on an axis that *survives* is no longer offered. Two:
     * dropping an axis leaves it identical to a variant that is already staying
     * — take Colour away and S/Red and S/Blue are both just S, and only the
     * first of them can be kept. Missing the second kind is what turned a
     * deleted option into a silent loss of every variant but one.
     */
    function doomed() {
        const kept = new Map();
        for (const axis of draftAxes) {
            if (axis.id == null) continue;
            kept.set(
                axis.id,
                new Set(axis.values.map((v) => v.trim().toLowerCase()).filter(Boolean)),
            );
        }
        // Same order as the engine's — the catalog's own — so the survivor the
        // panel names is the survivor the engine keeps.
        const claimed = new Set();
        return variants.filter((variant) => {
            const orphaned = axes.some((axis) => {
                const offered = kept.get(axis.id);
                if (!offered) return false;
                const held = valueOn(variant, axis);
                return !!held && !offered.has(held.toLowerCase());
            });
            if (orphaned) return true;
            const key = axes
                .filter((axis) => kept.has(axis.id))
                .map((axis) => `${axis.id}=${valueOn(variant, axis).toLowerCase()}`)
                .join(",");
            if (claimed.has(key)) return true;
            claimed.add(key);
            return false;
        });
    }

    const doomedList = $derived(doomed());

    function validateAxes() {
        const cleaned = cleanedAxes();
        for (const axis of cleaned) {
            if (!axis.name) return "Every option needs a name.";
            if (!axis.values.length) return `"${axis.name}" has no values.`;
        }
        const names = new Set();
        for (const axis of cleaned) {
            const key = axis.name.toLowerCase();
            if (names.has(key)) return `Two options are both called "${axis.name}".`;
            names.add(key);
        }
        // The engine refuses this too, and says why; catching it here saves a
        // round trip and keeps the sentence next to the field that caused it.
        const owner = new Map();
        for (const axis of cleaned) {
            for (const value of axis.values) {
                const key = value.toLowerCase();
                const previous = owner.get(key);
                if (previous && previous !== axis.name) {
                    return `"${value}" is a value on both "${previous}" and "${axis.name}", and a variant's options are matched by value, so the two could not be told apart.`;
                }
                owner.set(key, axis.name);
            }
        }
        return "";
    }

    function askSaveOptions() {
        axesError = validateAxes();
        if (axesError) return;
        if (doomedList.length) {
            confirmAxesOpen = true;
            return;
        }
        saveOptions();
    }

    async function saveOptions() {
        if (savingAxes) return;
        savingAxes = true;
        try {
            const body = { options: cleanedAxes(), generate_variants: generate };
            if (generate && generatePrice !== "") {
                body.price_minor = toMinor(generatePrice, currency);
            }
            const result = await request("PUT", `/api/admin/products/${product.id}/options`, {
                body,
            });
            product = result.product;
            lastChange = result.changed ?? null;
            axesDirty = false;
            for (const line of summarize(result.changed)) toast.success(line);
            if (!summarize(result.changed).length) toast.success("Options unchanged");
            onchange?.(result.product);
        } catch (err) {
            axesError = err.message;
            toast.error(err);
        } finally {
            savingAxes = false;
        }
    }

    /**
     * summarize turns the change report into sentences.
     *
     * The engine goes to the trouble of saying exactly what it did — including
     * which SKUs it deleted — and swallowing that in a generic "Saved" is how
     * an operator finds out a week later that a variant is gone.
     */
    function summarize(changed) {
        if (!changed) return [];
        const lines = [];
        const list = (values) => values.join(", ");

        if (changed.axes_added?.length) lines.push(`Added ${list(changed.axes_added)}`);
        if (changed.axes_renamed?.length) lines.push(`Renamed ${list(changed.axes_renamed)}`);
        if (changed.axes_removed?.length) lines.push(`Removed ${list(changed.axes_removed)}`);
        if (changed.values_added?.length) lines.push(`New values: ${list(changed.values_added)}`);
        if (changed.values_removed?.length) {
            lines.push(`Dropped values: ${list(changed.values_removed)}`);
        }
        if (changed.variants_created?.length) {
            const n = changed.variants_created.length;
            lines.push(`Created ${n} ${pluralize(n, "variant")}: ${list(changed.variants_created)}`);
        }
        if (changed.variants_removed?.length) {
            const n = changed.variants_removed.length;
            lines.push(`Deleted ${n} ${pluralize(n, "variant")}: ${list(changed.variants_removed)}`);
        }
        return lines;
    }

    // --------------------------------------------------------------- grouping

    let groupById = $state(null);
    let expanded = $state([]);
    let selected = $state([]);

    const groupAxis = $derived(axes.find((a) => a.id === groupById) ?? axes[0] ?? null);

    $effect(() => {
        // Follows the product: an axis that no longer exists cannot group by.
        const first = axes[0];
        if (!first) {
            groupById = null;
        } else if (!axes.some((a) => a.id === groupById)) {
            groupById = first.id;
        }
    });

    const groups = $derived.by(() => {
        if (!groupAxis) return [];
        const out = groupAxis.values.map((value) => ({
            key: String(value.id),
            label: value.value,
            items: variants.filter((v) => valueOn(v, groupAxis) === value.value),
        }));
        // A variant selecting nothing on this axis is the product's old default
        // variant, from before the axis existed. It is real and sellable, so it
        // gets a group of its own rather than disappearing from the table.
        const stray = variants.filter((v) => !valueOn(v, groupAxis));
        if (stray.length) {
            out.push({ key: "__none", label: `No ${groupAxis.name}`, items: stray });
        }
        return out;
    });

    /**
     * Combinations the axes describe that no variant exists for.
     *
     * This is the state that made the table look broken: an axis value with no
     * variant renders a group that expands to nothing. It happens to any
     * product whose options were saved while variant generation was off — which
     * was the default until recently — so the count is surfaced with a way to
     * fix it in place rather than requiring the operator to work out that
     * re-saving the options is the cure.
     */
    const missingCombinations = $derived.by(() => {
        if (!axes.length) return 0;
        let total = 1;
        for (const axis of axes) {
            const n = axis.values.length;
            if (!n) return 0;
            total *= n;
        }
        // Only variants that select a value on every axis count as filling a
        // combination; the product's pre-options default variant does not.
        const complete = variants.filter(
            (v) => axes.every((axis) => valueOn(v, axis) !== ""),
        ).length;
        return Math.max(0, total - complete);
    });

    /**
     * Fill the matrix without touching the axes: the same options back, with
     * generation on. The engine's SetOptions is a reconcile, so sending it what
     * it already has adds the missing combinations and changes nothing else.
     */
    async function generateMissing() {
        if (savingAxes) return;
        savingAxes = true;
        try {
            const result = await request("PUT", `/api/admin/products/${product.id}/options`, {
                body: {
                    options: axes.map((axis) => ({
                        id: axis.id,
                        name: axis.name,
                        values: axis.values.map((v) => v.value),
                    })),
                    generate_variants: true,
                },
            });
            product = result.product;
            lastChange = result.changed ?? null;
            for (const line of summarize(result.changed)) toast.success(line);
            onchange?.(result.product);
        } catch (err) {
            toast.error(err);
        } finally {
            savingAxes = false;
        }
    }

    const totalAvailable = $derived(
        variants.filter((v) => v.track_inventory).reduce((sum, v) => sum + v.available, 0),
    );
    const untracked = $derived(variants.filter((v) => !v.track_inventory).length);
    const allSelected = $derived(variants.length > 0 && selected.length === variants.length);

    function toggleExpanded(key) {
        expanded = expanded.includes(key)
            ? expanded.filter((k) => k !== key)
            : [...expanded, key];
    }

    function toggleAll() {
        selected = allSelected ? [] : variants.map((v) => v.id);
    }

    function toggleVariant(id) {
        selected = selected.includes(id)
            ? selected.filter((v) => v !== id)
            : [...selected, id];
    }

    function toggleGroup(group) {
        const ids = group.items.map((v) => v.id);
        const every = ids.every((id) => selected.includes(id));
        selected = every
            ? selected.filter((id) => !ids.includes(id))
            : [...new Set([...selected, ...ids])];
    }

    function groupSelected(group) {
        return group.items.length > 0 && group.items.every((v) => selected.includes(v.id));
    }

    // ------------------------------------------------------------- mutations

    let working = $state(false);
    let groupPrice = $state({});
    let groupStock = $state({});

    async function refresh() {
        product = await api.get(`/api/admin/products/${product.id}`);
        onchange?.(product);
    }

    /**
     * apply runs one change over a list of variants, one request at a time, and
     * reports each failure on its own.
     *
     * Aborting the batch on the first error is the wrong shape here: half the
     * group would already be at the new price with nothing saying which half.
     * Sequential rather than parallel because these all land on the same rows.
     */
    async function apply(list, fn, describe) {
        if (working || !list.length) return;
        working = true;
        let done = 0;
        const failures = [];
        try {
            for (const variant of list) {
                try {
                    await fn(variant);
                    done++;
                } catch (err) {
                    failures.push(`${variant.sku}: ${err.message}`);
                }
            }
            if (done) toast.success(`${describe} on ${done} ${pluralize(done, "variant")}`);
            for (const failure of failures) toast.error(failure);
            await refresh();
        } finally {
            working = false;
        }
    }

    function applyGroupPrice(group) {
        const value = groupPrice[group.key];
        if (value === undefined || value === "" || isNaN(parseFloat(value))) {
            toast.error("Enter a price first.");
            return;
        }
        const price_minor = toMinor(value, currency);
        apply(
            group.items,
            (v) => api.patch(`/api/admin/variants/${v.id}`, { price_minor }),
            "Price set",
        ).then(() => (groupPrice[group.key] = ""));
    }

    function applyGroupStock(group) {
        const value = groupStock[group.key];
        const count = parseInt(value, 10);
        if (isNaN(count)) {
            toast.error("Enter a whole number first.");
            return;
        }
        const tracked = group.items.filter((v) => v.track_inventory);
        if (!tracked.length) {
            toast.error(`Nothing in ${group.label} tracks inventory.`);
            return;
        }
        apply(
            tracked,
            (v) => api.post(`/api/admin/variants/${v.id}/inventory`, { set: count }),
            "Stock set",
        ).then(() => (groupStock[group.key] = ""));
    }

    async function commit(variant, body) {
        working = true;
        try {
            await api.patch(`/api/admin/variants/${variant.id}`, body);
        } catch (err) {
            toast.error(err);
        } finally {
            // Either way the row is re-read: a refused edit must not sit in the
            // box looking as though it took.
            await refresh().catch((err) => toast.error(err));
            working = false;
        }
    }

    function commitPrice(variant, raw) {
        if (raw === "" || isNaN(parseFloat(raw))) return;
        const price_minor = toMinor(raw, currency);
        if (price_minor === variant.price.amount_minor) return;
        commit(variant, { price_minor });
    }

    function commitSku(variant, raw) {
        const sku = raw.trim();
        if (!sku || sku === variant.sku) return;
        commit(variant, { sku });
    }

    async function commitStock(variant, raw) {
        const count = parseInt(raw, 10);
        if (isNaN(count) || count === variant.stock_on_hand) return;
        working = true;
        try {
            await api.post(`/api/admin/variants/${variant.id}/inventory`, { set: count });
        } catch (err) {
            // The engine refuses to drop on-hand below what open orders have
            // reserved, and explains itself better than this could.
            toast.error(err);
        } finally {
            await refresh().catch((err) => toast.error(err));
            working = false;
        }
    }

    function setActive(active) {
        apply(
            variants.filter((v) => selected.includes(v.id)),
            (v) => api.patch(`/api/admin/variants/${v.id}`, { active }),
            active ? "Activated" : "Deactivated",
        ).then(() => (selected = []));
    }

    // -------------------------------------------------------- add and delete

    let addOpen = $state(false);
    let addForm = $state({ sku: "", price: "", active: true, options: {} });
    let addErrors = $state({});
    let addSaving = $state(false);

    let deleteOpen = $state(false);
    let pendingDelete = $state(null);
    let bulkDeleteOpen = $state(false);

    function openAdd() {
        addForm = {
            sku: "",
            price: "",
            active: true,
            options: Object.fromEntries(axes.map((axis) => [axis.id, ""])),
        };
        addErrors = {};
        addOpen = true;
    }

    async function addVariant(event) {
        event?.preventDefault();
        if (addSaving) return;

        addErrors = {};
        const sku = addForm.sku.trim();
        if (!sku) addErrors.sku = "A SKU is required.";
        if (addForm.price === "" || isNaN(parseFloat(addForm.price))) {
            addErrors.price = "A price is required.";
        }
        if (axes.some((axis) => !addForm.options[axis.id])) {
            addErrors.options = "Choose a value on every option.";
        }
        if (Object.keys(addErrors).length) return;

        addSaving = true;
        try {
            await api.post(`/api/admin/products/${product.id}/variants`, {
                sku,
                price_minor: toMinor(addForm.price, currency),
                active: addForm.active,
                options: axes.map((axis) => addForm.options[axis.id]),
            });
            addOpen = false;
            toast.success(`Added ${sku}`);
            await refresh();
        } catch (err) {
            // "a variant with that combination of options already exists" is
            // the lesson; pass it through rather than paraphrasing it.
            toast.error(err);
        } finally {
            addSaving = false;
        }
    }

    function askDelete(variant) {
        pendingDelete = variant;
        deleteOpen = true;
    }

    async function doDelete() {
        try {
            await api.delete(`/api/admin/variants/${pendingDelete.id}`);
            toast.success(`Deleted ${pendingDelete.sku}`);
            selected = selected.filter((id) => id !== pendingDelete.id);
            await refresh();
        } catch (err) {
            // A product must keep something sellable, and the engine says so.
            toast.error(err);
        }
    }

    async function doBulkDelete() {
        const list = variants.filter((v) => selected.includes(v.id));
        await apply(list, (v) => api.delete(`/api/admin/variants/${v.id}`), "Deleted");
        selected = [];
    }
</script>

<h6 class="section-title">
    <i class="ri-list-settings-line" aria-hidden="true"></i>
    Options
</h6>

<!--
    Shopify's options editor: each axis is one collapsed row — a drag handle,
    its name, and its values as chips — until you open it.

    Always-expanded is what this was, and with two axes of five values it filled
    a screen with twenty labelled boxes to say something a single line of chips
    says better. Collapsed is also what makes the *shape* of the matrix legible
    at a glance, which is the thing you are actually checking when you look at
    this section.
-->
<div class="list option-list m-b-sm">
    {#each draftAxes as axis, axisIndex (axisIndex)}
        {@const open = expandedAxis === axisIndex}
        {@const chips = axis.values.map((v) => v.trim()).filter(Boolean)}

        <div
            class="list-item option-row"
            class:option-open={open}
            data-dragging={axisDrag === axisIndex}
            data-dropinto={axisOver === axisIndex && axisDrag !== axisIndex}
            draggable={!open && draftAxes.length > 1}
            ondragstart={(e) => startAxisDrag(e, axisIndex)}
            ondragover={(e) => dragOverAxis(e, axisIndex)}
            ondrop={(e) => dropAxis(e, axisIndex)}
            ondragend={endAxisDrag}
            role="listitem"
        >
            {#if open}
                <div class="content">
                    <div class="field">
                        <label for="axis-name-{axisIndex}">Option name</label>
                        <input
                            id="axis-name-{axisIndex}"
                            type="text"
                            placeholder="Size"
                            bind:value={axis.name}
                            oninput={() => (axesDirty = true)}
                        />
                    </div>

                    {#each axis.values as _, valueIndex (valueIndex)}
                        <div class="option-value-row">
                            <div class="field">
                                <label for="axis-{axisIndex}-value-{valueIndex}">
                                    Option value {valueIndex + 1}
                                </label>
                                <!--
                                    The last box is always blank and becomes real
                                    the moment it is typed into, so adding a
                                    value is never a separate button press.
                                -->
                                <input
                                    id="axis-{axisIndex}-value-{valueIndex}"
                                    type="text"
                                    placeholder={valueIndex === axis.values.length - 1
                                        ? "Add another value"
                                        : ""}
                                    bind:value={axis.values[valueIndex]}
                                    oninput={() => onValueInput(axisIndex, valueIndex)}
                                />
                            </div>
                            {#if valueIndex < axis.values.length - 1}
                                <button
                                    type="button"
                                    class="btn circle sm transparent secondary"
                                    aria-label="Remove the value {axis.values[valueIndex]}"
                                    title="Remove this value"
                                    onclick={() => removeValue(axisIndex, valueIndex)}
                                >
                                    <i class="ri-close-line" aria-hidden="true"></i>
                                </button>
                            {/if}
                        </div>
                    {/each}

                    <div class="inline-flex gap-sm m-t-sm">
                        <button
                            type="button"
                            class="btn sm secondary"
                            onclick={() => (expandedAxis = null)}
                        >
                            <span class="txt">Done</span>
                        </button>
                        <button
                            type="button"
                            class="btn sm transparent danger"
                            onclick={() => removeAxis(axisIndex)}
                        >
                            <span class="txt">Delete option</span>
                        </button>
                    </div>
                </div>
            {:else}
                <span class="option-handle" aria-hidden="true">
                    <i class="ri-draggable"></i>
                </span>
                <!-- The whole row opens it: a collapsed option has nothing else
                     to click, and a lone "edit" button beside it would be a
                     second way to do the only thing here. -->
                <button
                    type="button"
                    class="option-summary"
                    onclick={() => (expandedAxis = axisIndex)}
                >
                    <span class="option-name">{axis.name.trim() || "Untitled option"}</span>
                    <span class="option-chips">
                        {#each chips as value (value)}
                            <span class="label">{value}</span>
                        {/each}
                        {#if !chips.length}
                            <span class="txt-hint txt-sm">No values yet</span>
                        {/if}
                    </span>
                </button>
            {/if}
        </div>
    {/each}

    <!-- Inside the group, as Shopify has it: adding an option is another row of
         the same list rather than a button floating under it. -->
    <div class="list-item">
        <button type="button" class="btn sm transparent option-add" onclick={addAxis}>
            <i class="ri-add-circle-line" aria-hidden="true"></i>
            <span class="txt">Add another option</span>
        </button>
    </div>
</div>

{#if axesDirty}
    <div class="field m-t-sm">
        <input id="generate-variants" type="checkbox" class="switch" bind:checked={generate} />
        <label for="generate-variants">Create a variant for every missing combination</label>
    </div>

    {#if generate}
        <div class="field m-t-sm">
            <label for="generate-price">Price for the generated variants ({currency})</label>
            <input
                id="generate-price"
                type="text"
                inputmode="decimal"
                placeholder="Copies the product's existing price if left empty"
                bind:value={generatePrice}
            />
        </div>
    {/if}

    {#if axesError}<div class="field-help error">{axesError}</div>{/if}

    <div class="inline-flex gap-sm m-t-sm">
        <button
            type="button"
            class="btn sm"
            class:loading={savingAxes}
            disabled={savingAxes}
            onclick={askSaveOptions}
        >
            <span class="txt">Save options</span>
        </button>
        <button
            type="button"
            class="btn sm transparent secondary"
            disabled={savingAxes}
            onclick={() => ((axesDirty = false), (axesError = ""), (generate = true))}
        >
            <span class="txt">Discard</span>
        </button>
    </div>
    <div class="field-help">
        The whole matrix is applied in one transaction. A variant whose combination survives is
        left completely alone — price, SKU and stock included — and one whose combination is gone
        is deleted. Deleting an option merges the variants it separated: where several are left
        with the same combination, the first keeps its price, SKU and stock and the rest go.
        Renaming an option keeps its variants, because the option's identity travels with the
        request.
    </div>
{/if}

{#if lastChange && summarize(lastChange).length}
    <div class="field-help">
        {#each summarize(lastChange) as line (line)}
            <p>{line}</p>
        {/each}
    </div>
{/if}

<h6 class="section-title">
    <i class="ri-shopping-bag-3-line" aria-hidden="true"></i>
    Variants
    {#if axes.length}
        <button type="button" class="btn sm secondary" onclick={openAdd}>
            <i class="ri-add-line" aria-hidden="true"></i>
            <span class="txt">Add variant</span>
        </button>
    {/if}
</h6>

{#if !axes.length}
    <div class="field-help">
        This product has no options, so it is sold as one thing and has exactly one variant. Its
        price, stock and weight are the fields above. Add an option to sell it in more than one
        form.
    </div>
{:else}
    {#if missingCombinations > 0}
        <!-- Actionable, not just descriptive. The operator can see the empty
             groups; what they cannot see is that the cure is to re-save the
             options with generation on. -->
        <div class="missing-variants m-b-sm">
            <div>
                <strong>
                    {missingCombinations}
                    {pluralize(missingCombinations, "combination")}
                </strong>
                of these options {missingCombinations === 1 ? "has" : "have"} no variant, so
                {missingCombinations === 1 ? "its group is" : "their groups are"} empty below.
            </div>
            <button
                type="button"
                class="btn sm secondary"
                class:loading={savingAxes}
                disabled={savingAxes}
                onclick={generateMissing}
            >
                <span class="txt">Create them</span>
            </button>
        </div>
    {/if}

    <!--
        Shopify's toolbar: the label sits beside its control as plain text, not
        inside a filled box above it. These are not form fields being filled in —
        they set what the table below shows, and dressing them as fields made two
        of the tallest controls on the page out of a sort order and a checkbox.
    -->
    <div class="variant-toolbar m-b-sm">
        <label class="variant-toolbar-label" for="group-by">Group by</label>
        <Select
            id="group-by"
            class="compact"
            bind:value={groupById}
            options={axes.map((axis) => ({ value: axis.id, label: axis.name }))}
        />
        <div class="field variant-toolbar-check">
            <input id="select-all" type="checkbox" checked={allSelected} onchange={toggleAll} />
            <label for="select-all" class="txt-nowrap">
                Select all {variants.length} on page
            </label>
        </div>
    </div>

    <div class="table-scroll">
        <table class="table">
            <thead>
                <!--
                    Shopify's columns, in Shopify's order: the thumbnail sits
                    beside the name rather than in a column of its own, and the
                    expand chevron moves to the right-hand end.

                    Two of theirs are missing on purpose. "Publishing" counts
                    sales channels, which this engine has no model for, and a
                    column of zeroes would be a claim rather than a number. SKU
                    and Weight are theirs too — they live on Shopify's separate
                    variant page, and since this table is the only place to edit
                    them here, they stay in it.
                -->
                <tr>
                    <th class="col-bulk-select min-width"></th>
                    <th class="min-width"></th>
                    <th>Variant</th>
                    <th class="col-field-type-number min-width">Price</th>
                    <th class="col-field-type-number min-width">Available</th>
                    <th class="min-width">SKU</th>
                    <th class="col-field-type-number min-width">Weight</th>
                    <th class="col-meta min-width"></th>
                </tr>
            </thead>
            <tbody>
                {#each groups as group (group.key)}
                    <tr>
                        <td class="col-bulk-select min-width">
                            <div class="field">
                                <input
                                    id="group-select-{group.key}"
                                    type="checkbox"
                                    checked={groupSelected(group)}
                                    onchange={() => toggleGroup(group)}
                                />
                                <label
                                    for="group-select-{group.key}"
                                    aria-label="Select every variant in {group.label}"
                                ></label>
                            </div>
                        </td>
                        <td class="min-width">
                            <!-- A group is a set of variants and an image
                                 belongs to one of them, so Shopify leaves this
                                 empty too; the picker is on the rows below. -->
                            <span class="txt-hint txt-sm">—</span>
                        </td>
                        <td>
                            <strong>{group.label}</strong>
                            {#if group.items.length}
                                <span class="txt-hint txt-sm">
                                    {group.items.length}
                                    {pluralize(group.items.length, "variant")}
                                </span>
                            {:else}
                                <span class="txt-hint txt-sm">no variants yet</span>
                            {/if}
                        </td>
                        <td class="col-field-type-number min-width">
                            <!--
                                No Apply button: Shopify's group price is a box
                                you type in, and it lands when you leave it. The
                                button was there because writing a whole group is
                                a bulk change — but so is typing in it, and one
                                of the two had to be the commit.
                            -->
                            <div class="field money-field">
                                <span class="money-prefix" aria-hidden="true">{currencySymbol(currency)}</span>
                                <input
                                    type="text"
                                    inputmode="decimal"
                                    placeholder="0.00"
                                    disabled={working || !group.items.length}
                                    aria-label="Price for every variant in {group.label}"
                                    bind:value={groupPrice[group.key]}
                                    onchange={() => applyGroupPrice(group)}
                                />
                            </div>
                        </td>
                        <td class="col-field-type-number min-width">
                            <div class="field">
                                <input
                                    type="number"
                                    placeholder="0"
                                    disabled={working || !group.items.length}
                                    aria-label="On hand for every variant in {group.label}"
                                    bind:value={groupStock[group.key]}
                                    onchange={() => applyGroupStock(group)}
                                />
                            </div>
                        </td>
                        <td class="min-width txt-hint txt-sm">—</td>
                        <!-- SKU and weight are per variant; a group has neither
                             to show, and the column has to stay aligned. -->
                        <td class="min-width txt-hint txt-sm">—</td>
                        <td class="col-meta min-width">
                            <!-- The chevron lives at the right-hand end, as
                                 Shopify's does. A group with no variants has
                                 nothing to open onto, so it gets no control —
                                 offering one that opened onto nothing is what
                                 made the table look broken. -->
                            {#if group.items.length}
                                <button
                                    type="button"
                                    class="btn circle sm transparent secondary"
                                    aria-expanded={expanded.includes(group.key)}
                                    aria-label="{expanded.includes(group.key)
                                        ? 'Collapse'
                                        : 'Expand'} {group.label}"
                                    onclick={() => toggleExpanded(group.key)}
                                >
                                    <i
                                        class={expanded.includes(group.key)
                                            ? "ri-arrow-up-s-line"
                                            : "ri-arrow-down-s-line"}
                                        aria-hidden="true"
                                    ></i>
                                </button>
                            {/if}
                        </td>
                    </tr>

                    {#if expanded.includes(group.key)}
                        {#each group.items as variant (variant.id)}
                            <tr class="variant-child">
                                <td class="col-bulk-select min-width">
                                    <div class="field">
                                        <input
                                            id="variant-select-{variant.id}"
                                            type="checkbox"
                                            checked={selected.includes(variant.id)}
                                            onchange={() => toggleVariant(variant.id)}
                                        />
                                        <label
                                            for="variant-select-{variant.id}"
                                            aria-label="Select {variant.sku}"
                                        ></label>
                                    </div>
                                </td>
                                <td class="min-width">
                                    <button
                                        type="button"
                                        class="thumb sm variant-thumb"
                                        disabled={working}
                                        aria-label="Choose the image for {variant.sku}"
                                        title={variant.image
                                            ? "Change this variant's image"
                                            : "Choose an image for this variant"}
                                        onclick={() => (imageFor = variant)}
                                    >
                                        {#if variant.image}
                                            <img
                                                src={variant.image.url}
                                                alt={variant.image.alt || variant.sku}
                                            />
                                        {:else}
                                            <i class="ri-image-add-line" aria-hidden="true"></i>
                                        {/if}
                                    </button>
                                </td>
                                <td>
                                    <span class="txt-hint">{variant.label || "Default"}</span>
                                    {#if !variant.active}
                                        <span class="label">inactive</span>
                                    {/if}
                                </td>
                                <td class="col-field-type-number min-width">
                                    <div class="field money-field">
                                        <span class="money-prefix" aria-hidden="true"
                                            >{currencySymbol(currency)}</span
                                        >
                                        <input
                                            type="text"
                                            inputmode="decimal"
                                            aria-label="Price for {variant.sku}"
                                            value={fromMinor(
                                                variant.price.amount_minor,
                                                variant.price.currency,
                                            )}
                                            onchange={(e) =>
                                                commitPrice(variant, e.currentTarget.value)}
                                        />
                                    </div>
                                </td>
                                <td class="col-field-type-number min-width">
                                    {#if variant.track_inventory}
                                        <div class="field">
                                            <input
                                                type="number"
                                                aria-label="On hand for {variant.sku}"
                                                value={variant.stock_on_hand}
                                                onchange={(e) =>
                                                    commitStock(variant, e.currentTarget.value)}
                                            />
                                        </div>
                                        <!--
                                            Only when it differs from the box
                                            above it. On hand and available are
                                            the same number until an open order
                                            has reserved some, so printing both
                                            was the same figure twice; printing
                                            neither would hide the one case
                                            where a seller is about to oversell.
                                        -->
                                        {#if variant.available !== variant.stock_on_hand}
                                            <span
                                                class="txt-sm {stockClass(variant.available)}"
                                                title="{variant.stock_on_hand - variant.available} reserved by open orders"
                                            >
                                                {variant.available} free
                                            </span>
                                        {/if}
                                    {:else}
                                        <span class="txt-hint">not tracked</span>
                                    {/if}
                                </td>
                                <td class="min-width">
                                    <div class="field">
                                        <input
                                            type="text"
                                            class="txt-code"
                                            aria-label="SKU for {variant.sku}"
                                            value={variant.sku}
                                            onchange={(e) =>
                                                commitSku(variant, e.currentTarget.value)}
                                        />
                                    </div>
                                </td>
                                <td class="col-field-type-number min-width">
                                    <div class="fields">
                                        <div class="field">
                                            <input
                                                type="number"
                                                min="0"
                                                step="any"
                                                aria-label="Weight for {variant.sku}"
                                                value={weightValue(variant)}
                                                onchange={(e) =>
                                                    commitWeight(variant, e.currentTarget.value)}
                                            />
                                        </div>
                                        <span class="txt-hint txt-sm p-l-5">
                                            {variant.weight_unit || "g"}
                                        </span>
                                    </div>
                                </td>
                                <td class="col-meta min-width">
                                    <button
                                        type="button"
                                        class="btn circle sm transparent secondary"
                                        aria-label="Delete {variant.sku}"
                                        title="Delete"
                                        onclick={() => askDelete(variant)}
                                    >
                                        <i class="ri-delete-bin-7-line" aria-hidden="true"></i>
                                    </button>
                                </td>
                            </tr>
                        {/each}
                    {/if}
                {/each}

                {#if !variants.length}
                    <tr>
                        <td colspan="7" class="txt-center txt-hint p-base">
                            No variants yet. Save the options with "Create a variant for every
                            missing combination" on, or add one by hand.
                        </td>
                    </tr>
                {/if}
            </tbody>
        </table>
    </div>

    <div class="field-help">
        <!-- Shopify says "across all locations" here. This engine has exactly
             one, so the plain total is the honest wording; inventing a location
             count would imply a feature that does not exist. -->
        <strong>Total inventory: {totalAvailable} available</strong>
        {#if untracked}
            — {untracked}
            {pluralize(untracked, "variant")}
            {untracked === 1 ? "is" : "are"} not tracked and count for nothing here.
        {/if}
    </div>
    <div class="field-help">
        Changes apply as you leave a field. Available is on hand minus what open orders have
        reserved.
    </div>
{/if}

{#if selected.length}
    <div class="bulkbar">
        <span class="txt">
            {selected.length}
            {pluralize(selected.length, "variant")} selected
        </span>
        <div class="flex-fill"></div>
        <button
            type="button"
            class="btn sm secondary"
            disabled={working}
            onclick={() => setActive(true)}
        >
            <span class="txt">Activate</span>
        </button>
        <button
            type="button"
            class="btn sm secondary"
            disabled={working}
            onclick={() => setActive(false)}
        >
            <span class="txt">Deactivate</span>
        </button>
        <button
            type="button"
            class="btn sm danger"
            disabled={working}
            onclick={() => (bulkDeleteOpen = true)}
        >
            <span class="txt">Delete</span>
        </button>
        <button
            type="button"
            class="btn circle sm transparent secondary"
            aria-label="Clear the selection"
            onclick={() => (selected = [])}
        >
            <i class="ri-close-line" aria-hidden="true"></i>
        </button>
    </div>
{/if}

<Drawer
    open={addOpen}
    size="popup sm"
    title="New variant"
    onclose={() => (addOpen = false)}
>
    <form id="add-variant-form" onsubmit={addVariant}>
        <div class="field required" class:error={!!addErrors.sku}>
            <label for="add-sku">SKU</label>
            <input id="add-sku" type="text" placeholder="TEE-M" bind:value={addForm.sku} />
        </div>
        {#if addErrors.sku}<div class="field-help error">{addErrors.sku}</div>{/if}

        <div class="field required m-t-sm" class:error={!!addErrors.price}>
            <label for="add-price">Price ({currency})</label>
            <input id="add-price" type="text" inputmode="decimal" bind:value={addForm.price} />
        </div>
        {#if addErrors.price}<div class="field-help error">{addErrors.price}</div>{/if}

        {#each axes as axis (axis.id)}
            <div
                class="field required m-t-sm"
                class:error={!!addErrors.options && !addForm.options[axis.id]}
            >
                <label for="add-axis-{axis.id}">{axis.name}</label>
                <Select
                    id="add-axis-{axis.id}"
                    placeholder="Choose {axis.name.toLowerCase()}"
                    bind:value={addForm.options[axis.id]}
                    options={axis.values.map((v) => ({ value: v.value, label: v.value }))}
                />
            </div>
        {/each}
        {#if addErrors.options}<div class="field-help error">{addErrors.options}</div>{/if}

        <div class="field m-t-base">
            <input id="add-active" type="checkbox" class="switch" bind:checked={addForm.active} />
            <label for="add-active">Active — on sale</label>
        </div>

        <div class="field-help m-t-sm">
            A new variant starts with nothing on hand. Give it stock from the table, or from the
            Inventory screen.
        </div>
    </form>

    {#snippet footer()}
        <button type="button" class="btn transparent m-r-auto" onclick={() => (addOpen = false)}>
            <span class="txt">Cancel</span>
        </button>
        <button
            type="submit"
            form="add-variant-form"
            class="btn expanded"
            class:loading={addSaving}
            disabled={addSaving}
        >
            <span class="txt">Add variant</span>
        </button>
    {/snippet}
</Drawer>

<Confirm
    bind:open={confirmAxesOpen}
    title="This removes variants"
    message={`${doomedList
        .map((v) => v.sku)
        .join(", ")} will be deleted: their combination is gone from this matrix, or dropping an option leaves them identical to a variant that stays. Order lines keep their own snapshot, so history stays readable; carts holding them do not.`}
    confirmLabel="Apply anyway"
    danger
    onconfirm={saveOptions}
/>

<Confirm
    bind:open={deleteOpen}
    title="Delete this variant?"
    message={pendingDelete
        ? `${pendingDelete.sku} leaves every cart holding it. Order lines keep their snapshot. A product must keep at least one variant, so the last one cannot go.`
        : ""}
    confirmLabel="Delete"
    danger
    onconfirm={doDelete}
/>

<Confirm
    bind:open={bulkDeleteOpen}
    title="Delete {selected.length} {pluralize(selected.length, 'variant')}?"
    message="Each one is deleted on its own, so a refusal — the last variant of a product, for instance — stops only that one and the rest still go."
    confirmLabel="Delete"
    danger
    onconfirm={doBulkDelete}
/>

<!--
    Choosing a variant's image. It picks from the product's media rather than
    uploading, because that is the model: a variant nominates one of the
    pictures the product already shows, so the same file is stored once and a
    storefront can swap to it when a shopper picks a colour.
-->
<Drawer
    open={!!imageFor}
    title={imageFor ? `Image for ${imageFor.label || imageFor.sku}` : "Variant image"}
    size="sm"
    onclose={() => (imageFor = null)}
>
    {#if !imageChoices.length}
        <div class="txt-center txt-hint p-base">
            This product has no images yet. Add one in the Media section above and it becomes
            available here.
        </div>
    {:else}
        <div class="media-grid" role="list">
            {#each imageChoices as item (item.id)}
                {@const chosen = imageFor?.image?.media_id === item.id}
                <div class="media-cell" role="listitem">
                    <div class="media-tile" data-dropinto={chosen}>
                        <button
                            type="button"
                            class="btn transparent p-0 block"
                            style="height: 100%"
                            aria-pressed={chosen}
                            disabled={working}
                            aria-label="{chosen ? 'Currently chosen' : 'Choose'} {item.filename ||
                                `media ${item.id}`}"
                            onclick={() => setVariantImage(imageFor, item.id)}
                        >
                            <img src={item.url} alt={item.alt || item.filename || ""} />
                            <span class="media-check" data-checked={chosen} aria-hidden="true">
                                {#if chosen}<i class="ri-check-line"></i>{/if}
                            </span>
                        </button>
                    </div>
                    <div class="media-cell-name txt-sm txt-ellipsis">
                        {item.filename || `media ${item.id}`}
                    </div>
                </div>
            {/each}
        </div>
    {/if}

    <div class="field-help">
        One image per variant. Removing the file from the product removes it here too — the
        variant points at the product's picture rather than holding its own.
    </div>

    {#snippet footer()}
        <button type="button" class="btn transparent m-r-auto" onclick={() => (imageFor = null)}>
            <span class="txt">Cancel</span>
        </button>
        {#if imageFor?.image}
            <button
                type="button"
                class="btn secondary"
                disabled={working}
                onclick={() => setVariantImage(imageFor, null)}
            >
                <span class="txt">Remove image</span>
            </button>
        {/if}
    {/snippet}
</Drawer>
