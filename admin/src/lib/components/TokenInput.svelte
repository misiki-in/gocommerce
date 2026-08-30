<script>
    /**
     * A set of short strings, edited as chips.
     *
     * Tags are the case this exists for: the engine stores them trimmed,
     * de-duplicated case-insensitively and sorted, so the list is a set rather
     * than a sequence, and the control has to make that obvious. A comma is a
     * separator, not a character; a chip is one tag whether or not it contains
     * a space; and the suggestions are the tags the rest of the catalog is
     * already using, because two spellings of one idea is the failure mode a
     * free-text tag field has.
     *
     * The popover, the keyboard and the anchoring are Combobox.svelte's — see
     * the note at the top of that file for why the anchor is named inline.
     */
    let {
        values = $bindable([]),
        options = [],
        id = undefined,
        placeholder = "",
        disabled = false,
        emptyText = "Nothing suggested yet",
    } = $props();

    const dropdownId = "tokens-" + Math.random().toString(36).slice(2, 10);
    const anchorName = "--" + dropdownId;

    let root = $state(null);
    let field = $state(null);
    let dropdown = $state(null);
    let open = $state(false);
    let draft = $state("");

    const typed = $derived(draft.trim());
    const needle = $derived(typed.toLowerCase());

    // Already-chosen tags are dropped rather than shown as checked: a chip for
    // each of them is already on screen, two rows above.
    const suggestions = $derived(
        options.filter((option) => {
            const key = String(option).toLowerCase();
            if (values.some((v) => String(v).toLowerCase() === key)) return false;
            return !needle || key.includes(needle);
        }),
    );

    const isNew = $derived(
        !!typed &&
            !options.some((o) => String(o).toLowerCase() === needle) &&
            !values.some((v) => String(v).toLowerCase() === needle),
    );

    function isOpen() {
        return !!dropdown?.matches(":popover-open");
    }

    function show() {
        if (disabled || isOpen()) return;
        dropdown?.showPopover();
    }

    function close() {
        if (isOpen()) dropdown?.hidePopover();
    }

    /**
     * add splits on commas rather than stripping them, so a pasted
     * "cotton, linen, summer" becomes three tags instead of one unreadable
     * word. De-duplication is case-insensitive because the engine's is.
     */
    function add(raw) {
        const parts = String(raw)
            .split(",")
            .map((t) => t.trim())
            .filter(Boolean);
        draft = "";
        if (!parts.length) return;

        const next = [...values];
        for (const value of parts) {
            if (!next.some((t) => String(t).toLowerCase() === value.toLowerCase())) next.push(value);
        }
        values = next;
    }

    /**
     * A click in the input is a light dismiss — the input is not the popover's
     * invoker, so the platform hides the list on pointerup and our own handler
     * would then re-open it on the click that follows, one frame later. Reading
     * the state before the dismiss makes the click a toggle instead of a blink.
     */
    let openAtPointerDown = false;

    function onPointerDown() {
        openAtPointerDown = isOpen();
    }

    function onClick() {
        if (!openAtPointerDown) show();
    }

    function remove(tag) {
        values = values.filter((t) => t !== tag);
        field?.focus();
    }

    function pick(option) {
        add(option);
        // The list stays open: adding three tags in a row is the normal case,
        // and re-opening it between each would be three extra keystrokes.
        field?.focus();
    }

    function onToggle(event) {
        open = event.newState === "open";
        if (!open && dropdown?.contains(document.activeElement)) field?.focus();
    }

    function optionButtons() {
        return [...(dropdown?.querySelectorAll(".select-option") ?? [])];
    }

    function focusOption(index) {
        const items = optionButtons();
        if (!items.length) return;
        items[(index + items.length) % items.length].focus();
    }

    function onInputKeydown(event) {
        if (event.key === "Enter" || event.key === ",") {
            // A comma is a separator here, not a character, and Enter has to be
            // caught before the browser treats it as a default action.
            event.preventDefault();
            add(draft);
        } else if (event.key === "Backspace" && draft === "" && values.length) {
            values = values.slice(0, -1);
        } else if (event.key === "ArrowDown" || event.key === "ArrowUp") {
            event.preventDefault();
            show();
            queueMicrotask(() => focusOption(event.key === "ArrowUp" ? -1 : 0));
        }
    }

    function onDropdownKeydown(event) {
        const items = optionButtons();
        const here = items.indexOf(document.activeElement);
        if (event.key === "ArrowDown") {
            event.preventDefault();
            focusOption(here + 1);
        } else if (event.key === "ArrowUp") {
            event.preventDefault();
            focusOption(here - 1);
        } else if (event.key === "Home") {
            event.preventDefault();
            focusOption(0);
        } else if (event.key === "End") {
            event.preventDefault();
            focusOption(items.length - 1);
        }
    }

    /**
     * Leaving the control commits what is half-typed. Doing it on the input's
     * own blur instead would fire while focus was still travelling to a
     * suggestion, and the draft would be added alongside the tag that was
     * clicked.
     */
    function onFocusOut(event) {
        if (event.relatedTarget && root?.contains(event.relatedTarget)) return;
        add(draft);
        close();
    }
