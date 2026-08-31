<script>
    /**
     * What each role may do in this store.
     *
     * The engine ships a default set per role (rights.go) and this screen is
     * the store's departure from it. Two things follow from that and shape the
     * whole page: a role left alone keeps *tracking* the default rather than
     * freezing a copy, so "using defaults" and "customised" are worth saying
     * out loud; and a change lands on the affected operator's next request, so
     * nobody has to be signed out and nobody should be told to sign in again.
     *
     * Both axes come from the API — the roles and the closed list of rights —
     * so a right added to the engine appears here without this file changing.
     * The groups below are layout only, and anything they do not name falls
     * through to "Other" rather than disappearing.
     */
    import { roles as rolesApi, auth, getRecord, rights as myRights } from "$lib/api.js";
    import { toast } from "$lib/toast.svelte.js";
    import SettingsSidebar from "$lib/components/SettingsSidebar.svelte";
    import ThemeToggle from "$lib/components/ThemeToggle.svelte";

    let loading = $state(true);
    let matrix = $state(null);
    /** role -> the rights as edited here, before saving. */
    let draft = $state({});
    let busy = $state("");

    const me = getRecord();

    const ROLE_LABEL = { owner: "Owner", manager: "Manager", staff: "Staff" };
    const ROLE_BLURB = {
        owner: "Everything, including who else may do what.",
        manager: "Runs the shop day to day.",
        staff: "Works the orders.",
    };

    /*
     * One line per right, in the store's own words. The API sends the names;
     * a checkbox labelled `orders.refund` and nothing else asks the person
     * granting it to already know what it covers.
     */
    const RIGHT_TEXT = {
        "catalog.read": "See products, variants, categories, collections, media and stock levels",
        "catalog.write": "Edit any of them",
        "orders.read": "See orders and what customers bought",
        "orders.write": "Fulfil, edit and place orders",
        "orders.refund": "Send money back out of the store",
        "inventory.write": "Stock takes and adjustments",
        "customers.read": "Orders grouped by who placed them — personal data",
        "settings.write": "The store's settings, the team, and import / export",
    };

    const GROUPS = [
        { title: "Catalog", icon: "ri-price-tag-3-line", rights: ["catalog.read", "catalog.write"] },
        { title: "Orders", icon: "ri-shopping-bag-line", rights: ["orders.read", "orders.write", "orders.refund"] },
        { title: "Inventory", icon: "ri-stack-line", rights: ["inventory.write"] },
        { title: "Customers", icon: "ri-user-line", rights: ["customers.read"] },
        { title: "Settings", icon: "ri-settings-3-line", rights: ["settings.write"] },
    ];

    /** The groups, filtered to what this engine actually has, plus anything it
     *  has that the list above never heard of. */
    const grouped = $derived.by(() => {
        const all = matrix?.all_rights ?? [];
        const named = new Set(GROUPS.flatMap((g) => g.rights));
        const out = GROUPS.map((g) => ({ ...g, rights: g.rights.filter((r) => all.includes(r)) })).filter(
            (g) => g.rights.length,
        );
        const rest = all.filter((r) => !named.has(r));
        return rest.length ? [...out, { title: "Other", icon: "ri-question-line", rights: rest }] : out;
    });

    $effect(() => {
        load();
    });

    async function load() {
        loading = true;
        try {
            const result = await rolesApi.matrix();
            matrix = result.data ?? result;
            draft = {};
            for (const row of matrix.roles) draft[row.role] = [...row.rights];
        } catch (err) {
            toast.error(err);
        } finally {
            loading = false;
        }
    }

    const rowFor = (role) => matrix?.roles.find((r) => r.role === role);
    const has = (role, right) => (draft[role] ?? []).includes(right);

    /*
     * The two locks the API also enforces, mirrored here so nothing looks
     * clickable that the server would only refuse. Required rights are the
     * floor every role keeps; settings.write is what got you to this screen,
     * and an operator who saves it away from their own role has no way back
     * short of a static admin token.
     */
    const required = $derived(matrix?.required ?? []);
    const locked = (role, right) =>
        required.includes(right) || (role === me?.role && right === "settings.write");

    function lockReason(role, right) {
        if (required.includes(right)) {
            return "Every role keeps this: a role without it can sign in and see nothing.";
        }
        return "This is your own role — removing this would lock you out of this screen.";
    }

    function toggle(role, right) {
        if (locked(role, right)) return;
        const current = draft[role] ?? [];
        draft[role] = current.includes(right)
            ? current.filter((r) => r !== right)
            : [...current, right];
    }

    const same = (a, b) => a.length === b.length && [...a].sort().every((v, i) => v === [...b].sort()[i]);
    const dirty = (role) => !same(draft[role] ?? [], rowFor(role)?.rights ?? []);

    /** Whether the draft matches the engine's defaults — which is what "reset"
     *  would produce, so the button says nothing useful when it already does. */
    const isDefault = (role) => same(draft[role] ?? [], rowFor(role)?.default ?? []);

    async function save(role) {
        if (busy) return;
        busy = role;
        try {
            const result = await rolesApi.save(role, draft[role] ?? []);
            applySaved(result.data ?? result);
            toast.success(`Saved the ${ROLE_LABEL[role] ?? role} role`);
            await refreshMeIfAffected(role);
        } catch (err) {
            toast.error(err);
        } finally {
            busy = "";
        }
    }

    async function reset(role) {
        if (busy) return;
        busy = role;
        try {
            const result = await rolesApi.reset(role);
            applySaved(result.data ?? result);
            toast.success(`${ROLE_LABEL[role] ?? role} is back on the defaults`);
            await refreshMeIfAffected(role);
        } catch (err) {
            toast.error(err);
        } finally {
            busy = "";
        }
    }

    function applySaved(set) {
        matrix.roles = matrix.roles.map((r) => (r.role === set.role ? set : r));
        draft[set.role] = [...set.rights];
    }

    /*
     * The panel hides what it cannot do from the record it stored at sign-in.
     * Changing your own role's rights makes that record wrong, and the symptom
     * is a nav that offers a screen the engine will refuse — so re-read it.
     */
    async function refreshMeIfAffected(role) {
        if (me?.role !== role) return;
        try {
            await auth.refresh();
        } catch {
            /* the next request will correct it; nothing here is worth a toast */
        }
    }
