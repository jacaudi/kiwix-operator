// Package catalog looks up the newest kiwix.org catalog entry for a ZIM name and flavour and reads its .meta4 hash.
package catalog

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/jacaudi/kiwix-operator/internal/zim"
)

const (
	requestTimeout = 30 * time.Second
	// maxBodyBytes bounds what one catalog or meta4 response may make us read.
	maxBodyBytes   = 16 << 20
	acquisitionRel = "http://opds-spec.org/acquisition"
	meta4Ext       = ".meta4"
)

var (
	// ErrNotFound means the catalog has no entry for the requested name and flavour.
	ErrNotFound = errors.New("catalog entry not found")
	// ErrFlavourRequired means the book has several flavours and the request named none.
	ErrFlavourRequired = errors.New("catalog entry has several flavours; set a flavour")
)

// Entry is the newest catalog entry for a name and flavour, with the hash and size from its .meta4.
type Entry struct {
	Name    string
	Flavour string
	// File is the ZIM file name, the basename of URL.
	File string
	// Version is YYYY-MM.
	Version string
	// URL is the direct ZIM download URL.
	URL    string
	SHA256 string
	Size   int64
}

// Client queries a Kiwix OPDS catalog.
type Client struct {
	// BaseURL is the catalog root, such as https://opds.library.kiwix.org.
	BaseURL string
	// HTTP defaults to a client with a 30 s timeout.
	HTTP *http.Client
	// UserAgent is sent on every request when set.
	UserAgent string
}

type feed struct {
	Entries []struct {
		Name    string `xml:"name"`
		Flavour string `xml:"flavour"`
		Updated string `xml:"updated"`
		Links   []struct {
			Rel  string `xml:"rel,attr"`
			Href string `xml:"href,attr"`
		} `xml:"link"`
	} `xml:"http://www.w3.org/2005/Atom entry"`
}

// candidate is a feed entry reduced to what selection needs; meta4 is the advertised acquisition link.
type candidate struct {
	Entry
	meta4 string
}

// Latest returns the newest entry named name; flavour may be empty only for a book with a single flavour.
func (c *Client) Latest(ctx context.Context, name, flavour string) (Entry, error) {
	q := url.Values{"name": {name}, "count": {"-1"}}
	var f feed
	err := c.get(ctx, strings.TrimSuffix(c.BaseURL, "/")+"/catalog/v2/entries?"+q.Encode(), func(r io.Reader) error {
		return xml.NewDecoder(r).Decode(&f)
	})
	if err != nil {
		return Entry{}, fmt.Errorf("query catalog for %s: %w", name, err)
	}

	best, err := newest(candidates(f, name), name, flavour)
	if err != nil {
		return Entry{}, err
	}
	err = c.get(ctx, best.meta4, func(r io.Reader) (err error) {
		_, best.SHA256, best.Size, err = ParseMeta4(r)
		return err
	})
	if err != nil {
		return Entry{}, fmt.Errorf("read meta4 for %s: %w", best.File, err)
	}
	return best.Entry, nil
}

// candidates keeps the entries named name that have an acquisition link and a YYYY-MM version.
func candidates(f feed, name string) []candidate {
	var out []candidate
	for _, e := range f.Entries {
		if e.Name != name {
			continue
		}
		var href string
		for _, l := range e.Links {
			if strings.HasPrefix(l.Rel, acquisitionRel) {
				href = l.Href
				break
			}
		}
		u, err := url.Parse(href)
		if err != nil || href == "" {
			continue
		}
		file := path.Base(strings.TrimSuffix(u.Path, meta4Ext))
		version, ok := zim.NormalizeVersion(file)
		if !ok {
			version, ok = zim.NormalizeVersion(e.Updated)
		}
		if !ok {
			continue
		}
		out = append(out, candidate{
			Entry: Entry{Name: e.Name, Flavour: e.Flavour, File: file, Version: version, URL: strings.TrimSuffix(href, meta4Ext)},
			meta4: href,
		})
	}
	return out
}

// newest picks the highest-version candidate of the requested flavour; "" means the only flavour present.
func newest(cands []candidate, name, flavour string) (candidate, error) {
	flavours := map[string]struct{}{}
	for _, c := range cands {
		flavours[c.Flavour] = struct{}{}
	}
	if flavour == "" && len(flavours) > 1 {
		return candidate{}, fmt.Errorf("%s: %w", name, ErrFlavourRequired)
	}
	var best candidate
	for _, c := range cands {
		if flavour != "" && c.Flavour != flavour {
			continue
		}
		if best.File == "" || c.Version > best.Version {
			best = c
		}
	}
	if best.File == "" {
		return candidate{}, fmt.Errorf("%s flavour %q: %w", name, flavour, ErrNotFound)
	}
	return best, nil
}

// get issues a GET with the caller's context and hands the size-capped body to read.
func (c *Client) get(ctx context.Context, rawURL string, read func(io.Reader) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: requestTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", rawURL, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", rawURL, resp.Status)
	}
	return read(io.LimitReader(resp.Body, maxBodyBytes))
}
