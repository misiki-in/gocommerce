package cms

import (
	"net/http"
	"testing"

	"github.com/misiki/gocommerce/core"
	"github.com/misiki/gocommerce/gctest"
)

func TestPageLifecycle(t *testing.T) {
	app := gctest.New(t, New(Config{}))

	// Create as a draft.
	rec := gctest.AdminRequest(t, app, http.MethodPost, "/api/admin/x/cms/pages", map[string]any{
		"slug": "about", "title": "About us", "body": "We sell things.",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	var created Page
	gctest.DecodeData(t, rec, &created)
	if created.Status != StatusDraft {
		t.Errorf("status = %q, want draft", created.Status)
	}
	if created.PublishedAt != nil {
		t.Error("a draft should have no publication date")
	}

	// A draft is invisible to shoppers.
	if rec := gctest.Request(t, app, http.MethodGet, "/x/cms/pages/about", nil); rec.Code != http.StatusNotFound {
		t.Errorf("public status for a draft = %d, want 404", rec.Code)
	}

	// Publish it.
	rec = gctest.AdminRequest(t, app, http.MethodPatch,
		"/api/admin/x/cms/pages/"+itoa(created.ID), map[string]any{"status": StatusPublished})
	if rec.Code != http.StatusOK {
		t.Fatalf("publish status = %d: %s", rec.Code, rec.Body)
	}
	var published Page
	gctest.DecodeData(t, rec, &published)
	if published.PublishedAt == nil {
		t.Error("publishing should record when it happened")
	}

	// Now it is public, without a token of any kind.
	rec = gctest.Request(t, app, http.MethodGet, "/x/cms/pages/about", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public status = %d: %s", rec.Code, rec.Body)
	}
	var page Page
	gctest.DecodeData(t, rec, &page)
	if page.Title != "About us" || page.Body != "We sell things." {
		t.Errorf("page = %+v", page)
	}

	// Delete it.
	rec = gctest.AdminRequest(t, app, http.MethodDelete,
		"/api/admin/x/cms/pages/"+itoa(created.ID), nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("delete status = %d: %s", rec.Code, rec.Body)
	}
	if rec := gctest.Request(t, app, http.MethodGet, "/x/cms/pages/about", nil); rec.Code != http.StatusNotFound {
		t.Errorf("status after delete = %d, want 404", rec.Code)
	}
}

func TestSlugIsUniquePerLanguage(t *testing.T) {
	app := gctest.New(t, New(Config{}))

	create := func(slug, lang string) int {
		body := map[string]any{"slug": slug, "title": "T", "status": StatusPublished}
		if lang != "" {
			body["language"] = lang
		}
		return gctest.AdminRequest(t, app, http.MethodPost, "/api/admin/x/cms/pages", body).Code
	}

	if code := create("terms", "en"); code != http.StatusCreated {
		t.Fatalf("first create = %d", code)
	}
	// The same slug in another language is a translation, not a collision.
	if code := create("terms", "fr"); code != http.StatusCreated {
		t.Errorf("same slug in another language = %d, want 201", code)
	}
	// The same slug in the same language is a collision.
	if code := create("terms", "en"); code != http.StatusConflict {
		t.Errorf("duplicate slug and language = %d, want 409", code)
	}
}

// TestFallsBackToTheStoreLanguage: a shopper whose browser asks for a
// translation nobody has written should still get the page.
func TestFallsBackToTheStoreLanguage(t *testing.T) {
	app := gctest.NewWithConfig(t, gocommerce.Config{Languages: []string{"en", "fr"}}, New(Config{}))

	rec := gctest.AdminRequest(t, app, http.MethodPost, "/api/admin/x/cms/pages", map[string]any{
		"slug": "delivery", "title": "Delivery", "status": StatusPublished, "language": "en",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body)
	}

	// ?lang= is how a client asks for a translation explicitly.
	rec = gctest.Request(t, app, http.MethodGet, "/x/cms/pages/delivery?lang=fr", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the English page rather than a 404: %s", rec.Code, rec.Body)
	}
	var page Page
	gctest.DecodeData(t, rec, &page)
	if page.Language != "en" {
		t.Errorf("language = %q, want the en fallback", page.Language)
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}
