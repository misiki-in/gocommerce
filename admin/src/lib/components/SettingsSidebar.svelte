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
     * `right` gates the link, exactly as the main nav does. Your own account
     * has none: every operator reaches it, which is the whole reason it is a
     * separate screen from the team.
     */
    const groups = {
        System: [
            { href: "/settings", label: "Store", icon: "ri-home-gear-line", exact: true,
              right: "settings.write" },
            { href: "/settings/superusers", label: "Team", icon: "ri-group-line",
              right: "settings.write" },
            { href: "/settings/account", label: "Your account", icon: "ri-user-settings-line" },
        ],
        Data: [{ href: "/data", label: "Import / export", icon: "ri-file-transfer-line",
                 right: "settings.write" }],
    };

    /** A group with nothing left in it should not render its heading either. */
    const visible = $derived(
        Object.entries(groups)
            .map(([name, links]) => [name, links.filter((l) => !l.right || can(l.right))])
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
