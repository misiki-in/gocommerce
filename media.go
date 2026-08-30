package gocommerce

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The media library.
//
// This is a deliberate amendment to the plan, which said image storage stays
// out of core and files are URLs only (§30). Holding bytes is a real cost —
// a directory to back up, a disk to fill, a path traversal to get wrong — and
// the reason it is worth paying is that "paste a URL" presumes the operator
// already runs somewhere to host files, which a small store does not.
//
// The compromise that keeps the plan's intent: core owns the *records* and a
// tiny storage interface, not a storage implementation. `MediaStore` has one
// implementation here — the local filesystem — and S3 remains a module that
// swaps in through Config. Nothing above this file knows where bytes live.

// Media kinds. A closed set, because the panel renders each differently and an
// unknown kind has no sensible presentation.
const (
	MediaImage = "image"
	MediaVideo = "video"
	MediaModel = "model"
)

// MaxUploadBytes bounds a single uploaded file.
const MaxUploadBytes = 64 << 20 // 64 MiB

// MediaItem is one file in the library. It may be attached to any number of
// products, which is what makes "select existing" possible.
type MediaItem struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	URL       string    `json:"url"`
	Filename  string    `json:"filename,omitempty"`
	MIME      string    `json:"mime,omitempty"`
	SizeBytes int64     `json:"size_bytes,omitempty"`
	Width     *int      `json:"width,omitempty"`
	Height    *int      `json:"height,omitempty"`
	Alt       string    `json:"alt"`
	Metadata  Metadata  `json:"metadata"`
	CreatedAt time.Time `json:"created_at"`

	// storageKey is empty for media referenced by URL. It is the handle the
	// store uses, and its emptiness is what stops the engine deleting a file
	// from somebody else's server.
	storageKey string
}

// MediaStore is where bytes live. One method to put, one to remove; reading is
// the web server's job via the URL, not this interface's.
//
// This is the seam an object-storage module replaces. It is deliberately
// smaller than an S3 client — everything richer (signed URLs, transforms,
// CDN invalidation) belongs to the implementation, not to core's vocabulary.
type MediaStore interface {
	// Put stores r under a name derived from filename and returns the key it
	// chose and the URL the file is now readable at.
	Put(ctx context.Context, filename string, contentType string, r io.Reader) (key, url string, err error)
	// Delete removes a previously stored key. Deleting an absent key is not an
	// error — the record is going away either way, and a store that refuses
	// leaves a row nobody can remove.
	Delete(ctx context.Context, key string) error
}

// ---------------------------------------------------------------- local disk

// LocalMediaStore writes into a directory the binary also serves.
type LocalMediaStore struct {
	// Dir is the directory files are written to.
	Dir string
	// Prefix is the URL path the directory is served at, e.g. "/media".
	Prefix string
}

// Put writes the file under a random name, keeping only the extension.
//
// The original filename is never used as a path. It arrives from a browser and
// may contain "..", a drive letter, or a NUL — sanitising it well enough to
// trust is harder than not trusting it, and the display name is kept in the
// database where it cannot be executed as a path.
func (s *LocalMediaStore) Put(ctx context.Context, filename, contentType string, r io.Reader) (string, string, error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return "", "", fmt.Errorf("create media directory: %w", err)
	}

	// Take the filename's extension only if it is one we serve. "shot.png.html"
	// ends in .html, and honouring that would put an HTML file on our own
	// origin — so fall back to what the declared type implies rather than
	// storing the file with no extension at all.
	ext := strings.ToLower(filepath.Ext(filename))
	if !safeExtension(ext) {
		ext = ""
		for _, candidate := range mimeExtensions(contentType) {
			if safeExtension(candidate) {
				ext = candidate
				break
			}
		}
	}

	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", "", fmt.Errorf("generate media key: %w", err)
	}
	key := hex.EncodeToString(buf[:]) + ext

	// O_EXCL so a key collision fails loudly instead of overwriting a file
	// some other product is still showing.
	f, err := os.OpenFile(filepath.Join(s.Dir, key), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", "", fmt.Errorf("create media file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, io.LimitReader(r, MaxUploadBytes+1)); err != nil {
		os.Remove(f.Name())
		return "", "", fmt.Errorf("write media file: %w", err)
	}
	return key, strings.TrimRight(s.Prefix, "/") + "/" + key, nil
}

