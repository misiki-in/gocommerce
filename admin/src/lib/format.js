/**
 * Formatting helpers.
 *
 * The API sends money as an integer count of the currency's minor unit plus
 * its code, and deliberately never a formatted string — because how many
 * decimal places a currency has, and where the symbol goes, are the client's
 * business. This is the client, so it is this file's business.
 */

/** minorDigits returns how many decimal places a currency uses: 2 for USD, 0 for JPY, 3 for KWD. */
export function minorDigits(currency) {
    try {
        const fmt = new Intl.NumberFormat(undefined, { style: "currency", currency });
        return fmt.resolvedOptions().maximumFractionDigits ?? 2;
    } catch {
        return 2;
    }
}

/** formatMoney renders a {amount_minor, currency} pair for display. */
export function formatMoney(money) {
    if (!money || money.amount_minor === undefined || money.amount_minor === null) return "—";
    const currency = money.currency || "USD";
    try {
        return new Intl.NumberFormat(undefined, { style: "currency", currency }).format(
            money.amount_minor / 10 ** minorDigits(currency),
        );
    } catch {
        return `${money.amount_minor} ${currency}`;
    }
}

/** toMinor converts what a person typed ("24.99") into minor units. */
export function toMinor(value, currency) {
    const amount = parseFloat(String(value).replace(/,/g, ""));
    if (!isFinite(amount)) return 0;
    return Math.round(amount * 10 ** minorDigits(currency));
}

/** fromMinor converts minor units back into an editable decimal string. */
export function fromMinor(amountMinor, currency) {
    if (amountMinor === undefined || amountMinor === null) return "";
    const digits = minorDigits(currency);
    return (amountMinor / 10 ** digits).toFixed(digits);
}

/** formatDate renders an ISO timestamp in the reader's locale. */
export function formatDate(value, opts = {}) {
    if (!value) return "—";
    const date = new Date(value);
    if (isNaN(date.getTime())) return "—";
    return new Intl.DateTimeFormat(undefined, {
        dateStyle: "medium",
        timeStyle: opts.withTime === false ? undefined : "short",
    }).format(date);
}

/** relativeTime renders "3 minutes ago" for recent activity. */
export function relativeTime(value) {
    if (!value) return "—";
    const date = new Date(value);
    if (isNaN(date.getTime())) return "—";

    const seconds = Math.round((date.getTime() - Date.now()) / 1000);
    const units = [
        ["year", 31536000],
        ["month", 2592000],
        ["week", 604800],
        ["day", 86400],
        ["hour", 3600],
        ["minute", 60],
    ];
    const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
    for (const [unit, size] of units) {
        if (Math.abs(seconds) >= size) return rtf.format(Math.round(seconds / size), unit);
    }
    return rtf.format(Math.round(seconds), "second");
}

/**
 * The `.label` colour for each status. PocketBase's label has exactly four
 * colour variants — info, success, warning, danger — plus the neutral default,
 * so these return one of those five and nothing else.
 */
export function orderStatusClass(status) {
    switch (status) {
        case "delivered":
            return "success";
        case "shipped":
        case "confirmed":
            return "info";
        case "cancelled":
            return "danger";
        default:
            return "";
    }
}

export function paymentStatusClass(status) {
    switch (status) {
        case "paid":
            return "success";
        case "refunded":
            return "warning";
        case "failed":
            return "danger";
        default:
            return "";
    }
}

export function productStatusClass(status) {
    switch (status) {
        case "active":
            return "success";
        case "draft":
            return "warning";
        default:
            return "";
    }
}

/** stockClass colours a stock figure by how worried to be about it. */
export function stockClass(available, tracked = true) {
    if (!tracked) return "txt-hint";
    if (available <= 0) return "txt-danger";
    if (available <= 5) return "txt-warning";
    return "";
}

export function pluralize(count, singular, plural) {
    return count === 1 ? singular : plural || singular + "s";
}

/**
 * The symbol a person reads a price in — "$" for USD, "₹" for INR, "¥" for JPY.
 *
 * The engine never sends this, and should not: money crosses the API as minor
 * units plus a currency code, because the symbol and the separators belong to
 * whoever is reading, not to the store. That reader is this panel, so deriving
 * the symbol is exactly its job.
 *
 * `Intl` is asked rather than a lookup table, so a store that switches to a
 * currency nobody anticipated still gets the right glyph. When it has none it
 * hands back the code itself, which is a fine thing to show and better than a
 * guess.
 */
const symbolCache = new Map();

export function currencySymbol(currency) {
    const code = String(currency || "").toUpperCase();
    if (!code) return "";
    if (symbolCache.has(code)) return symbolCache.get(code);

    let symbol = code;
    try {
        const parts = new Intl.NumberFormat(undefined, {
            style: "currency",
            currency: code,
            currencyDisplay: "narrowSymbol",
        }).formatToParts(0);
        symbol = parts.find((p) => p.type === "currency")?.value || code;
    } catch {
        // An unknown code throws rather than falling back, and the code is a
        // perfectly readable label for it.
    }
    symbolCache.set(code, symbol);
    return symbol;
}
