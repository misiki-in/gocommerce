<script>
    import { api, query } from "$lib/api.js";
    import { toast } from "$lib/toast.svelte.js";

    /**
     * The product category picker: one node out of a tree.
     *
     * It is a `Select` in shape — same `.input.select.single` trigger, same
     * `.dropdown[popover]`, same keyboard contract — with two differences the
     * tree forces:
     *
     *   1. The list is indented, and the indentation is the only thing telling
     *      "Shirts" under Clothing apart from "Shirts" under Kids. Filtering
     *      throws that away, so a search match is shown by its full path
     *      instead — the tree collapses to a flat list of breadcrumbs while a
     *      search is running and returns to indented rows when it is cleared.
     *
     *   2. A branch is a real choice. Shopify lets you file a product at
     *      "Apparel" without picking a leaf, so every row is selectable and
     *      nothing here has a disabled parent.
     *
     * The `None` row is not decoration either: uncategorised is a normal state
     * for a draft, and the API expects an explicit null to get back to it.
     */
    let {
        value = $bindable(null),
        // Flat, depth-first, each carrying `depth` and `full_name` — the shape
        // `GET /api/admin/categories?flat=1` already returns, so nothing here
        // rebuilds a tree the server has already walked.
        //
        // It is the whole tree only while the tree is small. Importing
        // Shopify's taxonomy puts fourteen thousand rows in the table, and the
        // page stops fetching all of them; see `remote` below.
        categories = [],
        // True once the store holds more categories than are worth shipping to
        // the browser. The page sets it from the listing's own total.
        remote = false,
        // The node `value` names, for when the tree in `categories` does not
        // hold it. In remote mode it almost never does — the product knows its
        // own category, and without this the trigger would read "Uncategorised"
        // for a product that plainly has one.
        selectedCategory = null,
        id = undefined,
        placeholder = "Uncategorised",
        disabled = false,
        onchange,
    } = $props();

    const dropdownId = "category-picker-" + Math.random().toString(36).slice(2, 10);

    let dropdown = $state(null);
    let root = $state(null);
    let search = $state("");
    let open = $state(false);

    /*
     * The node just picked, kept because it may be in none of the other two
     * places. In remote mode `categories` holds a couple of hundred rows out of
     * fourteen thousand and `selectedCategory` is whatever the product was
     * loaded with — so choosing a searched category left the trigger reading
     * "Uncategorised" until the page was saved and reloaded.
     */
    let chosen = $state(null);

    const selected = $derived(
        (chosen && chosen.id === value ? chosen : null) ??
            categories.find((c) => c.id === value) ??
            (selectedCategory && selectedCategory.id === value ? selectedCategory : null),
    );

    const term = $derived(search.trim().toLowerCase());
    const searching = $derived(term.length > 0);

    /*
     * Two sources, one list.
     *
     * A hand-built tree of a few dozen arrives whole and is filtered here — no
     * request, instant. An imported taxonomy is far too large to ship, so the
     * same box asks the store instead. The rows come back in the same shape
     * either way, which is why everything below this line is unchanged.
     */
    let matches = $state([]);
    let loading = $state(false);
    let timer = null;

    /**
     * The tree, one level at a time.
     *
     * Browsing is the point: a category is a place, and "Bird Cage Accessories"
     * only means something once you can see it sits under Bird Supplies, under
     * Pet Supplies. A flat list of the first two hundred rows shows neither the
     * shape nor most of the tree.
     *
     * `nodes` holds the rows in draw order — roots, with each opened branch's
     * children spliced in beneath it — which is the model the Categories screen
     * uses too. Search replaces it with breadcrumbs, because once the tree is
     * filtered the indentation is no longer telling the truth.
     */
    let nodes = $state([]);
    let openIds = $state(new Set());
    let busyId = $state(null);

    const visible = $derived.by(() => {
        if (!searching) return nodes;
        if (remote) return matches;
        // Locally the whole tree is in hand, so a search is a filter. It matches
        // on the path, so "apparel shirt" — or just "apparel" — finds the leaves
        // under it, which is what someone who knows roughly where a thing lives
        // will type.
        return categories.filter((c) => (c.full_name || c.title).toLowerCase().includes(term));
    });

    /** One level of children, from the store or from the tree already in hand. */
    async function levelOf(parentID) {
        if (!remote) {
            return categories.filter((c) => (c.parent_id ?? null) === parentID);
        }
        const result = await api.get(
            "/api/admin/categories" + query({ parent: parentID === null ? "root" : parentID }),
        );
        return result.data ?? [];
    }

    async function loadRoots() {
        loading = true;
        try {
            nodes = await levelOf(null);
            openIds = new Set();
        } catch (err) {
            toast.error(err);
        } finally {
            loading = false;
        }
    }

    /**
     * Expanding splices a branch's children in under it; collapsing takes the
     * whole subtree with it, however deep it was opened.
     */
    async function toggleNode(node, event) {
        event.stopPropagation();
        const index = nodes.findIndex((n) => n.id === node.id);
        if (index < 0) return;

        if (openIds.has(node.id)) {
            let end = index + 1;
            while (end < nodes.length && nodes[end].depth > node.depth) end++;
            const removed = nodes.slice(index + 1, end);
            nodes = [...nodes.slice(0, index + 1), ...nodes.slice(end)];
            const next = new Set(openIds);
            next.delete(node.id);
            for (const r of removed) next.delete(r.id);
            openIds = next;
            return;
        }

        busyId = node.id;
        try {
            const children = await levelOf(node.id);
            nodes = [...nodes.slice(0, index + 1), ...children, ...nodes.slice(index + 1)];
            openIds = new Set(openIds).add(node.id);
        } catch (err) {
            toast.error(err);
        } finally {
            busyId = null;
        }
    }

    /**
     * Whether a row has anything to open. The store sends `child_count`; a local
     * tree has to be asked, which costs nothing because it is already here.
     */
    function hasChildren(node) {
        if (typeof node.child_count === "number") return node.child_count > 0;
        return categories.some((c) => (c.parent_id ?? null) === node.id);
    }

    /**
     * Debounced, because this one goes over the wire. A request per keystroke
     * sends five for "shirt" and their answers can land out of order, leaving
     * the list showing the results for "shi".
     */
    function onSearchInput() {
        if (!remote) return;
        clearTimeout(timer);
        const wanted = search;
        if (!wanted.trim()) {
            matches = [];
            loading = false;
            return;
        }
        loading = true;
        timer = setTimeout(async () => {
            try {
                const result = await api.get("/api/admin/categories" + query({ q: wanted, limit: 50 }));
                // The box may have moved on while this was in flight; a stale
                // answer overwriting a newer one is the bug the debounce is
                // only half a defence against.
                if (wanted !== search) return;
                matches = result.data ?? [];
            } catch (err) {
                toast.error(err);
            } finally {
                if (wanted === search) loading = false;
            }
        }, 250);
    }

    function close() {
        if (dropdown?.matches(":popover-open")) dropdown.hidePopover();
    }

    function pick(next, category = null) {
        value = next;
        chosen = category;
        onchange?.(next);
        close();
    }

    function onToggle(event) {
        open = event.newState === "open";
        if (open) {
            loadRoots();
            return;
        }
        search = "";
        matches = [];
    }

    function onTriggerKeydown(event) {
        if (event.key === "ArrowDown" || event.key === "ArrowUp" || event.key === "Enter") {
            if (!open) {
                event.preventDefault();
                dropdown?.showPopover();
                queueMicrotask(() => focusOption(event.key === "ArrowUp" ? -1 : 0));
            }
        }
    }

    function optionButtons() {
        return [...(dropdown?.querySelectorAll(".select-option") ?? [])];
    }

    function focusOption(index) {
        const items = optionButtons();
        if (!items.length) return;
        items[(index + items.length) % items.length].focus();
    }

    function onDropdownKeydown(event) {
        const items = optionButtons();
        const here = items.indexOf(document.activeElement);
        if (event.key === "ArrowDown") {
            event.preventDefault();
            focusOption(here + 1);
        } else if (event.key === "ArrowUp") {
            event.preventDefault();
            focusOption(here - 1);
        } else if (event.key === "Home") {
            event.preventDefault();
            focusOption(0);
        } else if (event.key === "End") {
            event.preventDefault();
            focusOption(items.length - 1);
        }
    }

    function onFocusOut(event) {
        if (!event.relatedTarget || !root?.contains(event.relatedTarget)) close();
    }
