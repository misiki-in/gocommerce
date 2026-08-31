<script>
    /**
     * Accepting an invitation.
     *
     * The only screen in the panel somebody reaches without an account, so it
     * stands on its own: it says who the invitation is for and what it grants
     * before asking for anything. It never asks for the email — that was
     * decided by whoever sent the link, and an editable field there is only a
     * way to typo yourself out of the invitation.
     */
    import { page } from "$app/state";
    import { base } from "$app/paths";
    import { auth } from "$lib/api.js";
    import { toast } from "$lib/toast.svelte.js";

    const token = $derived(page.params.token);

    let loading = $state(true);
    let invitation = $state(null);
    let problem = $state("");

    let password = $state("");
    let confirm = $state("");
    let showPassword = $state(false);
    let submitting = $state(false);
    let error = $state("");

    $effect(() => {
        load(token);
    });

    async function load(t) {
        loading = true;
        problem = "";
        try {
            invitation = await auth.invitation(t);
        } catch (err) {
            // A dead link is the expected failure here, not an exception: it
            // has been used, revoked, or left too long. The engine's message
            // says which, and that is the part the person needs.
            problem = err.message;
        } finally {
            loading = false;
        }
    }

    async function submit(e) {
        e.preventDefault();
        if (submitting) return;
        if (password !== confirm) {
            error = "The two passwords do not match.";
            return;
        }
        submitting = true;
        error = "";
        try {
            const record = await auth.accept(token, password);
            // Accepting signs you in, so there is nowhere to send them but in.
            // They have typed this password once already and asking them to
            // prove it immediately is how a first impression goes wrong.
            toast.success(`Welcome, ${record.email}`);

            // A whole page load, not `goto`. The shell decides whether you are
            // signed in from an effect that runs once on mount and reads
            // localStorage — which is not reactive, so nothing re-runs it. A
            // client-side navigation therefore lands on the login form holding
            // a session that works, which is the worst of both. Re-mounting is
            // what makes the shell look again.
            window.location.assign(base + "/");
        } catch (err) {
            error = err.message;
        } finally {
            submitting = false;
        }
    }

    /** A right reads better as a sentence than as a dotted identifier. */
    const RIGHT_LABELS = {
        "catalog.read": "See the catalog",
        "catalog.write": "Edit products and categories",
        "inventory.read": "See stock levels",
        "inventory.write": "Adjust stock",
        "discounts.read": "See discounts",
        "discounts.write": "Create and edit discounts",
        "taxes.read": "See tax rates",
        "taxes.write": "Edit tax rates",
        "locations.read": "See locations",
        "locations.write": "Edit locations",
        "orders.read": "See orders",
        "orders.write": "Place, edit and cancel orders",
        "orders.fulfill": "Fulfil and ship orders",
        "orders.refund": "Refund money",
        "customers.read": "See customers",
        "team.read": "See the team",
        "team.write": "Invite and manage the team",
        "roles.write": "Change what each role may do",
        "data.export": "Export the catalog and orders",
        "data.import": "Import the catalog and orders",
    };
</script>

<div class="page">
    <div class="wrapper sm m-auto p-b-base">
        <header class="txt-center m-b-base">
            <img class="main-logo" src="{base}/images/logo.svg" alt="" aria-hidden="true" />
            <h5 class="m-t-10">
                {#if loading}Checking your invitation{:else if problem}This link no longer
                    works{:else}Join as {invitation.email}{/if}
            </h5>
        </header>

        {#if loading}
            <div class="block txt-center"><span class="loader lg"></span></div>
        {:else if problem}
            <div class="content txt-center txt-hint m-b-base">
                <p>{problem}</p>
                <p>Ask whoever invited you to send a new one.</p>
            </div>
            <a href="{base}/" class="btn lg block secondary">
                <span class="txt">Go to the sign-in page</span>
            </a>
        {:else}
            <div class="content txt-center txt-hint m-b-base">
                <small>
                    {invitation.invited_by
                        ? `${invitation.invited_by} invited you`
                        : "You have been invited"}
                    as <strong>{invitation.role}</strong>. Choose a password — nobody else
                    will ever see it.
                </small>
            </div>

            <div class="list m-b-base">
                {#each invitation.rights as right (right)}
                    <div class="list-item">
                        <i class="ri-check-line" aria-hidden="true"></i>
                        <span class="txt">{RIGHT_LABELS[right] ?? right}</span>
                    </div>
                {/each}
            </div>

            <form class="grid" onsubmit={submit}>
                <div class="col-12">
                    <div class="fields">
                        <div class="field required" class:error={!!error}>
                            <label for="invite_pass">Password</label>
                            <!-- svelte-ignore a11y_autofocus -->
                            <input
                                id="invite_pass"
                                required
                                autofocus
                                type={showPassword ? "text" : "password"}
                                autocomplete="new-password"
                                bind:value={password}
                                oninput={() => (error = "")}
                            />
                        </div>
                        <div class="field addon">
                            <button
                                type="button"
                                tabindex="-1"
                                class="btn sm transparent secondary circle"
                                aria-label={showPassword ? "Hide password" : "Show password"}
                                onclick={() => (showPassword = !showPassword)}
                            >
                                <i
                                    class={showPassword ? "ri-eye-off-line" : "ri-eye-line"}
                                    aria-hidden="true"
                                ></i>
                            </button>
                        </div>
                    </div>
                </div>

                <div class="col-12">
                    <div class="field required" class:error={!!error}>
                        <label for="invite_confirm">Password again</label>
                        <input
                            id="invite_confirm"
                            required
                            type={showPassword ? "text" : "password"}
                            autocomplete="new-password"
                            bind:value={confirm}
                            oninput={() => (error = "")}
                        />
                    </div>
                    {#if error}<div class="field-help error">{error}</div>{/if}
                    <div class="field-help">At least 8 characters.</div>
                </div>

                <div class="col-12">
                    <button
                        type="submit"
                        class="btn lg block next"
                        class:loading={submitting}
                        disabled={submitting}
                    >
                        <span class="txt">Join the team</span>
                        <i class="ri-arrow-right-line" aria-hidden="true"></i>
                    </button>
                </div>
            </form>
        {/if}
    </div>
</div>
