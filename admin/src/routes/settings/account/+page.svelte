<script>
    /**
     * Your own account.
     *
     * This screen exists so that changing your own password is not gated behind
     * settings.write — which is also the right to change everybody's role. Put
     * it there and a staff member who suspects their password is known has to
     * ask an owner to choose a new one for them, which is the exact practice
     * invitations exist to end.
     *
     * The current password is asked for either way: a session left open on an
     * unlocked laptop should not be enough to take the account over.
     */
    import { auth, getRecord } from "$lib/api.js";
    import { formatDate } from "$lib/format.js";
    import { toast } from "$lib/toast.svelte.js";
    import Confirm from "$lib/components/Confirm.svelte";
    import SettingsSidebar from "$lib/components/SettingsSidebar.svelte";
    import ThemeToggle from "$lib/components/ThemeToggle.svelte";

    let loading = $state(true);
    let me = $state(null);
    let sessions = $state(0);
    let newestSession = $state(null);
    /* A static admin token has no account to show. Saying so beats an empty
       form that cannot be submitted. */
    let tokenOnly = $state(false);

    let form = $state({ current_password: "", email: "", password: "", confirm: "" });
    let saving = $state(false);
    let errors = $state({});

    let confirmOpen = $state(false);

    const RIGHT_LABELS = {
        "catalog.read": "See the catalog",
        "catalog.write": "Edit products and categories",
        "orders.read": "See orders",
        "orders.write": "Fulfil and edit orders",
        "orders.refund": "Refund money",
        "inventory.write": "Adjust stock",
        "customers.read": "See customers",
        "settings.write": "Change settings and the team",
    };

    $effect(() => {
        load();
    });

    async function load() {
        loading = true;
        try {
            const result = await auth.me();
            me = result.superuser;
            sessions = result.sessions ?? 0;
            newestSession = result.newest_session ?? null;
            form.email = me.email;
        } catch (err) {
            if (err.status === 403) {
                tokenOnly = true;
            } else {
                toast.error(err);
            }
        } finally {
            loading = false;
        }
    }

    async function save(event) {
        event?.preventDefault();
        if (saving) return;

        errors = {};
        if (!form.current_password) {
            errors.current = "Confirm your current password to make a change.";
        }
        if (form.password && form.password.length < 8) {
            errors.password = "A password must be at least 8 characters.";
        }
        if (form.password && form.password !== form.confirm) {
            errors.password = "The two passwords do not match.";
        }
        const emailChanged = form.email.trim() && form.email.trim() !== me.email;
        if (!emailChanged && !form.password) {
            errors.password = "Nothing to change.";
        }
        if (Object.keys(errors).length) return;

        saving = true;
        try {
            const body = { current_password: form.current_password };
            if (emailChanged) body.email = form.email.trim();
            if (form.password) body.password = form.password;

            await auth.updateMe(body);
            // The header shows the email, so the stored record has to catch up.
            await auth.refresh();
            toast.success(
                form.password
                    ? "Password changed — your other devices were signed out"
                    : "Account updated",
            );
            form.current_password = "";
            form.password = "";
            form.confirm = "";
            await load();
        } catch (err) {
            errors.current = err.message;
        } finally {
            saving = false;
        }
    }

    async function signOutEverywhere() {
        try {
            await auth.revokeMySessions();
            // Including this browser, which is what "everywhere" has to mean.
            // A reload lands on the login form because the token no longer
            // resolves — no special case needed.
            toast.info("Signed out everywhere");
            window.location.reload();
        } catch (err) {
            toast.error(err);
        }
    }
</script>

