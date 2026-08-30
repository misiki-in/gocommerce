/**
 * Moves a node to <body>, where PocketBase puts its modals too
 * (`ui/src/base/confirm.js`).
 *
 * This is not cosmetic. A modal is `position: fixed` and belongs to the
 * viewport, not to the page that opened it — and left in place it is a sibling
 * of `.page` inside `.app`, which quietly breaks layout.css:
 *
 *     .page:last-child { border-bottom-left-radius: 0; ... }
 *
 * That rule is what squares off the bottom of the page against the window
 * edge. One `<Drawer>` rendered after `.page` is enough to stop `:last-child`
 * matching, and the page grows rounded bottom corners with a strip of header
 * colour showing beneath them.
 */
export function portal(node) {
    document.body.appendChild(node);
    return {
        // Svelte no longer owns the node's position, so unmounting has to take
        // it out explicitly or a closed modal would linger on the body.
        destroy() {
            node.remove();
        },
    };
}
