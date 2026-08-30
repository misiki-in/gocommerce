/*
 * Colour scheme, applied before first paint so the panel never flashes the
 * wrong theme. This is a separate file rather than an inline <script> because
 * the panel is served under a Content-Security-Policy that only trusts scripts
 * it can name.
 *
 * Three states, as in PocketBase: an explicit "light" or "dark", or "" for
 * "follow the operating system". Only the *resolved* value is ever written to
 * the document, so the stylesheet has a single thing to key off.
 */
(function () {
    var KEY = "gocommerce_color_scheme";

    function stored() {
        try {
            return window.localStorage.getItem(KEY) || "";
        } catch (e) {
            return "";
        }
    }

    function resolve(preference) {
        if (preference === "light" || preference === "dark") return preference;
        return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
    }

    var timeoutId;
    function apply(preference) {
        // Repainting every surface mid-transition reads as a flicker, so
        // animation is switched off for the frame that swaps the palette.
        clearTimeout(timeoutId);
        document.documentElement.style.setProperty("--animationSpeed", "0");
        document.documentElement.setAttribute("data-color-scheme", resolve(preference));
        timeoutId = setTimeout(function () {
            document.documentElement.style.removeProperty("--animationSpeed");
        }, 100);
    }

    window.gcTheme = {
        key: KEY,
        get preference() {
            return stored();
        },
        get active() {
            return resolve(stored());
        },
        set: function (preference) {
            try {
                if (preference) window.localStorage.setItem(KEY, preference);
                else window.localStorage.removeItem(KEY);
            } catch (e) {
                /* storage disabled; the choice lasts for this page only */
            }
            apply(preference);
        },
    };

    apply(stored());

    // Follow the system only while the operator has expressed no preference.
    window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", function () {
        if (!stored()) apply("");
    });
})();
