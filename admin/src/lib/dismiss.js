/**
 * Backdrop and Escape dismissal for a `.modal`, matching PocketBase's own
 * handling in `ui/src/base/modal.js`.
 *
 * The subtlety is that there is no backdrop element to listen to. PocketBase
 * draws the dimmed page with the modal's own `::before` pseudo-element, and a
 * pseudo-element's clicks are attributed to the element that owns it — so a
 * click on the overlay arrives with `event.target === modal`, while a click on
 * anything real inside arrives with a descendant as the target. That single
 * comparison is the whole test.
 */

/**
 * dismissable closes the node when the backdrop is clicked or Escape pressed.
 *
 *   <div class="modal" use:dismissable={{ onclose, enabled }}>
 */
export function dismissable(node, params = {}) {
    let onclose = params.onclose;
    let enabled = params.enabled ?? true;

    // A press that began or ended on real content is a drag, not a dismissal.
    // Without this, selecting text in a field and releasing past the panel's
    // edge closes the drawer and throws the edit away — which is a horrible way
    // to lose work, and the reason PocketBase tracks both ends of the gesture
    // rather than just the click.
    let startedInside = false;
    let endedInside = false;

    const isInside = (event) => event.target !== node && node.contains(event.target);

    const onPressStart = (event) => (startedInside = isInside(event));
    const onPressEnd = (event) => (endedInside = isInside(event));

    const onClick = (event) => {
        if (!enabled || startedInside || endedInside || isInside(event)) return;
        onclose?.();
    };

    const onKeydown = (event) => {
        if (!enabled || event.key !== "Escape") return;
        // A select or menu inside the modal owns Escape first: the platform
        // closes the popover, and the same keystroke must not also close the
        // thing it was opened inside.
        if (document.querySelector("[popover]:popover-open")) return;
        event.stopPropagation();
        onclose?.();
    };

    node.addEventListener("mousedown", onPressStart);
    node.addEventListener("touchstart", onPressStart, { passive: true });
    node.addEventListener("mouseup", onPressEnd);
    node.addEventListener("touchend", onPressEnd, { passive: true });
    node.addEventListener("click", onClick);
    // Escape is bound to the window, not the node: focus is usually in a field
    // inside the modal, but it can also be nowhere in particular, and a person
    // pressing Escape means it either way.
    window.addEventListener("keydown", onKeydown);

    return {
        update(next = {}) {
            onclose = next.onclose;
            enabled = next.enabled ?? true;
        },
        destroy() {
            node.removeEventListener("mousedown", onPressStart);
            node.removeEventListener("touchstart", onPressStart);
            node.removeEventListener("mouseup", onPressEnd);
            node.removeEventListener("touchend", onPressEnd);
            node.removeEventListener("click", onClick);
            window.removeEventListener("keydown", onKeydown);
        },
    };
}
