<script>
    /**
     * PocketBase's settings layout: a `.page-sidebar` of nav groups beside a
     * `.wrapper` of fields.
     *
     * Every field here is read-only, and deliberately so — a store's currency,
     * language and installed payment providers are decisions the binary was
     * started with. Rendering them as disabled fields rather than as prose
     * says that plainly: this is where the setting lives, and it is not
     * something to change from a browser while orders are in flight.
     */
    import { api } from "$lib/api.js";
    import { toast } from "$lib/toast.svelte.js";
    import SettingsSidebar from "$lib/components/SettingsSidebar.svelte";

    import ThemeToggle from "$lib/components/ThemeToggle.svelte";
    let store = $state(null);
    let methods = $state(null);
    let loading = $state(true);

    $effect(() => {
        load();
    });

    async function load() {
        loading = true;
        try {
            const [ready, checkout] = await Promise.all([
                api.get("/health/ready", { admin: false }),
                api.get("/api/checkout", { admin: false }),
            ]);
            store = ready;
            methods = checkout;
        } catch (err) {
            toast.error(err);
        } finally {
            loading = false;
        }
    }
</script>

<div class="page page-settings">
    <SettingsSidebar />

    <div class="page-content full-height">
        <header class="page-header">
            <nav class="breadcrumbs">
                <div class="breadcrumb-item">Settings</div>
                <div class="breadcrumb-item">Store</div>
            </nav>
        </header>

        <div class="wrapper m-b-base">
            {#if loading}
                <div class="block txt-center"><span class="loader lg"></span></div>
            {:else}
                <div class="grid">
                    <div class="col-md-4">
                        <div class="field readonly">
                            <label for="currency">Settlement currency</label>
                            <input id="currency" type="text" readonly value={store?.currency ?? ""} />
                        </div>
                    </div>
                    <div class="col-md-4">
                        <div class="field readonly">
                            <label for="language">Default language</label>
                            <input id="language" type="text" readonly value={store?.language ?? ""} />
                        </div>
                    </div>
                    <div class="col-md-4">
                        <div class="field readonly">
                            <label for="version">Engine version</label>
                            <input id="version" type="text" readonly value={store?.version ?? ""} />
                        </div>
                    </div>

                    <div class="col-12">
                        <div class="field-help">
                            These come from the <code>Config</code> the binary was started with.
                            Money is stored in minor units alongside its code, which is what lets
                            any currency work without an engine change — and what makes changing
                            one mid-flight a migration rather than a setting.
                        </div>
                    </div>

                    <div class="col-12">
                        <h6 class="section-title">
                            <i class="ri-bank-card-line" aria-hidden="true"></i>
                            Installed capabilities
                        </h6>
                        <div class="flex flex-wrap gap-5 m-b-10">
                            {#each methods?.payment_methods ?? [] as code (code)}
                                <span class="label info">{code}</span>
                            {/each}
                        </div>
                        <div class="field-help">
                            Each one is a Go module wired into <code>main()</code>. Cash on delivery
                            is built in because it needs no third party; adding Stripe is one import
                            and one argument, and changes no engine code.
                        </div>
                    </div>

                    <div class="col-12">
                        <h6 class="section-title">
                            <i class="ri-code-s-slash-line" aria-hidden="true"></i>
                            API
                        </h6>
                        <div class="field-help m-b-sm">
                            This panel is a client of the same API as anything else — it has no
                            private endpoints. Everything you can do here, you can do with curl.
                        </div>
                        <div class="flex gap-10">
                            <a class="btn secondary" href="/docs" target="_blank" rel="noreferrer">
                                <i class="ri-book-open-line" aria-hidden="true"></i>
                                <span class="txt">Browse the API</span>
                            </a>
                            <a class="btn transparent secondary" href="/doc" target="_blank" rel="noreferrer">
                                <i class="ri-file-code-line" aria-hidden="true"></i>
                                <span class="txt">OpenAPI document</span>
                            </a>
                        </div>
                    </div>
                </div>
            {/if}
        </div>

        <footer class="page-footer">
            <span class="txt">GoCommerce {store?.version ?? ""}</span>
            <ThemeToggle />
        </footer>
    </div>
</div>
