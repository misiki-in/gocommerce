<script>
    /**
     * The category tree.
     *
     * A tree is rendered as a flat table with an indent rather than as nested
     * lists, because everything else on this screen — the row hover, the
     * responsive stacking, the meta column — is table behaviour that PocketBase
     * already has, and nesting <ul>s would rebuild all of it badly. The server
     * returns the tree flattened depth-first with a `depth` on each row, so the
     * indent is the only thing the table has to do that a flat list would not.
     *
     * Delete is refused by the engine while anything still points at a category
     * — subcategories or products — and says which. That refusal is the whole
     * safety story, so this page does not pre-empt it with a guess: it asks,
     * sends, and shows what came back.
     */
    import { api } from "$lib/api.js";
    import { toast } from "$lib/toast.svelte.js";
    import Drawer from "$lib/components/Drawer.svelte";
    import TokenInput from "$lib/components/TokenInput.svelte";
    import Confirm from "$lib/components/Confirm.svelte";
    import CategoryPicker from "$lib/components/CategoryPicker.svelte";
    import ThemeToggle from "$lib/components/ThemeToggle.svelte";

    let loading = $state(true);
    let saving = $state(false);
    /**
     * The rows on screen, in the order they are drawn: roots, with each
     * expanded branch's children spliced in directly beneath it.
     *
     * One level at a time, because the whole tree is not a thing that can be
     * asked for any more. Importing Shopify's taxonomy puts fourteen thousand
     * categories in this table; the listing that used to return all of them now
     * returns a bounded slice, and a page that rendered that slice would show
     * two hundred rows and call it the catalogue.
     */
    let categories = $state([]);
    let total = $state(0);
    let expanded = $state(new Set());
    let busyRow = $state(null);

    let editorOpen = $state(false);
    let editing = $state(null); // null = creating
    let form = $state({ title: "", slug: "", parent_id: null });
    /**
     * The fields products in this category are asked for, as the drawer edits
     * them. They live in the category's `metadata.attributes` — the same place
     * a taxonomy import would write them — and the product editor reads them
     * from there, inherited down the tree.
     */
    let attributes = $state([]);
    let errors = $state({});

    let confirmOpen = $state(false);
    let pendingDelete = $state(null);

    /**
     * The parent picker must not offer the category being edited, or anything
     * beneath it — those are exactly the moves the engine refuses as cycles,
     * and offering a choice that always fails is worse than not offering it.
     */
    const parentOptions = $derived.by(() => {
        if (!editing) return categories;
        const banned = new Set([editing.id]);
        // One pass is enough: the rows are in draw order, so a category's
        // ancestors are always seen before it.
        for (const c of categories) {
            if (c.parent_id !== null && banned.has(c.parent_id)) banned.add(c.id);
        }
        return categories.filter((c) => !banned.has(c.id));
    });

    // Past the point where the whole tree fits in one response the picker has
    // to search rather than filter what this page happens to have open.
    const parentRemote = $derived(total > categories.length);

    $effect(() => {
        load();
    });

    async function load() {
        loading = true;
        expanded = new Set();
        try {
            const [roots, counted] = await Promise.all([
                api.get("/api/admin/categories?parent=root"),
                // The bounded listing is the only thing that reports the real
                // size of the tree, and the footer should not claim the number
                // of rows it happens to be showing.
                api.get("/api/admin/categories?flat=1"),
            ]);
            categories = roots.data ?? [];
            total = counted.meta?.total ?? categories.length;
        } catch (err) {
            toast.error(err);
        } finally {
            loading = false;
        }
    }

    /**
     * Expanding splices a branch's children in under it; collapsing removes
     * that whole subtree, however deep it was opened.
     *
     * Children are fetched once and then kept, so re-opening a branch is
     * instant — and closing one does not throw away work the operator may be
     * about to want back.
     */
    async function toggle(category) {
        const index = categories.findIndex((c) => c.id === category.id);
        if (index < 0) return;

        if (expanded.has(category.id)) {
            // Everything deeper than this row, up to the next sibling-or-
            // shallower row, is its subtree.
            let end = index + 1;
            while (end < categories.length && categories[end].depth > category.depth) end++;
            const removed = categories.slice(index + 1, end);
            categories = [...categories.slice(0, index + 1), ...categories.slice(end)];
            const next = new Set(expanded);
            next.delete(category.id);
            for (const r of removed) next.delete(r.id);
            expanded = next;
            return;
        }

        busyRow = category.id;
        try {
            const result = await api.get(`/api/admin/categories?parent=${category.id}`);
            const children = result.data ?? [];
            categories = [
                ...categories.slice(0, index + 1),
                ...children,
                ...categories.slice(index + 1),
            ];
            expanded = new Set(expanded).add(category.id);
        } catch (err) {
            toast.error(err);
        } finally {
            busyRow = null;
        }
    }

    function openCreate(parentID = null) {
        editing = null;
        form = { title: "", slug: "", parent_id: parentID };
        attributes = [];
        errors = {};
        editorOpen = true;
    }

    function openEdit(category) {
        editing = category;
        form = {
            title: category.title,
            slug: category.slug,
            parent_id: category.parent_id ?? null,
        };
        attributes = (category.metadata?.attributes ?? []).map((a) => ({
            key: a.key ?? "",
            label: a.label ?? "",
            choices: [...(a.choices ?? [])],
        }));
        errors = {};
        editorOpen = true;
    }


    function addAttribute() {
        attributes = [...attributes, { key: "", label: "", choices: [] }];
    }

    function removeAttribute(index) {
        attributes = attributes.filter((_, i) => i !== index);
    }

    /**
     * The fields as they are stored.
     *
     * The key is derived from the label once and then left alone: it is what a
     * product's saved answers are filed under, so renaming "Skin type" to
     * "Suitable for skin type" must not orphan every answer already given.
     * A row with no label is dropped — an empty row is one somebody started and
     * abandoned, the same rule the product editor's own metafields use.
     */
    function cleanedAttributes() {
        const out = [];
        const taken = new Set();
        for (const attr of attributes) {
            const label = attr.label.trim();
            if (!label) continue;
            let key = attr.key || keyFor(label);
            while (taken.has(key)) key += "-2";
            taken.add(key);
            out.push({
                key,
                label,
                choices: attr.choices.map((c) => c.trim()).filter(Boolean),
            });
        }
        return out;
    }

    function keyFor(label) {
        return (
            label
                .toLowerCase()
                .replace(/[^a-z0-9]+/g, "_")
                .replace(/^_|_$/g, "") || "field"
        );
    }

    function addChild(category, event) {
        event.stopPropagation();
        openCreate(category.id);
    }

    async function save(event) {
        event?.preventDefault();
        if (saving) return;

        errors = {};
        if (!form.title.trim()) errors.title = "A title is required.";
        if (Object.keys(errors).length) return;

        saving = true;
        try {
            if (editing) {
                // parent_id always travels on an update, including as null:
                // that null is "move to the top level", and omitting it is
                // "leave the parent alone". The two are different requests.
                await api.patch(`/api/admin/categories/${editing.id}`, {
                    title: form.title.trim(),
                    slug: form.slug.trim() || undefined,
                    parent_id: form.parent_id,
                    // metadata is replaced whole, so whatever else is on the
                    // category rides along — `taxonomy_gid` is written by the
                    // importer and would be lost by a bare {attributes}.
                    metadata: { ...(editing.metadata ?? {}), attributes: cleanedAttributes() },
                });
                toast.success("Category saved");
            } else {
                await api.post("/api/admin/categories", {
                    title: form.title.trim(),
                    slug: form.slug.trim() || undefined,
                    parent_id: form.parent_id,
                    metadata: { attributes: cleanedAttributes() },
                });
                toast.success("Category created");
            }
            editorOpen = false;
            await load();
        } catch (err) {
            toast.error(err);
        } finally {
            saving = false;
        }
    }

    function askDelete(category, event) {
        event.stopPropagation();
        pendingDelete = category;
        confirmOpen = true;
    }

    async function doDelete() {
        try {
            await api.delete(`/api/admin/categories/${pendingDelete.id}`);
            toast.success(`Deleted ${pendingDelete.title}`);
            await load();
        } catch (err) {
            // A 409 here is the engine refusing while subcategories or products
            // still point at it, and its message already names the count.
            toast.error(err);
        }
    }
