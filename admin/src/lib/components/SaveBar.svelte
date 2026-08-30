<script>
    /**
     * The unsaved-changes bar: a dark pill fixed across the top of the window,
     * over the app header, while a form has edits that have not been saved.
     *
     * It is chrome, not an alert — it says what the state of the page is, not
     * that something went wrong — so it takes the primary near-black rather
     * than a status colour, and the buttons on it are ordinary PocketBase
     * buttons over a re-pointed surface (see the `save bar` section in
     * gocommerce.css).
     *
     * The element stays mounted and is hidden with `hidden` rather than being
     * added and removed. Two reasons: the live region has to exist before it
     * has something to announce, or a screen reader can miss the first
     * appearance entirely; and `hidden` → visible restarts the CSS animation,
     * so the entrance still plays every time.
     */
    let {
        dirty = false,
        saving = false,
        message = "Unsaved changes",
        ondiscard,
        onsave,
        saveLabel = "Save",
    } = $props();

    /**
     * The shortcut label, in the platform's own notation.
     *
     * `navigator.platform` is deprecated and still the only thing that answers
     * on every browser this panel runs in; `userAgentData` is asked first where
     * it exists. It only decides which glyph to print, so being wrong shows the
     * wrong hint rather than breaking the shortcut — both keys are accepted
     * either way.
     */
    const apple =
        typeof navigator !== "undefined" &&
        /mac|iphone|ipad/i.test(navigator.userAgentData?.platform || navigator.platform || "");
    const saveHint = apple ? "⌘S" : "Ctrl+S";

    $effect(() => {
        if (!dirty) return;
        // Lives here rather than in each page: a form that shows this bar has
        // work in it that the browser is one Ctrl+W away from discarding, and
        // remembering to add the listener is exactly the kind of thing that
        // gets remembered on four screens out of five.
        //
        // The message is the browser's own — every engine has ignored a custom
        // one for a decade — so preventDefault is the entire API.
        const warn = (event) => {
            event.preventDefault();
            event.returnValue = "";
        };

        /**
         * Ctrl+S — and Cmd+S — saves.
         *
         * On the window rather than the form, because the whole point is that
         * it works from wherever the cursor happens to be: a rich-text body, a
         * variant's price cell, a token input. preventDefault is not optional;
         * without it the browser offers to save the page as a file, which is
         * the one thing the operator certainly did not mean.
         *
         * It is bound only while the bar is showing, so on a page with nothing
         * to save the shortcut stays the browser's own.
         */
        const save = (event) => {
            if (event.key !== "s" && event.key !== "S") return;
            if (!event.ctrlKey && !event.metaKey) return;
            if (event.altKey || event.shiftKey) return;
            event.preventDefault();
            // A second press while the first save is in flight would be a
            // second POST of the same edit.
            if (!saving) onsave?.();
        };

        window.addEventListener("beforeunload", warn);
        window.addEventListener("keydown", save);
        return () => {
            window.removeEventListener("beforeunload", warn);
            window.removeEventListener("keydown", save);
        };
    });
</script>

<div class="save-bar" hidden={!dirty} role="region" aria-label="Unsaved changes">
    <span class="save-bar-message" aria-live="polite">{message}</span>

    <!-- `.sm` throughout: the bar re-points --smBtnHeight, so these stay
         ordinary PocketBase buttons and pick up its compact height. -->
    <button
        type="button"
        class="btn sm transparent"
        disabled={saving}
        onclick={() => ondiscard?.()}
    >
        <span class="txt">Discard</span>
    </button>

    <!-- .loading draws its own spinner from the icon font; an element here
         would be a second one. -->
    <!-- The shortcut is named on the control it fires, which is the only place
         an operator would look for it. `title` rather than visible text: the
         bar is 32px of chrome and the hint is for the second time you save,
         not the first. -->
    <button
        type="button"
        class="btn sm secondary"
        class:loading={saving}
        disabled={saving}
        title="{saveLabel} ({saveHint})"
        onclick={() => onsave?.()}
    >
        <span class="txt">{saveLabel}</span>
    </button>
</div>