func (s *LocalMediaStore) Delete(ctx context.Context, key string) error {
	// Reject anything that is not the flat name Put generates, so a corrupted
	// or hand-edited row cannot walk out of the media directory.
	if key == "" || key != filepath.Base(key) || strings.Contains(key, "..") {
		return fmt.Errorf("refusing to delete suspicious media key %q", key)
	}
	if err := os.Remove(filepath.Join(s.Dir, key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// mimeExtensions returns the extensions a content type implies, canonical
// first. mime.ExtensionsByType is alphabetical, which for image/jpeg puts
// ".jfif" ahead of ".jpg" — so the common ones are named explicitly and the
// system table is only a fallback.
func mimeExtensions(contentType string) []string {
	ct, _, _ := mime.ParseMediaType(contentType)
	switch ct {
	case "image/jpeg":
		return []string{".jpg"}
	case "image/png":
		return []string{".png"}
	case "image/gif":
		return []string{".gif"}
	case "image/webp":
		return []string{".webp"}
	case "image/avif":
		return []string{".avif"}
	case "video/mp4":
		return []string{".mp4"}
	case "video/webm":
		return []string{".webm"}
	case "video/quicktime":
		return []string{".mov"}
	case "model/gltf-binary":
		return []string{".glb"}
	case "model/gltf+json":
		return []string{".gltf"}
	case "model/vnd.usdz+zip":
		return []string{".usdz"}
	}
	exts, _ := mime.ExtensionsByType(ct)
	return exts
}

// safeExtension allows only what a storefront actually renders. An upload is
// served back from this store's own origin, so an .html or .svg would be a
// stored cross-site scripting vector rather than a picture.
func safeExtension(ext string) bool {
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif",
		".mp4", ".webm", ".mov",
		".glb", ".gltf", ".usdz":
		return true
	}
	return false
}

// kindForMIME maps a content type onto the closed set of kinds.
func kindForMIME(contentType, filename string) (string, bool) {
	ct, _, _ := mime.ParseMediaType(contentType)
	switch {
	case strings.HasPrefix(ct, "image/"):
		return MediaImage, true
	case strings.HasPrefix(ct, "video/"):
		return MediaVideo, true
	case ct == "model/gltf-binary", ct == "model/gltf+json", ct == "model/vnd.usdz+zip":
		return MediaModel, true
	}
	// Browsers send application/octet-stream for 3D files often enough that
	// the extension is the more reliable signal for them.
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".glb", ".gltf", ".usdz":
		return MediaModel, true
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif":
		return MediaImage, true
	case ".mp4", ".webm", ".mov":
		return MediaVideo, true
	}
	return "", false
}

// ------------------------------------------------------------------ service

// Media is the library service.
type Media struct {
	app *App
}

const mediaColumns = `id, kind, url, storage_key, filename, mime, size_bytes,
	width, height, alt, metadata, created_at`

func scanMedia(row interface{ Scan(...any) error }) (*MediaItem, error) {
	var m MediaItem
	var meta []byte
	if err := row.Scan(&m.ID, &m.Kind, &m.URL, &m.storageKey, &m.Filename, &m.MIME,
		&m.SizeBytes, &m.Width, &m.Height, &m.Alt, &meta, &m.CreatedAt); err != nil {
		return nil, err
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &m.Metadata)
	}
	if m.Metadata == nil {
		m.Metadata = Metadata{}
	}
	return &m, nil
}

// Upload stores the bytes and records them.
//
// The file is written before the row is inserted, so a failed insert can leave
// an orphaned file — which is the right way round. The opposite order risks a
// row pointing at nothing, and a broken image in a storefront is worse than a
// byte of wasted disk that a sweep can find later.
func (m *Media) Upload(ctx context.Context, filename, contentType string, r io.Reader, size int64) (*MediaItem, error) {
	if m.app.mediaStore == nil {
		return nil, &APIError{
			Status:  501,
			Code:    "media_store_unconfigured",
			Message: "this store has nowhere to put uploads; set Config.MediaDir or Config.MediaStore",
		}
	}
	if size > MaxUploadBytes {
		return nil, Validationf("that file is %s; the limit is %s",
			humanBytes(size), humanBytes(MaxUploadBytes))
	}
	kind, ok := kindForMIME(contentType, filename)
	if !ok {
		return nil, Validationf("%q is not an image, a video, or a 3D model", filename)
	}

	key, url, err := m.app.mediaStore.Put(ctx, filename, contentType, r)
	if err != nil {
		return nil, Internalf(err, "store the upload")
	}

	item, err := m.insert(ctx, &MediaItem{
		Kind: kind, URL: url, storageKey: key,
		Filename: filepath.Base(filename), MIME: contentType, SizeBytes: size,
	})
	if err != nil {
		_ = m.app.mediaStore.Delete(ctx, key)
		return nil, err
	}
	return item, nil
}

