<script>
    /**
     * PocketBase's toasts: a fixed container pinned to the bottom centre, each
     * toast a `.toast-container` with an icon rail on the left. The icon glyph
     * itself comes from CSS (`.toast.success .toast-icon::before`), so the
     * markup carries only the variant class.
     */
    import { toast } from "$lib/toast.svelte.js";
</script>

<div class="toasts-container">
    {#each toast.items as item (item.id)}
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div
            class="toast {item.type}"
            class:removing={item.removing}
            onmouseenter={() => toast.hold(item.id)}
            onmouseleave={() => toast.release(item.id)}
        >
            <div class="toast-container">
                <div class="toast-icon"></div>
                <div class="toast-content">
                    <span>{item.message}</span>
                    <button
                        type="button"
                        class="m-l-auto btn circle sm transparent secondary toast-remove"
                        aria-label="Dismiss"
                        onclick={() => toast.remove(item.id)}
                    >
                        <i class="ri-close-line" aria-hidden="true"></i>
                    </button>
                </div>
            </div>
        </div>
    {/each}
</div>
