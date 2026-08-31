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
     * It is drawn as a matrix — rights down the side, roles across — because
     * that is the question the screen exists to answer. "May staff refund?" and
     * "what has manager got that staff has not?" are each one glance along a
     * line here; as three stacked lists they were a screenful apart and could
     * only be compared from memory.
     *
     * Both axes come from the API, so a right added to the engine appears here
     * without this file changing. The groups below are layout only, and
     * anything they do not name falls through to "Other" rather than
     * disappearing.
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
    /*
     * One line per right, in the store's own words. The API sends the names; a
     * row labelled `orders.refund` and nothing else asks the person granting it
     * to already know what it covers.
     */
    const RIGHT_TEXT = {
        "catalog.read": "Products, variants, categories, collections, media",
        "catalog.write": "Editing any of them",
        "inventory.read": "Stock levels and the low-stock report",
        "inventory.write": "Stock takes, adjustments and transfers",
        "discounts.read": "Discount codes and what they take off",
        "discounts.write": "Creating, editing and ending discounts",
        "taxes.read": "The tax rates orders are charged at",
        "taxes.write": "Changing what every future order collects",
        "locations.read": "The places stock lives",
        "locations.write": "Opening, closing and choosing the default",
        "orders.read": "Orders and what customers bought",
        "orders.write": "Placing, editing, cancelling, settling payment",
        "orders.fulfill": "Fulfilling and shipping",
        "orders.refund": "Sending money back out of the store",
        "customers.read": "Orders grouped by who placed them — personal data",
        "team.read": "Who is on the team, and who has been invited",
        "team.write": "Inviting, removing and changing roles",
        "roles.write": "This screen — what each role may do",
        "data.export": "The catalog or every order, as a file",
        "data.import": "Changing prices and stock in bulk, from a file",
    };

    const GROUPS = [
        { title: "Catalog", rights: ["catalog.read", "catalog.write"] },
        { title: "Inventory", rights: ["inventory.read", "inventory.write"] },
        { title: "Discounts", rights: ["discounts.read", "discounts.write"] },
        { title: "Tax", rights: ["taxes.read", "taxes.write"] },
        { title: "Locations", rights: ["locations.read", "locations.write"] },
        {
            title: "Orders",
            rights: ["orders.read", "orders.write", "orders.fulfill", "orders.refund"],
        },
        { title: "Customers", rights: ["customers.read"] },
        { title: "Team and access", rights: ["team.read", "team.write", "roles.write"] },
        { title: "Data", rights: ["data.export", "data.import"] },
    ];

    /** The groups, filtered to what this engine actually has, plus anything it
     *  has that the list above never heard of. */
    const grouped = $derived.by(() => {
        const all = matrix?.all_rights ?? [];
        const named = new Set(GROUPS.flatMap((g) => g.rights));
        const out = GROUPS.map((g) => ({
            title: g.title,
            rights: g.rights.filter((r) => all.includes(r)),
        })).filter((g) => g.rights.length);
        const rest = all.filter((r) => !named.has(r));
        return rest.length ? [...out, { title: "Other", rights: rest }] : out;
    });

    $effect(() => {
        load();
    });

    async function load() {
        loading = true;
        try {
            matrix = unwrap(await rolesApi.matrix());
            draft = {};
            for (const row of matrix.roles) draft[row.role] = [...row.rights];
        } catch (err) {
            toast.error(err);
        } finally {
            loading = false;
        }
    }

    const unwrap = (result) => result.data ?? result;
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
        required.includes(right) || (role === me?.role && right === "roles.write");

    const lockReason = (role, right) =>
        required.includes(right)
            ? "Every role keeps this: a role without it can sign in and see nothing."
            : "This is your own role — removing this would lock you out of this screen.";

    function toggle(role, right) {
        if (locked(role, right)) return;
        const current = draft[role] ?? [];
        draft[role] = current.includes(right)
            ? current.filter((r) => r !== right)
            : [...current, right];
    }

    const same = (a, b) => {
        if (a.length !== b.length) return false;
        const x = [...a].sort();
        const y = [...b].sort();
        return x.every((v, i) => v === y[i]);
    };
    const dirty = (role) => !same(draft[role] ?? [], rowFor(role)?.rights ?? []);

    /** Whether the draft already matches the engine's defaults, which is what
     *  "reset" would produce — so the button has nothing left to offer. */
    const isDefault = (role) => same(draft[role] ?? [], rowFor(role)?.default ?? []);

    async function save(role) {
        if (busy) return;
        busy = role;
        try {
            applySaved(unwrap(await rolesApi.save(role, draft[role] ?? [])));
            toast.success("Saved the " + (ROLE_LABEL[role] ?? role) + " role");
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
            applySaved(unwrap(await rolesApi.reset(role)));
            toast.success((ROLE_LABEL[role] ?? role) + " is back on the defaults");
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
     * is a nav offering a screen the engine will refuse — so re-read it.
     */
    async function refreshMeIfAffected(role) {
        if (me?.role !== role) return;
        try {
            await auth.refresh();
        } catch {
            /* the next request corrects it; nothing here is worth a toast */
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

        {#if loading}
            <div class="block txt-center"><span class="loader lg"></span></div>
        {:else if matrix}
            <div class="roles-intro field-help">
                What each role may do in this store. A change applies on the next request somebody
                makes — nobody has to sign out. A role left on its defaults keeps tracking them, so
                a right added to it in a later release arrives on its own. Owner is fixed and
                always carries every right, so a store that has narrowed itself too far still has
                a way back.
            </div>

            <div class="page-table-wrapper">
                <table class="table roles-table">
                    <thead class="sticky">
                        <tr>
                            <th class="col-right">Right</th>
                            {#each matrix.roles as row (row.role)}
                                <th class="col-role" class:mine={row.role === me?.role}>
                                    <span class="role-name">
                                        {ROLE_LABEL[row.role] ?? row.role}
                                        {#if row.role === me?.role}
                                            <span class="label sm">you</span>
                                        {/if}
                                    </span>
                                    <span class="role-state">
                                        {#if !row.configurable}
                                            <i class="ri-lock-line" aria-hidden="true"></i>
                                            <span>fixed</span>
                                        {:else if row.customized}
                                            <span class="label warning">customised</span>
                                        {:else}
                                            <span>using defaults</span>
                                        {/if}
                                    </span>
                                    <span class="role-count">
                                        {(draft[row.role] ?? []).length} of {matrix.all_rights.length}
                                    </span>
                                    {#if row.configurable}
                                        <span class="role-actions">
                                            <button
                                                type="button"
                                                class="btn sm"
                                                disabled={busy === row.role || !dirty(row.role)}
                                                onclick={() => save(row.role)}
                                            >
                                                <span class="txt">Save</span>
                                            </button>
                                            <button
                                                type="button"
                                                class="btn sm transparent secondary"
                                                disabled={busy === row.role ||
                                                    (!row.customized && isDefault(row.role))}
                                                onclick={() => reset(row.role)}
                                            >
                                                <span class="txt">Reset</span>
                                            </button>
                                        </span>
                                        {#if dirty(row.role)}
                                            <span class="role-dirty">unsaved</span>
                                        {/if}
                                    {/if}
                                </th>
                            {/each}
                        </tr>
                    </thead>

                    <tbody>
                        {#each grouped as group (group.title)}
                            <tr class="group-row">
                                <th colspan={matrix.roles.length + 1} scope="colgroup">
                                    {group.title}
                                </th>
                            </tr>
                            {#each group.rights as right (right)}
                                <tr>
                                    <td class="col-right">
                                        <code class="right-name">{right}</code>
                                        <span class="right-help">{RIGHT_TEXT[right] ?? ""}</span>
                                    </td>
                                    {#each matrix.roles as row (row.role)}
                                        <td
                                            class="col-role"
                                            class:mine={row.role === me?.role}
                                            title={row.configurable && locked(row.role, right)
                                                ? lockReason(row.role, right)
                                                : null}
                                        >
                                            <div class="field">
                                                <input
                                                    type="checkbox"
                                                    id="{row.role}-{right}"
                                                    checked={row.configurable
                                                        ? has(row.role, right)
                                                        : true}
                                                    disabled={!row.configurable ||
                                                        locked(row.role, right) ||
                                                        busy === row.role}
                                                    onchange={() => toggle(row.role, right)}
                                                />
                                                <!-- The label is the visible control: PocketBase
                                                     hides the box and draws it here, so the text
                                                     that names the cell is for a screen reader. -->
                                                <label for="{row.role}-{right}">
                                                    <span class="cell-name">
                                                        {right} for {ROLE_LABEL[row.role] ?? row.role}
                                                    </span>
                                                </label>
                                            </div>
                                        </td>
                                    {/each}
                                </tr>
                            {/each}
                        {/each}
                    </tbody>

                </table>
            </div>

            {#if !myRights()}
                <div class="roles-intro field-help">
                    You are signed in with a static admin token, which carries every right and has
                    no role. Nothing here applies to it.
                </div>
            {/if}
        {/if}

        <footer class="page-footer">
            <ThemeToggle />
        </footer>
    </div>
</div>
