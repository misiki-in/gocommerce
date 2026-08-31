/**
 * The panel is a client of the same public API as anything else — it has no
 * private endpoints and no server of its own. Everything it can do, you can do
 * with curl.
 */

import { browser } from "$app/environment";

const TOKEN_KEY = "gocommerce_admin_token";
const RECORD_KEY = "gocommerce_admin_record";

/** ApiError carries the engine's own error envelope. */
export class ApiError extends Error {
    constructor(status, code, message, details) {
        super(message || "request failed");
        this.name = "ApiError";
        this.status = status;
        this.code = code;
        this.details = details;
    }

    get isAuth() {
        return this.status === 401;
    }
}

export function getToken() {
    if (!browser) return "";
    try {
        return localStorage.getItem(TOKEN_KEY) || "";
    } catch {
        return "";
    }
}

export function setToken(token) {
    if (!browser) return;
    try {
        if (token) localStorage.setItem(TOKEN_KEY, token);
        else localStorage.removeItem(TOKEN_KEY);
    } catch {
        /* storage disabled; the session simply will not survive a reload */
    }
}

export function clearToken() {
    setToken("");
    setRecord(null);
}

/** getRecord returns the signed-in operator, as last seen from the server. */
export function getRecord() {
    if (!browser) return null;
    try {
        return JSON.parse(localStorage.getItem(RECORD_KEY) || "null");
    } catch {
        return null;
    }
}

/**
 * What the signed-in operator may do.
 *
 * The record carries `rights`, spelled out by the engine from their role, so
 * the panel never keeps its own copy of the permission table — see rights.go.
 *
 * A missing rights list means the credential is a static admin token, which
 * carries everything: scripts and the bootstrap operator have no role to read.
 * Erring that way keeps a token-authenticated panel fully usable, and the
 * engine refuses anything this is wrong about.
 */
export function can(right) {
    const record = getRecord();
    if (!record || !Array.isArray(record.rights)) return true;
    return record.rights.includes(right);
}

/** The rights, so a screen can test several without re-reading storage. */
export function rights() {
    const record = getRecord();
    return Array.isArray(record?.rights) ? record.rights : null;
}

function setRecord(record) {
    if (!browser) return;
    try {
        if (record) localStorage.setItem(RECORD_KEY, JSON.stringify(record));
        else localStorage.removeItem(RECORD_KEY);
    } catch {
        /* storage disabled */
    }
}

/**
 * request performs one API call and unwraps the {"data": ...} envelope.
 *
 * Admin calls carry the bearer token, which is either a superuser session from
 * signing in or a static admin token from the store's configuration. The
 * server treats them identically, so nothing here has to know which it holds.
 */
export async function request(method, path, options = {}) {
    const { body, admin = true, raw = false, headers = {} } = options;

    const init = { method, headers: { ...headers } };

    if (admin) {
        const token = getToken();
        if (token) init.headers["Authorization"] = "Bearer " + token;
    }

    if (body !== undefined && body !== null) {
        if (typeof body === "string") {
            init.headers["Content-Type"] = init.headers["Content-Type"] || "text/plain";
            init.body = body;
        } else {
            init.headers["Content-Type"] = "application/json";
            init.body = JSON.stringify(body);
        }
    }

    let response;
    try {
        response = await fetch(path, init);
    } catch (err) {
        // A network failure is not an API error; say so rather than showing a
        // status code that never arrived.
        throw new ApiError(0, "network_error", "Could not reach the store. Is it still running?");
    }

    if (raw) {
        if (!response.ok) throw await toApiError(response);
        return await response.text();
    }

    if (response.status === 204) return null;

    const payload = await response.json().catch(() => null);

    if (!response.ok) {
        const err = payload?.error;
        throw new ApiError(
            response.status,
            err?.code || "error",
            err?.message || response.statusText,
            err?.details,
        );
    }

    // List responses carry a meta block; hand both back so callers can page.
    if (payload && payload.meta) {
        return { data: payload.data ?? [], meta: payload.meta };
    }
    return payload?.data ?? payload;
}

async function toApiError(response) {
    const text = await response.text().catch(() => "");
    let parsed = null;
    try {
        parsed = JSON.parse(text);
    } catch {
        /* not JSON */
    }
    return new ApiError(
        response.status,
        parsed?.error?.code || "error",
        parsed?.error?.message || response.statusText,
        parsed?.error?.details,
    );
}

export const api = {
    get: (path, options) => request("GET", path, options),
    post: (path, body, options) => request("POST", path, { ...options, body }),
    patch: (path, body, options) => request("PATCH", path, { ...options, body }),
    delete: (path, options) => request("DELETE", path, options),
};

