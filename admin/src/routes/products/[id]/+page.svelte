<script>
    /**
     * The product editor: Shopify's layout, this panel's vocabulary.
     *
     * The page is split in two along one line — what is saved by the save bar,
     * and what is not.
     *
     * Title, description, organisation, SEO and the default variant's own
     * fields are a form: they are edited freely, compared against the snapshot
     * that came off the server, and written when Save is pressed. Media,
     * options and the variant matrix are not, because each of them has its own
     * route in the engine and each of those routes is a transaction. Attaching
     * a file, replacing the option matrix and moving stock are all things that
     * have already happened by the time you look at them, and pretending they
     * are pending until Save would be a lie about the state of the store.
     *
     * The sections that belong to the default variant only exist while the
     * product has no options. With options, price and stock live on each
     * variant, and a page-level price field would be a control that writes to
     * nothing — so those sections say where the fields went instead.
     */
    import { page } from "$app/state";
    import { base } from "$app/paths";
    import { goto } from "$app/navigation";
    import { api, query, request } from "$lib/api.js";
    import { toMinor, fromMinor, stockClass, pluralize } from "$lib/format.js";
    import { toast } from "$lib/toast.svelte.js";
    import RichText from "$lib/components/RichText.svelte";
    import SaveBar from "$lib/components/SaveBar.svelte";
    import Select from "$lib/components/Select.svelte";
    import { COUNTRIES } from "$lib/countries.js";
    import Combobox from "$lib/components/Combobox.svelte";
    import TokenInput from "$lib/components/TokenInput.svelte";
    import CategoryPicker from "$lib/components/CategoryPicker.svelte";
    import Confirm from "$lib/components/Confirm.svelte";
    import MediaZone from "$lib/components/MediaZone.svelte";
    import VariantMatrix from "$lib/components/VariantMatrix.svelte";
    import ThemeToggle from "$lib/components/ThemeToggle.svelte";

    const SEO_TITLE_LIMIT = 60;
    const SEO_DESCRIPTION_LIMIT = 155;

    const productId = $derived(page.params.id);

    let loading = $state(true);
    let saving = $state(false);
    let product = $state(null);
    let collections = $state([]);
    // The whole tree, flattened depth-first. Unlike the vocabularies below it
    // is a real table with a route of its own, so it is asked for rather than
    // derived from a page of the catalog.
    let categories = $state([]);
    // The store holds more categories than the listing will send whole, so the
    // picker searches instead of filtering what it was handed. See
    // MaxWholeCategoryTree in categories.go.
    let categoriesTruncated = $state(false);
    let media = $state([]);
    let snapshot = $state(null);
    let form = $state(blank());
    let errors = $state({});
    let deleteOpen = $state(false);
    let duplicating = $state(false);
    let archiving = $state(false);
    // The search listing shows its preview and keeps the three fields behind an
    // Edit button, as Shopify does: most visits to that card are to check how
    // the result reads, not to rewrite it.
    let seoOpen = $state(false);

    /**
     * The products either side of this one, for the ↑ ↓ pair.
     *
     * They come from the same listing the Products screen shows, in the same
     * order, so "next" means what it looks like it means. Only the ids are
     * kept — walking the catalog is a navigation, not a reason to hold a
     * hundred products in memory.
     */
    let neighbours = $state({ prev: null, next: null });

    /**
     * The three free-text vocabularies the organisation fields suggest from.
     *
     * Vendor, product type and tag are columns on `products`, not tables, so
     * there is no list route to ask — the catalog is its own index. See load().
     */
    let vendors = $state([]);
    let productTypes = $state([]);
    let tagPool = $state([]);

    let collectionFilter = $state("");
    let creatingCollection = $state(false);
    let collectionOpen = $state(false);
    let collectionRoot = $state(null);
    let collectionTrigger = $state(null);
    let collectionDropdown = $state(null);
    let collectionFilterField = $state(null);
    const collectionDropdownId = "collection-picker";

    /**
     * The unit the number in the weight box is currently expressed in.
     *
     * It trails `form.weight_unit` by exactly one change, and has to: Select
     * writes the new unit through its binding before it calls onchange, so the
     * conversion would otherwise have nothing to convert *from*.
     */
    let shownIn = $state("g");

    // Bumped on discard so TokenInput remounts. The half-typed tag it is
    // holding is part of what discarding means, and it lives inside the
    // component rather than in `form`.
    let discardCount = $state(0);

    const hasOptions = $derived((product?.options?.length ?? 0) > 0);

    /**
     * The country list, with an empty choice at the top — "not recorded" is a
     * real answer here and the only way back to it once one is picked.
     *
     * A code the list does not carry is kept as its own option rather than
     * silently swapped for nothing: the engine accepts any well-formed code, so
     * a record written by an import or an older list must survive being looked
     * at in the panel.
     */
    const originCountryOptions = $derived.by(() => {
        const options = [{ value: "", label: "Not recorded" }, ...COUNTRIES];
        const held = form.origin_country;
        if (held && !COUNTRIES.some((c) => c.value === held)) {
            options.push({ value: held, label: held });
        }
        return options;
    });


    // ------------------------------------------------- category metafields
    /**
     * The chosen category and its ancestors, root first, from what is already
     * loaded. `categories` is the flat listing the picker uses, and it carries
     * `parent_id`, so a small store's whole chain is here with no request.
     *
     * It stops at whatever the listing holds: a truncated tree can be missing
     * an ancestor, and a chain that ends early is better than one that loops.
     * The gap is what fetchedChain below covers.
     */
    const localChain = $derived.by(() => {
        const byID = new Map(categories.map((c) => [c.id, c]));
        const chain = [];
        const seen = new Set();
        let node = byID.get(form.category_id);
        while (node && !seen.has(node.id)) {
            seen.add(node.id);
            chain.unshift(node);
            node = node.parent_id ? byID.get(node.parent_id) : null;
        }
        return chain;
    });

    /**
     * The same chain, from the server, for the categories the listing left out.
     *
     * A store carrying Shopify's taxonomy has fourteen thousand categories and
     * the listing sends a page of them, so walking `categories` finds nothing
     * for most products — which reads as a category that declares no fields
     * rather than as one nobody has asked about yet.
     *
     * It is also the only chain that carries the choices each field offers. The
     * listing does not: written onto every category, Shopify's value lists come
     * to 29MB. So the local walk is a first paint — right labels, no choices —
     * and the answer replaces it.
     */
    let fetchedChain = $state({ id: null, nodes: [] });

    const categoryChain = $derived(
        fetchedChain.id === form.category_id ? fetchedChain.nodes : localChain,
    );

    $effect(() => {
        const id = form.category_id;
        if (!id || fetchedChain.id === id) return;
        let stale = false;
        api.get(`/api/admin/categories/${id}/ancestors`)
            .then((result) => {
                if (!stale) fetchedChain = { id, nodes: result.data ?? [] };
            })
            .catch(() => {
                // The card is an extra, not the page. A category that cannot be
                // read is one whose fields go unasked, which is what the empty
                // chain already means.
                if (!stale) fetchedChain = { id, nodes: [] };
            });
        return () => {
            stale = true;
        };
    });

    /**
     * The fields the category asks for, inherited down the tree.
     *
     * A field declared on Bath & Body applies to Bar Soap under it — that is
     * what a taxonomy is for, and repeating "Age group" on every leaf would be
     * the alternative. The leaf wins a key its ancestor also declares, so a
     * category can narrow a parent's choices.
     *
     * `attributes` is what the ancestors route answers with: the fields the
     * category's own metadata declares, each with the choices the shared
     * taxonomy_attributes table holds for it. `metadata.attributes` is the
     * fallback, and carries no choices — the value lists are not written onto
     * every category, because Shopify's are 29MB when they are.
     */
    const categoryFields = $derived.by(() => {
        const byKey = new Map();
        for (const node of categoryChain) {
            for (const attr of node.attributes ?? node.metadata?.attributes ?? []) {
                if (!attr?.key || !attr?.label) continue;
                byKey.set(attr.key, {
                    key: attr.key,
                    label: attr.label,
                    choices: (attr.choices ?? []).filter(Boolean),
                });
            }
        }
        return [...byKey.values()];
    });

    /**
     * Fields the operator opened from the footer this session. A field with an
     * answer is always shown; this is what keeps an empty one on screen after
     * it is added, without writing an empty value into the form and calling
     * that an unsaved change.
     */
    let openedFields = $state(new Set());

    /*
     * A field is on screen once it has an entry, answered or not — removing the
     * last chip leaves an empty list behind, and the row has to stay for the
     * one after it to be typed. It goes back to being a chip on the next load,
     * because an empty answer is not saved.
     */
    const isShown = (f) => f.key in form.category_meta || openedFields.has(f.key);
    const filledFields = $derived(categoryFields.filter(isShown));
    const unfilledFields = $derived(categoryFields.filter((f) => !isShown(f)));

    function openField(key) {
        openedFields = new Set(openedFields).add(key);
        if (form.category_meta[key]) return;
        // An empty answer and no answer are the same fact, so a row appearing
        // is not an unsaved change. The baseline gets the empty list too, or
        // the save bar would announce work nobody has done yet.
        form.category_meta[key] = [];
        if (snapshot) snapshot.category_meta[key] = [];
    }

    /** The values as they go on the wire: empty answers are absent, not `[]`. */
    function categoryMetaObject() {
        const out = {};
        for (const field of categoryFields) {
            const values = form.category_meta[field.key] ?? [];
            if (values.length) out[field.key] = [...values];
        }
        // An answer whose field the category no longer declares is kept rather
        // than dropped: recategorising a product should not throw away what was
        // typed under the old one, in case it is moved back.
        for (const [key, values] of Object.entries(form.category_meta)) {
            if (!(key in out) && values.length && !categoryFields.some((f) => f.key === key)) {
                out[key] = [...values];
            }
        }
        return out;
    }
    const defaultVariant = $derived(product?.variants?.[0] ?? null);

    /**
     * A switch commits on its own.
     *
     * The rest of this form is a draft until the save bar is pressed, and a
     * switch is not: it has two states, both meaningful, and no third state
     * that means "I have started changing it". Leaving one pending makes the
     * page claim unsaved changes because somebody looked at a toggle.
     *
     * It patches only its own field rather than calling save(), so flipping a
     * switch cannot commit a half-typed title sitting beside it. The snapshot
     * moves with it, or the save bar would go on offering to save a value that
     * is already saved.
     *
     * On refusal the switch goes back. The engine has a real one to refuse —
     * turning off "sell when out of stock" while the count is negative — and a
     * control that stays where you put it after the store said no is a control
     * that lies.
     */
    async function saveToggle(field, value) {
        if (!defaultVariant) return;
        const was = snapshot[field];
        form[field] = value;
        try {
            const updated = await api.patch(`/api/admin/variants/${defaultVariant.id}`, {
                [field]: value,
            });
            snapshot[field] = value;
            // Keep the record in step so the readouts under the switch — the
            // available count, the stock help — describe what was just saved.
            if (product?.variants?.length) {
                product.variants[0] = updated;
            }
        } catch (err) {
            form[field] = was;
            toast.error(err);
        }
    }
    const currency = $derived(
        defaultVariant?.price?.currency ?? product?.currency ?? "USD",
    );

    /**
     * Profit and margin, from the price and the cost.
     *
     * Derived rather than stored: they are two subtractions away from numbers
     * the variant already holds, and a stored copy is a number that can be
     * wrong. Both read "—" until there is a cost, because a margin computed
     * from an absent cost is a claim of 100%.
     */
    const profit = $derived.by(() => {
        const price = parseFloat(form.price);
        const cost = parseFloat(form.cost);
        if (!isFinite(price) || !isFinite(cost)) return null;
        return price - cost;
    });
    const margin = $derived.by(() => {
        const price = parseFloat(form.price);
        if (profit === null || !isFinite(price) || price <= 0) return null;
        return (profit / price) * 100;
    });

    // Comparing the two serialisations is enough because both are built by the
    // same function, so their key order matches by construction.
    const dirty = $derived(!!snapshot && JSON.stringify(form) !== JSON.stringify(snapshot));

    const chosenCollections = $derived(
        form.collection_ids
            .map((id) => collections.find((c) => c.id === id))
            .filter(Boolean),
    );

    const collectionNeedle = $derived(collectionFilter.trim().toLowerCase());
    const visibleCollections = $derived(
        collectionNeedle
            ? collections.filter((c) => c.title.toLowerCase().includes(collectionNeedle))
            : collections,
    );
    // "New" is no collection by that name, not an empty result: typing "Summer"
    // while "Summer Sale" exists is still a new collection, and the row saying
    // so is the only thing offering to make it.
    const collectionIsNew = $derived(
        !!collectionNeedle &&
            !collections.some((c) => c.title.trim().toLowerCase() === collectionNeedle),
    );

    /** The derived readouts, in the store's currency and its own decimals. */
    function formatMoney(value) {
        return `${currency} ${value.toFixed(2)}`;
    }

    function blank() {
        return {
            title: "",
            slug: "",
            description: "",
            status: "draft",
            product_type: "",
            vendor: "",
            tags: [],
            seo_title: "",
            seo_description: "",
            // null rather than "" — the API's uncategorised is a JSON null,
            // and an empty string here would be sent as one.
            category_id: null,
            collection_ids: [],
            metafields: [],
            // Values for the fields the chosen category defines. Keyed by the
            // attribute key, so a renamed label keeps the answers under it.
            category_meta: {},
            sku: "",
            barcode: "",
            price: "",
            compare_at: "",
            cost: "",
            taxable: true,
            track_inventory: true,
            continue_selling: false,
            origin_country: "",
            hs_code: "",
            // Numbers rather than strings, because `bind:value` on a number
            // input writes a number back — a string here would make the field
            // read as changed the first time it was touched.
            stock_on_hand: 0,
            weight: 0,
            weight_unit: "g",
        };
    }

    /**
     * The product's answers to its category's fields, as the form holds them.
     *
     * Stored under `metadata.category` rather than beside the operator's own
     * `metadata.custom`, for the same reason `custom` is namespaced: these are
     * the category's fields, not free-form ones, and a store that renames a
     * category field must not be able to collide with something a module keeps.
     *
     * Values are lists throughout, because most of these fields take more than
     * one answer — "Sensitive" and "All skin types" are both true of one soap —
     * and a single-answer field is a list of one rather than a second shape.
     */
    function categoryMetaOf(p) {
        const raw = p.metadata?.category ?? {};
        const out = {};
        for (const [key, value] of Object.entries(raw)) {
            const values = (Array.isArray(value) ? value : [value])
                .map((v) => (typeof v === "string" ? v : String(v)))
                .filter(Boolean);
            if (values.length) out[key] = values;
        }
        return out;
    }

    function shapeOf(p) {
        const variant = p.variants?.[0] ?? null;
        const code = variant?.price?.currency ?? p.currency ?? "USD";
        return {
            title: p.title ?? "",
            slug: p.slug ?? "",
            description: p.description ?? "",
            status: p.status ?? "draft",
            product_type: p.product_type ?? "",
            vendor: p.vendor ?? "",
            tags: [...(p.tags ?? [])],
            seo_title: p.seo_title ?? "",
            seo_description: p.seo_description ?? "",
            category_id: p.category?.id ?? null,
            collection_ids: (p.collections ?? []).map((c) => c.id),
            category_meta: categoryMetaOf(p),
            metafields: Object.entries(p.metadata?.custom ?? {}).map(([key, value]) => ({
                key,
                // Anything a module wrote as a number or an object is shown as
                // JSON rather than "[object Object]", so it round-trips.
                value: typeof value === "string" ? value : JSON.stringify(value),
            })),
            sku: variant?.sku ?? "",
            barcode: variant?.barcode ?? "",
            price: variant ? fromMinor(variant.price.amount_minor, code) : "",
            compare_at: variant?.compare_at_price
                ? fromMinor(variant.compare_at_price.amount_minor, code)
                : "",
            cost: variant?.cost ? fromMinor(variant.cost.amount_minor, code) : "",
            taxable: variant?.taxable ?? true,
            track_inventory: variant?.track_inventory ?? true,
            continue_selling: variant?.continue_selling ?? false,
            origin_country: variant?.origin_country ?? "",
            hs_code: variant?.hs_code ?? "",
            stock_on_hand: variant?.stock_on_hand ?? 0,
            weight: weightNumber(variant),
            weight_unit: variant?.weight_unit ?? "g",
        };
    }

    /**
     * The number to put in the weight box.
     *
     * The engine already renders the stored mass the way its unit wants to be
     * read — "2.5 kg" — so the figure is taken back out of that string rather
     * than computed here. One less place that could come to disagree with
     * weight.go about how many grams a pound is.
     */
    function weightNumber(variant) {
        const shown = parseFloat(String(variant?.weight ?? ""));
        return isFinite(shown) ? shown : (variant?.weight_grams ?? 0);
    }

    /**
     * The distinct spellings of one free-text column across the catalog.
     *
     * Folded case-insensitively because the engine folds tags that way, and the
     * first spelling seen wins — so the suggestions read the way the catalog
     * already reads rather than the way this function would have written them.
     */
    function distinct(rows, values) {
        const seen = new Map();
        for (const row of rows) {
            for (const raw of values(row)) {
                const value = String(raw ?? "").trim();
                if (!value) continue;
                const key = value.toLowerCase();
                if (!seen.has(key)) seen.set(key, value);
            }
        }
        return [...seen.values()].sort((a, b) => a.localeCompare(b));
    }

    // The fields the variant matrix can move underneath the form.
    const VARIANT_KEYS = [
        "sku",
        "barcode",
        "price",
        "compare_at",
        "track_inventory",
        "continue_selling",
        "origin_country",
        "hs_code",
        "stock_on_hand",
        "weight",
        "weight_unit",
    ];

    function seed(p) {
        snapshot = shapeOf(p);
        form = shapeOf(p);
        shownIn = form.weight_unit;
        errors = {};
    }

    /**
     * resync follows a write the matrix made through its own routes.
     *
     * Re-seeding the whole form would be wrong: the operator may be mid-sentence
     * in the title when they press "Save options", and throwing that away to
     * take a product back that never carried it is the sort of loss that is
     * never noticed until later. Only the fields that write could have moved
     * are taken.
     */
    function resync(fresh) {
        snapshot = shapeOf(fresh);
        for (const key of VARIANT_KEYS) form[key] = snapshot[key];
        shownIn = form.weight_unit;
    }

    $effect(() => {
        const id = productId;
        if (!id) return;
        load(id);
    });

    async function load(id) {
        loading = true;
        try {
            /*
             * Three requests, and two of them are vocabularies rather than the
             * record: the collections list is the picker's whole set of
             * choices, and one page of the catalog is where the vendor,
             * product type and tag suggestions come from — those are columns on
             * `products`, so the products *are* the index.
             *
             * 200 is the trade. It is enough for the suggestions to be useful
             * on any catalog a panel like this is opened against, and few
             * enough that opening a product stays one round trip. A catalog
             * past that will show a name that is already in use as "new"; it
             * still saves correctly, because the column is free text either
             * way.
             */
            const [fresh, cols, cats, catalog] = await Promise.all([
                api.get(`/api/admin/products/${id}`),
                api.get("/api/admin/collections" + query({ limit: 200 })),
                // Flat and unpaged: the picker needs the whole tree to draw it,
                // and a page of a tree is a forest of stumps.
                api.get("/api/admin/categories?flat=1"),
                api.get("/api/admin/products" + query({ limit: 200 })),
            ]);
            product = fresh;
            collections = cols.data ?? [];
            categories = cats.data ?? [];
            categoriesTruncated = (cats.meta?.total ?? 0) > categories.length;

            const rows = catalog.data ?? [];
            vendors = distinct(rows, (p) => [p.vendor]);
            productTypes = distinct(rows, (p) => [p.product_type]);
            tagPool = distinct(rows, (p) => p.tags ?? []);

            seed(fresh);
            loadNeighbours(id);
        } catch (err) {
            toast.error(err);
            if (err.status === 404) goto(`${base}/products`);
        } finally {
            loading = false;
        }
    }

    function addMetafield() {
        form.metafields = [...form.metafields, { key: "", value: "" }];
    }

    function removeMetafield(index) {
        form.metafields = form.metafields.filter((_, i) => i !== index);
    }

    /**
     * Two rows with the same name would silently collapse into one on save,
     * because the object they become has one slot per key. Saying so before
     * Save is pressed beats losing the row afterwards.
     */
    const duplicateMetafield = $derived.by(() => {
        const seen = new Set();
        for (const f of form.metafields) {
            const key = f.key.trim();
            if (!key) continue;
            if (seen.has(key)) return key;
            seen.add(key);
        }
        return "";
    });

    /**
     * The metafields as the object that goes on the wire, under `custom`.
     *
     * Namespaced rather than written at the top level of `metadata`, because
     * that top level is the engine's extension point: a module keeps its own
     * data there under its own key, and an operator typing "invoices" into a
     * free-form field would overwrite it. A row with no name is dropped rather
     * than saved as "": an empty box is a row someone started and abandoned.
     */
    function metafieldsObject() {
        const out = {};
        for (const f of form.metafields) {
            const key = f.key.trim();
            if (key) out[key] = f.value;
        }
        return out;
    }

    /**
     * The public URL for this product — what a storefront would fetch.
     *
     * This engine serves an API rather than a shop, so "preview" means the
     * product as the outside world can actually see it. Draft and archived
     * products 404 there on purpose, so the button says so instead of opening
     * an error.
     */
    const publicURL = $derived(
        product?.slug ? `${location.origin}/api/products/slug/${product.slug}` : "",
    );
    const previewable = $derived(product?.status === "active");

    async function loadNeighbours(id) {
        try {
            const result = await api.get("/api/admin/products" + query({ limit: 200 }));
            const ids = (result.data ?? []).map((p) => p.id);
            const at = ids.indexOf(Number(id));
            neighbours =
                at < 0
                    ? { prev: null, next: null }
                    : { prev: ids[at - 1] ?? null, next: ids[at + 1] ?? null };
        } catch {
            // Navigation is a convenience; failing to work out the neighbours
            // should not colour the page with an error toast.
            neighbours = { prev: null, next: null };
        }
    }

    async function share() {
        try {
            await navigator.clipboard.writeText(publicURL);
            toast.success("Link copied");
        } catch {
            // Clipboard access is denied outside a secure context, and there is
            // nothing the operator can do about it from here.
            toast.error("Could not copy the link. It is " + publicURL);
        }
    }

    /**
     * Picking an item closes the menu.
     *
     * A popover only light-dismisses on a click *outside* it, so without this
     * the menu stays open behind the toast and the next click on "More actions"
     * reads as toggle-shut rather than open.
     */
    function closeActions() {
        document.getElementById("product-actions")?.hidePopover();
    }

    /**
     * Duplicating builds a new product from this one's shape.
     *
     * SKUs are unique store-wide, so every variant needs a fresh one — the copy
     * is a different thing to sell, not the same thing twice. It lands as a
     * draft whatever the original's status, because a copy nobody has reviewed
     * should not go on sale by being created.
     */
    async function duplicate() {
        if (duplicating || !product) return;
        closeActions();
        duplicating = true;

        const shape = (title, mark) => ({
            title,
            description: product.description,
            status: "draft",
            product_type: product.product_type,
            vendor: product.vendor,
            tags: [...(product.tags ?? [])],
            category_id: product.category?.id ?? null,
            seo_title: product.seo_title,
            seo_description: product.seo_description,
            metadata: product.metadata ?? {},
            options: (product.options ?? []).map((axis) => ({
                name: axis.name,
                values: axis.values.map((v) => v.value),
            })),
            variants: (product.variants ?? []).map((v) => ({
                sku: `${v.sku}-${mark}`,
                barcode: "",
                price_minor: v.price.amount_minor,
                compare_at_price_minor: v.compare_at_price?.amount_minor ?? null,
                cost_minor: v.cost?.amount_minor ?? null,
                taxable: v.taxable,
                options: [...(v.options ?? [])],
                track_inventory: v.track_inventory,
                continue_selling: v.continue_selling,
                origin_country: v.origin_country ?? "",
                hs_code: v.hs_code ?? "",
                // Stock is not copied. It is a count of things on a shelf, and
                // the copy has none of them.
                stock_on_hand: 0,
                weight_grams: v.weight_grams ?? null,
                weight_unit: v.weight_unit || "g",
            })),
        });

        const stamp = Date.now().toString(36).toUpperCase().slice(-5);
        try {
            // The slug is derived from the title, so duplicating the same
            // product twice asks for a slug that already exists. Number the
            // copies the way Shopify does, and let the engine's own uniqueness
            // check decide which number is free — asking first would only race
            // another operator doing the same thing.
            let copy = null;
            for (let attempt = 1; !copy; attempt++) {
                try {
                    copy = await api.post(
                        "/api/admin/products",
                        shape(
                            `${product.title} (copy)` + (attempt > 1 ? ` ${attempt}` : ""),
                            attempt > 1 ? `${stamp}${attempt}` : stamp,
                        ),
                    );
                } catch (err) {
                    // Anything that is not a name already taken — and any
                    // store with ten copies of one product — is the operator's
                    // to see.
                    if (err?.status !== 409 || attempt >= 10) throw err;
                }
            }
            toast.success("Product duplicated");
            goto(`${base}/products/${copy.id}`);
        } catch (err) {
            toast.error(err);
        } finally {
            duplicating = false;
        }
    }

    async function toggleArchive() {
        if (archiving || !product) return;
        closeActions();
        archiving = true;
        const next = product.status === "archived" ? "draft" : "archived";
        try {
            await api.patch(`/api/admin/products/${product.id}`, { status: next });
            toast.success(next === "archived" ? "Product archived" : "Product restored as a draft");
            await load(product.id);
        } catch (err) {
            toast.error(err);
        } finally {
            archiving = false;
        }
    }

    function discard() {
        form = JSON.parse(JSON.stringify(snapshot));
        shownIn = form.weight_unit;
        errors = {};
        discardCount++;
    }

    async function save() {
        if (saving || !product) return;

        errors = {};
        if (!form.title.trim()) errors.title = "A title is required.";
        if (!hasOptions) {
            if (!form.sku.trim()) errors.sku = "A SKU is required.";
            if (form.price === "" || isNaN(parseFloat(form.price))) {
                errors.price = "A price is required.";
            }
        }
        if (duplicateMetafield) {
            // Blocked rather than merged: two rows sharing a name become one
            // key, and picking a winner silently for the operator is how the
            // other row's value disappears without anyone seeing it go.
            toast.error(`Two metafields are called “${duplicateMetafield}”.`);
            return;
        }
        if (Object.keys(errors).length) {
            toast.error("Some fields still need attention.");
            return;
        }

        saving = true;
        try {
            const patch = {};
            for (const key of [
                "title",
                "description",
                "status",
                "product_type",
                "vendor",
                "seo_title",
                "seo_description",
            ]) {
                if (form[key] !== snapshot[key]) {
                    patch[key] = typeof form[key] === "string" ? form[key].trim() : form[key];
                }
            }
            // An emptied slug means "I did not want to change it" far more
            // often than "make it blank", and blank is not a slug the engine
            // could route to anyway.
            if (form.slug.trim() && form.slug.trim() !== snapshot.slug) {
                patch.slug = form.slug.trim();
            }
            // Tags have no identity of their own, so the patch replaces the
            // whole set — it goes whole or not at all.
            if (JSON.stringify(form.tags) !== JSON.stringify(snapshot.tags)) {
                patch.tags = [...form.tags];
            }
            // A null goes on the wire when the category is cleared, and that
            // null is the request: the engine tells an omitted field ("leave
            // it") apart from an explicit null ("uncategorise"), which is why
            // "None" is a choice the picker can actually make.
            if (form.category_id !== snapshot.category_id) {
                patch.category_id = form.category_id;
            }
            if (
                JSON.stringify(form.metafields) !== JSON.stringify(snapshot.metafields) ||
                JSON.stringify(form.category_meta) !== JSON.stringify(snapshot.category_meta)
            ) {
                // The patch replaces `metadata` whole, so the rest of it has to
                // be carried across: writing only `{custom: …}` would delete
                // every key a module keeps on this product.
                patch.metadata = {
                    ...(product.metadata ?? {}),
                    custom: metafieldsObject(),
                    category: categoryMetaObject(),
                };
            }
            if (Object.keys(patch).length) {
                await api.patch(`/api/admin/products/${product.id}`, patch);
            }

            if (
                JSON.stringify(form.collection_ids) !== JSON.stringify(snapshot.collection_ids)
            ) {
                await request("PUT", `/api/admin/products/${product.id}/collections`, {
                    body: { collection_ids: [...form.collection_ids] },
                });
            }

            if (!hasOptions && defaultVariant) {
                const body = {};
                if (form.sku.trim() !== snapshot.sku) body.sku = form.sku.trim();
                if (form.barcode.trim() !== snapshot.barcode) body.barcode = form.barcode.trim();
                if (form.price !== snapshot.price) {
                    body.price_minor = toMinor(form.price, currency);
                }
                if (form.compare_at !== snapshot.compare_at) {
                    if (form.compare_at.trim() === "") {
                        // The patch cannot express "unset". Its fields are
                        // pointers, so a missing one means "leave it alone",
                        // and 0 is stored as a real zero — a storefront would
                        // then strike through $0.00. Saying so beats writing a
                        // price nobody asked for.
                        toast.warning(
                            "A compare-at price cannot be cleared through the API, so it was left as it was.",
                        );
                    } else {
                        body.compare_at_price_minor = toMinor(form.compare_at, currency);
                    }
                }
                if (form.cost !== snapshot.cost) {
                    // An emptied box is "no cost recorded", which the column
                    // stores as null — not as zero, which would report a 100%
                    // margin on something nobody has costed.
                    body.cost_minor =
                        form.cost.trim() === "" ? null : toMinor(form.cost, currency);
                }
                if (form.taxable !== snapshot.taxable) body.taxable = form.taxable;
                // The two switches commit themselves, so these are normally
                // already equal — they stay as the backstop for a toggle whose
                // own save was refused and left the form ahead of the record.
                if (form.track_inventory !== snapshot.track_inventory) {
                    body.track_inventory = form.track_inventory;
                }
                if (form.continue_selling !== snapshot.continue_selling) {
                    body.continue_selling = form.continue_selling;
                }
                if (form.origin_country.trim().toUpperCase() !== snapshot.origin_country) {
                    body.origin_country = form.origin_country.trim().toUpperCase();
                }
                if (form.hs_code.trim() !== snapshot.hs_code) {
                    body.hs_code = form.hs_code.trim();
                }
                if (
                    form.weight !== snapshot.weight ||
                    form.weight_unit !== snapshot.weight_unit
                ) {
                    // The value and its unit, never grams. resolveWeight() in
                    // the engine owns the conversion; a second copy of the
                    // factors on this side of the wire is how the two would
                    // eventually disagree about a pound.
                    body.weight = parseFloat(form.weight) || 0;
                    body.weight_unit = form.weight_unit;
                }
                if (Object.keys(body).length) {
                    await api.patch(`/api/admin/variants/${defaultVariant.id}`, body);
                }

                // Stock is deliberately not part of that patch. The engine takes
                // it as a movement so a sale that lands mid-edit is not silently
                // overwritten by a number read before it happened.
                if (form.track_inventory && form.stock_on_hand !== snapshot.stock_on_hand) {
                    await api.post(`/api/admin/variants/${defaultVariant.id}/inventory`, {
                        set: parseInt(form.stock_on_hand, 10) || 0,
                    });
                }
            }

            product = await api.get(`/api/admin/products/${product.id}`);
            seed(product);
            toast.success("Product saved");
        } catch (err) {
            toast.error(err);
        } finally {
            saving = false;
        }
    }

    async function doDelete() {
        try {
            await api.delete(`/api/admin/products/${product.id}`);
            toast.success(`Deleted ${product.title}`);
            // Nothing left to edit; the snapshot goes with it so the save bar
            // cannot come back on the way out.
            snapshot = null;
            await goto(`${base}/products`);
        } catch (err) {
            toast.error(err);
        }
    }

    function toggleCollection(id) {
        form.collection_ids = form.collection_ids.includes(id)
            ? form.collection_ids.filter((c) => c !== id)
            : [...form.collection_ids, id];
    }

    /**
     * Creating a collection writes immediately; membership still waits for the
     * save bar.
     *
     * That is the same line the rest of the page draws. A collection is a
     * record with a route of its own, so it either exists or it does not, and
     * holding a title as "pending" until Save means another tab could take it
     * first. Which products are in it is a property of this form.
     */
    async function createCollection() {
        const title = collectionFilter.trim();
        if (!title || creatingCollection) return;

        creatingCollection = true;
        try {
            // The slug derives from the title server-side, so it is not sent.
            const created = await api.post("/api/admin/collections", { title });
            collections = [...collections, created];
            if (!form.collection_ids.includes(created.id)) {
                form.collection_ids = [...form.collection_ids, created.id];
            }
            collectionFilter = "";
            // Clearing the filter unmounts the row that was just clicked, and
            // focus would fall to the body — which reads as the popover
            // closing itself, since focus leaving it is what closes it.
            collectionFilterField?.focus();
            toast.success(`Created ${created.title}`);
        } catch (err) {
            toast.error(err);
        } finally {
            creatingCollection = false;
        }
    }

    // Select.svelte's keyboard, over the same `.select-option` rows — the
    // create row is the last of them whenever there is one.
    function collectionItems() {
        return [...(collectionDropdown?.querySelectorAll(".select-option") ?? [])];
    }

    function focusCollection(index) {
        const items = collectionItems();
        if (!items.length) return;
        items[(index + items.length) % items.length].focus();
    }

    function onCollectionTriggerKeydown(event) {
        if (event.key !== "ArrowDown" && event.key !== "ArrowUp" && event.key !== "Enter") return;
        if (collectionDropdown?.matches(":popover-open")) return;
        event.preventDefault();
        collectionDropdown?.showPopover();
        queueMicrotask(() => focusCollection(event.key === "ArrowUp" ? -1 : 0));
    }

    function onCollectionFilterKeydown(event) {
        // Only Enter: the arrows are left to bubble to the dropdown's own
        // handler, which reads "nothing focused yet" as "start at the top".
        if (event.key !== "Enter") return;
        event.preventDefault();
        if (collectionIsNew) createCollection();
    }

    function onCollectionDropdownKeydown(event) {
        const items = collectionItems();
        const here = items.indexOf(document.activeElement);
        if (event.key === "ArrowDown") {
            event.preventDefault();
            focusCollection(here + 1);
        } else if (event.key === "ArrowUp") {
            event.preventDefault();
            focusCollection(here - 1);
        } else if (event.key === "Home") {
            event.preventDefault();
            focusCollection(0);
        } else if (event.key === "End") {
            event.preventDefault();
            focusCollection(items.length - 1);
        }
    }

    function onCollectionToggle(event) {
        collectionOpen = event.newState === "open";
        if (collectionOpen) {
            // Typing a name nothing matches is how a collection gets created,
            // so the caret starts in the box that does it rather than one tab
            // away from it.
            queueMicrotask(() => collectionFilterField?.focus());
            return;
        }
        collectionFilter = "";
        // Escape and light dismiss leave focus on a checkbox that is no longer
        // rendered, which drops it to the body and loses the operator's place.
        if (collectionDropdown?.contains(document.activeElement)) collectionTrigger?.focus();
    }

    function onCollectionFocusOut(event) {
        if (event.relatedTarget && collectionRoot?.contains(event.relatedTarget)) return;
        if (collectionDropdown?.matches(":popover-open")) collectionDropdown.hidePopover();
    }

    /*
     * The unit factors, exact by definition rather than by measurement — the
     * same table weight.go holds.
     *
     * They are here for *display* only. What gets saved is always the number
     * and the unit as typed, so the engine remains the single thing that
     * decides how many grams that is; this only answers "the same parcel, read
     * in a different unit", which is a question no request can be made of.
     */
    const GRAMS_PER = { g: 1, kg: 1000, oz: 28.349523125, lb: 453.59237 };

    /**
     * Changing the unit re-reads the same mass; it does not relabel the number.
     * 2.5 kg becomes 5.512 lb, because the parcel did not get lighter when the
     * operator changed how they wanted to read it.
     */
    function onWeightUnitChange(next) {
        if (next === shownIn) return;
        const grams = (parseFloat(form.weight) || 0) * (GRAMS_PER[shownIn] ?? 1);
        const value = grams / (GRAMS_PER[next] ?? 1);
        // Grams are stored whole and the engine renders every other unit to
        // three decimals, so matching it means the box says what the record
        // will say once it is saved.
        form.weight = next === "g" ? Math.round(value) : Math.round(value * 1000) / 1000;
        shownIn = next;
    }

    // The storefront chooses its own paths, so this is the store's host and the
    // slug — the part the engine actually owns — rather than a guessed route.
    const previewHost = $derived(typeof location === "undefined" ? "" : location.host);

    const availableTotal = $derived(
        (product?.variants ?? [])
            .filter((v) => v.track_inventory)
            .reduce((sum, v) => sum + v.available, 0),
    );

    // Tones, not verdicts: a draft is a state the operator chose, not a warning
    // about one. Green for on sale, blue for not yet, grey for retired.
    const statusLabel = { active: "success", draft: "info", archived: "" };
