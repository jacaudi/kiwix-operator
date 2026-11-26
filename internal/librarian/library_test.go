package librarian

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jacaudi/kiwix-operator/internal/zim"
)

var clockT = time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

// fakeConfirmer delegates to fn and records the file's mtime at every call.
type fakeConfirmer struct {
	path   string
	fn     func(call int, c Change) (bool, error)
	calls  atomic.Int32
	mu     sync.Mutex
	mtimes []time.Time
}

func (f *fakeConfirmer) Confirm(_ context.Context, c Change) (bool, error) {
	if st, err := os.Stat(f.path); err == nil {
		f.mu.Lock()
		if n := len(f.mtimes); n == 0 || !f.mtimes[n-1].Equal(st.ModTime()) {
			f.mtimes = append(f.mtimes, st.ModTime().UTC())
		}
		f.mu.Unlock()
	}
	return f.fn(int(f.calls.Add(1)), c)
}

func newTestWriter(t *testing.T, fn func(call int, c Change) (bool, error)) (*Writer, *fakeConfirmer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "library.xml")
	fc := &fakeConfirmer{path: path, fn: fn}
	return &Writer{
		Path:       path,
		Confirmer:  fc,
		Now:        func() time.Time { return clockT },
		Poll:       5 * time.Millisecond,
		Window:     40 * time.Millisecond,
		MaxRenders: 3,
	}, fc
}

func setMtime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	require.NoError(t, os.Chtimes(path, mtime, mtime))
}

func mtimeOf(t *testing.T, path string) time.Time {
	t.Helper()
	st, err := os.Stat(path)
	require.NoError(t, err)
	return st.ModTime()
}

func manifestOf(books map[string]string) *Manifest {
	m := newManifest()
	for file, element := range books {
		m.Books[file] = Book{Path: "/data/" + file, Element: element}
	}
	return m
}

func confirmAlways(int, Change) (bool, error) { return true, nil }

func TestRenderSortedAndComplete(t *testing.T) {
	m := manifestOf(map[string]string{
		"c.zim": `<book id="c"/>`,
		"a.zim": `<book id="a"/>`,
		"b.zim": `<book id="b"/>`,
	})

	got := string(Render(m))

	assert.Equal(t, `<library version="20110515"><book id="a"/><book id="b"/><book id="c"/></library>`+"\n", got)
}

func TestRenderEmptyLibrary(t *testing.T) {
	assert.Equal(t, "<library version=\"20110515\"></library>\n", string(Render(newManifest())))
}

func TestWriteBumpsMtime(t *testing.T) {
	tests := []struct {
		name string
		prev *time.Time
		now  time.Time
		want time.Time
	}{
		{"now equals previous", &clockT, clockT, clockT.Add(time.Second)},
		{"now behind previous", &clockT, clockT.Add(-time.Hour), clockT.Add(time.Second)},
		{"now ahead of previous", &clockT, clockT.Add(time.Minute), clockT.Add(time.Minute)},
		{"sub-second now truncates then bumps", &clockT, clockT.Add(500 * time.Millisecond), clockT.Add(time.Second)},
		{"sub-second previous truncates", new(clockT.Add(300 * time.Millisecond)), clockT, clockT.Add(time.Second)},
		{"missing file uses now as previous", nil, clockT, clockT.Add(time.Second)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := newTestWriter(t, confirmAlways)
			w.Now = func() time.Time { return tc.now }
			if tc.prev != nil {
				setMtime(t, w.Path, *tc.prev)
			}

			confirmed, err := w.Write(t.Context(), manifestOf(nil), Change{})

			require.NoError(t, err)
			assert.True(t, confirmed)
			assert.True(t, mtimeOf(t, w.Path).Equal(tc.want), "got %s want %s", mtimeOf(t, w.Path), tc.want)
		})
	}
}

func TestWriteConfirmsFirstTry(t *testing.T) {
	w, fc := newTestWriter(t, confirmAlways)
	m := manifestOf(map[string]string{"a.zim": `<book id="a"/>`})
	change := Change{Present: map[string]string{uuidA: "a"}, Absent: []string{uuidB}}

	confirmed, err := w.Write(t.Context(), m, change)

	require.NoError(t, err)
	assert.True(t, confirmed)
	assert.Equal(t, []time.Time{clockT.Add(time.Second)}, fc.mtimes)
	data, err := os.ReadFile(w.Path)
	require.NoError(t, err)
	assert.Equal(t, Render(m), data)
}

func TestWritePassesChangeToConfirmer(t *testing.T) {
	var got Change
	w, _ := newTestWriter(t, func(_ int, c Change) (bool, error) {
		got = c
		return true, nil
	})
	want := Change{Present: map[string]string{uuidA: "a"}, Absent: []string{uuidB}}

	_, err := w.Write(t.Context(), manifestOf(nil), want)

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestWriteRetriesThenGivesUp(t *testing.T) {
	w, fc := newTestWriter(t, func(int, Change) (bool, error) { return false, nil })

	confirmed, err := w.Write(t.Context(), manifestOf(nil), Change{})

	require.NoError(t, err)
	assert.False(t, confirmed)
	assert.Equal(t, []time.Time{clockT.Add(time.Second), clockT.Add(2 * time.Second), clockT.Add(3 * time.Second)}, fc.mtimes, "exactly 3 renders, strictly increasing")
}

func TestWriteConfirmsOnLaterRender(t *testing.T) {
	w, fc := newTestWriter(t, nil)
	// Confirms only once the second render is on disk.
	fc.fn = func(int, Change) (bool, error) {
		return mtimeOf(t, w.Path).Equal(clockT.Add(2 * time.Second)), nil
	}

	confirmed, err := w.Write(t.Context(), manifestOf(nil), Change{})

	require.NoError(t, err)
	assert.True(t, confirmed)
	assert.Equal(t, []time.Time{clockT.Add(time.Second), clockT.Add(2 * time.Second)}, fc.mtimes)
}

func TestWriteConfirmErrorIsNotFatal(t *testing.T) {
	w, fc := newTestWriter(t, func(call int, _ Change) (bool, error) {
		if call < 3 {
			return false, errors.New("connection refused")
		}
		return true, nil
	})

	confirmed, err := w.Write(t.Context(), manifestOf(nil), Change{})

	require.NoError(t, err)
	assert.True(t, confirmed)
	assert.Equal(t, int32(3), fc.calls.Load())
	assert.Len(t, fc.mtimes, 1, "errors are polled through within one render")
}

func TestWriteContextCancelled(t *testing.T) {
	w, _ := newTestWriter(t, func(int, Change) (bool, error) { return false, nil })
	w.Window = time.Minute
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	confirmed, err := w.Write(ctx, manifestOf(nil), Change{})

	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, confirmed)
}