</script>

<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<div bind:this={root} class="input select single category-picker" class:disabled onfocusout={onFocusOut}>
    <button
        type="button"
        {id}
        {disabled}
        class="selected-container"
        popovertarget={dropdownId}
        aria-haspopup="listbox"
        aria-expanded={open}
        onkeydown={onTriggerKeydown}
    >
        {#if selected}
            <!-- The full path, not the leaf: "Shirts" alone does not say which
                 Shirts, and the trigger is the only place it is visible once
                 the list is closed. -->
            <div class="selected-item">{selected.full_name || selected.title}</div>
        {:else}
            <span class="placeholder">{placeholder}</span>
        {/if}
    </button>

    <div
        bind:this={dropdown}
        id={dropdownId}
        class="dropdown"
        popover="auto"
        tabindex="-1"
        role="listbox"
        ontoggle={onToggle}
        onkeydown={onDropdownKeydown}
    >
        <div class="fields dropdown-search">
            <div class="field">
                <input
                    type="text"
                    placeholder="Search categories…"
                    bind:value={search}
                    oninput={onSearchInput}
                />
            </div>
            {#if search}
                <div class="field addon p-r-5">
                    <button
                        type="button"
                        title="Clear"
                        class="btn sm secondary transparent circle"
                        onclick={() => ((search = ""), (matches = []))}
                    >
                        <i class="ri-close-line" aria-hidden="true"></i>
                    </button>
                </div>
            {/if}
        </div>

        {#if !searching}
            <button
                type="button"
                role="option"
                aria-selected={value === null}
                class="dropdown-item select-option"
                class:active={value === null}
                onclick={() => pick(null)}
            >
                <span class="txt-hint">{placeholder}</span>
            </button>
        {/if}

        {#each visible as category (category.id)}
            <div class="category-row">
                {#if !searching}
                    <!-- The indent is a real element rather than padding on the
                         row: `.dropdown-item` sets its own padding, and
                         overriding it per depth would need a rule per level. -->
                    <span
                        class="category-indent"
                        class:nested={category.depth > 0}
                        style="--depth: {category.depth || 0}"
                    ></span>
                    {#if hasChildren(category)}
                        <button
                            type="button"
                            class="btn circle sm transparent secondary category-expand"
                            class:loading={busyId === category.id}
                            aria-expanded={openIds.has(category.id)}
                            aria-label="{openIds.has(category.id)
                                ? 'Collapse'
                                : 'Expand'} {category.title}"
                            onclick={(e) => toggleNode(category, e)}
                        >
                            <i
                                class={openIds.has(category.id)
                                    ? "ri-arrow-down-s-line"
                                    : "ri-arrow-right-s-line"}
                                aria-hidden="true"
                            ></i>
                        </button>
                    {:else}
                        <span class="expand-spacer" aria-hidden="true"></span>
                    {/if}
                {/if}

                <!-- A branch is a real choice: Shopify lets you file a product at
                     "Apparel" without picking a leaf, so every row selects. -->
                <button
                    type="button"
                    role="option"
                    aria-selected={category.id === value}
                    class="dropdown-item select-option category-choose"
                    class:active={category.id === value}
                    onclick={() => pick(category.id, category)}
                >
                    <span class="txt-ellipsis">
                        {searching ? category.full_name || category.title : category.title}
                    </span>
                </button>
            </div>
        {/each}

        {#if loading && !visible.length}
            <div class="txt-hint txt-center m-0 p-5">Searching…</div>
        {:else if !visible.length}
            <div class="txt-hint txt-center m-0 p-5">
                {#if searching}
                    No categories found
                {:else if categories.length || nodes.length}
                    No categories found
                {:else}
                    No categories yet
                {/if}
            </div>
        {/if}
    </div>
</div>