</script>

<!-- `shopify-skin` re-skins this one screen: grey ground, white cards, labels
     above their fields. It is scoped to the page rather than global so the rest
     of the panel stays PocketBase — see the block in gocommerce.css. -->
<div class="page page-products shopify-skin">
    <div class="page-content full-height">
        <SaveBar
            {dirty}
            {saving}
            message="Unsaved changes"
            saveLabel="Save"
            onsave={save}
            ondiscard={discard}
        />

        <header class="page-header">
            <nav class="breadcrumbs">
                <a href="{base}/products">Products</a>
                <div>{product?.title || "…"}</div>
            </nav>

            <div class="page-header-primary-btns">
                {#if product}
                    <span class="label status-chip {statusLabel[product.status] ?? ''}">{product.status}</span>

                    <!--
                        Shopify's header actions. Preview and Share both point at
                        the public product, which for a headless engine is the
                        API route a storefront would fetch — that is the product
                        as the outside world can see it.
                    -->
                    <a
                        class="btn sm secondary"
                        class:disabled={!previewable}
                        href={previewable ? publicURL : undefined}
                        target="_blank"
                        rel="noreferrer"
                        title={previewable
                            ? "Open this product as the storefront sees it"
                            : "Only active products are published"}
                    >
                        <span class="txt">Preview</span>
                    </a>

                    <button type="button" class="btn sm secondary" onclick={share}>
                        <span class="txt">Share</span>
                    </button>

                    <button
                        type="button"
                        class="btn sm secondary"
                        popovertarget="product-actions"
                        aria-haspopup="menu"
                    >
                        <span class="txt">More actions</span>
                        <i class="ri-arrow-down-s-line" aria-hidden="true"></i>
                    </button>
                    <div id="product-actions" class="dropdown dropdown-sm" popover="auto" role="menu">
                        <button
                            type="button"
                            role="menuitem"
                            class="dropdown-item"
                            class:loading={duplicating}
                            disabled={duplicating}
                            onclick={duplicate}
                        >
                            <i class="ri-file-copy-line" aria-hidden="true"></i>
                            <span class="txt">Duplicate</span>
                        </button>
                        <button
                            type="button"
                            role="menuitem"
                            class="dropdown-item"
                            class:loading={archiving}
                            disabled={archiving}
                            onclick={toggleArchive}
                        >
                            <i class="ri-archive-line" aria-hidden="true"></i>
                            <span class="txt">
                                {product.status === "archived" ? "Restore as draft" : "Archive"}
                            </span>
                        </button>
                        <button
                            type="button"
                            role="menuitem"
                            class="dropdown-item txt-danger"
                            onclick={() => {
                                closeActions();
                                deleteOpen = true;
                            }}
                        >
                            <i class="ri-delete-bin-7-line" aria-hidden="true"></i>
                            <span class="txt">Delete</span>
                        </button>
                    </div>

                    <!-- Through the catalog in the order the Products screen
                         lists it, so "next" means what it looks like. -->
                    <div class="split-btn even" role="group" aria-label="Move through products">
                        <button
                            type="button"
                            class="btn sm secondary"
                            disabled={!neighbours.prev}
                            title="Previous product"
                            aria-label="Previous product"
                            onclick={() => goto(`${base}/products/${neighbours.prev}`)}
                        >
                            <i class="ri-arrow-up-s-line" aria-hidden="true"></i>
                        </button>
                        <button
                            type="button"
                            class="btn sm secondary split-btn-toggle"
                            disabled={!neighbours.next}
                            title="Next product"
                            aria-label="Next product"
                            onclick={() => goto(`${base}/products/${neighbours.next}`)}
                        >
                            <i class="ri-arrow-down-s-line" aria-hidden="true"></i>
                        </button>
                    </div>
                {/if}
            </div>
        </header>

        {#if loading && !product}
            <div class="block txt-center p-base"><span class="loader lg"></span></div>
        {:else if product}
            <div class="grid product-columns">
                <div class="col-lg-8">
                    <section class="card">
                    <div class="field required" class:error={!!errors.title}>
                        <label for="title">Title</label>
                        <input id="title" type="text" bind:value={form.title} />
                    </div>
                    {#if errors.title}<div class="field-help error">{errors.title}</div>{/if}

                    <div class="field m-t-sm">
                        <label for="description">Description</label>
                        <RichText
                            id="description"
                            placeholder="What is it, and why would someone want it?"
                            bind:value={form.description}
                        />
                    </div>

                    </section>
                    <section class="card">
                    <h6 class="section-title">
                        <i class="ri-image-line" aria-hidden="true"></i>
                        Media
                    </h6>
                    <MediaZone productId={product.id} bind:media />

                    </section>

                    {#if !hasOptions}
                    <section class="card">
                    <h6 class="section-title">
                        <i class="ri-money-dollar-circle-line" aria-hidden="true"></i>
                        Pricing
                    </h6>

                        <div class="fields">
                            <div class="field required" class:error={!!errors.price}>
                                <label for="price">Price</label>
                                <input
                                    id="price"
                                    type="text"
                                    inputmode="decimal"
                                    bind:value={form.price}
                                />
                            </div>
                            <div class="field">
                                <label for="compare-at">Compare-at price</label>
                                <input
                                    id="compare-at"
                                    type="text"
                                    inputmode="decimal"
                                    bind:value={form.compare_at}
                                />
                            </div>
                        </div>
                        {#if errors.price}<div class="field-help error">{errors.price}</div>{/if}
                        <div class="field-help">
                            Compare-at is the price a storefront strikes through. Both are in
                            {currency}.
                        </div>

                        <div class="field m-t-sm">
                            <input id="taxable" type="checkbox" bind:checked={form.taxable} />
                            <label for="taxable">Charge tax on this product</label>
                        </div>

                        <hr class="m-t-base m-b-base" />

                        <!--
                            Cost, and the two numbers that fall out of it. Shopify
                            groups them on one row because they are one thought:
                            what it cost, what is left, and what that is as a
                            share of the price.
                        -->
                        <div class="fields">
                            <div class="field">
                                <label for="cost">Cost per item</label>
                                <input
                                    id="cost"
                                    type="text"
                                    inputmode="decimal"
                                    bind:value={form.cost}
                                />
                            </div>
                            <div class="field readonly-field">
                                <span class="readonly-label">Profit</span>
                                <output class="readonly-value">
                                    {profit === null ? "—" : formatMoney(profit)}
                                </output>
                            </div>
                            <div class="field readonly-field">
                                <span class="readonly-label">Margin</span>
                                <output class="readonly-value">
                                    {margin === null ? "—" : margin.toFixed(1) + "%"}
                                </output>
                            </div>
                        </div>
                        <div class="field-help">
                            Cost is yours alone — a storefront never sees it. Profit and margin
                            follow from it and the price.
                        </div>

                    </section>
                    {/if}
                    {#if !hasOptions}
                    <section class="card">
                    <h6 class="section-title">
                        <i class="ri-archive-2-line" aria-hidden="true"></i>
                        Inventory
                    </h6>

                        <div class="fields">
                            <div class="field required" class:error={!!errors.sku}>
                                <label for="sku">SKU</label>
                                <input id="sku" type="text" placeholder="TEE-001" bind:value={form.sku} />
                            </div>
                            <div class="delimiter"></div>
                            <div class="field">
                                <label for="barcode">Barcode</label>
                                <input id="barcode" type="text" bind:value={form.barcode} />
                            </div>
                        </div>
                        {#if errors.sku}<div class="field-help error">{errors.sku}</div>{/if}

                        <div class="field m-t-sm">
                            <!-- Committed on the spot, not with the save bar: see
                                 saveToggle. `checked` plus `onchange` rather than
                                 `bind:`, so the switch shows what is saved and the
                                 save is what moves it. -->
                            <input
                                id="track-inventory"
                                type="checkbox"
                                class="switch"
                                checked={form.track_inventory}
                                onchange={(e) => saveToggle("track_inventory", e.currentTarget.checked)}
                            />
                            <label for="track-inventory">Track quantity</label>
                        </div>

                        <div class="field m-t-sm" class:disabled={!form.track_inventory}>
                            <label for="stock">Quantity on hand</label>
                            <input
                                id="stock"
                                type="number"
                                min="0"
                                disabled={!form.track_inventory}
                                bind:value={form.stock_on_hand}
                            />
                        </div>
                        <div class="field-help">
                            {#if form.track_inventory}
                                Saving this sends a stock take to the inventory endpoint, not the
                                variant patch — stock only ever moves as a transactional
                                adjustment, so a sale that lands mid-edit cannot be overwritten.
                                {#if defaultVariant}
                                    <strong>{defaultVariant.available}</strong> available right now
                                    ({defaultVariant.stock_reserved} reserved for open orders).
                                {/if}
                            {:else}
                                Untracked variants can always be bought. Turn tracking on to hold a
                                count.
                            {/if}
                        </div>

                        <!--
                            Shopify's "Sell when out of stock", under the count
                            it modifies. Only meaningful while tracking is on:
                            an untracked variant has no zero to sell past.
                        -->
                        {#if form.track_inventory}
                            <div class="field m-t-sm">
                                <input
                                    id="continue-selling"
                                    type="checkbox"
                                    class="switch"
                                    checked={form.continue_selling}
                                    onchange={(e) =>
                                        saveToggle("continue_selling", e.currentTarget.checked)}
                                />
                                <label for="continue-selling">Sell when out of stock</label>
                            </div>
                            <div class="field-help">
                                {#if form.continue_selling}
                                    Orders are taken past zero and the count goes negative, so the
                                    backlog is visible rather than hidden. The store has to be able
                                    to make or buy the difference.
                                {:else}
                                    A shopper is refused once the last one is spoken for. Switching
                                    this on cannot be undone while the count is negative — restock
                                    first.
                                {/if}
                            </div>
                        {/if}

                    </section>
                    {/if}
                    {#if !hasOptions}
                    <section class="card">
                    <h6 class="section-title">
                        <i class="ri-truck-line" aria-hidden="true"></i>
                        Shipping
                    </h6>

                        <!--
                            Weight, its unit and the tariff number on one row.
                            All three are short — a number, a word and six
                            digits — and the row is what a shipping label needs
                            read off in one go.
                        -->
                        <div class="fields">
                            <div class="field">
                                <label for="weight">Weight</label>
                                <input
                                    id="weight"
                                    type="number"
                                    min="0"
                                    step="any"
                                    bind:value={form.weight}
                                />
                            </div>
                            <div class="delimiter"></div>
                            <!-- No label: the options name themselves, and the
                                 half is read as part of the number beside it. -->
                            <div class="field">
                                <Select
                                    id="weight-unit"
                                    ariaLabel="Weight unit"
                                    bind:value={form.weight_unit}
                                    onchange={onWeightUnitChange}
                                    options={[
                                        { value: "g", label: "Grams (g)" },
                                        { value: "kg", label: "Kilograms (kg)" },
                                        { value: "oz", label: "Ounces (oz)" },
                                        { value: "lb", label: "Pounds (lb)" },
                                    ]}
                                />
                            </div>
                            <div class="delimiter"></div>
                            <div class="field">
                                <label for="hs-code">HS code</label>
                                <input
                                    id="hs-code"
                                    type="text"
                                    inputmode="numeric"
                                    placeholder="6109.10"
                                    bind:value={form.hs_code}
                                />
                            </div>
                        </div>
                        <div class="field-help">
                            The record holds whole grams; the unit is how you read them back,
                            exactly as a currency code is. Both are sent as typed and converted by
                            the engine, so switching units here shows the same mass in the new
                            unit rather than relabelling the figure. The tariff number is stored as
                            digits, so 6109.10 and 610910 are the same code.
                        </div>

                        <!-- The country keeps the row below to itself: it is the
                             one control here that is a name rather than a
                             figure, and squeezed into the row above its list
                             becomes too narrow to read a country out of. -->
                        <div class="field m-t-sm">
                            <label for="origin-country">Country of origin</label>
                            <!-- A list, not a two-letter box: nobody knows the
                                 codes, and Select reveals its own search once
                                 the options pass its threshold. -->
                            <Select
                                id="origin-country"
                                placeholder="Not recorded"
                                bind:value={form.origin_country}
                                options={originCountryOptions}
                            />
                        </div>
                        <div class="field-help">
                            For the customs form on a cross-border parcel, and fine left empty.
                            Stored as its two-letter code.
                        </div>

                    </section>
                    {/if}

                    <!-- Its own card, outside the Shipping guard above: the
                         options editor and the variant table are the one part
                         of this page that matters *more* once a product has
                         options, so they must not share a card that hides
                         itself when it does. -->
                    <section class="card">
                    <VariantMatrix bind:product {media} onchange={resync} />
                    </section>
                    <!--
                        Shopify's Category metafields card: the fields the chosen
                        category asks for, with the ones nobody has answered yet
                        collapsed into a row of chips at the foot. Sixteen empty
                        boxes is what the chips exist to avoid — a soap category
                        asks about fragrance, skin type and solvent content, and
                        most products answer three of them.

                        It is absent, not empty, when the category declares no
                        fields: a card that only ever says "nothing here" is a
                        card that teaches operators to scroll past it.
                    -->
                    {#if categoryFields.length}
                    <section class="card">
                    <!-- No icon and no rule, which is the one heading on this page
                         that departs from PocketBase's: Shopify's card is a plain
                         line of text with the category at the far end, and a rule
                         between the two would read as a separator between a title
                         and its own subject. -->
                    <h6 class="section-title cat-meta-title">
                        Category metafields
                        <span class="label cat-meta-scope">
                            {categoryChain[categoryChain.length - 1]?.title}
                            {#if categoryChain.length > 1}
                                <span class="txt-hint">
                                    in {categoryChain[categoryChain.length - 2].title}
                                </span>
                            {/if}
                        </span>
                    </h6>

                    {#if filledFields.length}
                        <div class="cat-meta-rows">
                            {#each filledFields as field (field.key)}
                                <div class="cat-meta-row">
                                    <span class="cat-meta-label">{field.label}</span>
                                    <TokenInput
                                        bind:values={form.category_meta[field.key]}
                                        options={field.choices}
                                        emptyText={field.choices.length
                                            ? "Nothing left to choose"
                                            : "This field has no set choices — type your own."}
                                    />
                                </div>
                            {/each}
                        </div>
                    {/if}

                    {#if unfilledFields.length}
                        <!-- The footer strip is recessed, as Shopify has it, so
                             the chips read as things you could add rather than
                             as answers already given. -->
                        <div class="cat-meta-add">
                            {#each unfilledFields as field (field.key)}
                                <button
                                    type="button"
                                    class="cat-meta-chip"
                                    onclick={() => openField(field.key)}
                                >
                                    <i class="ri-add-line" aria-hidden="true"></i>
                                    <span class="txt">{field.label}</span>
                                </button>
                            {/each}
                        </div>
                    {/if}
                    </section>
                    {/if}

                    <section class="card">
                    <h6 class="section-title">
                        <i class="ri-price-tag-2-line" aria-hidden="true"></i>
                        Product metafields
                    </h6>

                    {#if form.metafields.length}
                        <div class="metafields m-b-sm">
                            {#each form.metafields as field, i (i)}
                                <div class="metafield-row">
                                    <div class="field">
                                        <label for="mf-key-{i}">Name</label>
                                        <input
                                            id="mf-key-{i}"
                                            type="text"
                                            placeholder="care_instructions"
                                            bind:value={field.key}
                                        />
                                    </div>
                                    <div class="field">
                                        <label for="mf-value-{i}">Value</label>
                                        <input
                                            id="mf-value-{i}"
                                            type="text"
                                            bind:value={field.value}
                                        />
                                    </div>
                                    <button
                                        type="button"
                                        class="btn circle sm transparent secondary"
                                        aria-label="Remove {field.key || 'this metafield'}"
                                        title="Remove"
                                        onclick={() => removeMetafield(i)}
                                    >
                                        <i class="ri-close-line" aria-hidden="true"></i>
                                    </button>
                                </div>
                            {/each}
                        </div>
                    {/if}

                    {#if duplicateMetafield}
                        <div class="field-help error">
                            Two metafields are called “{duplicateMetafield}”. Only the last would
                            be saved, so rename one first.
                        </div>
                    {/if}

                    <button type="button" class="btn sm secondary" onclick={addMetafield}>
                        <i class="ri-add-line" aria-hidden="true"></i>
                        <span class="txt">Add metafield</span>
                    </button>

                    <div class="field-help">
                        Your own fields, for a storefront or an integration to read. Stored under
                        <code>metadata.custom</code>.
                    </div>

                    </section>
                    <section class="card">
                    <!--
                        Shopify's shape: the listing as it would read, and the
                        fields that produce it behind an Edit button. Most
                        visits to this card are to check the preview rather than
                        to change it, and three boxes open by default is three
                        boxes of noise for everyone who came to look.
                    -->
                    <h6 class="section-title">
                        <i class="ri-search-eye-line" aria-hidden="true"></i>
                        Search engine listing
                        <!-- `.section-title` puts its rule last, so a button in
                             the markup lands beside the caption; `.seo-edit`
                             orders it after the rule instead. -->
                        <button
                            type="button"
                            class="btn sm transparent seo-edit"
                            aria-expanded={seoOpen}
                            onclick={() => (seoOpen = !seoOpen)}
                        >
                            <span class="txt">{seoOpen ? "Done" : "Edit"}</span>
                        </button>
                    </h6>

                    {#if form.title || form.seo_title || form.seo_description}
                        <!-- A result page's own order: the url, the link, the
                             sentence under it. -->
                        <div class="seo-preview">
                            <div class="seo-preview-url">{previewHost}/{form.slug}</div>
                            <div class="seo-preview-title">
                                {form.seo_title || form.title}
                            </div>
                            <div class="seo-preview-description">
                                {form.seo_description ||
                                    "No meta description yet — a search engine will pick a sentence out of the page instead."}
                            </div>
                        </div>
                    {:else}
                        <p class="seo-preview-empty">
                            Add a title and description to see how this product might appear in a
                            search engine listing.
                        </p>
                    {/if}

                    {#if seoOpen}
                        <div class="seo-fields">
                            <div class="field">
                                <label for="seo-title">Page title</label>
                                <input
                                    id="seo-title"
                                    type="text"
                                    placeholder={form.title}
                                    bind:value={form.seo_title}
                                />
                            </div>
                            <div class="field-help">
                                {form.seo_title.length} of {SEO_TITLE_LIMIT} characters used. Empty
                                uses the product title.
                            </div>

                            <div class="field m-t-sm">
                                <label for="seo-description">Description</label>
                                <textarea
                                    id="seo-description"
                                    rows="3"
                                    bind:value={form.seo_description}
                                ></textarea>
                            </div>
                            <div class="field-help">
                                {form.seo_description.length} of {SEO_DESCRIPTION_LIMIT} characters used.
                            </div>

                            <!--
                                Prefixed the way Shopify prefixes it, with the store's
                                host and nothing after it. The storefront chooses its
                                own paths, so printing /products/ here would show the
                                operator a url that may not exist.
                            -->
                            <div class="field m-t-sm">
                                <label for="slug">URL handle</label>
                                <div class="seo-handle-row">
                                    <span class="seo-handle-prefix">{previewHost}/</span>
                                    <input id="slug" type="text" bind:value={form.slug} />
                                </div>
                            </div>
                            <div class="field-help">
                                Emptying the box keeps the current handle.
                            </div>
                        </div>
                    {/if}
                    </section>
                </div>

                <div class="col-lg-4">
                    <section class="card">
                    <h6 class="section-title m-t-0">
                        <i class="ri-checkbox-circle-line" aria-hidden="true"></i>
                        Status
                    </h6>
                    <div class="field">
                        <label for="status">Status</label>
                        <Select
                            id="status"
                            bind:value={form.status}
                            options={[
                                { value: "draft", label: "Draft — hidden from shoppers" },
                                { value: "active", label: "Active — on sale" },
                                { value: "archived", label: "Archived" },
                            ]}
                        />
                    </div>
                    <div class="field-help">
                        Only active products appear on your storefront.
                    </div>

                    </section>
                    <section class="card">
                    <h6 class="section-title">
                        <i class="ri-price-tag-3-line" aria-hidden="true"></i>
                        Product organization
                    </h6>

                    <!--
                        Category leads the section. It is the widest claim the
                        product makes about itself — what a feed and a tax rule
                        read — and the fields under it narrow from there: what
                        kind of thing, who makes it, which lists it is on.
                    -->
                    <div class="field">
                        <label for="category">
                            Category
                            <!-- Shopify counts them beside the label, which is
                                 the only place the number means anything: it is
                                 what changing the category would change. -->
                            {#if categoryFields.length}
                                <span class="txt-hint cat-meta-count">
                                    {categoryFields.length}
                                    {categoryFields.length === 1 ? "metafield" : "metafields"}
                                </span>
                            {/if}
                        </label>
                        <CategoryPicker
                            id="category"
                            bind:value={form.category_id}
                            {categories}
                            remote={categoriesTruncated}
                            selectedCategory={product?.category ?? null}
                        />
                    </div>
                    <div class="field-help">
                        Sets tax rates and helps a storefront sort this product.
                        <a href="{base}/categories">Manage the tree</a>.
                    </div>

                    <div class="field m-t-sm">
                        <label for="product-type">Product type</label>
                        <Combobox
                            id="product-type"
                            bind:value={form.product_type}
                            options={productTypes}
                            placeholder="Shirt"
                            emptyText="No product types in the catalog yet"
                        />
                    </div>

                    <div class="field m-t-sm">
                        <label for="vendor">Vendor</label>
                        <Combobox
                            id="vendor"
                            bind:value={form.vendor}
                            options={vendors}
                            placeholder="Acme"
                            emptyText="No vendors in the catalog yet"
                        />
                    </div>
                    <div class="field-help">
                        Suggestions come from your catalog. Type a new name to add it.
                    </div>

                    <div class="field m-t-sm">
                        <label for="collection-pick">Collections</label>
                        <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
                        <div
                            bind:this={collectionRoot}
                            class="input select multiple"
                            onfocusout={onCollectionFocusOut}
                        >
                            <button
                                bind:this={collectionTrigger}
                                type="button"
                                id="collection-pick"
                                class="selected-container"
                                popovertarget={collectionDropdownId}
                                aria-haspopup="listbox"
                                aria-expanded={collectionOpen}
                                onkeydown={onCollectionTriggerKeydown}
                            >
                                <!-- Chips, not comma-joined text: Shopify
                                     shows each membership as its own token so a
                                     long list wraps and stays countable at a
                                     glance. -->
                                {#each chosenCollections as collection (collection.id)}
                                    <span class="label">{collection.title}</span>
                                {/each}
                                <!--
                                    The add control is a chip of its own, sitting
                                    where the next one will appear. It is never
                                    hidden: it is the only way into the picker,
                                    so showing it only once the box already has
                                    something in it means a store with no
                                    collections can never make its first.
                                -->
                                <span class="label chip-new">
                                    <i class="ri-add-line" aria-hidden="true"></i>
                                    {chosenCollections.length ? "Add" : "Add collection"}
                                </span>
                            </button>

                            {#if chosenCollections.length}
                                <!-- The ⊕ only appears once there is something
                                     in the box. While it is empty the box's own
                                     "Add collections" is the affordance, and two
                                     of them would just be two ways to say it. -->
                                <button
                                    type="button"
                                    class="chip-add"
                                    popovertarget={collectionDropdownId}
                                    aria-label="Add a collection"
                                    title="Add a collection"
                                >
                                    <i class="ri-add-circle-line" aria-hidden="true"></i>
                                </button>
                            {/if}

                            <div
                                bind:this={collectionDropdown}
                                id={collectionDropdownId}
                                class="dropdown"
                                popover="auto"
                                tabindex="-1"
                                role="listbox"
                                aria-multiselectable="true"
                                aria-label="Collections"
                                ontoggle={onCollectionToggle}
                                onkeydown={onCollectionDropdownKeydown}
                            >
                                <div class="fields dropdown-search">
                                    <div class="field">
                                        <input
                                            bind:this={collectionFilterField}
                                            type="text"
                                            aria-label="Filter or name a collection"
                                            placeholder="Filter or name a new one…"
                                            bind:value={collectionFilter}
                                            onkeydown={onCollectionFilterKeydown}
                                        />
                                    </div>
                                    {#if collectionFilter}
                                        <div class="field addon p-r-5">
                                            <button
                                                type="button"
                                                title="Clear"
                                                class="btn sm secondary transparent circle"
                                                onclick={() => (collectionFilter = "")}
                                            >
                                                <i class="ri-close-line" aria-hidden="true"></i>
                                            </button>
                                        </div>
                                    {/if}
                                </div>

                                <!--
                                    The tick is an icon on a button rather than
                                    an <input type="checkbox">. A popover is a
                                    DOM descendant of the field it belongs to
                                    even while it is drawn in the top layer, so
                                    a real checkbox in here matches form.css's
                                    `.field:has(input[type="checkbox"])` — a
                                    rule written for a field that *is* a
                                    checkbox. It would strip the fill off the
                                    control, draw a phantom box beside the
                                    "Collections" label, and hide this
                                    popover's own filter box, which that rule
                                    shrinks to 1px.
                                -->
                                {#each visibleCollections as collection (collection.id)}
                                    {@const chosen = form.collection_ids.includes(collection.id)}
                                    <button
                                        type="button"
                                        role="option"
                                        aria-selected={chosen}
                                        class="dropdown-item select-option"
                                        onclick={() => toggleCollection(collection.id)}
                                    >
                                        <i
                                            class={chosen
                                                ? "ri-checkbox-line txt-success"
                                                : "ri-checkbox-blank-line txt-hint"}
                                            aria-hidden="true"
                                        ></i>
                                        {collection.title}
                                    </button>
                                {/each}

                                {#if collectionIsNew}
                                    <button
                                        type="button"
                                        class="dropdown-item select-option"
                                        disabled={creatingCollection}
                                        onclick={createCollection}
                                    >
                                        <i class="ri-add-line" aria-hidden="true"></i>
                                        Create “{collectionFilter.trim()}”
                                    </button>
                                {/if}

                                {#if !visibleCollections.length && !collectionIsNew}
                                    <div class="txt-hint txt-center m-0 p-5">
                                        No collections yet
                                    </div>
                                {/if}
                            </div>
                        </div>
                    </div>
                    <div class="field-help">
                        Type a name that does not exist yet to create it.
                    </div>

                    {#key discardCount}
                        <div class="field m-t-sm">
                            <label for="tags">Tags</label>
                            <TokenInput
                                id="tags"
                                bind:values={form.tags}
                                options={tagPool}
                                placeholder="Add tags"
                                emptyText="No tags in the catalog yet"
                            />
                            {#if form.tags.length}
                                <!-- A <label>, not a button: clicking it focuses
                                     the box, which is the whole action, and the
                                     browser wires that up without a handler. -->
                                <label for="tags" class="chip-add" title="Add a tag">
                                    <i class="ri-add-circle-line" aria-hidden="true"></i>
                                </label>
                            {/if}
                        </div>
                    {/key}
                    <div class="field-help">
                        Enter or a comma adds a tag.
                    </div>
                    </section>
                </div>
            </div>
        {/if}

        <footer class="page-footer">
            <span class="txt">
                {#if product}
                    {product.variants?.length ?? 0}
                    {pluralize(product.variants?.length ?? 0, "variant")},
                    <span class={stockClass(availableTotal)}>{availableTotal}</span> available
                {:else}
                    …
                {/if}
            </span>
            <ThemeToggle />
        </footer>
    </div>
</div>

<Confirm
    bind:open={deleteOpen}
    title="Delete this product?"
    message={product
        ? `"${product.title}" and its variants will be removed. Orders that included it keep their own snapshot, so history stays readable.`
        : ""}
    confirmLabel="Delete"
    danger
    onconfirm={doDelete}
/>