func TestWriteSerialized(t *testing.T) {
	var active, overlaps atomic.Int32
	w, _ := newTestWriter(t, func(int, Change) (bool, error) {
		if active.Add(1) > 1 {
			overlaps.Add(1)
		}
		time.Sleep(3 * time.Millisecond)
		active.Add(-1)
		return true, nil
	})

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := w.Write(t.Context(), manifestOf(nil), Change{}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	assert.Zero(t, overlaps.Load(), "writes overlapped")
}

func TestWriteUnconfirmed(t *testing.T) {
	w, fc := newTestWriter(t, confirmAlways)
	setMtime(t, w.Path, clockT)
	m := manifestOf(map[string]string{"a.zim": `<book id="a"/>`})

	require.NoError(t, w.WriteUnconfirmed(m))

	data, err := os.ReadFile(w.Path)
	require.NoError(t, err)
	assert.Equal(t, Render(m), data)
	assert.True(t, mtimeOf(t, w.Path).Equal(clockT.Add(time.Second)))
	assert.Zero(t, fc.calls.Load())
}

// catalogServer answers /catalog/v2/entry/<id> from entries: id -> body (200), absent ids -> 404, ids in broken -> 500.
func catalogServer(t *testing.T, entries map[string]string, broken ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := strings.CutPrefix(r.URL.Path, "/catalog/v2/entry/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		if slices.Contains(broken, id) {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		body, found := entries[id]
		if !found {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func entryBody(stem string) string {
	return `<entry><link rel="alternate" href="/content/` + stem + `" type="text/html"/></entry>`
}

func TestHTTPConfirmerPresentAbsent(t *testing.T) {
	const aaa, bbb = "aaaaaaaa-0000-0000-0000-000000000000", "bbbbbbbb-0000-0000-0000-000000000000"
	srv := catalogServer(t, map[string]string{aaa: entryBody("arm-reference")}, "cccccccc-0000-0000-0000-000000000000")

	tests := []struct {
		name    string
		change  Change
		want    bool
		wantErr bool
	}{
		{"present and absent", Change{Present: map[string]string{aaa: "arm-reference"}, Absent: []string{bbb}}, true, false},
		{"present entry missing", Change{Present: map[string]string{bbb: "arm-reference"}}, false, false},
		{"absent entry still served", Change{Absent: []string{aaa}}, false, false},
		{"present with wrong stem", Change{Present: map[string]string{aaa: "other"}}, false, false},
		{"present server error", Change{Present: map[string]string{"cccccccc-0000-0000-0000-000000000000": "x"}}, false, true},
		{"absent server error", Change{Absent: []string{"cccccccc-0000-0000-0000-000000000000"}}, false, true},
		{"empty change", Change{}, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hc := HTTPConfirmer{BaseURL: srv.URL}

			got, err := hc.Confirm(t.Context(), tc.change)

			assert.Equal(t, tc.want, got)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestHTTPConfirmerStemIsExact(t *testing.T) {
	const id = "aaaaaaaa-0000-0000-0000-000000000000"
	srv := catalogServer(t, map[string]string{id: entryBody("devdocs_en_python_2026-10")})
	hc := HTTPConfirmer{BaseURL: srv.URL, HTTP: srv.Client()}

	prefix, err := hc.Confirm(t.Context(), Change{Present: map[string]string{id: "devdocs"}})
	require.NoError(t, err)
	assert.False(t, prefix, "a stem that is only a prefix of the href must not match")

	full, err := hc.Confirm(t.Context(), Change{Present: map[string]string{id: "devdocs_en_python_2026-10"}})
	require.NoError(t, err)
	assert.True(t, full)
}

func TestHTTPConfirmerEncodesStem(t *testing.T) {
	const id = "aaaaaaaa-0000-0000-0000-000000000000"
	// ContentID keeps non-Latin letters; the catalog href percent-encodes their UTF-8 bytes.
	stem := zim.ContentID("日本 wiki+.zim")
	require.Equal(t, "日本_wikiplus", stem)
	srv := catalogServer(t, map[string]string{id: entryBody("%E6%97%A5%E6%9C%AC_wikiplus")})
	hc := HTTPConfirmer{BaseURL: srv.URL}

	got, err := hc.Confirm(t.Context(), Change{Present: map[string]string{id: stem}})

	require.NoError(t, err)
	assert.True(t, got)
}

func TestHTTPConfirmerTransportError(t *testing.T) {
	srv := catalogServer(t, nil)
	hc := HTTPConfirmer{BaseURL: srv.URL}
	srv.Close()

	got, err := hc.Confirm(t.Context(), Change{Absent: []string{"aaaaaaaa-0000-0000-0000-000000000000"}})

	require.Error(t, err)
	assert.False(t, got)
}
