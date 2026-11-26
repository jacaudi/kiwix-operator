package catalog

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testUA = "kiwix-operator/test"

// fakeTransport answers feed requests with feed and .meta4 requests with the devdocs meta4, never touching the network.
type fakeTransport struct {
	t    *testing.T
	feed []byte
	meta []byte
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if got := req.Header.Get("User-Agent"); got != testUA {
		f.t.Errorf("User-Agent = %q, want %q", got, testUA)
	}
	var body []byte
	switch {
	case req.URL.Path == "/catalog/v2/entries":
		body = f.feed
	case strings.HasSuffix(req.URL.Path, ".meta4"):
		body = f.meta
	default:
		return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: http.NoBody, Request: req}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(string(body))), Request: req}, nil
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return b
}

func clientFor(t *testing.T, feed []byte) *Client {
	t.Helper()
	return &Client{
		BaseURL:   "https://catalog.test",
		HTTP:      &http.Client{Transport: &fakeTransport{t: t, feed: feed, meta: readTestdata(t, "devdocs_en_go_2026-10.zim.meta4")}},
		UserAgent: testUA,
	}
}

func TestLatestSendsCountAndUA(t *testing.T) {
	var gotQuery, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotUA = r.URL.RawQuery, r.Header.Get("User-Agent")
		_, _ = io.WriteString(w, `<feed xmlns="http://www.w3.org/2005/Atom"></feed>`)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, UserAgent: testUA}
	_, err := c.Latest(t.Context(), "wikipedia_en_all", "")

	require.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, "count=-1&name=wikipedia_en_all", gotQuery)
	assert.Equal(t, testUA, gotUA)
}

func TestLatestFlavourRequired(t *testing.T) {
	c := clientFor(t, readTestdata(t, "entries-wikipedia_en_all.xml"))
	_, err := c.Latest(t.Context(), "wikipedia_en_all", "")
	require.ErrorIs(t, err, ErrFlavourRequired)
}

func TestLatestPicksFlavour(t *testing.T) {
	c := clientFor(t, readTestdata(t, "entries-wikipedia_en_all.xml"))
	got, err := c.Latest(t.Context(), "wikipedia_en_all", "maxi")
	require.NoError(t, err)

	assert.Equal(t, "wikipedia_en_all", got.Name)
	assert.Equal(t, "maxi", got.Flavour)
	assert.Equal(t, "2026-08", got.Version)
	assert.Equal(t, "wikipedia_en_all_maxi_"+got.Version+".zim", got.File)
	assert.Equal(t, "https://lb.download.kiwix.org/zim/wikipedia/wikipedia_en_all_maxi_2026-08.zim", got.URL)
	assert.Equal(t, "ce9ac3fa1b480e4707e53ecabb8b1e98764bd806cafb8d562bc15fe0e05accf8", got.SHA256)
	assert.Equal(t, int64(1696105), got.Size)
}

func TestLatestSingleFlavourMayBeOmitted(t *testing.T) {
	c := clientFor(t, readTestdata(t, "entries-wikipedia_en_100.xml"))
	got, err := c.Latest(t.Context(), "wikipedia_en_100", "")
	require.NoError(t, err)
	assert.Equal(t, "full", got.Flavour)
}

func TestLatestEmptyFlavourName(t *testing.T) {
	c := clientFor(t, readTestdata(t, "entries-stardew.xml"))
	got, err := c.Latest(t.Context(), "stardewvalleywiki_en_all", "")
	require.NoError(t, err)
	assert.Empty(t, got.Flavour)
	assert.Equal(t, "stardewvalleywiki_en_all_2026-10.zim", got.File)
}