<div class="page page-account">
    <SettingsSidebar />

    <div class="page-content full-height">
        <header class="page-header">
            <nav class="breadcrumbs">
                <div class="breadcrumb-item">Settings</div>
                <div class="breadcrumb-item">Your account</div>
            </nav>
        </header>

        {#if loading}
            <div class="block txt-center p-base"><span class="loader lg"></span></div>
        {:else if tokenOnly}
            <div class="alert info">
                <p>
                    <i class="ri-information-line" aria-hidden="true"></i>
                    You are signed in with the store's admin token. It is a credential rather
                    than a person, so there is no account here to change — sign in with an
                    email and password to manage one.
                </p>
            </div>
        {:else}
            <div class="section-title">Who you are</div>
            <div class="list m-b-base">
                <div class="list-item">
                    <span class="txt-hint">Email</span>
                    <div class="flex-fill"></div>
                    <span class="txt">{me.email}</span>
                </div>
                <div class="list-item">
                    <span class="txt-hint">Role</span>
                    <div class="flex-fill"></div>
                    <span class="label">{me.role}</span>
                </div>
                <div class="list-item">
                    <span class="txt-hint">Signed in on</span>
                    <div class="flex-fill"></div>
                    <span class="txt">
                        {sessions}
                        {sessions === 1 ? "device" : "devices"}
                        {#if newestSession}
                            <span class="txt-hint txt-sm">
                                · most recent {formatDate(newestSession)}
                            </span>
                        {/if}
                    </span>
                </div>
            </div>

            <div class="section-title">What you may do</div>
            <div class="list m-b-base">
                {#each me.rights ?? [] as right (right)}
                    <div class="list-item">
                        <i class="ri-check-line" aria-hidden="true"></i>
                        <span class="txt">{RIGHT_LABELS[right] ?? right}</span>
                    </div>
                {/each}
                {#if !(me.rights ?? []).length}
                    <div class="list-item txt-hint">
                        Nothing yet — ask an owner to give you a role.
                    </div>
                {/if}
            </div>

            <div class="section-title">Change your details</div>
            <form onsubmit={save} class="m-b-base">
                <div class="field required" class:error={!!errors.current}>
                    <label for="acc-current">Current password</label>
                    <input
                        id="acc-current"
                        type="password"
                        autocomplete="current-password"
                        bind:value={form.current_password}
                        oninput={() => (errors = {})}
                    />
                </div>
                {#if errors.current}
                    <div class="field-help error">{errors.current}</div>
                {:else}
                    <div class="field-help">
                        Asked for either change, so that an unlocked laptop is not enough to
                        take your account over.
                    </div>
                {/if}

                <div class="field m-t-base">
                    <label for="acc-email">Email</label>
                    <input id="acc-email" type="email" bind:value={form.email} />
                </div>

                <div class="fields m-t-sm">
                    <div class="field" class:error={!!errors.password}>
                        <label for="acc-password">New password</label>
                        <input
                            id="acc-password"
                            type="password"
                            autocomplete="new-password"
                            placeholder="Leave empty to keep it"
                            bind:value={form.password}
                        />
                    </div>
                    <div class="delimiter"></div>
                    <div class="field" class:error={!!errors.password}>
                        <label for="acc-confirm">Again</label>
                        <input
                            id="acc-confirm"
                            type="password"
                            autocomplete="new-password"
                            bind:value={form.confirm}
                        />
                    </div>
                </div>
                {#if errors.password}
                    <div class="field-help error">{errors.password}</div>
                {:else}
                    <div class="field-help">
                        At least 8 characters. Changing it signs you out of every other device
                        and keeps this one.
                    </div>
                {/if}

                <button
                    type="submit"
                    class="btn m-t-base"
                    class:loading={saving}
                    disabled={saving}
                >
                    <span class="txt">Save changes</span>
                </button>
            </form>

            <div class="section-title">Lost a device?</div>
            <div class="content txt-hint m-b-sm">
                Ends every session including this one, so you will be asked to sign in again.
            </div>
            <button type="button" class="btn secondary" onclick={() => (confirmOpen = true)}>
                <i class="ri-logout-circle-line" aria-hidden="true"></i>
                <span class="txt">Sign out everywhere</span>
            </button>
        {/if}

        <footer class="page-footer">
            <span class="txt">
                {#if me}Signed in as {me.email}{/if}
            </span>
            <ThemeToggle />
        </footer>
    </div>
</div>

<Confirm
    bind:open={confirmOpen}
    title="Sign out everywhere?"
    message="Every device signed in as you will be signed out, including this one."
    confirmLabel="Sign out everywhere"
    onconfirm={signOutEverywhere}
/>
