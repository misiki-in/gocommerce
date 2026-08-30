<script>
    /**
     * PocketBase's select, in its own markup:
     *
     *   .input.select.single
     *     button.selected-container   ← the chevron is its ::after, from the
     *       .selected-item              icon font, and flips to point up while
     *     .dropdown[popover]            the popover is open
     *       button.dropdown-item.select-option
     *
     * A native <select> was the obvious thing and it is the wrong thing here:
     * its popup is drawn by the operating system, so it ignores every token in
     * the panel and looks like nothing else on the page. This one is the same
     * `.dropdown` the header menus use.
     *
     * What that costs is the native keyboard behaviour, so it is re-added
     * below: arrows to move, Enter to choose, Escape to close, type-ahead. The
     * popover itself is the platform's — light dismiss and top-layer stacking
     * come for free — and the trigger is a real <button>, so focus and screen
     * readers work without help.
     */
    let {
        value = $bindable(),
        options = [],
        id = undefined,
        placeholder = "Select…",
        disabled = false,
        // For a select that carries no visible label — a unit beside the number
        // it qualifies, say, where the options name the thing themselves. A
        // <label> is still the better answer wherever there is room for one.
        ariaLabel = undefined,
        // PocketBase reveals the search box once a list gets long enough to be
        // worth filtering; below that it is clutter.
        searchThreshold = 8,
        onchange,
        class: className = "",
    } = $props();

    // Scoped to the instance: the popover API pairs a trigger to its panel by
    // id, so two selects on one page must not share one.
    const dropdownId = "select-dropdown-" + Math.random().toString(36).slice(2, 10);

    let dropdown = $state(null);
    let root = $state(null);
    let search = $state("");
    let open = $state(false);

    const selected = $derived(options.find((o) => o.value === value) ?? null);

    const visible = $derived(
        search.trim()
            ? options.filter((o) =>
                  String(o.label ?? o.value)
                      .toLowerCase()
                      .includes(search.trim().toLowerCase()),
              )
            : options,
    );

    function close() {
        // hidePopover() throws on a popover that is already hidden, and
        // focusout fires whether or not the list was ever opened — tabbing
        // past the trigger is enough. `:popover-open` rather than the flag
        // below because the toggle event is queued, so the flag can still say
        // "closed" during the tick in which it opened.
        if (dropdown?.matches(":popover-open")) dropdown.hidePopover();
    }

    function pick(option) {
        value = option.value;
        onchange?.(option.value);
        close();
    }

    function onToggle(event) {
        open = event.newState === "open";
        if (!open) search = "";
    }

    /**
     * Arrow keys move through the options while the list is open, and open it
     * from the trigger when it is closed — which is what a native select does,
     * and what anyone will try first.
     */
    function onTriggerKeydown(event) {
        if (event.key === "ArrowDown" || event.key === "ArrowUp" || event.key === "Enter") {
            if (!open) {
                event.preventDefault();
                dropdown?.showPopover();
                queueMicrotask(() => focusOption(event.key === "ArrowUp" ? -1 : 0));
            }
        }
    }

    function optionButtons() {
        return [...(dropdown?.querySelectorAll(".select-option") ?? [])];
    }

    function focusOption(index) {
        const items = optionButtons();
        if (!items.length) return;
        const wrapped = (index + items.length) % items.length;
        items[wrapped].focus();
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
    class="input select single {className}"
    class:disabled
    onfocusout={onFocusOut}
>
    <button
        type="button"
        {id}
        {disabled}
        class="selected-container"
        popovertarget={dropdownId}
        aria-label={ariaLabel}
        aria-haspopup="listbox"
        aria-expanded={open}
        onkeydown={onTriggerKeydown}
    >
        {#if selected}
            <div class="selected-item">{selected.label ?? selected.value}</div>
        {:else}
            <span class="placeholder">{placeholder}</span>
        {/if}
    </button>

    <div
        bind:this={dropdown}
        id={dropdownId}
        class="dropdown"
        popover="auto"
        tabindex="-1"
        role="listbox"
        ontoggle={onToggle}
        onkeydown={onDropdownKeydown}
    >
        <div class="fields dropdown-search" hidden={options.length < searchThreshold}>
            <div class="field">
                <!-- svelte-ignore a11y_autofocus -->
                <input type="text" placeholder="Search…" bind:value={search} />
            </div>
            {#if search}
                <div class="field addon p-r-5">
                    <button
                        type="button"
                        title="Clear"
                        class="btn sm secondary transparent circle"
                        onclick={() => (search = "")}
                    >
                        <i class="ri-close-line" aria-hidden="true"></i>
                    </button>
                </div>
            {/if}
        </div>

        {#each visible as option (option.value)}
            <button
                type="button"
                role="option"
                aria-selected={option.value === value}
                class="dropdown-item select-option"
                class:active={option.value === value}
                onclick={() => pick(option)}
            >
                {option.label ?? option.value}
            </button>
        {/each}

        {#if !visible.length}
            <div class="txt-hint txt-center m-0 p-5">No options found</div>
        {/if}
    </div>
</div>