</script>

<div class="page page-roles">
    <SettingsSidebar />

    <div class="page-content full-height">
        <header class="page-header">
            <nav class="breadcrumbs">
                <div class="breadcrumb-item">Settings</div>
                <div class="breadcrumb-item">Roles</div>
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
        </header>

        <div class="wrapper m-b-base">
            {#if loading}
                <div class="block txt-center"><span class="loader lg"></span></div>
            {:else if matrix}
                <div class="field-help m-b-base">
                    What each role may do in this store. A change applies on the next request
                    somebody makes — nobody has to sign out. A role left on its defaults keeps
                    tracking them, so a right added to it in a later release arrives on its own.
                </div>

                {#each matrix.roles as row (row.role)}
                    <h6 class="section-title">
                        <i class="ri-shield-user-line" aria-hidden="true"></i>
                        <span>{ROLE_LABEL[row.role] ?? row.role}</span>
                        {#if !row.configurable}
                            <span class="label" title="The owner is the way back into a store that has been configured into a corner.">
                                <i class="ri-lock-line" aria-hidden="true"></i>
                                <span class="txt">fixed — always every right</span>
                            </span>
                        {:else if row.customized}
                            <span class="label warning">customised</span>
                        {:else}
                            <span class="label">using defaults</span>
                        {/if}
                        {#if row.role === me?.role}
                            <span class="label sm">your role</span>
                        {/if}
                        <small class="txt-hint m-l-auto">
                            {(draft[row.role] ?? []).length} of {matrix.all_rights.length}
                        </small>
                    </h6>

                    <div class="field-help m-b-sm">{ROLE_BLURB[row.role] ?? ""}</div>

                    <div class="grid m-b-base">
                        {#each grouped as group (group.title)}
                            <div class="col-md-4">
                                <div class="txt-hint txt-sm m-b-5">
                                    <i class={group.icon} aria-hidden="true"></i>
                                    {group.title}
                                </div>
                                {#each group.rights as right (right)}
                                    <!-- The right's name is the label and the
                                         sentence sits under it: the name is what
                                         a refusal quotes back at an operator
                                         ("does not carry orders.refund"), so it
                                         is the thing to scan a column for. -->
                                    <div class="field">
                                        <input
                                            type="checkbox"
                                            id="{row.role}-{right}"
                                            checked={row.configurable ? has(row.role, right) : true}
                                            disabled={!row.configurable ||
                                                locked(row.role, right) ||
                                                busy === row.role}
                                            onchange={() => toggle(row.role, right)}
                                        />
                                        <label for="{row.role}-{right}">
                                            <code class="txt">{right}</code>
                                            {#if row.configurable && locked(row.role, right)}
                                                <i
                                                    class="ri-lock-line txt-hint m-l-5"
                                                    aria-hidden="true"
                                                    title={lockReason(row.role, right)}
                                                ></i>
                                            {/if}
                                        </label>
                                    </div>
                                    <div class="txt-hint txt-sm m-l-25 m-b-10">
                                        {RIGHT_TEXT[right] ?? ""}
                                    </div>
                                {/each}
                            </div>
                        {/each}
                    </div>

                    {#if row.configurable}
                        <div class="flex gap-10 m-b-base">
                            <button
                                type="button"
                                class="btn"
                                disabled={busy === row.role || !dirty(row.role)}
                                onclick={() => save(row.role)}
                            >
                                {#if busy === row.role}
                                    <span class="loader"></span>
                                {/if}
                                <span class="txt">Save {ROLE_LABEL[row.role] ?? row.role}</span>
                            </button>
                            <button
                                type="button"
                                class="btn secondary"
                                disabled={busy === row.role || (!row.customized && isDefault(row.role))}
                                onclick={() => reset(row.role)}
                            >
                                <i class="ri-arrow-go-back-line" aria-hidden="true"></i>
                                <span class="txt">Reset to default</span>
                            </button>
                            {#if dirty(row.role)}
                                <span class="txt-hint txt-sm inline-flex">Unsaved changes</span>
                            {/if}
                        </div>
                    {/if}
                {/each}

                {#if !myRights()}
                    <div class="field-help">
                        You are signed in with a static admin token, which carries every right and
                        has no role. Nothing here applies to it.
                    </div>
                {/if}
            {/if}
        </div>

        <footer class="page-footer">
            <ThemeToggle />
        </footer>
    </div>
</div>
