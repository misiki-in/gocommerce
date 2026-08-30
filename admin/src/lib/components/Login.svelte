<script>
    /**
     * PocketBase's superuser login, structurally: a centred `.wrapper.sm` with
     * the logo above an `.grid` form of `.col-12` rows, the password field
     * paired with an eye toggle in a `.field.addon`, and a full-width
     * `.btn.lg.block.next` at the bottom.
     *
     * The install branch is the same form under a different verb. A fresh
     * database has no operator, and asking someone to sign in with credentials
     * that cannot exist yet is a dead end — so the server is asked first.
     */
    import { base } from "$app/paths";
    import { auth } from "$lib/api.js";
    import { toast } from "$lib/toast.svelte.js";

    let { onauthenticated } = $props();

    let identity = $state("");
    let password = $state("");
    let showPassword = $state(false);
    let submitting = $state(false);

    // null while we are still asking; true once we know an operator exists.
    let installed = $state(null);

    $effect(() => {
        auth.state()
            .then((s) => (installed = !!s.installed))
            // If the probe itself fails the store is unreachable, and a login
            // form is the more useful of the two guesses.
            .catch(() => (installed = true));
    });

    async function submit(e) {
        e.preventDefault();
        if (submitting) return;
        submitting = true;
        try {
            const record = installed
                ? await auth.login(identity, password)
                : await auth.install(identity, password);
            if (!installed) toast.success("Welcome to GoCommerce");
            onauthenticated?.(record);
        } catch (err) {
            toast.error(err.message || "Invalid login credentials.");
        } finally {
            submitting = false;
        }
    }
</script>

<div class="wrapper sm m-auto p-b-base">
    <header class="txt-center m-b-base">
        <img class="main-logo" src="{base}/images/logo.svg" alt="" aria-hidden="true" />
        <h5 class="m-t-10">
            {#if installed === false}Create your first superuser{:else}Superuser login{/if}
        </h5>
    </header>

    {#if installed === null}
        <div class="block txt-center"><span class="loader lg"></span></div>
    {:else}
        <form class="grid auth-with-password-form" onsubmit={submit}>
            {#if !installed}
                <div class="col-12">
                    <div class="content txt-center txt-hint">
                        <small>
                            This store has no operator yet. The account you create here signs in to
                            the panel from now on.
                        </small>
                    </div>
                </div>
            {/if}

            <div class="col-12">
                <div class="field">
                    <label for="login_identity">Email</label>
                    <!-- svelte-ignore a11y_autofocus -->
                    <input
                        id="login_identity"
                        name="identity"
                        type="email"
                        required
                        autofocus
                        autocomplete="username"
                        bind:value={identity}
                    />
                </div>
            </div>

            <div class="col-12">
                <div class="fields">
                    <div class="field">
                        <label for="login_pass">Password</label>
                        <input
                            id="login_pass"
                            name="password"
                            required
                            type={showPassword ? "text" : "password"}
                            autocomplete={installed ? "current-password" : "new-password"}
                            bind:value={password}
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
                            <i class={showPassword ? "ri-eye-off-line" : "ri-eye-line"}></i>
                        </button>
                    </div>
                </div>
                {#if !installed}
                    <div class="link-hint m-t-5"><small>At least 8 characters.</small></div>
                {/if}
            </div>

            <div class="col-12">
                <button class="btn lg block next" class:loading={submitting} disabled={submitting}>
                    <span class="txt">{installed ? "Login" : "Create and sign in"}</span>
                    <i class="ri-arrow-right-line"></i>
                </button>
            </div>
        </form>
    {/if}
</div>
