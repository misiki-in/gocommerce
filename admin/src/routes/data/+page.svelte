<script>
    import { getToken, request } from "$lib/api.js";
    import { toast } from "$lib/toast.svelte.js";
    import SettingsSidebar from "$lib/components/SettingsSidebar.svelte";
    import Select from "$lib/components/Select.svelte";
    import ThemeToggle from "$lib/components/ThemeToggle.svelte";

    let importing = $state(false);
    let dryRun = $state(true);
    let fireEvents = $state(false);
    let kind = $state("products");
    let csv = $state("");
    let result = $state(null);
    let fileInput;

    async function download(path, filename) {
        try {
            // A fetch rather than a plain link, because the export needs the
            // admin token and a link cannot carry a header.
            const response = await fetch(path, {
                headers: { Authorization: "Bearer " + getToken() },
            });
            if (!response.ok) {
                toast.danger(`Export failed: ${response.status}`);
                return;
            }
            const blob = await response.blob();
            const url = URL.createObjectURL(blob);
            const a = document.createElement("a");
            a.href = url;
            a.download = filename;
            a.click();
            URL.revokeObjectURL(url);
            toast.success("Export downloaded");
        } catch (err) {
            toast.error(err);
        }
    }

    function pickFile() {
        fileInput?.click();
    }

    async function onFile(event) {
        const file = event.target.files?.[0];
        if (!file) return;
        csv = await file.text();
        toast.info(`Loaded ${file.name}`);
        event.target.value = "";
    }

    async function runImport() {
        if (!csv.trim() || importing) return;
        importing = true;
        result = null;
        try {
            const params = new URLSearchParams();
            if (dryRun) params.set("dry_run", "1");
            if (kind === "orders" && fireEvents) params.set("fire_events", "1");
            const suffix = params.toString() ? "?" + params.toString() : "";

            result = await request("POST", `/api/admin/import/${kind}${suffix}`, {
                body: csv,
                headers: { "Content-Type": "text/csv" },
            });

            if (result.errors?.length) {
                toast.warning(`${result.errors.length} row(s) had problems`);
            } else if (dryRun) {
                toast.success("Dry run looks good — nothing was written");
            } else {
                toast.success(`Imported: ${result.created} created, ${result.updated} updated`);
            }
        } catch (err) {
            toast.error(err);
        } finally {
            importing = false;
        }
    }
</script>

