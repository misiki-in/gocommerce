<script>
    /**
     * A text field that suggests what the catalog already says.
     *
     * `products.vendor` and `products.product_type` are free text, not foreign
     * keys, so the set of choices is only ever "what has been typed before".
     * A <Select> is the wrong control for that — the first product to carry a
     * name could never be given it. A bare <input> is wrong the other way:
     * every product invents its own spelling and the exact-match filters stop
     * grouping anything. This is both. The value is whatever is in the box,
     * and the box says what is already in use.
     *
     * The shell is PocketBase's `.input.select` and the popover is the native
     * one, driven exactly as Select.svelte drives it — same `popovertarget`,
     * same `focusout` close, same arrow keys.
     *
     * One thing has to differ. A popover is positioned and sized against its
     * invoker, and here the invoker is the small chevron button rather than
     * the whole control, so `.dropdown`'s `width: anchor-size(width)` would
     * measure the chevron. `anchor-name` re-points it at the control, and it
     * is written inline because the name has to be unique per instance and a
     * stylesheet cannot mint one.
     */
    let {
        value = $bindable(""),
        options = [],
        id = undefined,
        placeholder = "",
        disabled = false,
        allowCustom = true,
        emptyText = "Nothing suggested yet",
    } = $props();

    // Scoped to the instance: the popover API pairs a trigger to its panel by
    // id, so two comboboxes on one page must not share one.
    const dropdownId = "combobox-" + Math.random().toString(36).slice(2, 10);
    const anchorName = "--" + dropdownId;

    let root = $state(null);
    let field = $state(null);
    let dropdown = $state(null);
    let open = $state(false);

    const typed = $derived(String(value ?? "").trim());
    const needle = $derived(typed.toLowerCase());

    const visible = $derived(
        needle ? options.filter((o) => String(o).toLowerCase().includes(needle)) : [...options],
    );

    /**
     * "New" means no existing option is this same word — not that nothing
     * matched. Typing "Acme" while "Acme Corp" exists is still a new vendor,
     * and the row that says so is the only thing telling the operator that
     * pressing Enter will keep it.
     */
    const isNew = $derived(
        allowCustom && !!typed && !options.some((o) => String(o).toLowerCase() === needle),
    );

    /**
     * `:popover-open` rather than the `open` flag below: the toggle event is
     * queued, so `open` can still say "closed" during the tick in which the
     * popover opened, and show/hide throw when they disagree with reality.
     */
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

    function pick(option) {
        value = option;
        close();
        field?.focus();
    }

    function onToggle(event) {
        open = event.newState === "open";
        // Escape and light dismiss can leave focus on an option that is no
        // longer rendered, which drops it to the body and loses the operator's
        // place in the form.
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
        if (event.key === "ArrowDown" || event.key === "ArrowUp") {
            event.preventDefault();
            show();
            queueMicrotask(() => focusOption(event.key === "ArrowUp" ? -1 : 0));
        } else if (event.key === "Enter") {
            // The value is already what was typed, so Enter only has to stop
            // the list covering the rest of the form — and stop the browser
            // treating it as a form submission.
            event.preventDefault();
            close();
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
     * Closing on focusout rather than on blur: blur fires while focus is still
     * moving inside the dropdown and would shut it the moment an option was
     * reached with the keyboard.
     */
    function onFocusOut(event) {
        if (!event.relatedTarget || !root?.contains(event.relatedTarget)) close();
    }
</script>

<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<div
    bind:this={root}
    class="input select single combobox"
    class:disabled
    style="anchor-name: {anchorName}"
    onfocusout={onFocusOut}
>
    <input
        bind:this={field}
        {id}
        {disabled}
        {placeholder}
        type="text"
        class="combobox-input"
        role="combobox"
        aria-expanded={open}
        aria-controls={dropdownId}
        aria-autocomplete="list"
        autocomplete="off"
        bind:value
        oninput={show}
        onpointerdown={onPointerDown}
        onclick={onClick}
        onkeydown={onInputKeydown}
    />

    <!-- Out of the tab order on purpose: the input is the control, and a
         second stop that only re-opens a list the arrow keys already open is
         one more thing between the operator and the next field. -->
    <button
        type="button"
        {disabled}
        tabindex="-1"
        class="selected-container combobox-toggle"
        popovertarget={dropdownId}
        aria-label="Show suggestions"
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
        {#each visible as option (option)}
            <button
                type="button"
                role="option"
                aria-selected={option === value}
                class="dropdown-item select-option"
                class:active={option === value}
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

        {#if !visible.length && !isNew}
            <div class="txt-hint txt-center m-0 p-5">{emptyText}</div>
        {/if}
    </div>
</div>