// AddURL records media this store does not hold. No storage key, so Delete
// only ever removes the row.
func (m *Media) AddURL(ctx context.Context, url, kind, alt string) (*MediaItem, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil, Validationf("a url is required")
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "/") {
		return nil, Validationf("a media url must be absolute or site-relative")
	}
	switch kind {
	case MediaImage, MediaVideo, MediaModel:
	case "":
		// Read it off the URL rather than assuming an image. A caller that
		// pastes a link and says nothing else is the common case, and calling
		// a .mp4 an image makes the panel render an <img> for it — a broken
		// thumbnail whose cause is invisible. Image stays the fallback for a
		// URL whose path says nothing, which is what a CDN link often is.
		kind = kindForURL(url)
	default:
		return nil, Validationf("kind must be image, video or model")
	}
	return m.insert(ctx, &MediaItem{
		Kind: kind, URL: url, Alt: alt, Filename: filenameForURL(url),
	})
}

// filenameForURL is the last path segment, for display only.
//
// It is set here rather than left to each client because every one of them
// needs the same thing — something to put under a thumbnail — and "media 47" is
// what they fall back to otherwise. Nothing keys off it: deletion goes by
// storage_key, which stays empty for linked media, so a name taken from
// somebody else's URL can never point Delete at a local file.
func filenameForURL(url string) string {
	path := url
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	name := path[strings.LastIndexByte(path, '/')+1:]
	// A URL ending in a slash, or one that is all host, has no segment to use.
	// Empty is the honest answer and the clients already handle it.
	if name == "." || name == ".." {
		return ""
	}
	return name
}

// kindForURL guesses a kind from a URL's path extension, defaulting to image.
//
// The query string and fragment are cut first: a signed CDN link ends in
// "?sig=…" far more often than in ".jpg", and matching the extension against
// the whole URL would find nothing on exactly the links people paste most.
func kindForURL(url string) string {
	path := url
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	if kind, ok := kindForMIME("", path); ok {
		return kind
	}
	return MediaImage
}

func (m *Media) insert(ctx context.Context, in *MediaItem) (*MediaItem, error) {
	meta, err := json.Marshal(in.Metadata)
	if err != nil || in.Metadata == nil {
		meta = []byte(`{}`)
	}
	row := m.app.db.QueryRowContext(ctx, `
		INSERT INTO media (kind, url, storage_key, filename, mime, size_bytes, width, height, alt, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING `+mediaColumns,
		in.Kind, in.URL, in.storageKey, in.Filename, in.MIME, in.SizeBytes,
		in.Width, in.Height, in.Alt, meta)
	item, err := scanMedia(row)
	if err != nil {
		return nil, Internalf(err, "record the media")
	}
	return item, nil
}

// Usage filters a listing by whether anything is displaying the file.
const (
	// UsedAnywhere matches media attached to at least one product.
	UsedAnywhere = "used"
	// UsedNowhere matches media nothing displays — the orphans worth tidying,
	// and the reason this filter exists at all.
	UsedNowhere = "unused"
)

// MediaQuery narrows a library listing. Every field is optional; a zero one
// does not narrow the result.
type MediaQuery struct {
	// Search matches the filename, the alt text or the URL, case-insensitively.
	// Not full text: a library is names, not prose, and a substring is what
	// somebody typing half a filename means.
	Search string
	Kind   string
	// Usage is UsedAnywhere or UsedNowhere. Anything else does not narrow.
	Usage string
	// ProductID restricts the listing to what one product displays.
	ProductID int64
	// MinBytes and MaxBytes bound the file size. MaxBytes of zero is no upper
	// bound, which is what makes "over 5 MB" expressible without a sentinel.
	MinBytes int64
	MaxBytes int64
	Limit    int
	Offset   int
}

