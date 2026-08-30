<script>
    /**
     * Managing the people who can sign in to this panel — PocketBase's
     * `_superusers` collection, in the shape this engine needs.
     *
     * The password field is write-only in both directions: the API never sends
     * a hash back, and this form never shows one. Changing a password signs
     * that operator out everywhere, which is stated on the form rather than
     * discovered afterwards.
     */
    import { auth, getRecord } from "$lib/api.js";
    import { formatDate } from "$lib/format.js";
    import { toast } from "$lib/toast.svelte.js";
    import Drawer from "$lib/components/Drawer.svelte";
    import Select from "$lib/components/Select.svelte";
    import Confirm from "$lib/components/Confirm.svelte";
    import SettingsSidebar from "$lib/components/SettingsSidebar.svelte";
    import ThemeToggle from "$lib/components/ThemeToggle.svelte";

    let loading = $state(true);
    let saving = $state(false);
    let superusers = $state([]);
    let invitations = $state([]);

    let inviteOpen = $state(false);
    let invite = $state({ email: "", role: "staff" });
    let inviting = $state(false);
    // The link, held only for as long as the drawer that shows it is open. The
    // engine cannot produce it a second time, so this is the one chance to
    // copy it — and it must not end up anywhere it would outlive that.
    let issued = $state(null);
    let copied = $state(false);

    let editorOpen = $state(false);
    let editing = $state(null); // null = creating
    let form = $state({ email: "", password: "", role: "staff" });

    /*
     * The roles, and what each is for in one line — the picker is where an
     * operator decides how much of the store somebody gets, and a bare list of
     * three words is not enough to decide on. The engine owns the actual
     * rights (rights.go); these are the sentences.
     */
    const ROLES = [
        { value: "owner", label: "Owner — everything, including the team" },
        { value: "manager", label: "Manager — the catalog, orders and refunds" },
        { value: "staff", label: "Staff — sees the shop, moves orders along" },
    ];
    const roleName = (role) => ({ owner: "Owner", manager: "Manager", staff: "Staff" })[role] ?? role;
    let errors = $state({});

    let confirmOpen = $state(false);
    let pendingDelete = $state(null);

    const me = getRecord();

    $effect(() => {
        load();
    });

    async function load() {
        loading = true;
        try {
            const [people, invites] = await Promise.all([auth.list(), auth.invitations()]);
            superusers = people.data;
            invitations = invites.data ?? [];
        } catch (err) {
            toast.error(err);
        } finally {
            loading = false;
        }
    }

    /** Outstanding ones only: accepted invitations are history, and the person
     *  they let in is already in the table above. */
    const outstanding = $derived(invitations.filter((i) => i.status !== "accepted"));

    function openInvite() {
        invite = { email: "", role: "staff" };
        issued = null;
        copied = false;
        errors = {};
        inviteOpen = true;
    }

    async function sendInvite(event) {
        event?.preventDefault();
        if (inviting) return;
        errors = {};
        if (!invite.email.trim()) {
            errors.invite = "An email is required.";
            return;
        }
        inviting = true;
        try {
            const result = await auth.invite(invite.email.trim(), invite.role);
            issued = result.data ?? result;
            await load();
        } catch (err) {
            errors.invite = err.message;
        } finally {
            inviting = false;
        }
    }

    async function copyLink() {
        try {
            await navigator.clipboard.writeText(issued.accept_url);
            copied = true;
        } catch {
            // Clipboard access is refused in plenty of ordinary situations —
            // an insecure origin, a browser setting. The link is on screen and
            // selectable, so say that rather than pretending it worked.
            toast.info("Copy it from the box above");
        }
    }

    async function revokeInvite(inv) {
        try {
            await auth.revokeInvitation(inv.id);
            toast.success(`Invitation to ${inv.email} revoked`);
            await load();
        } catch (err) {
            toast.error(err);
        }
    }

    async function signOutEverywhere(su, event) {
        event.stopPropagation();
        try {
            const result = await auth.revokeSessions(su.id);
            const n = result.revoked ?? 0;
            toast.success(
                n === 0
                    ? `${su.email} was not signed in anywhere`
                    : `Signed ${su.email} out of ${n} ${n === 1 ? "device" : "devices"}`,
            );
        } catch (err) {
            toast.error(err);
        }
    }

