<script>
    /**
     * PocketBase's confirm: a `.modal.popup.sm`, centred, scaling 0.98 → 1. It
     * scales rather than slides because it belongs to the button you just
     * pressed rather than to the page.
     *
     * The overlay is the modal's own `::before` pseudo-element, so there is no
     * wrapper element here to click through — the backdrop click is handled by
     * a sibling button that covers the viewport behind it.
     */
    import { dismissable } from "$lib/dismiss.js";
    import { portal } from "$lib/portal.js";

    let {
        open = $bindable(false),
        title = "Are you sure?",
        message = "",
        confirmLabel = "Confirm",
        danger = false,
        onconfirm,
    } = $props();

    let working = $state(false);

    async function confirm() {
        working = true;
        try {
            await onconfirm?.();
            open = false;
        } finally {
            working = false;
        }
    }

    // Dismissal is disabled while the action runs: closing a confirmation
    // mid-delete would hide the outcome of something already in flight.
</script>

<div
    class="modal popup sm"
    data-modal-state={open ? "open" : "closed"}
    inert={!open}
    role="alertdialog"
    aria-modal="true"
    aria-label={title}
    use:portal
    use:dismissable={{ onclose: () => (open = false), enabled: open && !working }}
>
    <div class="modal-content">
        <h5 class="m-b-sm">{title}</h5>
        {#if message}
            <div class="txt-hint">{message}</div>
        {/if}
    </div>
    <footer class="modal-footer">
        <button type="button" class="btn transparent m-r-auto" onclick={() => (open = false)}>
            <span class="txt">Cancel</span>
        </button>
        <button
            type="button"
            class="btn"
            class:danger
            class:loading={working}
            disabled={working}
            onclick={confirm}
        >
            <span class="txt">{confirmLabel}</span>
        </button>
    </footer>
</div>
