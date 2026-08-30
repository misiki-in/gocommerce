<script>
    /**
     * A rich text field for a product description, built on `contenteditable`
     * with nothing behind it.
     *
     * That is a constraint before it is a preference: the panel is served
     * under `script-src 'self'`, so an editor cannot come from a CDN, and
     * bundling TinyMCE or ProseMirror into the Go binary to put <strong>
     * around a word is a few hundred kilobytes for one field on one screen.
     *
     * `document.execCommand` is deprecated and has no successor — the
     * libraries that avoid it reimplement selection, undo and input handling
     * on top of `beforeinput`, which is precisely the size we are declining to
     * ship. It is still implemented by every browser this panel runs in, and
     * the commands used here (the four inline formats, formatBlock, the two
     * lists, createLink/unlink, removeFormat) are its interoperable core.
     * Using it is a decision, not an oversight.
     *
     * What the browser does not give away for free is state: a caret sitting
     * inside a <b> looks like any other caret, so `selectionchange` drives the
     * toolbar's pressed state, and every command re-reads it afterwards.
     */
    let {
        value = $bindable(""),
        id = undefined,
        placeholder = "",
        disabled = false,
    } = $props();

    let root = $state(null);
    let toolbar = $state(null);
    let body = $state(null);
    let focused = $state(false);
    let linkOpen = $state(false);
    let linkUrl = $state("");

    /** @type {string[]} */
    let active = $state([]);

    // The toolbar is one tab stop with arrow keys inside it, as a toolbar is
    // supposed to be. Twelve buttons that each take a Tab is twelve presses
    // between the field before this one and the text you came to write.
    let toolIndex = $state(0);

    // Kept off `$state` deliberately: it is a live DOM Range read back when
    // the link is applied, and nothing renders from it.
    let savedRange = null;

    // `group` only draws the dividers; `toggle` decides whether the button is
    // a format that can be on (aria-pressed) or an action that just happens.
    const tools = [
        { name: "bold", group: 0, icon: "ri-bold", label: "Bold", hint: "Bold (Ctrl+B)", toggle: true },
        { name: "italic", group: 0, icon: "ri-italic", label: "Italic", hint: "Italic (Ctrl+I)", toggle: true },
        { name: "underline", group: 0, icon: "ri-underline", label: "Underline", hint: "Underline (Ctrl+U)", toggle: true },
        { name: "strikeThrough", group: 0, icon: "ri-strikethrough", label: "Strikethrough", hint: "Strikethrough", toggle: true },
        { name: "h2", group: 1, icon: "ri-h-2", label: "Heading 2", hint: "Heading 2", toggle: true },
        { name: "h3", group: 1, icon: "ri-h-3", label: "Heading 3", hint: "Heading 3", toggle: true },
        { name: "p", group: 1, icon: "ri-paragraph", label: "Paragraph", hint: "Paragraph", toggle: true },
        { name: "insertUnorderedList", group: 2, icon: "ri-list-unordered", label: "Bulleted list", hint: "Bulleted list", toggle: true },
        { name: "insertOrderedList", group: 2, icon: "ri-list-ordered", label: "Numbered list", hint: "Numbered list", toggle: true },
        { name: "link", group: 3, icon: "ri-link", label: "Link", hint: "Link", toggle: true },
        { name: "unlink", group: 3, icon: "ri-link-unlink", label: "Remove link", hint: "Remove link", toggle: false },
        { name: "clear", group: 4, icon: "ri-format-clear", label: "Clear formatting", hint: "Clear formatting", toggle: false },
    ];

    function caretInside() {
        const node = document.getSelection()?.anchorNode ?? null;
        return !!node && !!body?.contains(node);
    }

    function currentLink() {
        const node = document.getSelection()?.anchorNode ?? null;
        const element = node instanceof Element ? node : (node?.parentElement ?? null);
        return element && body?.contains(element) ? element.closest("a") : null;
    }

    function readState() {
        if (!caretInside()) return [];

        const names = [];
        for (const command of [
            "bold",
            "italic",
            "underline",
            "strikeThrough",
            "insertUnorderedList",
            "insertOrderedList",
        ]) {
            if (document.queryCommandState(command)) names.push(command);
        }

        const block = String(document.queryCommandValue("formatBlock") || "").toLowerCase();
        if (block === "h2") names.push("h2");
        if (block === "h3") names.push("h3");
        // An unwrapped caret and a Chrome <div> are both "a paragraph" as far
        // as this toolbar is concerned — but not inside a list item, where
        // formatBlock reports nothing and the list button is the true state.
        const inList = names.includes("insertUnorderedList") || names.includes("insertOrderedList");
        if (!inList && (block === "" || block === "p" || block === "div")) names.push("p");

        if (currentLink()) names.push("link");
        return names;
    }

    function exec(command, argument) {
        if (disabled) return;
        body?.focus();
        document.execCommand(command, false, argument);
        sync();
    }

    /**
     * The editor's HTML is the value, but only in that direction while it has
     * focus — see the effect below.
     */
    function sync() {
        if (!body) return;
        value = body.innerHTML;
        active = readState();
    }

    /**
     * Deleting the last character leaves the browser's own <br>, or a
     * <div><br></div>, behind. That is not `:empty`, so the placeholder never
     * comes back and the field stores markup for nothing.
     *
     * It runs on input and on the way out, never after a command: pressing H2
     * on an empty field produces exactly this shape on purpose, and clearing
     * it would delete the block the person just asked for.
     */
    function dropEmptyMarkup() {
        if (!body || body.innerHTML === "" || body.textContent?.trim()) return;
        body.innerHTML = "";
    }

    function run(tool) {
        switch (tool.name) {
            case "h2":
            case "h3":
            case "p":
                // formatBlock does not toggle: applying the block that is
                // already there does nothing, so pressing H2 again has to mean
                // "back to a paragraph" or the button looks broken.
                exec("formatBlock", active.includes(tool.name) ? "<p>" : `<${tool.name}>`);
                break;
            case "link":
                openLink();
                break;
            case "clear":
                // removeFormat leaves anchors alone, and an unstyled link is
                // still a link — "clear formatting" has to mean both.
                exec("removeFormat");
                exec("unlink");
                break;
            default:
                exec(tool.name);
        }
    }

    /**
     * Pasted content is stripped to plain text.
     *
     * A paste out of Word or a web page carries that document's entire
     * stylesheet inline — fonts, pixel sizes, `mso-` properties, colours that
     * belong to a white page — and it is the single fastest way to make stored
     * descriptions unusable, because none of it can be edited back out here
     * and all of it survives into the storefront. Text is what the person
     * meant to bring; the formatting is what they were standing next to.
     *
     * `insertText` rather than range surgery: it keeps the browser's own undo
     * stack intact, so Ctrl+Z after a paste still works.
     */
    function onPaste(event) {
        event.preventDefault();
        const text = event.clipboardData?.getData("text/plain") ?? "";
        if (!text) return;
        document.execCommand("insertText", false, text);
        onInput();
    }

    function onInput() {
        dropEmptyMarkup();
        sync();
    }

    /**
     * Ctrl+B with nothing selected changes the typing state and not the
     * selection, so `selectionchange` never fires and the toolbar would go on
     * claiming the format is off until the caret moved.
     */
    function onBodyKeyup(event) {
        if (event.ctrlKey || event.metaKey) active = readState();
    }

    /**
     * Focus is tracked on the whole component, not on the editable: the link
     * row is outside the contenteditable, and typing a URL must not count as
     * having left the field. Same reason Select.svelte watches focusout rather
     * than blur.
     */
    function onFocusOut(event) {
        if (event.relatedTarget && root?.contains(event.relatedTarget)) return;
        focused = false;
        // A block created from the toolbar and never typed into is markup for
        // nothing. Leaving is the moment it is safe to drop — doing it under a
        // live caret would take the caret with it.
        if (body && body.innerHTML !== "" && !body.textContent?.trim()) {
            dropEmptyMarkup();
            value = "";
        }
    }

    function toolButtons() {
        return [...(toolbar?.querySelectorAll("button") ?? [])];
    }

    function focusTool(index) {
        const items = toolButtons();
        if (!items.length) return;
        const wrapped = (index + items.length) % items.length;
        toolIndex = wrapped;
        items[wrapped].focus();
    }

    function onToolbarKeydown(event) {
        const here = toolButtons().indexOf(document.activeElement);
        if (event.key === "ArrowRight") {
            event.preventDefault();
            focusTool(here + 1);
        } else if (event.key === "ArrowLeft") {
            event.preventDefault();
            focusTool(here - 1);
        } else if (event.key === "Home") {
            event.preventDefault();
            focusTool(0);
        } else if (event.key === "End") {
            event.preventDefault();
            focusTool(toolButtons().length - 1);
        }
    }

    function openLink() {
        // The selection has to be captured before focus moves into the URL
        // field; by the time Add is pressed, the caret the link belongs to is
        // no longer the document's current one.
        const selection = document.getSelection();
        savedRange = selection?.rangeCount ? selection.getRangeAt(0).cloneRange() : null;
        linkUrl = currentLink()?.getAttribute("href") ?? "";
        linkOpen = true;
    }

    function closeLink() {
        linkOpen = false;
        linkUrl = "";
        body?.focus();
    }

    function escapeHtml(text) {
        return text
            .replace(/&/g, "&amp;")
            .replace(/</g, "&lt;")
            .replace(/>/g, "&gt;")
            .replace(/"/g, "&quot;");
    }

    function normalizeUrl(raw) {
        const url = raw.trim();
        // A `javascript:` href in a description is stored XSS on whatever
        // renders it, and this field is the only place it could get in.
        if (/^\s*javascript:/i.test(url)) return "";
        if (!url) return "";
        // "example.com" with no scheme resolves against the page it is
        // rendered on, so a storefront link silently becomes an internal one.
        if (/^([a-z][a-z0-9+.-]*:|\/|#|\?)/i.test(url)) return url;
        return "https://" + url;
    }

    function applyLink() {
        const url = normalizeUrl(linkUrl);
        if (!url) return;

        body?.focus();
        const selection = document.getSelection();
        if (savedRange && selection) {
            selection.removeAllRanges();
            selection.addRange(savedRange);
        }

        if (savedRange?.collapsed) {
            // createLink needs something to wrap, so with nothing selected it
            // silently does nothing. Making the URL its own label is the only
            // outcome that is not a button that appears broken.
            document.execCommand("insertHTML", false, `<a href="${escapeHtml(url)}">${escapeHtml(url)}</a>`);
        } else {
            document.execCommand("createLink", false, url);
        }

        savedRange = null;
        linkOpen = false;
        linkUrl = "";
        sync();
    }

    function onLinkKeydown(event) {
        if (event.key === "Enter") {
            // The editor is usually inside a form, where a bare Enter in a
            // text input submits it — here that would save the product from
            // the link row.
            event.preventDefault();
            applyLink();
        } else if (event.key === "Escape") {
            // Escape belongs to the link row while it is open. Drawer.svelte
            // listens for it on the window and would otherwise close the whole
            // editor, throwing the edit away.
            event.preventDefault();
            event.stopPropagation();
            closeLink();
        }
    }

    $effect(() => {
        // Chrome wraps each new line in a <div> and both engines will happily
        // emit `<span style="font-weight: bold">`. Neither survives contact
        // with a storefront's own stylesheet; <p> and <b> do.
        document.execCommand("defaultParagraphSeparator", false, "p");
        document.execCommand("styleWithCSS", false, "false");
    });

    $effect(() => {
        function onSelectionChange() {
            if (!caretInside()) return;
            active = readState();
        }
        // Ctrl+B and friends are the browser's, not ours: nothing here runs
        // when they are pressed, so the pressed state has to come from the
        // selection changing rather than from the toolbar being clicked.
        document.addEventListener("selectionchange", onSelectionChange);
        return () => document.removeEventListener("selectionchange", onSelectionChange);
    });

    $effect(() => {
        const incoming = value ?? "";
        // Writing innerHTML collapses the selection to the start of the
        // element, so a value arriving from outside is only applied while the
        // editor is not focused — otherwise every keystroke would send the
        // caret back to the first character. `focused` is reactive so a change
        // that arrives mid-edit is still applied, on blur.
        if (!body || focused) return;
        if (body.innerHTML !== incoming) body.innerHTML = incoming;
    });
</script>

<div
    bind:this={root}
    class="rich-text"
    class:disabled
    onfocusin={() => (focused = true)}
    onfocusout={onFocusOut}
>
    <div
        bind:this={toolbar}
        class="rich-text-toolbar"
        role="toolbar"
        tabindex="-1"
        aria-label="Formatting"
        aria-controls={id}
        onkeydown={onToolbarKeydown}
    >
        {#each tools as tool, i (tool.name)}
            {#if i > 0 && tool.group !== tools[i - 1].group}
                <span class="rich-text-divider"></span>
            {/if}
            <button
                type="button"
                class="btn sm transparent secondary"
                {disabled}
                title={tool.hint}
                aria-label={tool.label}
                aria-pressed={tool.toggle ? active.includes(tool.name) : undefined}
                tabindex={i === toolIndex ? 0 : -1}
                onclick={() => run(tool)}
                onfocus={() => (toolIndex = i)}
                onmousedown={(event) => event.preventDefault()}
            >
                <i class={tool.icon} aria-hidden="true"></i>
            </button>
        {/each}
    </div>

    <!--
        The URL prompt is a row in the component rather than `window.prompt`:
        a browser dialog blocks the page, is drawn by the operating system and
        looks like nothing else here — and on the way back it takes the
        selection with it.
    -->
    {#if linkOpen}
        <div class="rich-text-link">
            <div class="field">
                <!-- svelte-ignore a11y_autofocus -->
                <input
                    type="text"
                    autofocus
                    placeholder="https://example.com"
                    aria-label="Link URL"
                    bind:value={linkUrl}
                    onkeydown={onLinkKeydown}
                />
            </div>
            <button type="button" class="btn sm" onclick={applyLink}>
                <span class="txt">Add</span>
            </button>
            <button type="button" class="btn sm secondary transparent" onclick={closeLink}>
                <span class="txt">Cancel</span>
            </button>
        </div>
    {/if}

    <div
        bind:this={body}
        {id}
        class="rich-text-body"
        contenteditable={!disabled}
        role="textbox"
        aria-multiline="true"
        aria-placeholder={placeholder}
        data-placeholder={placeholder}
        tabindex={disabled ? -1 : 0}
        oninput={onInput}
        onkeyup={onBodyKeyup}
        onpaste={onPaste}
    ></div>
</div>