/**
     * Changing a role is its own request, and its own confirmation.
     *
     * The engine refuses to leave the store without an owner, so the failure
     * that matters is already handled there — this just carries the reason back
     * and puts the picker where it was.
     */
    async function changeRole(su, role) {
        if (role === su.role) return;
        const was = su.role;
        su.role = role;
        try {
            await auth.setRole(su.id, role);
            toast.success(`${su.email} is now ${roleName(role).toLowerCase()}`);
            // Their own rights just changed: re-read the record so the nav and
            // the buttons match what the engine will now allow.
            if (me && su.id === me.id) {
                await auth.refresh();
                window.location.reload();
            }
        } catch (err) {
            su.role = was;
            superusers = [...superusers];
            toast.error(err);
        }
    }

    function openCreate() {
        editing = null;
        // Staff by default: the least a new person can be given, and the
        // easiest thing to widen once you know what they need.
        form = { email: "", password: "", role: "staff" };
        errors = {};
        editorOpen = true;
    }

    function openEdit(su) {
        editing = su;
        form = { email: su.email, password: "", role: su.role };
        errors = {};
        editorOpen = true;
    }

    async function save(event) {
        event?.preventDefault();
        if (saving) return;

        errors = {};
        if (!form.email.trim()) errors.email = "An email is required.";
        if (!editing && form.password.length < 8) {
            errors.password = "A password of at least 8 characters is required.";
        }
        if (editing && form.password && form.password.length < 8) {
            errors.password = "A password must be at least 8 characters.";
        }
        if (Object.keys(errors).length) return;

        saving = true;
        try {
            if (editing) {
                const body = {};
                if (form.email.trim() !== editing.email) body.email = form.email.trim();
                if (form.password) body.password = form.password;
                if (!Object.keys(body).length) {
                    editorOpen = false;
                    return;
                }
                await auth.update(editing.id, body);
                toast.success(
                    form.password
                        ? "Password changed — their other sessions were signed out"
                        : "Superuser saved",
                );
            } else {
                await auth.create(form.email.trim(), form.password, form.role);
                toast.success("Superuser created");
            }
            editorOpen = false;
            await load();
        } catch (err) {
            toast.error(err);
        } finally {
            saving = false;
        }
    }

    function askDelete(su, event) {
        event.stopPropagation();
        pendingDelete = su;
        confirmOpen = true;
    }

    async function doDelete() {
        try {
            await auth.remove(pendingDelete.id);
            toast.success(`Removed ${pendingDelete.email}`);
            await load();
        } catch (err) {
            toast.error(err);
        }
    }
</script>