</script>

<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<div
    bind:this={root}
    class="input select token-input"
    class:disabled
    style="anchor-name: {anchorName}"
    onfocusout={onFocusOut}
>
    <div class="token-list token-input-field">
        {#if !values.length && !draft}
            <!--
                The same chip Collections uses, for the same reason: these two
                sit one above the other in the same card, and an inline hint
                beside a chip put their text on two different left edges.
                Click-through, so the field below still takes the click and
                focuses the box.
            -->
            <span class="label chip-new token-empty" aria-hidden="true">
                <i class="ri-add-line"></i>
                {placeholder}
            </span>
        {/if}

        {#each values as tag (tag)}
            <span class="label">
                {tag}
                <button
                    type="button"
                    {disabled}
                    class="btn circle sm transparent secondary"
                    aria-label="Remove the tag {tag}"
                    title="Remove"
                    onclick={() => remove(tag)}
                >
                    <i class="ri-close-line" aria-hidden="true"></i>
                </button>
            </span>
        {/each}

        <!-- No placeholder attribute: the hint above carries that text, and
             both would render one on top of the other. `aria-label` keeps the
             box named for a screen reader, which the placeholder was doing. -->
        <input
            bind:this={field}
            {id}
            {disabled}
            type="text"
            aria-label={placeholder}
            role="combobox"
            aria-expanded={open}
            aria-controls={dropdownId}
            aria-autocomplete="list"
            autocomplete="off"
            bind:value={draft}
            oninput={show}
            onpointerdown={onPointerDown}
            onclick={onClick}
            onkeydown={onInputKeydown}
        />
    </div>

    <button
        type="button"
        {disabled}
        tabindex="-1"
        class="selected-container combobox-toggle"
        popovertarget={dropdownId}
        aria-label="Show tags used across the catalog"
    ></button>

    <div
        bind:this={dropdown}
        id={dropdownId}
        class="dropdown"
        popover="auto"
        tabindex="-1"
        role="listbox"
        style="position-anchor: {anchorName}"
        ontoggle={onToggle}
        onkeydown={onDropdownKeydown}
    >
        {#each suggestions as option (option)}
            <button
                type="button"
                role="option"
                aria-selected="false"
                class="dropdown-item select-option"
                onclick={() => pick(option)}
            >
                {option}
            </button>
        {/each}

        {#if isNew}
            <button
                type="button"
                role="option"
                aria-selected="false"
                class="dropdown-item select-option"
                onclick={() => pick(typed)}
            >
                <i class="ri-add-line" aria-hidden="true"></i>
                Create “{typed}”
            </button>
        {/if}

        {#if !suggestions.length && !isNew}
            <div class="txt-hint txt-center m-0 p-5">{emptyText}</div>
        {/if}
    </div>
</div>
