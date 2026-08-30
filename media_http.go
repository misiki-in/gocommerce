package gocommerce

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The media library's HTTP surface: upload, list, attach.
func (a *App) mountMediaRoutes() {
	a.HandleAdminFunc("GET /api/admin/media", a.handleListMedia, RightCatalogRead)
	a.HandleAdminFunc("POST /api/admin/media", a.handleUploadMedia, RightCatalogWrite)
	a.HandleAdminFunc("POST /api/admin/media/link", a.handleLinkMedia, RightCatalogWrite)
	a.HandleAdminFunc("GET /api/admin/media/{id}", a.handleGetMedia, RightCatalogRead)
	a.HandleAdminFunc("PATCH /api/admin/media/{id}", a.handleUpdateMedia, RightCatalogWrite)
	a.HandleAdminFunc("DELETE /api/admin/media/{id}", a.handleDeleteMedia, RightCatalogWrite)
	// Read before write. PUT replaces the whole ordered list, so without a way
	// to read the current one a client that attaches a single file silently
	// discards everything else on the product.
	a.HandleAdminFunc("GET /api/admin/products/{id}/media", a.handleProductMedia, RightCatalogRead)
	a.HandleAdminFunc("PUT /api/admin/products/{id}/media", a.handleSetProductMedia, RightCatalogWrite)
	// A variant nominates one of its product's images; it never owns one, so
	// this takes a media id rather than a file.
	a.HandleAdminFunc("PUT /api/admin/variants/{id}/media", a.handleSetVariantMedia, RightCatalogWrite)

	// Uploaded files are served from the same origin so a storefront can use
	// them without CORS. Only mounted when this store actually holds files.
	if local, ok := a.mediaStore.(*LocalMediaStore); ok {
		a.mountLocalMedia(local)
	}
}

// mountLocalMedia serves MediaDir read-only.
//
// It deliberately does not use http.FileServer: that would serve directory
// listings and follow any path the URL describes. Every stored key is a flat
// random name, so the handler resolves exactly one file in exactly one
// directory and refuses anything else.
func (a *App) mountLocalMedia(store *LocalMediaStore) {
	prefix := strings.TrimRight(store.Prefix, "/") + "/"
	a.HandleFunc("GET "+prefix+"{key}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if key == "" || key != filepath.Base(key) || strings.Contains(key, "..") {
			RespondError(w, r, NotFoundf("no such media"))
			return
		}
		path := filepath.Join(store.Dir, key)
		f, err := os.Open(path)
		if err != nil {
			RespondError(w, r, NotFoundf("no such media"))
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || info.IsDir() {
			RespondError(w, r, NotFoundf("no such media"))
			return
		}

		// Content is immutable — the key is random and a new upload gets a new
		// one — so it can be cached hard.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		// Belt and braces against a file that slipped past the extension
		// allow-list: never let the browser sniff an upload into markup.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, key, info.ModTime(), f)
	})
}