<div class="page page-superusers">
    <SettingsSidebar />

    <div class="page-content full-height">
        <header class="page-header">
            <nav class="breadcrumbs">
                <div class="breadcrumb-item">Settings</div>
                <div class="breadcrumb-item">Superusers</div>
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
                <!-- Secondary, because inviting is what you should nearly always
                     do: choosing somebody else's password means two people know
                     it from the moment it exists. Creating stays for the cases
                     invitations cannot serve — a shared account, or somebody
                     with no reachable inbox. -->
                <button type="button" class="btn secondary" onclick={openCreate}>
                    <i class="ri-add-line" aria-hidden="true"></i>
                    <span class="txt">Create directly</span>
                </button>
                <button type="button" class="btn" onclick={openInvite}>
                    <i class="ri-mail-send-line" aria-hidden="true"></i>
                    <span class="txt">Invite</span>
                </button>
            </div>
        </header>

        <div class="page-table-wrapper">
            <table class="table responsive-table">
                <thead class="sticky">
                    <tr>
                        <th class="col-field-name-id">Email</th>
                        <th class="col-field-type-select">Role</th>
                        <th class="col-field-type-date">Created</th>
                        <th class="col-field-type-date">Updated</th>
                        <th class="col-meta min-width"></th>
                    </tr>
                </thead>
                <tbody>
                    {#each superusers as su (su.id)}
                        <tr class="handle" onclick={() => openEdit(su)}>
                            <td class="col-field-name-id" data-name="Email">
                                <span class="txt-bold">{su.email}</span>
                                {#if me && su.id === me.id}
                                    <span class="label sm">you</span>
                                {/if}
                            </td>
                            <!-- The picker is the control, not a link into the
                                 editor: a role is one choice from three, and
                                 opening a drawer to make it would be a step for
                                 nothing. stopPropagation because the row itself
                                 opens the editor. -->
                            <td
                                class="col-field-type-select"
                                data-name="Role"
                                onclick={(e) => e.stopPropagation()}
                            >
                                <div class="field">
                                    <Select
                                        id="role-{su.id}"
                                        ariaLabel="Role for {su.email}"
                                        value={su.role}
                                        onchange={(role) => changeRole(su, role)}
                                        options={ROLES}
                                    />
                                </div>
                            </td>
                            <td class="col-field-type-date txt-hint txt-sm" data-name="Created">
                                {formatDate(su.created_at)}
                            </td>
                            <td class="col-field-type-date txt-hint txt-sm" data-name="Updated">
                                {formatDate(su.updated_at)}
                            </td>
                            <td class="col-meta min-width">
                                <button
                                    type="button"
                                    class="btn circle sm transparent secondary"
                                    aria-label="Sign {su.email} out everywhere"
                                    title="Sign out everywhere"
                                    onclick={(e) => signOutEverywhere(su, e)}
                                >
                                    <i class="ri-logout-circle-line" aria-hidden="true"></i>
                                </button>
                                {#if superusers.length > 1}
                                    <button
                                        type="button"
                                        class="btn circle sm transparent secondary row-delete"
                                        aria-label="Remove {su.email}"
                                        title="Remove"
                                        onclick={(e) => askDelete(su, e)}
                                    >
                                        <i class="ri-delete-bin-7-line" aria-hidden="true"></i>
                                    </button>
                                {/if}
                                <i class="ri-arrow-right-s-line" aria-hidden="true"></i>
                            </td>
                        </tr>
                    {/each}

                    {#if loading && !superusers.length}
                        {#each Array(3) as _, i (i)}
                            <tr><td colspan="4"><span class="skeleton-loader"></span></td></tr>
                        {/each}
                    {/if}
                </tbody>
            </table>
        </div>

        {#if outstanding.length}
            <!-- Below the team rather than beside it: these are people who are
                 not here yet, and mixing them into the list would say they are. -->
            <div class="m-t-base">
                <div class="section-title">Invited, not yet joined</div>
                <div class="list">
                    {#each outstanding as inv (inv.id)}
                        <div class="list-item">
                            <i class="ri-mail-line" aria-hidden="true"></i>
                            <span class="txt">{inv.email}</span>
                            <span class="label">{roleName(inv.role)}</span>
                            {#if inv.status === "expired"}
                                <span class="label warning">expired</span>
                            {/if}
                            <div class="flex-fill"></div>
                            <span class="txt-hint txt-sm">
                                {inv.invited_by ? `invited by ${inv.invited_by}` : "invited"}
                                · {inv.status === "expired" ? "expired" : "expires"}
                                {formatDate(inv.expires_at)}
                            </span>
                            <button
                                type="button"
                                class="btn circle sm transparent secondary"
                                aria-label="Revoke the invitation to {inv.email}"
                                title="Revoke"
                                onclick={() => revokeInvite(inv)}
                            >
                                <i class="ri-close-line" aria-hidden="true"></i>
                            </button>
                        </div>
                    {/each}
                </div>
            </div>
        {/if}

        <footer class="page-footer">
            <span class="txt">
                {superusers.length}
                {superusers.length === 1 ? "superuser" : "superusers"}{#if outstanding.length},
                    {outstanding.length} invited{/if}
            </span>
            <ThemeToggle />
        </footer>
    </div>
</div>

<Drawer
    open={editorOpen}
    title={editing ? editing.email : "New superuser"}
    size="sm"
    onclose={() => (editorOpen = false)}
>
    <form id="superuser-form" onsubmit={save}>
        <div class="field required" class:error={!!errors.email}>
            <label for="su_email">Email</label>
            <input id="su_email" type="email" autocomplete="off" bind:value={form.email} />
        </div>
        {#if errors.email}<div class="field-help error">{errors.email}</div>{/if}

        <div class="field m-t-sm" class:required={!editing} class:error={!!errors.password}>
            <label for="su_password">
                {editing ? "New password" : "Password"}
            </label>
            <input
                id="su_password"
                type="password"
                autocomplete="new-password"
                placeholder={editing ? "Leave empty to keep the current one" : ""}
                bind:value={form.password}
            />
        </div>
        {#if errors.password}
            <div class="field-help error">{errors.password}</div>
        {:else}
            <div class="field-help">
                At least 8 characters.{#if editing}
                    Changing it signs this operator out of every other session.{/if}

        {#if !editing}
            <div class="field m-t-sm">
                <label for="su-role">Role</label>
                <Select id="su-role" bind:value={form.role} options={ROLES} />
            </div>
            <div class="field-help">
                What they may do. It can be changed later from the list, and the store always
                keeps at least one owner.
            </div>
        {/if}
            </div>
        {/if}
    </form>

    {#snippet footer()}
        <button type="button" class="btn transparent m-r-auto" onclick={() => (editorOpen = false)}>
            <span class="txt">Cancel</span>
        </button>
        <button
            type="submit"
            form="superuser-form"
            class="btn"
            class:loading={saving}
            disabled={saving}
        >
            <span class="txt">{editing ? "Save changes" : "Create superuser"}</span>
        </button>
    {/snippet}
</Drawer>

<Drawer
    open={inviteOpen}
    title={issued ? "Send this link" : "Invite somebody"}
    size="sm"
    onclose={() => (inviteOpen = false)}
>
    {#if issued}
        <!-- The link exists in this response and nowhere else: the store kept
             only its hash. Saying so is what stops somebody closing the drawer
             expecting to find it again on the list. -->
        <div class="alert info m-b-base">
            <p>
                <i class="ri-information-line" aria-hidden="true"></i>
                This is the only time this link is shown. Close this and it cannot be
                recovered — you would have to invite {issued.email} again.
            </p>
        </div>

        <div class="field">
            <label for="invite-link">Link for {issued.email}</label>
            <input id="invite-link" type="text" readonly value={issued.accept_url} />
        </div>
        <div class="field-help">
            They choose their own password when they open it. It works until {formatDate(
                issued.expires_at,
            )}.
        </div>
    {:else}
        <form id="invite-form" onsubmit={sendInvite}>
            <div class="field required" class:error={!!errors.invite}>
                <label for="invite-email">Email</label>
                <!-- svelte-ignore a11y_autofocus -->
                <input
                    id="invite-email"
                    type="email"
                    autocomplete="off"
                    autofocus
                    bind:value={invite.email}
                    oninput={() => (errors = {})}
                />
            </div>
            {#if errors.invite}<div class="field-help error">{errors.invite}</div>{/if}

            <div class="field m-t-sm">
                <label for="invite-role">Role</label>
                <Select id="invite-role" bind:value={invite.role} options={ROLES} />
            </div>
            <div class="field-help">
                They pick their own password, so nobody else ever knows it. You get a link to
                send them.
            </div>
        </form>
    {/if}

    {#snippet footer()}
        {#if issued}
            <button type="button" class="btn transparent m-r-auto" onclick={() => (inviteOpen = false)}>
                <span class="txt">Done</span>
            </button>
            <button type="button" class="btn" onclick={copyLink}>
                <i class={copied ? "ri-check-line" : "ri-file-copy-line"} aria-hidden="true"></i>
                <span class="txt">{copied ? "Copied" : "Copy link"}</span>
            </button>
        {:else}
            <button type="button" class="btn transparent m-r-auto" onclick={() => (inviteOpen = false)}>
                <span class="txt">Cancel</span>
            </button>
            <button
                type="submit"
                form="invite-form"
                class="btn"
                class:loading={inviting}
                disabled={inviting}
            >
                <span class="txt">Create invitation</span>
            </button>
        {/if}
    {/snippet}
</Drawer>

<Confirm
    bind:open={confirmOpen}
    title="Remove this superuser?"
    message={pendingDelete
        ? `${pendingDelete.email} will lose access to this panel immediately, and every session they hold ends with them.`
        : ""}
    confirmLabel="Remove"
    danger
    onconfirm={doDelete}
/>
