<script>
    /**
     * One button, two states: click flips light and dark.
     *
     * It replaces a three-option dropdown (light / dark / auto). "Auto" is
     * still what a browser that has never been told anything gets — theme.js
     * resolves the system preference on first paint — but it is no longer a
     * thing you can *choose*, because a menu is a poor trade for a switch you
     * flip twice a year.
     *
     * The label says where the click goes, not where you are: a control named
     * for its current state reads as a status display and gets clicked by
     * accident.
     */
    let scheme = $state("light");

    $effect(() => {
        scheme = window.gcTheme?.active || "light";
    });

    function toggle() {
        const next = scheme === "dark" ? "light" : "dark";
        window.gcTheme?.set(next);
        scheme = next;
    }
</script>

<button
    type="button"
    class="btn sm transparent secondary theme-toggle"
    onclick={toggle}
    title={scheme === "dark" ? "Switch to light" : "Switch to dark"}
    aria-label={scheme === "dark" ? "Switch to light theme" : "Switch to dark theme"}
    aria-pressed={scheme === "dark"}
>
    <i class={scheme === "dark" ? "ri-moon-line" : "ri-sun-line"} aria-hidden="true"></i>
    <span class="txt">{scheme === "dark" ? "Dark" : "Light"}</span>
</button>