func (a *App) handleListMedia(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := Page(r)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	q := r.URL.Query()
	kind := q.Get("kind")
	switch kind {
	case "", MediaImage, MediaVideo, MediaModel:
	default:
		RespondError(w, r, Validationf("kind must be image, video or model"))
		return
	}
	usage := q.Get("usage")
	switch usage {
	case "", UsedAnywhere, UsedNowhere:
	default:
		RespondError(w, r, Validationf("usage must be used or unused"))
		return
	}
	productID, err := queryInt64(q, "product_id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	minBytes, err := queryInt64(q, "min_bytes")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	maxBytes, err := queryInt64(q, "max_bytes")
	if err != nil {
		RespondError(w, r, err)
		return
	}

	items, total, err := a.media.List(r.Context(), MediaQuery{
		Search:    q.Get("q"),
		Kind:      kind,
		Usage:     usage,
		ProductID: productID,
		MinBytes:  minBytes,
		MaxBytes:  maxBytes,
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		RespondError(w, r, err)
		return
	}
	RespondList(w, items, ListMeta{Total: total, Limit: limit, Offset: offset})
}

// handleUploadMedia takes one file from a multipart form.
//
// The size limit is enforced twice: MaxBytesReader caps what the server will
// read at all, and the service re-checks the reported size. The first stops a
// malicious stream, the second gives an honest error message.
func (a *App) handleUploadMedia(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadBytes+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		RespondError(w, r, Validationf("expected a multipart upload with a %q field", "file"))
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		RespondError(w, r, Validationf("no %q field in the upload", "file"))
		return
	}
	defer file.Close()

	contentType := header.Header.Get("Content-Type")
	item, err := a.media.Upload(r.Context(), header.Filename, contentType, file, header.Size)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	if alt := r.FormValue("alt"); alt != "" {
		if updated, err := a.media.SetAlt(r.Context(), item.ID, alt); err == nil {
			item = updated
		}
	}
	a.log.Info("media uploaded", "id", item.ID, "kind", item.Kind, "bytes", item.SizeBytes)
	Respond(w, http.StatusCreated, item)
}

// handleLinkMedia records media hosted elsewhere. Separate from upload because
// the request is JSON rather than multipart, and because nothing this store
// holds is created by it.
func (a *App) handleLinkMedia(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL  string `json:"url"`
		Kind string `json:"kind"`
		Alt  string `json:"alt"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	item, err := a.media.AddURL(r.Context(), in.URL, in.Kind, in.Alt)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusCreated, item)
}

func (a *App) handleGetMedia(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	item, err := a.media.Get(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, item)
}

func (a *App) handleUpdateMedia(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var in struct {
		Alt string `json:"alt"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	item, err := a.media.SetAlt(r.Context(), id, in.Alt)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, item)
}

func (a *App) handleDeleteMedia(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	if err := a.media.Delete(r.Context(), id); err != nil {
		RespondError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleProductMedia(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	// A product that does not exist and one with no media are different
	// answers, and a client building a replace-list needs to tell them apart.
	if _, err := a.catalog.GetProduct(r.Context(), id); err != nil {
		RespondError(w, r, err)
		return
	}
	items, err := a.media.ForProduct(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	RespondList(w, items, ListMeta{Total: len(items), Limit: len(items)})
}

// handleSetProductMedia replaces the product's media list. PUT rather than
// POST because the body is the whole list, in order — a reorder and an add are
// the same request.
func (a *App) handleSetProductMedia(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var in struct {
		MediaIDs []int64 `json:"media_ids"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	if err := a.media.SetProductMedia(r.Context(), id, in.MediaIDs); err != nil {
		RespondError(w, r, err)
		return
	}
	items, err := a.media.ForProduct(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	RespondList(w, items, ListMeta{Total: len(items), Limit: len(items)})
}

// handleSetVariantMedia nominates one of the product's images for a variant.
//
// `media_id` is a NullableID for the reason it always is: an omitted field and
// an explicit null decode to the same nil pointer, and "this variant has no
// image of its own" is a choice an operator makes rather than an absence.
func (a *App) handleSetVariantMedia(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var in struct {
		MediaID NullableID `json:"media_id"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	if !in.MediaID.Present {
		RespondError(w, r, Validationf("media_id is required; send null to clear the image"))
		return
	}
	if err := a.media.SetVariantMedia(r.Context(), id, in.MediaID.Value); err != nil {
		RespondError(w, r, err)
		return
	}
	// The variant back, so the caller sees the image the store now holds rather
	// than the one it just asked for.
	v, err := a.catalog.GetVariant(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, v)
}

// Media exposes the library so a module can attach files of its own.
func (a *App) MediaLibrary() *Media { return a.media }

// mediaUploadsEnabled reports whether this store can accept uploads, so the
// panel can show the reason rather than a button that fails.
func (a *App) mediaUploadsEnabled() bool { return a.mediaStore != nil }
