package gocommerce

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The media library holds bytes, which is the first thing in this engine that
// can be got wrong in a way a database constraint cannot catch: a file written
// outside its directory, an upload served back as markup, a picture deleted
// out from under six products. Each of those is a test below.

// mediaApp boots a store that can hold files.
//
// MediaDir goes through Config rather than being attached afterwards, because
// the file-serving route is mounted during New — a store assembled after boot
// records media happily and then 404s every URL it hands out, which is exactly
// the bug this helper existed to hide the first time it was written.
func mediaApp(t *testing.T) (*App, string) {
	t.Helper()
	dsn := requireDB(t)
	resetSchema(t, dsn)
	dir := t.TempDir()

	cfg := testConfig(dsn)
	cfg.MediaDir = dir

	app, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app, dir
}

// rawBody attaches an already-encoded body to a request built by do, which
// leaves it bodyless.
func rawBody(buf *bytes.Buffer) func(*http.Request) {
	encoded := buf.Bytes()
	return func(r *http.Request) {
		r.Body = io.NopCloser(bytes.NewReader(encoded))
		r.ContentLength = int64(len(encoded))
	}
}

func uploadBody(t *testing.T, filename, contentType string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="file"; filename="` + filename + `"`}
	h["Content-Type"] = []string{contentType}
	part, err := w.CreatePart(h)
	if err != nil {
		t.Fatalf("create multipart part: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	w.Close()
	return &buf, w.FormDataContentType()
}

func TestMediaUploadStoresAndServes(t *testing.T) {
	app, dir := mediaApp(t)
	ctx := context.Background()

	item, err := app.MediaLibrary().Upload(ctx, "photo.png", "image/png",
		strings.NewReader("not really a png"), 16)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	if item.Kind != MediaImage {
		t.Errorf("kind = %q, want %q", item.Kind, MediaImage)
	}
	if item.Filename != "photo.png" {
		t.Errorf("filename = %q, want the original name kept for display", item.Filename)
	}
	// The stored name must not be the supplied one — that is what makes a
	// hostile filename harmless.
	if strings.Contains(item.URL, "photo.png") {
		t.Errorf("url %q reuses the uploaded filename as a path", item.URL)
	}
	if !strings.HasPrefix(item.URL, "/media/") || !strings.HasSuffix(item.URL, ".png") {
		t.Errorf("url = %q, want /media/<random>.png", item.URL)
	}

	written, err := os.ReadDir(dir)
	if err != nil || len(written) != 1 {
		t.Fatalf("expected exactly one file on disk, got %d (%v)", len(written), err)
	}

	// And it comes back over HTTP, cached hard and never sniffed.
	rec := do(t, app, http.MethodGet, item.URL)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", item.URL, rec.Code, rec.Body)
	}
	if got := rec.Body.String(); got != "not really a png" {
		t.Errorf("served %q, want the bytes that were uploaded", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff — an upload must never be sniffed into markup", got)
	}
	if !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("Cache-Control = %q, want immutable: the key is random per upload",
			rec.Header().Get("Cache-Control"))
	}
}

// A filename is attacker-controlled. It is never used as a path, and this
// pins that: the traversal attempt lands as one flat file in the directory.
func TestMediaUploadIgnoresHostileFilenames(t *testing.T) {
	app, dir := mediaApp(t)
	ctx := context.Background()

	for _, name := range []string{
		`../../escape.png`,
		`..\..\escape.png`,
		`C:\Windows\System32\evil.png`,
		`sneaky.png.html`,
	} {
		if _, err := app.MediaLibrary().Upload(ctx, name, "image/png",
			strings.NewReader("x"), 1); err != nil {
			t.Fatalf("upload %q: %v", name, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read media dir: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("expected 4 files in the media directory, found %d", len(entries))
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("upload created a directory %q", e.Name())
		}
		if strings.ContainsAny(e.Name(), `/\`) || strings.Contains(e.Name(), "..") {
			t.Errorf("stored name %q still carries path syntax", e.Name())
		}
		// "sneaky.png.html" must not keep its .html — that would be an HTML
		// file served from the panel's own origin. It should fall back to the
		// extension the declared image/png implies.
		if ext := filepath.Ext(e.Name()); !safeExtension(ext) {
			t.Errorf("stored %q with extension %q, which is not on the allow-list", e.Name(), ext)
		}
	}

	// Nothing escaped upwards.
	if _, err := os.Stat(filepath.Join(dir, "..", "escape.png")); err == nil {
		t.Fatal("a file was written outside the media directory")
	}
}

func TestMediaRejectsUnsupportedKinds(t *testing.T) {
	app, _ := mediaApp(t)
	ctx := context.Background()

	for _, tc := range []struct{ name, ct string }{
		{"script.js", "application/javascript"},
		{"page.html", "text/html"},
		{"sheet.csv", "text/csv"},
	} {
		if _, err := app.MediaLibrary().Upload(ctx, tc.name, tc.ct, strings.NewReader("x"), 1); err == nil {
			t.Errorf("uploading %s (%s) was allowed; images, video and 3D models only", tc.name, tc.ct)
		}
	}

	// The three kinds that are allowed, including a 3D model a browser sends
	// as octet-stream.
	for _, tc := range []struct {
		name, ct, want string
	}{
		{"a.png", "image/png", MediaImage},
		{"b.mp4", "video/mp4", MediaVideo},
		{"c.glb", "application/octet-stream", MediaModel},
	} {
		item, err := app.MediaLibrary().Upload(ctx, tc.name, tc.ct, strings.NewReader("x"), 1)
		if err != nil {
			t.Errorf("upload %s: %v", tc.name, err)
			continue
		}
		if item.Kind != tc.want {
			t.Errorf("%s classified as %q, want %q", tc.name, item.Kind, tc.want)
		}
	}
}

// "Select existing" is the whole reason media is a library rather than a
// per-product list: one file, many products.
func TestMediaIsReusedAcrossProducts(t *testing.T) {
	app, _ := mediaApp(t)
	ctx := context.Background()

	shared, err := app.MediaLibrary().AddURL(ctx, "https://cdn.example/shared.jpg", MediaImage, "A shared shot")
	if err != nil {
		t.Fatalf("add by url: %v", err)
	}
	one := simpleProduct(t, app, "MEDIA-1", 1000, 5)
	two := simpleProduct(t, app, "MEDIA-2", 1000, 5)

	for _, p := range []*Product{one, two} {
		if err := app.MediaLibrary().SetProductMedia(ctx, p.ID, []int64{shared.ID}); err != nil {
			t.Fatalf("attach to product %d: %v", p.ID, err)
		}
	}

	for _, p := range []*Product{one, two} {
		items, err := app.MediaLibrary().ForProduct(ctx, p.ID)
		if err != nil {
			t.Fatalf("read product media: %v", err)
		}
		if len(items) != 1 || items[0].ID != shared.ID {
			t.Errorf("product %d has %d media, want the shared one", p.ID, len(items))
		}
	}

	// Deleting a library item two products still show must be refused, not
	// silently strip them.
	err = app.MediaLibrary().Delete(ctx, shared.ID)
	if err == nil {
		t.Fatal("deleted media that two products still display")
	}
	if !strings.Contains(err.Error(), "2 products") {
		t.Errorf("error = %q, want it to say how many products are using it", err.Error())
	}

	// Detach everywhere, and it deletes.
	for _, p := range []*Product{one, two} {
		if err := app.MediaLibrary().SetProductMedia(ctx, p.ID, nil); err != nil {
			t.Fatalf("detach: %v", err)
		}
	}
	if err := app.MediaLibrary().Delete(ctx, shared.ID); err != nil {
		t.Errorf("delete after detaching: %v", err)
	}
}

// SetProductMedia replaces rather than appends, and the order it is given is
// the order it returns — a drag-and-drop reorder is the same call as an add.
func TestSetProductMediaReplacesAndOrders(t *testing.T) {
	app, _ := mediaApp(t)
	ctx := context.Background()
	p := simpleProduct(t, app, "MEDIA-ORDER", 1000, 5)

	var ids []int64
	for _, name := range []string{"a", "b", "c"} {
		item, err := app.MediaLibrary().AddURL(ctx, "https://cdn.example/"+name+".jpg", MediaImage, name)
		if err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
		ids = append(ids, item.ID)
	}

	if err := app.MediaLibrary().SetProductMedia(ctx, p.ID, ids); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, _ := app.MediaLibrary().ForProduct(ctx, p.ID)
	if len(got) != 3 {
		t.Fatalf("got %d media, want 3", len(got))
	}

	// Reverse it: same call, and the result follows.
	reversed := []int64{ids[2], ids[1], ids[0]}
	if err := app.MediaLibrary().SetProductMedia(ctx, p.ID, reversed); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	got, _ = app.MediaLibrary().ForProduct(ctx, p.ID)
	for i, want := range reversed {
		if got[i].ID != want {
			t.Errorf("position %d holds media %d, want %d", i, got[i].ID, want)
		}
	}

	// A shorter list removes the rest rather than merging.
	if err := app.MediaLibrary().SetProductMedia(ctx, p.ID, ids[:1]); err != nil {
		t.Fatalf("shrink: %v", err)
	}
	if got, _ = app.MediaLibrary().ForProduct(ctx, p.ID); len(got) != 1 {
		t.Errorf("after replacing with one id the product has %d media, want 1", len(got))
	}

	// An id that does not exist is the caller's mistake, not a 500.
	err := app.MediaLibrary().SetProductMedia(ctx, p.ID, []int64{999999})
	if err == nil {
		t.Fatal("attaching a nonexistent media id succeeded")
	}
	if apiErr, ok := err.(*APIError); !ok || apiErr.Status != http.StatusBadRequest {
		t.Errorf("error = %v, want a 400 validation error", err)
	}
}

// The read route exists because the write route replaces everything. Without
// it a client cannot construct a correct list, and "add one photo" quietly
// becomes "delete the other four".
func TestProductMediaCanBeReadBeforeItIsWritten(t *testing.T) {
	app, _ := mediaApp(t)
	ctx := context.Background()
	p := simpleProduct(t, app, "READ-MEDIA", 1000, 5)

	path := "/api/admin/products/" + strconv.FormatInt(p.ID, 10) + "/media"

	rec := do(t, app, http.MethodGet, path, withAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Errorf("a product with no media should return an empty list, got %s", rec.Body)
	}

	var ids []int64
	for _, name := range []string{"one", "two"} {
		item, err := app.MediaLibrary().AddURL(ctx, "https://cdn.example/"+name+".jpg", MediaImage, name)
		if err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
		ids = append(ids, item.ID)
	}
	if err := app.MediaLibrary().SetProductMedia(ctx, p.ID, ids); err != nil {
		t.Fatalf("attach: %v", err)
	}

	rec = do(t, app, http.MethodGet, path, withAdmin)
	var body struct {
		Data []struct {
			ID       int64  `json:"id"`
			Position int    `json:"position"`
			Alt      string `json:"alt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if len(body.Data) != 2 {
		t.Fatalf("read back %d media, want 2", len(body.Data))
	}
	// Order is the contract — it is what the client sends back on the next PUT.
	if body.Data[0].ID != ids[0] || body.Data[1].ID != ids[1] {
		t.Errorf("read back in the wrong order: %+v", body.Data)
	}
	if body.Data[0].Position != 0 || body.Data[1].Position != 1 {
		t.Errorf("positions = %d, %d; want 0, 1", body.Data[0].Position, body.Data[1].Position)
	}

	// A product that does not exist is a 404, not an empty list — a client
	// cannot tell "no media" from "no product" otherwise.
	rec = do(t, app, http.MethodGet, "/api/admin/products/999999/media", withAdmin)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET media for a missing product = %d, want 404", rec.Code)
	}
	if rec := do(t, app, http.MethodGet, path); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated read = %d, want 401", rec.Code)
	}
}

