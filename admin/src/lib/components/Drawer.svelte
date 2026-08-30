<script>
    /**
     * PocketBase's editor panel: a right-anchored, full-height `.modal` that
     * slides 30px in from the edge over a dimmed page.
     *
     * The open/closed state is an attribute rather than a class because the
     * transition is driven by `@starting-style` and `transition-behavior:
     * allow-discrete` in modal.css — the element goes from `display: none` to
     * `display: flex` and still animates. Nothing here has to know that; it
     * just has to set the attribute the stylesheet reads.
     */
    import { dismissable } from "$lib/dismiss.js";
    import { portal } from "$lib/portal.js";

    let {
        open = false,
        title = "",
        size = "",
        onclose,
        header,
        children,
        footer,
    } = $props();
</script>

<div
    class="modal {size}"
    data-modal-state={open ? "open" : "closed"}
    inert={!open}
    use:portal
    use:dismissable={{ onclose, enabled: open }}
>
    <header class="modal-header">
        {#if header}
            {@render header()}
        {:else}
            <h5 class="modal-title">{title}</h5>
        {/if}
        <button
            type="button"
            class="btn circle transparent secondary modal-close-btn m-l-auto"
            aria-label="Close"
            onclick={() => onclose?.()}
        >
            <i class="ri-close-line" aria-hidden="true"></i>
        </button>
    </header>

    <div class="modal-content">
        {@render children?.()}
    </div>

    {#if footer}
        <footer class="modal-footer">
            {@render footer()}
        </footer>
    {/if}
</div>