// List returns the library, newest first — which is what "select existing"
// wants, because the thing you just uploaded is usually the thing you want.
func (m *Media) List(ctx context.Context, q MediaQuery) ([]*MediaItem, int, error) {
	where, args := []string{"1 = 1"}, []any{}
	add := func(clause string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}

	if q.Kind != "" {
		add("kind = $%d", q.Kind)
	}
	if s := strings.TrimSpace(q.Search); s != "" {
		// One placeholder used three times: the driver sends the value once and
		// the three columns are searched with it.
		args = append(args, "%"+strings.ToLower(s)+"%")
		where = append(where, fmt.Sprintf(
			"(lower(filename) LIKE $%d OR lower(alt) LIKE $%d OR lower(url) LIKE $%d)",
			len(args), len(args), len(args)))
	}
	if q.MinBytes > 0 {
		add("size_bytes >= $%d", q.MinBytes)
	}
	if q.MaxBytes > 0 {
		add("size_bytes <= $%d", q.MaxBytes)
	}
	if q.ProductID > 0 {
		add("EXISTS (SELECT 1 FROM product_media pm"+
			" WHERE pm.media_id = media.id AND pm.product_id = $%d)", q.ProductID)
	}
	switch q.Usage {
	case UsedAnywhere:
		where = append(where, "EXISTS (SELECT 1 FROM product_media pm WHERE pm.media_id = media.id)")
	case UsedNowhere:
		where = append(where, "NOT EXISTS (SELECT 1 FROM product_media pm WHERE pm.media_id = media.id)")
	}
	clause := " WHERE " + strings.Join(where, " AND ")

	var total int
	if err := m.app.db.QueryRowContext(ctx,
		`SELECT count(*) FROM media`+clause, args...).Scan(&total); err != nil {
		return nil, 0, Internalf(err, "count media")
	}

	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	rows, err := m.app.db.QueryContext(ctx,
		`SELECT `+mediaColumns+` FROM media`+clause+
			fmt.Sprintf(" ORDER BY id DESC LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2),
		append(args, limit, q.Offset)...)
	if err != nil {
		return nil, 0, Internalf(err, "list media")
	}
	defer rows.Close()

	out := []*MediaItem{}
	for rows.Next() {
		item, err := scanMedia(rows)
		if err != nil {
			return nil, 0, Internalf(err, "scan media")
		}
		out = append(out, item)
	}
	return out, total, rows.Err()
}

func (m *Media) Get(ctx context.Context, id int64) (*MediaItem, error) {
	item, err := scanMedia(m.app.db.QueryRowContext(ctx,
		`SELECT `+mediaColumns+` FROM media WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NotFoundf("media %d not found", id)
	}
	if err != nil {
		return nil, Internalf(err, "read media")
	}
	return item, nil
}

// SetAlt updates the alt text, which is the only field of a stored file that
// is worth editing — everything else describes bytes that did not change.
func (m *Media) SetAlt(ctx context.Context, id int64, alt string) (*MediaItem, error) {
	item, err := scanMedia(m.app.db.QueryRowContext(ctx,
		`UPDATE media SET alt = $2 WHERE id = $1 RETURNING `+mediaColumns, id, alt))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NotFoundf("media %d not found", id)
	}
	if err != nil {
		return nil, Internalf(err, "update media")
	}
	return item, nil
}

// Delete removes the record and, if this store holds the file, the file.
//
// product_media references media with ON DELETE RESTRICT, so deleting an item
// six products still display fails at the constraint rather than stripping
// them silently. That error is translated here into a sentence that says what
// to do about it.
func (m *Media) Delete(ctx context.Context, id int64) error {
	item, err := m.Get(ctx, id)
	if err != nil {
		return err
	}

	var used int
	if err := m.app.db.QueryRowContext(ctx,
		`SELECT count(*) FROM product_media WHERE media_id = $1`, id).Scan(&used); err != nil {
		return Internalf(err, "count media usage")
	}
	if used > 0 {
		return Conflictf("that media is still on %d %s; remove it from them first",
			used, pluralWord(used, "product"))
	}

	if _, err := m.app.db.ExecContext(ctx, `DELETE FROM media WHERE id = $1`, id); err != nil {
		return Internalf(err, "delete media")
	}
	// The row is gone; a file left behind is recoverable waste, whereas a row
	// pointing at a deleted file is a broken image. Log and move on.
	if item.storageKey != "" && m.app.mediaStore != nil {
		if err := m.app.mediaStore.Delete(ctx, item.storageKey); err != nil {
			m.app.log.Error("media file left behind", "id", id, "key", item.storageKey, "error", err)
		}
	}
	return nil
}

// ------------------------------------------------------- product attachment

// ProductMedia is one library item as it appears on a product.
type ProductMedia struct {
	*MediaItem
	Position  int    `json:"position"`
	VariantID *int64 `json:"variant_id,omitempty"`
}

// SetProductMedia replaces a product's media list in one transaction.
//
// Replace rather than append: the panel sends the order it is showing, and a
// reorder is the same operation as an add. Two calls that each mutate half of
// it would let a drag-and-drop leave the list in a state neither the operator
// nor the database intended.
func (m *Media) SetProductMedia(ctx context.Context, productID int64, mediaIDs []int64) error {
	return InTx(ctx, m.app.db, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM products WHERE id = $1)`, productID).Scan(&exists); err != nil {
			return Internalf(err, "check product")
		}
		if !exists {
			return NotFoundf("product %d not found", productID)
		}

		// Remove what is no longer in the list, rather than clearing the lot and
		// rebuilding it. The difference matters: `variant_id` lives on these
		// rows, so a delete-and-reinsert silently dropped every variant image
		// each time somebody added a file or dragged one into a new position.
		// Upserting leaves a surviving row — and its variant — alone.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM product_media
			WHERE product_id = $1 AND media_id <> ALL($2::bigint[])`,
			productID, int64Array(mediaIDs)); err != nil {
			return Internalf(err, "clear product media")
		}
		for i, id := range mediaIDs {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO product_media (product_id, media_id, position)
				VALUES ($1, $2, $3)
				ON CONFLICT (product_id, media_id) DO UPDATE SET position = EXCLUDED.position`,
				productID, id, i); err != nil {
				if isForeignKeyViolation(err) {
					return Validationf("media %d does not exist", id)
				}
				return Internalf(err, "attach media")
			}
		}
		return nil
	})
}