func TestMediaUploadOverHTTP(t *testing.T) {
	app, _ := mediaApp(t)

	body, contentType := uploadBody(t, "shot.jpg", "image/jpeg", []byte("jpegbytes"))
	rec := do(t, app, http.MethodPost, "/api/admin/media", withAdmin,
		header("Content-Type", contentType),
		rawBody(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"kind":"image"`) {
		t.Errorf("response does not classify the upload: %s", rec.Body)
	}
	// The storage key is an internal handle; it must not leak to a client.
	if strings.Contains(rec.Body.String(), "storage_key") {
		t.Errorf("response exposes the storage key: %s", rec.Body)
	}

	// Uploads need admin auth like everything else under /api/admin.
	body2, ct2 := uploadBody(t, "shot.jpg", "image/jpeg", []byte("x"))
	rec = do(t, app, http.MethodPost, "/api/admin/media",
		header("Content-Type", ct2),
		rawBody(body2))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated upload = %d, want 401", rec.Code)
	}
}

// A store with nowhere to put files still runs; it just says so, rather than
// offering an upload that fails obscurely.
func TestUploadsDisabledWithoutAStore(t *testing.T) {
	app := newTestApp(t)
	if app.mediaUploadsEnabled() {
		t.Fatal("a test app without MediaDir reports uploads as available")
	}
	_, err := app.MediaLibrary().Upload(context.Background(), "a.png", "image/png",
		strings.NewReader("x"), 1)
	if err == nil {
		t.Fatal("upload succeeded with no media store configured")
	}
	if !strings.Contains(err.Error(), "nowhere to put") {
		t.Errorf("error = %q, want it to name the missing configuration", err.Error())
	}

	// Recording media by URL still works, which is the documented fallback.
	if _, err := app.MediaLibrary().AddURL(context.Background(),
		"https://cdn.example/x.jpg", MediaImage, ""); err != nil {
		t.Errorf("AddURL without a store: %v", err)
	}
}

// TestAddURLInfersKind pins the reading of an omitted kind. The panel's "Add
// media from URL" is a single box with no kind picker, so if the engine called
// everything an image a linked .mp4 would come back as one and the panel would
// render an <img> for it — a broken thumbnail with no visible cause.
func TestAddURLInfersKind(t *testing.T) {
	app, _ := mediaApp(t)
	ctx := context.Background()

	for _, tc := range []struct {
		url  string
		want string
	}{
		{"https://cdn.example/shirt.jpg", MediaImage},
		{"https://cdn.example/shirt.PNG", MediaImage},
		{"https://cdn.example/clip.mp4", MediaVideo},
		{"https://cdn.example/clip.webm", MediaVideo},
		{"https://cdn.example/chair.glb", MediaModel},
		{"https://cdn.example/chair.usdz", MediaModel},
		// The query string is cut before the extension is read: a signed CDN
		// link ends in a signature far more often than in ".mp4".
		{"https://cdn.example/clip.mp4?sig=abc123&expires=99", MediaVideo},
		{"https://cdn.example/shot.jpg#hero", MediaImage},
		// Nothing to go on. Image is the fallback because that is what a
		// bare CDN path almost always is.
		{"https://cdn.example/asset/9f3b21", MediaImage},
		// A dot in a directory must not be mistaken for the file's extension.
		{"https://cdn.example/v1.2/asset", MediaImage},
	} {
		item, err := app.MediaLibrary().AddURL(ctx, tc.url, "", "")
		if err != nil {
			t.Fatalf("AddURL(%q): %v", tc.url, err)
		}
		if item.Kind != tc.want {
			t.Errorf("AddURL(%q) kind = %q, want %q", tc.url, item.Kind, tc.want)
		}
	}

	// The last path segment rides along as a display name, so a thumbnail has
	// something to sit under other than "media 47".
	named, err := app.MediaLibrary().AddURL(ctx, "https://cdn.example/a/b/hero-shot.jpg?v=2", "", "")
	if err != nil {
		t.Fatalf("AddURL: %v", err)
	}
	if named.Filename != "hero-shot.jpg" {
		t.Errorf("filename = %q, want hero-shot.jpg", named.Filename)
	}
	// Nothing keys off it, so a URL with no segment to use is simply unnamed
	// rather than an error.
	bare, err := app.MediaLibrary().AddURL(ctx, "https://cdn.example/", "", "")
	if err != nil {
		t.Fatalf("AddURL: %v", err)
	}
	if bare.Filename != "" {
		t.Errorf("filename = %q, want it empty", bare.Filename)
	}
	if bare.storageKey != "" {
		t.Error("linked media got a storage key; Delete could then remove a local file")
	}

	// An explicit kind still wins over the guess — a .jpg that is really the
	// poster frame of a video is the caller's call to make.
	item, err := app.MediaLibrary().AddURL(ctx, "https://cdn.example/poster.jpg", MediaVideo, "")
	if err != nil {
		t.Fatalf("AddURL with an explicit kind: %v", err)
	}
	if item.Kind != MediaVideo {
		t.Errorf("explicit kind = %q, want it honoured", item.Kind)
	}
	if _, err := app.MediaLibrary().AddURL(ctx, "https://cdn.example/x.jpg", "audio", ""); !errors.Is(err, ErrValidation) {
		t.Errorf("unknown kind = %v, want a validation error", err)
	}
}

// TestMediaListFilters covers the picker's filter row. Each of these narrows in
// the store rather than in the panel, which is the whole point: filtering the
// page already on screen answers a different question, and answers it wrong the
// moment the library is bigger than one page.
func TestMediaListFilters(t *testing.T) {
	app, _ := mediaApp(t)
	ctx := context.Background()
	lib := app.MediaLibrary()

	shirt, err := lib.AddURL(ctx, "https://cdn.example/blue-shirt.jpg", "", "A blue shirt")
	if err != nil {
		t.Fatalf("AddURL: %v", err)
	}
	clip, err := lib.AddURL(ctx, "https://cdn.example/promo-clip.mp4", "", "")
	if err != nil {
		t.Fatalf("AddURL: %v", err)
	}
	if _, err := lib.AddURL(ctx, "https://cdn.example/chair.glb", "", ""); err != nil {
		t.Fatalf("AddURL: %v", err)
	}

	// Two products, so "on this product" is narrower than "used anywhere".
	price := int64(1000)
	mine, err := app.Products().CreateProduct(ctx, ProductInput{
		Title: "Mine", SKU: "MF-1", PriceMinor: &price,
	})
	if err != nil {
		t.Fatalf("create product: %v", err)
	}
	theirs, err := app.Products().CreateProduct(ctx, ProductInput{
		Title: "Theirs", SKU: "MF-2", PriceMinor: &price,
	})
	if err != nil {
		t.Fatalf("create product: %v", err)
	}
	if err := lib.SetProductMedia(ctx, mine.ID, []int64{shirt.ID}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := lib.SetProductMedia(ctx, theirs.ID, []int64{clip.ID}); err != nil {
		t.Fatalf("attach: %v", err)
	}

	names := func(q MediaQuery) []string {
		t.Helper()
		items, total, err := lib.List(ctx, q)
		if err != nil {
			t.Fatalf("List(%+v): %v", q, err)
		}
		if total != len(items) {
			t.Errorf("total %d disagrees with the %d rows returned", total, len(items))
		}
		out := make([]string, 0, len(items))
		for _, i := range items {
			out = append(out, i.Filename)
		}
		return out
	}

	for _, tc := range []struct {
		name string
		q    MediaQuery
		want []string
	}{
		{"everything", MediaQuery{}, []string{"chair.glb", "promo-clip.mp4", "blue-shirt.jpg"}},
		{"kind", MediaQuery{Kind: MediaVideo}, []string{"promo-clip.mp4"}},
		// The search covers filename, alt and url — "blue" is only in two of
		// the three, which is what proves it is not just matching the name.
		{"search by name", MediaQuery{Search: "clip"}, []string{"promo-clip.mp4"}},
		{"search by alt", MediaQuery{Search: "A BLUE"}, []string{"blue-shirt.jpg"}},
		{"search by url", MediaQuery{Search: "cdn.example/chair"}, []string{"chair.glb"}},
		{"search misses", MediaQuery{Search: "nothing-like-this"}, []string{}},
		{"used anywhere", MediaQuery{Usage: UsedAnywhere}, []string{"promo-clip.mp4", "blue-shirt.jpg"}},
		{"used nowhere", MediaQuery{Usage: UsedNowhere}, []string{"chair.glb"}},
		{"on one product", MediaQuery{ProductID: mine.ID}, []string{"blue-shirt.jpg"}},
		// Filters compose rather than replace each other.
		{"kind and usage", MediaQuery{Kind: MediaVideo, Usage: UsedNowhere}, []string{}},
		{"kind and search", MediaQuery{Kind: MediaImage, Search: "shirt"}, []string{"blue-shirt.jpg"}},
	} {
		if got := names(tc.q); !equalStrings(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}

	// Size bounds are only meaningful for files this store holds; linked media
	// reports zero bytes, so an upper bound must not sweep all of it in.
	uploaded, err := lib.Upload(ctx, "big.png", "image/png",
		strings.NewReader(strings.Repeat("x", 2048)), 2048)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := names(MediaQuery{MinBytes: 1024}); !equalStrings(got, []string{uploaded.Filename}) {
		t.Errorf("min_bytes: got %v, want only the uploaded file", got)
	}
	if got := names(MediaQuery{MinBytes: 1, MaxBytes: 1023}); !equalStrings(got, []string{}) {
		t.Errorf("a range around nothing: got %v, want empty", got)
	}
}

// TestMediaListRoute checks the query string the panel actually sends, and that
// a bad filter is refused rather than silently ignored — a filter that quietly
// matches everything is indistinguishable from one that worked.
func TestMediaListRoute(t *testing.T) {
	app, _ := mediaApp(t)
	ctx := context.Background()
	if _, err := app.MediaLibrary().AddURL(ctx, "https://cdn.example/a.mp4", "", ""); err != nil {
		t.Fatalf("AddURL: %v", err)
	}
	if _, err := app.MediaLibrary().AddURL(ctx, "https://cdn.example/b.jpg", "", ""); err != nil {
		t.Fatalf("AddURL: %v", err)
	}

	count := func(qs string) int {
		t.Helper()
		rec := do(t, app, http.MethodGet, "/api/admin/media"+qs, withAdmin)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", qs, rec.Code, rec.Body)
		}
		var body struct {
			Data []json.RawMessage `json:"data"`
		}
		decodeJSONBody(t, rec.Body.Bytes(), &body)
		return len(body.Data)
	}

	if got := count(""); got != 2 {
		t.Errorf("unfiltered = %d, want 2", got)
	}
	if got := count("?kind=video"); got != 1 {
		t.Errorf("?kind=video = %d, want 1", got)
	}
	if got := count("?q=b.jpg"); got != 1 {
		t.Errorf("?q=b.jpg = %d, want 1", got)
	}
	if got := count("?usage=unused"); got != 2 {
		t.Errorf("?usage=unused = %d, want both", got)
	}

	for _, bad := range []string{"?kind=audio", "?usage=maybe", "?product_id=abc", "?min_bytes=-1"} {
		if rec := do(t, app, http.MethodGet, "/api/admin/media"+bad, withAdmin); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", bad, rec.Code)
		}
	}
}

func TestLocalStoreRefusesSuspiciousKeys(t *testing.T) {
	store := &LocalMediaStore{Dir: t.TempDir(), Prefix: "/media"}
	for _, key := range []string{"", "../escape", `..\escape`, "sub/dir.png"} {
		if err := store.Delete(context.Background(), key); err == nil {
			t.Errorf("Delete(%q) was allowed; it must refuse anything but a flat name", key)
		}
	}
	// A key that was never written is not an error: the row is going away
	// either way, and refusing would strand it.
	if err := store.Delete(context.Background(), "deadbeef.png"); err != nil {
		t.Errorf("deleting an absent key: %v", err)
	}
}

// TestVariantMedia covers the per-variant image: a variant nominates one of its
// product's pictures rather than owning a file, and the assignment has to
// survive the thing most likely to destroy it — a reorder of the product's list.
func TestVariantMedia(t *testing.T) {
	app, _ := mediaApp(t)
	ctx := context.Background()
	lib := app.MediaLibrary()

	price := int64(1500)
	p, err := app.Products().CreateProduct(ctx, ProductInput{
		Title: "Tee", SKU: "VM-1", PriceMinor: &price,
		Options: []OptionInput{{Name: "Color", Values: []string{"Red", "Blue"}}},
		Variants: []VariantInput{
			{SKU: "VM-RED", PriceMinor: price, Options: []string{"Red"}},
			{SKU: "VM-BLUE", PriceMinor: price, Options: []string{"Blue"}},
		},
	})
	if err != nil {
		t.Fatalf("create product: %v", err)
	}
	red, blue := p.Variants[0], p.Variants[1]

	one, err := lib.AddURL(ctx, "https://cdn.example/red.jpg", "", "")
	if err != nil {
		t.Fatalf("AddURL: %v", err)
	}
	two, err := lib.AddURL(ctx, "https://cdn.example/blue.jpg", "", "")
	if err != nil {
		t.Fatalf("AddURL: %v", err)
	}
	if err := lib.SetProductMedia(ctx, p.ID, []int64{one.ID, two.ID}); err != nil {
		t.Fatalf("SetProductMedia: %v", err)
	}

	imageOf := func(variantID int64) *VariantImage {
		t.Helper()
		v, err := app.Products().GetVariant(ctx, variantID)
		if err != nil {
			t.Fatalf("GetVariant: %v", err)
		}
		return v.Image
	}

	if got := imageOf(red.ID); got != nil {
		t.Errorf("a fresh variant has image %v, want none", got)
	}

	if err := lib.SetVariantMedia(ctx, red.ID, &one.ID); err != nil {
		t.Fatalf("SetVariantMedia: %v", err)
	}
	img := imageOf(red.ID)
	if img == nil || img.MediaID != one.ID || img.URL != "https://cdn.example/red.jpg" {
		t.Fatalf("red's image = %+v, want media %d", img, one.ID)
	}
	if got := imageOf(blue.ID); got != nil {
		t.Errorf("blue picked up red's image: %+v", got)
	}

	// Re-pointing replaces rather than adding a second: the unique index would
	// reject the insert, so the service has to clear first.
	if err := lib.SetVariantMedia(ctx, red.ID, &two.ID); err != nil {
		t.Fatalf("re-point: %v", err)
	}
	if img := imageOf(red.ID); img == nil || img.MediaID != two.ID {
		t.Errorf("after re-pointing = %+v, want media %d", img, two.ID)
	}

	// Two variants may not share one media row — it carries a single variant_id.
	if err := lib.SetVariantMedia(ctx, blue.ID, &one.ID); err != nil {
		t.Fatalf("blue: %v", err)
	}

	// The regression this whole test exists for: reordering the product's media
	// used to DELETE every row and re-insert it, silently dropping variant_id.
	if err := lib.SetProductMedia(ctx, p.ID, []int64{two.ID, one.ID}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	if img := imageOf(red.ID); img == nil || img.MediaID != two.ID {
		t.Errorf("red's image after a reorder = %+v, want it kept", img)
	}
	if img := imageOf(blue.ID); img == nil || img.MediaID != one.ID {
		t.Errorf("blue's image after a reorder = %+v, want it kept", img)
	}

	// Detaching a picture from the product takes the nomination with it, which
	// is the only sane reading: the variant pointed at a product image.
	if err := lib.SetProductMedia(ctx, p.ID, []int64{two.ID}); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if got := imageOf(blue.ID); got != nil {
		t.Errorf("blue kept an image whose file left the product: %+v", got)
	}

	// Clearing, and the refusal to nominate something not on the product.
	if err := lib.SetVariantMedia(ctx, red.ID, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := imageOf(red.ID); got != nil {
		t.Errorf("image = %+v after clearing, want none", got)
	}
	if err := lib.SetVariantMedia(ctx, red.ID, &one.ID); !errors.Is(err, ErrValidation) {
		t.Errorf("nominating media not on the product = %v, want a validation error", err)
	}
	if err := lib.SetVariantMedia(ctx, 999999, &two.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown variant = %v, want a not-found error", err)
	}
}

// TestVariantMediaRoute checks the wire shape, including the null that clears.
func TestVariantMediaRoute(t *testing.T) {
	app, _ := mediaApp(t)
	ctx := context.Background()
	price := int64(900)
	p, err := app.Products().CreateProduct(ctx, ProductInput{
		Title: "Cap", SKU: "VMR-1", PriceMinor: &price,
	})
	if err != nil {
		t.Fatalf("create product: %v", err)
	}
	v := p.Variants[0]
	item, err := app.MediaLibrary().AddURL(ctx, "https://cdn.example/cap.jpg", "", "")
	if err != nil {
		t.Fatalf("AddURL: %v", err)
	}
	if err := app.MediaLibrary().SetProductMedia(ctx, p.ID, []int64{item.ID}); err != nil {
		t.Fatalf("SetProductMedia: %v", err)
	}
	target := "/api/admin/variants/" + strconv.FormatInt(v.ID, 10) + "/media"

	rec := do(t, app, http.MethodPut, target, withAdmin,
		jsonBody(t, map[string]any{"media_id": item.ID}))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200: %s", rec.Code, rec.Body)
	}
	var body struct {
		Data Variant `json:"data"`
	}
	decodeJSONBody(t, rec.Body.Bytes(), &body)
	if body.Data.Image == nil || body.Data.Image.MediaID != item.ID {
		t.Errorf("response image = %+v, want media %d", body.Data.Image, item.ID)
	}

	cleared := do(t, app, http.MethodPut, target, withAdmin,
		jsonBody(t, map[string]any{"media_id": nil}))
	if cleared.Code != http.StatusOK {
		t.Fatalf("PUT null = %d, want 200: %s", cleared.Code, cleared.Body)
	}
	// A fresh target, not the one above: `image` is omitempty, so decoding a
	// cleared response over a populated struct leaves the old value sitting
	// there and the assertion passes on stale data.
	var after struct {
		Data Variant `json:"data"`
	}
	decodeJSONBody(t, cleared.Body.Bytes(), &after)
	if after.Data.Image != nil {
		t.Errorf("image = %+v after null, want none", after.Data.Image)
	}

	// An omitted field is not the same as null, and guessing which was meant is
	// how an image gets removed by a request that never mentioned it.
	if rec := do(t, app, http.MethodPut, target, withAdmin, jsonBody(t, map[string]any{})); rec.Code != http.StatusBadRequest {
		t.Errorf("PUT with no media_id = %d, want 400", rec.Code)
	}
}