func TestLatestNotFound(t *testing.T) {
	feed := `<feed xmlns="http://www.w3.org/2005/Atom"></feed>`
	tests := []struct {
		name     string
		feed     []byte
		wantName string
		flavour  string
	}{
		{name: "empty feed", feed: []byte(feed), wantName: "wikipedia_en_all"},
		{name: "other name only", feed: readTestdata(t, "entries-stardew.xml"), wantName: "wikipedia_en_all"},
		{name: "unknown flavour", feed: readTestdata(t, "entries-wikipedia_en_all.xml"), wantName: "wikipedia_en_all", flavour: "nosuch"},
		{name: "flavour on flavourless book", feed: readTestdata(t, "entries-stardew.xml"), wantName: "stardewvalleywiki_en_all", flavour: "maxi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := clientFor(t, tt.feed).Latest(t.Context(), tt.wantName, tt.flavour)
			require.ErrorIs(t, err, ErrNotFound)
		})
	}
}

func TestLatestPicksNewestVersion(t *testing.T) {
	feed := `<feed xmlns="http://www.w3.org/2005/Atom">
<entry><name>x</name><flavour>f</flavour><updated>2026-01-01T00:00:00Z</updated>
<link rel="http://opds-spec.org/acquisition/open-access" href="https://d.test/zim/x_f_2026-03.zim.meta4"/></entry>
<entry><name>x</name><flavour>f</flavour><updated>2026-01-01T00:00:00Z</updated>
<link rel="http://opds-spec.org/acquisition/open-access" href="https://d.test/zim/x_f_2026-10.zim.meta4"/></entry>
<entry><name>x</name><flavour>f</flavour><updated>2026-01-01T00:00:00Z</updated>
<link rel="http://opds-spec.org/acquisition/open-access" href="https://d.test/zim/x_f_2026-07.zim.meta4"/></entry>
</feed>`
	got, err := clientFor(t, []byte(feed)).Latest(t.Context(), "x", "f")
	require.NoError(t, err)
	assert.Equal(t, "2026-10", got.Version)
	assert.Equal(t, "x_f_2026-10.zim", got.File)
}

func TestLatestVersionFallsBackToUpdated(t *testing.T) {
	feed := `<feed xmlns="http://www.w3.org/2005/Atom">
<entry><name>x</name><updated>2026-05-17T00:00:00Z</updated>
<link rel="http://opds-spec.org/acquisition/open-access" href="https://d.test/zim/x.zim.meta4"/></entry>
</feed>`
	got, err := clientFor(t, []byte(feed)).Latest(t.Context(), "x", "")
	require.NoError(t, err)
	assert.Equal(t, "2026-05", got.Version)
	assert.Equal(t, "x.zim", got.File)
}

func TestLatestHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := (&Client{BaseURL: srv.URL}).Latest(t.Context(), "x", "")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNotFound), "a server error is not a missing book")
	assert.Contains(t, err.Error(), "500")
}

func TestParseMeta4(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "devdocs_en_go_2026-10.zim.meta4"))
	require.NoError(t, err)
	defer f.Close()

	file, sum, size, err := ParseMeta4(f)
	require.NoError(t, err)
	assert.Equal(t, "devdocs_en_go_2026-10.zim", file)
	assert.Regexp(t, `^[0-9a-f]{64}$`, sum)
	assert.Positive(t, size)
}

func TestParseMeta4Rejects(t *testing.T) {
	const ns = `xmlns="urn:ietf:params:xml:ns:metalink"`
	good := strings.Repeat("a", 64)
	tests := []struct {
		name string
		in   string
	}{
		{name: "empty", in: ""},
		{name: "not xml", in: "not xml"},
		{name: "no file", in: `<metalink ` + ns + `></metalink>`},
		{name: "no sha-256", in: `<metalink ` + ns + `><file name="a.zim"><size>5</size><hash type="md5">abc</hash></file></metalink>`},
		{name: "short sha-256", in: `<metalink ` + ns + `><file name="a.zim"><size>5</size><hash type="sha-256">abc</hash></file></metalink>`},
		{name: "no size", in: `<metalink ` + ns + `><file name="a.zim"><hash type="sha-256">` + good + `</hash></file></metalink>`},
		{name: "no name", in: `<metalink ` + ns + `><file><size>5</size><hash type="sha-256">` + good + `</hash></file></metalink>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := ParseMeta4(strings.NewReader(tt.in))
			require.Error(t, err)
		})
	}
}