// SetVariantMedia points a variant at one of its product's images, or clears
// the assignment when mediaID is nil.
//
// The media must already be attached to the variant's product. That is the
// whole model: a variant does not own a picture, it *nominates* one of the
// product's — which is what lets a storefront swap image as a shopper picks a
// colour without the file existing twice.
func (m *Media) SetVariantMedia(ctx context.Context, variantID int64, mediaID *int64) error {
	return InTx(ctx, m.app.db, func(tx *sql.Tx) error {
		var productID int64
		err := tx.QueryRowContext(ctx,
			`SELECT product_id FROM variants WHERE id = $1`, variantID).Scan(&productID)
		if errors.Is(err, sql.ErrNoRows) {
			return NotFoundf("variant %d not found", variantID)
		}
		if err != nil {
			return Internalf(err, "read variant")
		}

		// Always clear first. A variant shows one image, and the unique index
		// added in M9 would reject the second assignment rather than replace
		// it — so the replace has to be spelled out.
		if _, err := tx.ExecContext(ctx, `
			UPDATE product_media SET variant_id = NULL
			WHERE product_id = $1 AND variant_id = $2`, productID, variantID); err != nil {
			return Internalf(err, "clear variant media")
		}
		if mediaID == nil {
			return nil
		}

		res, err := tx.ExecContext(ctx, `
			UPDATE product_media SET variant_id = $1
			WHERE product_id = $2 AND media_id = $3`, variantID, productID, *mediaID)
		if err != nil {
			return Internalf(err, "set variant media")
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Attaching it to the product first would be the friendly thing and
			// the wrong one: the product's media list is ordered, and adding to
			// it from here would put a file in a position nobody chose.
			return Validationf("media %d is not on this product; add it to the product first", *mediaID)
		}
		return nil
	})
}

// ForProduct returns a product's media in display order.
func (m *Media) ForProduct(ctx context.Context, productID int64) ([]*ProductMedia, error) {
	rows, err := m.app.db.QueryContext(ctx, `
		SELECT `+prefixColumns(mediaColumns, "m")+`, pm.position, pm.variant_id
		FROM product_media pm
		JOIN media m ON m.id = pm.media_id
		WHERE pm.product_id = $1
		ORDER BY pm.position, m.id`, productID)
	if err != nil {
		return nil, Internalf(err, "read product media")
	}
	defer rows.Close()

	out := []*ProductMedia{}
	for rows.Next() {
		var (
			item      MediaItem
			meta      []byte
			pos       int
			variantID *int64
		)
		if err := rows.Scan(&item.ID, &item.Kind, &item.URL, &item.storageKey, &item.Filename,
			&item.MIME, &item.SizeBytes, &item.Width, &item.Height, &item.Alt, &meta,
			&item.CreatedAt, &pos, &variantID); err != nil {
			return nil, Internalf(err, "scan product media")
		}
		if len(meta) > 0 {
			_ = json.Unmarshal(meta, &item.Metadata)
		}
		if item.Metadata == nil {
			item.Metadata = Metadata{}
		}
		out = append(out, &ProductMedia{MediaItem: &item, Position: pos, VariantID: variantID})
	}
	return out, rows.Err()
}

// prefixColumns qualifies a column list with a table alias, so the shared
// column constant can be reused in a join without being written twice and
// drifting.
func prefixColumns(cols, alias string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func pluralWord(n int, singular string) string {
	if n == 1 {
		return singular
	}
	return singular + "s"
}

// sortedInts is used by tests and by SetProductMedia's callers to make an id
// list deterministic before comparison.
func sortedInts(in []int64) []int64 {
	out := append([]int64(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