/** query builds a query string, dropping empty values. */
export function query(params) {
    const search = new URLSearchParams();
    for (const [key, value] of Object.entries(params || {})) {
        if (value === undefined || value === null || value === "") continue;
        search.set(key, String(value));
    }
    const s = search.toString();
    return s ? "?" + s : "";
}

/**
 * auth is the sign-in surface: an email and a password in, a session token
 * kept in localStorage, and the operator's record for the header to show.
 */
export const auth = {
    /** state asks whether this installation has an operator yet. */
    async state() {
        return await api.get("/api/admin/auth-state", { admin: false });
    },

    /** login exchanges credentials for a session. */
    async login(identity, password) {
        const result = await api.post(
            "/api/admin/auth-with-password",
            { identity, password },
            { admin: false },
        );
        setToken(result.token);
        setRecord(result.record);
        return result.record;
    },

    /** invitation says who a link is for, before asking for a password. */
    async invitation(token) {
        const result = await api.get(`/api/admin/invitations/accept/${token}`, { admin: false });
        return result.data ?? result;
    },

    /**
     * accept turns an invitation into an account and signs in with it. The
     * engine returns the same shape login does, so arriving by invitation and
     * arriving by password end in exactly the same state.
     */
    async accept(token, password) {
        const result = await api.post(
            `/api/admin/invitations/accept/${token}`,
            { password },
            { admin: false },
        );
        setToken(result.token);
        setRecord(result.record);
        return result.record;
    },

    /** install creates the first operator on a fresh database and signs in. */
    async install(email, password) {
        const result = await api.post("/api/admin/install", { email, password }, { admin: false });
        setToken(result.token);
        setRecord(result.record);
        return result.record;
    },

    /**
     * refresh turns a stored token back into an identity on boot and extends
     * its life. The token itself does not change — see Superusers.Touch — so
     * this never invalidates a request already in flight, or another tab.
     *
     * A static admin token is not a session, so a 400 here means "this
     * credential is fine, it just isn't a person" — keep it and carry on.
     */
    async refresh() {
        const token = getToken();
        if (!token) return null;
        try {
            const result = await api.post("/api/admin/auth-refresh", null);
            setToken(result.token);
            setRecord(result.record);
            return result.record;
        } catch (err) {
            if (err.status === 400) return getRecord();
            if (err.isAuth) clearToken();
            throw err;
        }
    },

    async logout() {
        try {
            await api.post("/api/admin/auth-logout", null);
        } catch {
            // Signing out must always succeed locally. If the server never
            // heard about it the session simply expires on its own.
        }
        clearToken();
    },

    list: () => api.get("/api/admin/superusers"),
    create: (email, password, role) => api.post("/api/admin/superusers", { email, password, role }),
    update: (id, body) => api.patch(`/api/admin/superusers/${id}`, body),
    remove: (id) => api.delete(`/api/admin/superusers/${id}`),
    // A role change is its own request, as it is its own act: administering
    // somebody is not the same as granting them access.
    setRole: (id, role) => request("PUT", `/api/admin/superusers/${id}/role`, { body: { role } }),

    // Inviting somebody rather than choosing a password for them. The creating
    // response is the only one that carries the link, so a caller that drops it
    // has to issue a new invitation.
    invitations: () => api.get("/api/admin/invitations"),
    invite: (email, role) => api.post("/api/admin/invitations", { email, role }),
    revokeInvitation: (id) => api.delete(`/api/admin/invitations/${id}`),

    // Ending sessions. Somebody else's is team management; your own is the
    // "I have lost a laptop" button and takes this browser with it.
    revokeSessions: (id) => api.post(`/api/admin/superusers/${id}/revoke-sessions`, {}),
    revokeMySessions: () => api.post("/api/admin/me/revoke-sessions", {}),

    // Self-service, which deliberately needs no right beyond being signed in.
    me: () => api.get("/api/admin/me"),
    updateMe: (body) => api.patch("/api/admin/me", body),
};

/**
 * roles is what each role may do in this store.
 *
 * The engine ships a default set per role and this is the store's departure
 * from it, so `save` sends the whole set rather than a change to it — that is
 * what the screen has in front of the operator — and `reset` removes the
 * override entirely, which is not the same as saving the defaults back: a role
 * with no override goes on tracking a default a later release may widen.
 *
 * A change lands on each affected operator's next request. Nothing has to be
 * revoked, and nobody is signed out.
 */
export const roles = {
    matrix: () => api.get("/api/admin/roles"),
    save: (role, rights) => request("PUT", `/api/admin/roles/${role}`, { body: { rights } }),
    reset: (role) => api.delete(`/api/admin/roles/${role}`),
};
