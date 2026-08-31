<script>
    /**
     * PocketBase's settings sidebar: `<details class="nav-group">` groups of
     * `.nav-item` links inside a `.sidebar-content.scrollable`.
     *
     * The `<summary tabindex="-1">` is deliberate — layout.css hides the
     * disclosure triangle and drops the pointer cursor for exactly that case,
     * which turns the group into a plain static heading rather than something
     * that looks collapsible but never collapses.
     */
    import { base } from "$app/paths";
    import { page } from "$app/state";
    import { can } from "$lib/api.js";

    /*
     * `right` gates the link, exactly as the main nav does. Two of them have
     * none. Your own account: every operator reaches it, which is the whole
     * reason it is a separate screen from the team. And Store, which reads the
     * store's own currency, languages and providers from public endpoints —
     * there is nothing there to gate.
     *
     * Import / export takes either half: the screen offers both and shows only
     * the half you carry, so requiring both to reach it would hide it from the
     * person who may do one of them.
     */
    const groups = {
        System: [
            { href: "/settings", label: "Store", icon: "ri-home-gear-line", exact: true },
            { href: "/settings/superusers", label: "Team", icon: "ri-group-line",
              right: "team.read" },
            { href: "/settings/roles", label: "Roles", icon: "ri-shield-user-line",
              right: "roles.write" },
            { href: "/settings/account", label: "Your account", icon: "ri-user-settings-line" },
        ],
        Data: [{ href: "/data", label: "Import / export", icon: "ri-file-transfer-line",
                 anyOf: ["data.export", "data.import"] }],
    };

    /** A group with nothing left in it should not render its heading either. */
    const allowed = (l) =>
        (!l.right || can(l.right)) && (!l.anyOf || l.anyOf.some((r) => can(r)));

    const visible = $derived(
        Object.entries(groups)
            .map(([name, links]) => [name, links.filter(allowed)])
            .filter(([, links]) => links.length),
    );

    function isActive(item) {
        const path = page.url.pathname.replace(base, "") || "/";
        return item.exact ? path === item.href : path.startsWith(item.href);
    }
</script>

<aside class="page-sidebar settings-sidebar">
    <nav class="sidebar-content scrollable">
        {#each visible as [name, links] (name)}
            <details class="nav-group" open>
                <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
                <summary tabindex="-1">{name}</summary>
                {#each links as link (link.href)}
                    <a
                        href="{base}{link.href}"
                        class="nav-item"
                        class:active={isActive(link)}
                    >
                        <i class={link.icon} aria-hidden="true"></i>
                        <span class="txt">{link.label}</span>
                    </a>
                {/each}
            </details>
        {/each}
    </nav>
</aside>