</script>

<div class="page page-categories">
    <div class="page-content full-height">
        <header class="page-header">
            <nav class="breadcrumbs">
                <div class="breadcrumb-item">Categories</div>
            </nav>

            <div class="inline-flex gap-sm">
                <button
                    type="button"
                    class="btn circle transparent secondary"
                    title="Refresh"
                    aria-label="Refresh"
                    onclick={load}
                >
                    <i class="ri-refresh-line" aria-hidden="true"></i>
                </button>
            </div>

            <div class="page-header-primary-btns">
                <button type="button" class="btn" onclick={() => openCreate(null)}>
                    <i class="ri-add-line" aria-hidden="true"></i>
                    <span class="txt">New category</span>
                </button>
            </div>
        </header>

        <div class="page-table-wrapper">
            <table class="table responsive-table">
                <thead class="sticky">
                    <tr>
                        <th class="col-field-name-id">Category</th>
                        <th class="col-type-text">Slug</th>
                        <th class="col-meta min-width"></th>
                    </tr>
                </thead>
                <tbody>
                    {#each categories as category (category.id)}
                        <tr class="handle" onclick={() => openEdit(category)}>
                            <td class="col-field-name-id" data-name="Category">
                                <span
                                    class="category-indent"
                                    class:nested={category.depth > 0}
                                    style="--depth: {category.depth}"
                                ></span>
                                <!-- A leaf gets a spacer rather than a disabled
                                     chevron: an expander that opens onto
                                     nothing reads as broken, and `child_count`
                                     is on the row precisely so this can tell. -->
                                {#if category.child_count > 0}
                                    <button
                                        type="button"
                                        class="btn circle sm transparent secondary"
                                        class:loading={busyRow === category.id}
                                        aria-expanded={expanded.has(category.id)}
                                        aria-label="{expanded.has(category.id)
                                            ? 'Collapse'
                                            : 'Expand'} {category.title}"
                                        onclick={(e) => (e.stopPropagation(), toggle(category))}
                                    >
                                        <i
                                            class={expanded.has(category.id)
                                                ? "ri-arrow-down-s-line"
                                                : "ri-arrow-right-s-line"}
                                            aria-hidden="true"
                                        ></i>
                                    </button>
                                {:else}
                                    <span class="expand-spacer" aria-hidden="true"></span>
                                {/if}
                                <span class="txt-bold">{category.title}</span>
                                {#if category.child_count > 0}
                                    <span class="txt-hint txt-sm">
                                        {category.child_count}
                                    </span>
                                {/if}
                            </td>
                            <td class="col-type-text txt-hint txt-sm" data-name="Slug">
                                <span class="txt-ellipsis">{category.slug}</span>
                            </td>
                            <td class="col-meta min-width">
                                <button
                                    type="button"
                                    class="btn circle sm transparent secondary"
                                    aria-label="Add a subcategory under {category.title}"
                                    title="Add a subcategory"
                                    onclick={(e) => addChild(category, e)}
                                >
                                    <i class="ri-node-tree" aria-hidden="true"></i>
                                </button>
                                <button
                                    type="button"
                                    class="btn circle sm transparent secondary row-delete"
                                    aria-label="Delete {category.title}"
                                    title="Delete"
                                    onclick={(e) => askDelete(category, e)}
                                >
                                    <i class="ri-delete-bin-7-line" aria-hidden="true"></i>
                                </button>
                                <i class="ri-arrow-right-s-line" aria-hidden="true"></i>
                            </td>
                        </tr>
                    {/each}

                    {#if loading && !categories.length}
                        {#each Array(4) as _, i (i)}
                            <tr><td colspan="3"><span class="skeleton-loader"></span></td></tr>
                        {/each}
                    {:else if !categories.length}
                        <tr>
                            <td colspan="3" class="txt-hint txt-center p-base">
                                No categories yet. A category is where a product sits in your
                                taxonomy — one place, with a parent.
                            </td>
                        </tr>
                    {/if}
                </tbody>
            </table>
        </div>

        <footer class="page-footer">
            <span class="txt">
                {total}
                {total === 1 ? "category" : "categories"}
                {#if categories.length !== total}
                    <span class="txt-hint">· {categories.length} shown</span>
                {/if}
            </span>
            <ThemeToggle />
        </footer>
    </div>
</div>

<Drawer
    open={editorOpen}
    title={editing ? editing.title : "New category"}
    size="sm"
    onclose={() => (editorOpen = false)}
>
    <form id="category-form" onsubmit={save}>
        <div class="field required" class:error={!!errors.title}>
            <label for="cat_title">Title</label>
            <input id="cat_title" type="text" autocomplete="off" bind:value={form.title} />
        </div>
        {#if errors.title}<div class="field-help error">{errors.title}</div>{/if}

        <div class="field m-t-sm">
            <label for="cat_parent">Parent</label>
            <CategoryPicker
                id="cat_parent"
                bind:value={form.parent_id}
                categories={parentOptions}
                remote={parentRemote}
                selectedCategory={editing?.parent_id ? { id: editing.parent_id } : null}
                placeholder="Top level"
            />
        </div>
        <div class="field-help">
            A category can hold products and subcategories at once. Moving one takes its
            subcategories with it.
        </div>

        <div class="field m-t-sm">
            <label for="cat_slug">Slug</label>
            <input id="cat_slug" type="text" autocomplete="off" bind:value={form.slug} />
        </div>
        <div class="field-help">Derived from the title when left empty.</div>

        <!--
            The fields products in this category are asked for. Shopify gets
            these from its own taxonomy; a store that built its own tree has to
            say what it wants asked, so this is where that is said. Inherited
            downward — a field on Bath & Body is asked of every soap under it.
        -->
        <h6 class="section-title">
            <i class="ri-list-settings-line" aria-hidden="true"></i>
            Category metafields
        </h6>

        {#each attributes as attr, i (i)}
            <div class="attr-row" class:m-t-sm={i > 0}>
                <div class="field">
                    <label for="attr-label-{i}">Field {i + 1}</label>
                    <input
                        id="attr-label-{i}"
                        type="text"
                        autocomplete="off"
                        placeholder="Age group"
                        bind:value={attr.label}
                    />
                </div>
                <div class="field m-t-5">
                    <label for="attr-choices-{i}">Choices</label>
                    <TokenInput
                        id="attr-choices-{i}"
                        bind:values={attr.choices}
                        emptyText="Type a choice and press Enter. None means the field takes free text."
                    />
                </div>
                <button
                    type="button"
                    class="btn sm transparent danger attr-remove"
                    onclick={() => removeAttribute(i)}
                >
                    <span class="txt">Remove</span>
                </button>
            </div>
        {/each}

        <button type="button" class="btn sm transparent m-t-sm" onclick={addAttribute}>
            <i class="ri-add-circle-line" aria-hidden="true"></i>
            <span class="txt">Add a metafield</span>
        </button>
        <div class="field-help">
            Products in this category — and in every category under it — are asked for these on
            their own page. Renaming one keeps the answers already given.
        </div>
    </form>

    {#snippet footer()}
        <button type="button" class="btn transparent m-r-auto" onclick={() => (editorOpen = false)}>
            <span class="txt">Cancel</span>
        </button>
        <button
            type="submit"
            form="category-form"
            class="btn"
            class:loading={saving}
            disabled={saving}
        >
            <span class="txt">{editing ? "Save changes" : "Create category"}</span>
        </button>
    {/snippet}
</Drawer>

<Confirm
    bind:open={confirmOpen}
    title="Delete this category?"
    message={pendingDelete
        ? `${pendingDelete.title} will be removed. If any subcategories or products still point at it, the store will refuse and tell you how many.`
        : ""}
    confirmLabel="Delete"
    danger
    onconfirm={doDelete}
/>
