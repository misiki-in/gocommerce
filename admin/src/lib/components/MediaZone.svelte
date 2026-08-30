<script>
    /**
     * A product's media: a drop zone, a reorderable grid of thumbnails, and a
     * picker onto the store-wide library.
     *
     * Three things here are shaped by the engine rather than by taste.
     *
     * The list is written with PUT and the whole order every time, because that
     * is what the route takes — an add and a reorder are the same request, so a
     * drag cannot settle half applied.
     *
     * The upload does not go through api.js. `request()` JSON-encodes any body
     * that is not a string, and a multipart upload has to keep the boundary
     * FormData generated for it, so this one call is a bare fetch — built to
     * throw the same ApiError, out of the same envelope, as everything else.
     *
     * And the list is read back before it is written. `readable` guards a build
     * whose engine mounts PUT on this path but not GET; the shipped engine
     * mounts both, and the notice exists because a zone that silently showed an
     * empty grid would invite an operator to replace files they cannot see.
     */
    import { api, request, getToken, query, ApiError } from "$lib/api.js";
    import { toast } from "$lib/toast.svelte.js";
    import { pluralize } from "$lib/format.js";
    import Drawer from "$lib/components/Drawer.svelte";
    import Select from "$lib/components/Select.svelte";

    let { productId, media = $bindable([]), disabled = false } = $props();

    const LIBRARY_PER_PAGE = 24;

    let busy = $state(false);
    let readable = $state(true);
    let over = $state(false);
    let fileInput = $state(null);

    // Set once the engine answers 501: the store has nowhere to put files,
    // which is a configuration answer rather than a failure to retry.
    let uploadsBlocked = $state(false);
    let uploadsMessage = $state("");

    let dragIndex = $state(-1);
    let overIndex = $state(-1);

    let libraryOpen = $state(false);
    let libraryItems = $state([]);
    let libraryMeta = $state(null);
    let libraryLoading = $state(false);
    let libraryPage = $state(1);
    let libraryKind = $state("");
    let librarySearch = $state("");
    let libraryUsage = $state("");
    let librarySize = $state("");
    let libraryScope = $state("");
    let libraryView = $state("grid");
    let picked = $state([]);

    /**
     * Shopify's File size filter, as the byte bounds the engine takes.
     *
     * Named buckets rather than a pair of number boxes: nobody searching a
     * media library thinks in bytes, and "under 100 KB" is the actual question
     * — which of these is small enough to put on a page.
     */
    const SIZE_BUCKETS = {
        small: { min: 0, max: 100 * 1024, label: "Under 100 KB" },
        medium: { min: 100 * 1024, max: 1024 * 1024, label: "100 KB – 1 MB" },
        large: { min: 1024 * 1024, max: 5 * 1024 * 1024, label: "1 MB – 5 MB" },
        huge: { min: 5 * 1024 * 1024, max: 0, label: "Over 5 MB" },
    };

    // The library's own upload input, separate from the zone's: one <input
    // type="file"> cannot be open in two places, and the drawer sits above the
    // zone while both are mounted.
    let libraryFileInput = $state(null);
    let libraryBusy = $state(false);

    let linkUrl = $state("");
    let linking = $state(false);
    let urlPopover = $state(null);

    const attachedIds = $derived(new Set(media.map((m) => m.id)));
    const libraryHasMore = $derived(!!libraryMeta && libraryItems.length < libraryMeta.total);

    // Whether any filter is narrowing the list, which is what tells an empty
    // result "nothing matches" apart from "the library is empty".
    const libraryFiltered = $derived(
        !!(librarySearch.trim() || libraryKind || libraryUsage || librarySize || libraryScope),
    );

    function label(item) {
        return item.alt || item.filename || `media ${item.id}`;
    }

    function kindIcon(kind) {
        if (kind === "video") return "ri-movie-line";
        if (kind === "model") return "ri-box-3-line";
        return "ri-image-line";
    }

    /**
     * The line under a thumbnail: "PNG", "MP4". The extension rather than the
     * MIME type, because that is what the operator recognises and what Shopify
     * shows; the kind is the fallback for a URL that carries no extension.
     */
    function fileType(item) {
        const name = item.filename || item.url || "";
        const path = name.split(/[?#]/)[0];
        const dot = path.lastIndexOf(".");
        if (dot > 0 && dot > path.lastIndexOf("/")) {
            return path.slice(dot + 1).toUpperCase();
        }
        return item.kind === "model" ? "3D model" : item.kind.toUpperCase();
    }

    /** Bytes as a person reads them. Zero means unknown — linked media. */
    function fileSize(bytes) {
        if (!bytes) return "";
        if (bytes < 1024) return `${bytes} B`;
        if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
        return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
    }

    $effect(() => {
        const id = productId;
        if (!id) return;

        let cancelled = false;
        (async () => {
            try {
                const result = await api.get(`/api/admin/products/${id}/media`);
                if (cancelled) return;
                media = result?.data ?? result ?? [];
                readable = true;
            } catch (err) {
                if (cancelled) return;
                // 405 is what the router answers for a path it serves under a
                // different method, which is exactly this case; 404 covers a
                // build where the path is not mounted at all.
                if (err.status === 405 || err.status === 404) {
                    readable = false;
                    media = [];
                    return;
                }
                toast.error(err);
            }
        })();
        return () => {
            cancelled = true;
        };
    });

    /**
     * persist writes the whole ordered list and takes the server's answer back
     * as the new truth. The optimistic assignment is undone on failure: a grid
     * left showing an order the engine refused is worse than a jump.
     */
    async function persist(next) {
        const previous = media;
        media = next;
        busy = true;
        try {
            const result = await request("PUT", `/api/admin/products/${productId}/media`, {
                body: { media_ids: next.map((m) => m.id) },
            });
            media = result?.data ?? result ?? [];
        } catch (err) {
            media = previous;
            toast.error(err);
        } finally {
            busy = false;
        }
    }

    async function uploadOne(file) {
        const body = new FormData();
        body.append("file", file);

        const token = getToken();
        const headers = token ? { Authorization: "Bearer " + token } : {};

        let response;
        try {
            response = await fetch("/api/admin/media", { method: "POST", headers, body });
        } catch {
            throw new ApiError(0, "network_error", "Could not reach the store. Is it still running?");
        }

        const payload = await response.json().catch(() => null);
        if (!response.ok) {
            const err = payload?.error;
            throw new ApiError(
                response.status,
                err?.code || "error",
                err?.message || response.statusText,
                err?.details,
            );
        }
        return payload?.data ?? payload;
    }

    async function addFiles(files) {
        const list = [...(files ?? [])];
        if (!list.length || disabled) return;

        busy = true;
        const added = [];
        try {
            for (const file of list) {
                try {
                    added.push(await uploadOne(file));
                } catch (err) {
                    if (err.status === 501) {
                        // The envelope carries the reason and the fix; nothing
                        // this layer could invent would be more useful, and the
                        // rest of the batch would fail identically.
                        uploadsBlocked = true;
                        uploadsMessage = err.message;
                        break;
                    }
                    toast.error(`${file.name}: ${err.message}`);
                }
            }
        } finally {
            busy = false;
        }

        if (added.length) {
            await persist([...media, ...added]);
            toast.success(`Added ${added.length} ${pluralize(added.length, "file")}`);
        }
    }

    function onPick(event) {
        addFiles(event.currentTarget.files);
        // Clearing it means picking the same file twice in a row still fires.
        event.currentTarget.value = "";
    }

    function onDragOverZone(event) {
        if (disabled) return;
        event.preventDefault();
        over = true;
    }

    function onDropZone(event) {
        if (disabled) return;
        event.preventDefault();
        over = false;
        // A tile being dragged within the grid is a reorder, never an upload —
        // belt to dropTile's braces, since a drop that misses every tile still
        // lands here with the dragged image on the dataTransfer.
        if (dragIndex >= 0) {
            dragIndex = -1;
            overIndex = -1;
            return;
        }
        addFiles(event.dataTransfer?.files);
    }

    function remove(index) {
        persist(media.filter((_, i) => i !== index));
    }

    function move(from, to) {
        if (from === to || to < 0 || to >= media.length) return;
        const next = [...media];
        const [item] = next.splice(from, 1);
        next.splice(to, 0, item);
        persist(next);
    }

    function startDrag(event, index) {
        if (disabled) return;
        dragIndex = index;
        // Firefox will not begin a drag unless the payload is set.
        event.dataTransfer?.setData("text/plain", String(index));
        if (event.dataTransfer) event.dataTransfer.effectAllowed = "move";
    }

    function dragOverTile(event, index) {
        if (dragIndex < 0) return;
        event.preventDefault();
        // See dropTile: the zone behind this tile is a file drop target, and it
        // must not treat a reorder as an upload in progress.
        event.stopPropagation();
        overIndex = index;
    }

    /**
     * Reordering, and the reason it stops propagating.
     *
     * The zone behind these tiles accepts dropped files, and this event bubbles
     * straight into it. Chrome puts a dragged <img> on the dataTransfer as a
     * file, so dropping a tile back on the grid read as "the operator dropped a
     * picture here" — the reorder ran, and then the same image uploaded again as
     * a second library item. One drag, two copies.
     */
    function dropTile(event, index) {
        if (dragIndex < 0) return;
        event.preventDefault();
        event.stopPropagation();
        over = false;
        const from = dragIndex;
        dragIndex = -1;
        overIndex = -1;
        move(from, index);
    }

    function endDrag() {
        dragIndex = -1;
        overIndex = -1;
    }

    /**
     * The arrow keys on the handle are not a nicety: dragging is the only way
     * to reorder, and a pointer gesture has no keyboard equivalent unless one
     * is written.
     */
    function onHandleKey(event, index) {
        if (event.key === "ArrowLeft" || event.key === "ArrowUp") {
            event.preventDefault();
            move(index, index - 1);
        } else if (event.key === "ArrowRight" || event.key === "ArrowDown") {
            event.preventDefault();
            move(index, index + 1);
        }
    }

    function openLibrary() {
        picked = [];
        libraryPage = 1;
        libraryItems = [];
        librarySearch = "";
        libraryUsage = "";
        librarySize = "";
        libraryScope = "";
        linkUrl = "";
        libraryOpen = true;
        loadLibrary();
    }

    /**
     * Uploading from inside the picker. It lands in the library and is
     * pre-selected rather than attached straight to the product, because the
     * drawer's contract is "choose things, then press Done" — attaching behind
     * Cancel would write something the operator then could not take back.
     */
    async function addToLibrary(files) {
        const list = [...(files ?? [])];
        if (!list.length) return;

        libraryBusy = true;
        const added = [];
        try {
            for (const file of list) {
                try {
                    added.push(await uploadOne(file));
                } catch (err) {
                    if (err.status === 501) {
                        uploadsBlocked = true;
                        uploadsMessage = err.message;
                        break;
                    }
                    toast.error(`${file.name}: ${err.message}`);
                }
            }
        } finally {
            libraryBusy = false;
        }
        if (added.length) {
            libraryItems = [...added, ...libraryItems];
            picked = [...picked, ...added];
        }
    }

    function onLibraryPick(event) {
        addToLibrary(event.currentTarget.files);
        event.currentTarget.value = "";
    }

    function onLibraryDrop(event) {
        event.preventDefault();
        over = false;
        addToLibrary(event.dataTransfer?.files);
    }

    async function loadLibrary() {
        libraryLoading = true;
        const bucket = SIZE_BUCKETS[librarySize];
        try {
            const result = await api.get(
                "/api/admin/media" +
                    query({
                        q: librarySearch.trim(),
                        kind: libraryKind,
                        usage: libraryUsage,
                        // "On this product" is the same filter the engine takes
                        // by id; the picker just knows which product it is.
                        product_id: libraryScope === "product" ? productId : "",
                        min_bytes: bucket?.min || "",
                        max_bytes: bucket?.max || "",
                        page: libraryPage,
                        limit: LIBRARY_PER_PAGE,
                    }),
            );
            libraryItems =
                libraryPage === 1 ? result.data : [...libraryItems, ...result.data];
            libraryMeta = result.meta;
        } catch (err) {
            toast.error(err);
        } finally {
            libraryLoading = false;
        }
    }

    function reloadLibrary() {
        libraryPage = 1;
        loadLibrary();
    }

    /**
     * The search box asks the server, so it is debounced: a filter that fires a
     * request per keystroke sends five for "shirt" and the answers can land out
     * of order, leaving the list showing the results for "shi".
     */
    let searchTimer = null;
    function onSearchInput() {
        clearTimeout(searchTimer);
        searchTimer = setTimeout(reloadLibrary, 250);
    }

    function clearSearch() {
        librarySearch = "";
        reloadLibrary();
    }

    function togglePick(item) {
        picked = picked.some((p) => p.id === item.id)
            ? picked.filter((p) => p.id !== item.id)
            : [...picked, item];
    }

    async function addPicked() {
        const fresh = picked.filter((p) => !attachedIds.has(p.id));
        libraryOpen = false;
        if (fresh.length) await persist([...media, ...fresh]);
    }

    /**
     * "Add media from URL". No kind picker, which is Shopify's shape and is
     * only honest because the engine reads the kind off the URL's extension
     * — see kindForURL in media.go. Sending an explicit kind here would just be
     * this side guessing instead.
     */
    async function linkByUrl(event) {
        event?.preventDefault();
        if (linking || !linkUrl.trim()) return;
        linking = true;
        try {
            const item = await api.post("/api/admin/media/link", { url: linkUrl.trim() });
            linkUrl = "";
            if (urlPopover?.matches(":popover-open")) urlPopover.hidePopover();
            libraryItems = [item, ...libraryItems];
            picked = [...picked, item];
        } catch (err) {
            toast.error(err);
        } finally {
            linking = false;
        }
    }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
    class="media-zone"
    ondragover={onDragOverZone}
    ondragleave={() => (over = false)}
    ondrop={onDropZone}
>
    {#if media.length}
        <div class="media-grid m-b-sm" role="list">
            {#each media as item, index (item.id)}
                <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
                <div
                    class="media-tile"
                    role="listitem"
                    draggable={!disabled}
                    data-dragging={dragIndex === index}
                    data-dropinto={overIndex === index && dragIndex !== index}
                    ondragstart={(e) => startDrag(e, index)}
                    ondragover={(e) => dragOverTile(e, index)}
                    ondrop={(e) => dropTile(e, index)}
                    ondragend={endDrag}
                >
                    {#if item.kind === "image"}
                        <img src={item.url} alt={label(item)} />
                    {:else if item.kind === "video"}
                        <!-- svelte-ignore a11y_media_has_caption -->
                        <video src={item.url} preload="metadata" muted playsinline></video>
                    {:else}
                        <!--
                            A .glb cannot be drawn without a viewer library, and
                            this panel ships no third-party JavaScript. An icon
                            that admits what the file is beats a broken <img>.
                        -->
                        <i class="ri-box-3-line" style="font-size: 30px" aria-hidden="true"></i>
                        <span class="media-tile-name txt-sm">{item.filename || "3D model"}</span>
                    {/if}

                    <div class="media-tile-actions">
                        <button
                            type="button"
                            class="btn circle sm transparent secondary"
                            {disabled}
                            title="Drag to reorder, or use the arrow keys"
                            aria-label="Reorder {label(item)} — use the arrow keys to move it"
                            onkeydown={(e) => onHandleKey(e, index)}
                        >
                            <i class="ri-draggable" aria-hidden="true"></i>
                        </button>
                        <button
                            type="button"
                            class="btn circle sm transparent secondary"
                            {disabled}
                            title="Detach from this product. The file stays in the library."
                            aria-label="Detach {label(item)} from this product"
                            onclick={() => remove(index)}
                        >
                            <i class="ri-close-line" aria-hidden="true"></i>
                        </button>
                    </div>

                    {#if index === 0}
                        <span class="label sm media-tile-badge">First</span>
                    {/if}
                </div>
            {/each}
        </div>
    {/if}

    <div class="media-dropzone" data-over={over}>
        <i class="ri-image-add-line" style="font-size: 26px" aria-hidden="true"></i>
        <div class="txt-sm">Accepts images, videos, or 3D models</div>

        <input
            bind:this={fileInput}
            type="file"
            multiple
            class="hidden"
            aria-label="Files to upload"
            onchange={onPick}
        />

        <div class="inline-flex gap-sm flex-wrap">
            <button
                type="button"
                class="btn sm"
                class:loading={busy}
                disabled={disabled || busy || uploadsBlocked}
                onclick={() => fileInput?.click()}
            >
                <i class="ri-upload-cloud-2-line" aria-hidden="true"></i>
                <span class="txt">Upload new</span>
            </button>
            <button
                type="button"
                class="btn sm secondary"
                {disabled}
                onclick={openLibrary}
            >
                <i class="ri-folder-image-line" aria-hidden="true"></i>
                <span class="txt">Select existing</span>
            </button>
        </div>
    </div>

    {#if uploadsBlocked}
        <div class="field-help error">
            {uploadsMessage} Until then, "Select existing" can still record a file hosted
            somewhere else by its URL.
        </div>
    {/if}

    {#if !readable}
        <div class="field-help error">
            This engine has no route that reads a product's media back — only one that replaces
            it — so this grid shows what was attached here, not what the product already has.
            Adding or removing anything writes the whole list, which will drop files that were
            attached elsewhere.
        </div>
    {/if}

    <div class="field-help">
        The first file leads. Removing one keeps it in your library.
    </div>
</div>

<Drawer open={libraryOpen} title="Select file" onclose={() => (libraryOpen = false)}>
    <!-- Search left, layout right, as Shopify has it. Together on one row so
         the toggle does not strand on a line of its own once four filter chips
         fill the row below. -->
    <div class="media-toolbar m-b-sm">
    <div class="fields searchbar">
        <div class="field">
            <input
                type="text"
                class="p-l-30"
                placeholder="Search files"
                aria-label="Search files"
                bind:value={librarySearch}
                oninput={onSearchInput}
            />
            <!-- After the input, not before it: `.field` is display:block and
                 the input is width:100%, so a leading sibling would stack above
                 it. The icon is positioned out of flow — see gocommerce.css. -->
            <i class="ri-search-line searchbar-icon" aria-hidden="true"></i>
        </div>
        {#if librarySearch}
            <div class="field addon p-r-5">
                <button
                    type="button"
                    title="Clear"
                    class="btn sm secondary transparent circle"
                    onclick={clearSearch}
                >
                    <i class="ri-close-line" aria-hidden="true"></i>
                </button>
            </div>
        {/if}
    </div>

        <div class="split-btn" role="group" aria-label="Layout">
            <button
                type="button"
                class="btn sm secondary"
                class:active={libraryView === "grid"}
                aria-pressed={libraryView === "grid"}
                title="Grid"
                aria-label="Grid"
                onclick={() => (libraryView = "grid")}
            >
                <i class="ri-grid-fill" aria-hidden="true"></i>
            </button>
            <button
                type="button"
                class="btn sm secondary split-btn-toggle"
                class:active={libraryView === "list"}
                aria-pressed={libraryView === "list"}
                title="List"
                aria-label="List"
                onclick={() => (libraryView = "list")}
            >
                <i class="ri-list-unordered" aria-hidden="true"></i>
            </button>
        </div>
    </div>

    <div class="media-filters m-b-sm">
        <Select
            class="sm"
            bind:value={libraryKind}
            onchange={reloadLibrary}
            placeholder="File type"
            options={[
                { value: "", label: "File type: any" },
                { value: "image", label: "Images" },
                { value: "video", label: "Video" },
                { value: "model", label: "3D models" },
            ]}
        />
        <Select
            class="sm"
            bind:value={librarySize}
            onchange={reloadLibrary}
            options={[
                { value: "", label: "File size: any" },
                ...Object.entries(SIZE_BUCKETS).map(([value, b]) => ({
                    value,
                    label: b.label,
                })),
            ]}
        />
        <Select
            class="sm"
            bind:value={libraryUsage}
            onchange={reloadLibrary}
            options={[
                { value: "", label: "Used in: anything" },
                { value: "used", label: "Used on a product" },
                { value: "unused", label: "Not used anywhere" },
            ]}
        />
        <Select
            class="sm"
            bind:value={libraryScope}
            onchange={reloadLibrary}
            options={[
                { value: "", label: "Product: any" },
                { value: "product", label: "On this product" },
            ]}
        />
    </div>

    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div
        class="media-dropzone m-b-base"
        data-over={over}
        ondragover={(e) => (e.preventDefault(), (over = true))}
        ondragleave={() => (over = false)}
        ondrop={onLibraryDrop}
    >
        <input
            bind:this={libraryFileInput}
            type="file"
            multiple
            class="hidden"
            aria-label="Files to add to the library"
            onchange={onLibraryPick}
        />

        <!-- Shopify's split button: the label uploads, the caret opens the URL
             form. Two buttons rather than one with a menu, because the common
             action should not cost a menu to reach. -->
        <div class="split-btn">
            <button
                type="button"
                class="btn sm secondary"
                class:loading={libraryBusy}
                disabled={libraryBusy || uploadsBlocked}
                onclick={() => libraryFileInput?.click()}
            >
                <span class="txt">Add media</span>
            </button>
            <button
                type="button"
                class="btn sm secondary split-btn-toggle"
                popovertarget="add-media-url"
                aria-label="Add media from URL"
                title="Add media from URL"
            >
                <i class="ri-arrow-down-s-line" aria-hidden="true"></i>
            </button>

            <div
                bind:this={urlPopover}
                id="add-media-url"
                class="dropdown add-media-url"
                popover="auto"
            >
                <div class="add-media-url-title txt-bold">Add media from URL</div>
                <form onsubmit={linkByUrl}>
                    <div class="field">
                        <label for="link-url">Image, video or 3D model URL</label>
                        <input
                            id="link-url"
                            type="text"
                            placeholder="https://"
                            bind:value={linkUrl}
                        />
                    </div>
                    <button
                        type="submit"
                        class="btn sm m-t-sm"
                        class:loading={linking}
                        disabled={linking || !linkUrl.trim()}
                    >
                        <span class="txt">Add file</span>
                    </button>
                </form>
            </div>
        </div>

        <div class="txt-sm txt-hint">
            Drag and drop images, videos, 3D models, and files
        </div>
    </div>

    <div class="media-grid" class:media-list={libraryView === "list"} role="list">
        {#each libraryItems as item (item.id)}
            {@const chosen = picked.some((p) => p.id === item.id)}
            {@const already = attachedIds.has(item.id)}
            <div class="media-cell" role="listitem">
                <div class="media-tile" data-dropinto={chosen}>
                    <button
                        type="button"
                        class="btn transparent p-0 block"
                        style="height: 100%"
                        aria-pressed={chosen}
                        aria-label="{chosen ? 'Deselect' : 'Select'} {label(item)}"
                        title={already ? "Already on this product" : label(item)}
                        onclick={() => togglePick(item)}
                    >
                        {#if item.kind === "image"}
                            <img src={item.url} alt={label(item)} />
                        {:else}
                            <i
                                class={kindIcon(item.kind)}
                                style="font-size: 30px"
                                aria-hidden="true"
                            ></i>
                        {/if}

                        <!-- Decorative: the button already carries the state as
                             aria-pressed, and a real checkbox inside a button is
                             invalid markup that screen readers announce twice. -->
                        <span class="media-check" data-checked={chosen} aria-hidden="true">
                            {#if chosen}<i class="ri-check-line"></i>{/if}
                        </span>
                    </button>

                    {#if already}
                        <span class="label sm media-tile-badge">On product</span>
                    {/if}
                </div>

                <!-- The filename, like Shopify — not `label()`, which prefers
                     the alt text. Alt describes the picture; this line
                     identifies the file, and two files can share a description. -->
                <div class="media-cell-name txt-sm txt-ellipsis" title={label(item)}>
                    {item.filename || label(item)}
                </div>
                <div class="media-cell-meta txt-sm txt-hint">
                    {fileType(item)}{#if libraryView === "list" && item.size_bytes}
                        · {fileSize(item.size_bytes)}{/if}
                </div>
            </div>
        {/each}
    </div>

    {#if libraryLoading && !libraryItems.length}
        <div class="block txt-center p-base"><span class="loader lg"></span></div>
    {/if}

    {#if !libraryLoading && !libraryItems.length}
        <div class="txt-center txt-hint p-base">
            {#if libraryFiltered}
                No files match these filters.
            {:else}
                The library is empty. Add a file above, or paste the URL of one hosted
                elsewhere.
            {/if}
        </div>
    {/if}

    {#if libraryHasMore}
        <button
            type="button"
            class="btn expanded block m-t-sm"
            class:loading={libraryLoading}
            disabled={libraryLoading}
            onclick={() => (libraryPage += 1, loadLibrary())}
        >
            <i class="ri-arrow-down-s-line" aria-hidden="true"></i>
            <span class="txt">Load more</span>
        </button>
    {/if}

    <div class="field-help">
        Search covers your whole library. A URL only records a link — the file is never
        copied, and off-origin ones will not preview here.
    </div>

    {#snippet footer()}
        <button
            type="button"
            class="btn transparent m-r-auto"
            onclick={() => (libraryOpen = false)}
        >
            <span class="txt">Cancel</span>
        </button>
        <button type="button" class="btn" disabled={!picked.length} onclick={addPicked}>
            <span class="txt">
                Done{picked.length ? ` (${picked.length})` : ""}
            </span>
        </button>
    {/snippet}
</Drawer>