<div class="page page-data">
    <SettingsSidebar />

    <div class="page-content">
        <header class="page-header">
            <nav class="breadcrumbs">
                <div>Settings</div>
                <div>Import / export</div>
            </nav>
        </header>

        <div class="wrapper m-b-base">
            <h6 class="section-title">
                <i class="ri-download-2-line" aria-hidden="true"></i>
                Export
            </h6>

            <div class="field-help m-b-sm">
                One row per variant, and one row per order line — the shape a spreadsheet and
                an accountant actually want. Cells that begin with <code>=</code>,
                <code>+</code>, <code>-</code> or <code>@</code> are escaped so opening the
                file cannot run them; importing strips the escape again, so the round trip is
                lossless.
            </div>

            <div class="flex gap-10">
                <button
                    type="button"
                    class="btn secondary"
                    onclick={() => download("/api/admin/export/admin-products", "products.csv")}
                >
                    <i class="ri-price-tag-3-line" aria-hidden="true"></i>
                    <span class="txt">Products CSV</span>
                </button>
                <button
                    type="button"
                    class="btn secondary"
                    onclick={() => download("/api/admin/export/admin-orders", "orders.csv")}
                >
                    <i class="ri-shopping-bag-3-line" aria-hidden="true"></i>
                    <span class="txt">Orders CSV</span>
                </button>
            </div>

            <h6 class="section-title">
                <i class="ri-upload-2-line" aria-hidden="true"></i>
                Import
            </h6>

            <div class="field">
                <label for="kind">What is in the file</label>
                <Select
                    id="kind"
                    bind:value={kind}
                    options={[
                        { value: "products", label: "Products and variants" },
                        { value: "orders", label: "Historical orders" },
                    ]}
                />
            </div>

            <div class="flex gap-20 flex-wrap m-t-base">
                <div class="field">
                    <input type="checkbox" id="dry-run" class="switch" bind:checked={dryRun} />
                    <label for="dry-run">Dry run</label>
                </div>
                {#if kind === "orders"}
                    <div class="field">
                        <input
                            type="checkbox"
                            id="fire-events"
                            class="switch"
                            bind:checked={fireEvents}
                        />
                        <label for="fire-events">Fire events</label>
                    </div>
                {/if}
            </div>

            {#if dryRun}
                <div class="alert info m-t-base">
                    <p>
                        A dry run validates the whole file and rolls back, reporting what it
                        <em>would</em> have done. Worth doing first, always.
                    </p>
                </div>
            {/if}

            {#if kind === "orders" && fireEvents}
                <div class="alert warning m-t-base">
                    <p>
                        With events on, every imported order announces itself — which means
                        confirmation emails to people who bought something a year ago. Leave
                        this off for a migration.
                    </p>
                </div>
            {/if}

            <div class="field m-t-base">
                <label for="csv">CSV</label>
                <textarea
                    id="csv"
                    class="txt-code"
                    rows="8"
                    placeholder="Paste CSV here, or choose a file"
                    bind:value={csv}
                ></textarea>
            </div>

            <input
                type="file"
                accept=".csv,text/csv"
                bind:this={fileInput}
                onchange={onFile}
                hidden
            />

            <div class="flex gap-10 m-t-base">
                <button type="button" class="btn secondary" onclick={pickFile}>
                    <i class="ri-folder-open-line" aria-hidden="true"></i>
                    <span class="txt">Choose a file…</span>
                </button>
                <div class="flex-fill"></div>
                {#if csv}
                    <button
                        type="button"
                        class="btn transparent secondary"
                        onclick={() => (csv = "")}
                    >
                        <span class="txt">Clear</span>
                    </button>
                {/if}
                <button
                    type="button"
                    class="btn expanded"
                    class:loading={importing}
                    disabled={!csv.trim() || importing}
                    onclick={runImport}
                >
                    <span class="txt">{dryRun ? "Dry run" : "Import"}</span>
                </button>
            </div>

            {#if result}
                <h6 class="section-title">
                    <i
                        class={result.errors?.length
                            ? "ri-error-warning-line"
                            : "ri-checkbox-circle-line"}
                        aria-hidden="true"
                    ></i>
                    {result.dry_run ? "Dry run result" : "Import result"}
                    <span class="label">{result.duration}</span>
                </h6>

                <div class="grid">
                    <div class="col-4">
                        <div class="stat-card">
                            <span class="stat-label">Created</span>
                            <span class="stat-value txt-success">{result.created}</span>
                        </div>
                    </div>
                    <div class="col-4">
                        <div class="stat-card">
                            <span class="stat-label">Updated</span>
                            <span class="stat-value">{result.updated}</span>
                        </div>
                    </div>
                    <div class="col-4">
                        <div class="stat-card">
                            <span class="stat-label">Skipped</span>
                            <span class="stat-value txt-hint">{result.skipped}</span>
                        </div>
                    </div>
                </div>

                {#if result.errors?.length}
                    <h6 class="section-title">Rows that need attention</h6>

                    <table class="table responsive-table">
                        <thead class="sticky">
                            <tr>
                                <th class="col-field-type-number min-width">Line</th>
                                <th>Problem</th>
                            </tr>
                        </thead>
                        <tbody>
                            {#each result.errors as row, i (i)}
                                <tr>
                                    <td
                                        class="col-field-type-number min-width txt-code"
                                        data-name="Line"
                                    >
                                        {row.line}
                                    </td>
                                    <td class="txt-danger" data-name="Problem">{row.message}</td>
                                </tr>
                            {/each}
                        </tbody>
                    </table>

                    <div class="field-help">
                        A bad row never aborts the file — the good ones still applied.
                    </div>
                {/if}
            {/if}
        </div>

        <footer class="page-footer">
            <span class="txt">CSV in, CSV out — the same shape both ways</span>
            <ThemeToggle />
        </footer>
    </div>
</div>
