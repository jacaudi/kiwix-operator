package librarian

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxEntryBody caps how much of one catalog entry the confirmer reads.
const maxEntryBody = 4 << 20

// Change is what a library write must make visible: books that must be served (book ID to content stem, as zim.ContentID returns it) and book IDs that must be gone.
type Change struct {
	Present map[string]string
	Absent  []string
}

// Confirmer reports whether kiwix-serve already reflects a Change. An error means the answer is unknown, not that the change failed.
type Confirmer interface {
	Confirm(ctx context.Context, c Change) (bool, error)
}

// HTTPConfirmer asks kiwix-serve's OPDS catalog, at BaseURL, about each book in a Change. A nil HTTP means http.DefaultClient; the caller's ctx bounds each call.
type HTTPConfirmer struct {
	BaseURL string
	HTTP    *http.Client
}

// Confirm is true when every present book answers 200 with its exact content href and every absent book answers 404; any other status or a transport error returns false and the error.
func (h HTTPConfirmer) Confirm(ctx context.Context, c Change) (bool, error) {
	for id, stem := range c.Present {
		status, body, err := h.entry(ctx, id)
		if err != nil {
			return false, err
		}
		// The closing quote makes the match exact: stem "devdocs" must not match href "/content/devdocs_en_python".
		if status != http.StatusOK || !strings.Contains(body, `href="/content/`+url.PathEscape(stem)+`"`) {
			return false, nil
		}
	}
	for _, id := range c.Absent {
		status, _, err := h.entry(ctx, id)
		if err != nil {
			return false, err
		}
		if status != http.StatusNotFound {
			return false, nil
		}
	}
	return true, nil
}

// entry fetches one catalog entry; only 200 and 404 are answers, any other status is an error.
func (h HTTPConfirmer) entry(ctx context.Context, id string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+"/catalog/v2/entry/"+url.PathEscape(id), nil)
	if err != nil {
		return 0, "", fmt.Errorf("build catalog request for %s: %w", id, err)
	}
	resp, err := cmp.Or(h.HTTP, http.DefaultClient).Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("get catalog entry %s: %w", id, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxEntryBody))
	_ = resp.Body.Close()
	if readErr != nil {
		return 0, "", fmt.Errorf("read catalog entry %s: %w", id, readErr)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return resp.StatusCode, "", fmt.Errorf("get catalog entry %s: unexpected status %d", id, resp.StatusCode)
	}
	return resp.StatusCode, string(body), nil
}
