// Package extract pulls link references out of HTML documents.
//
// It tokenizes rather than builds a DOM: the tokenizer streams over the input
// without materializing a tree and is considerably faster than html.Parse,
// which matters when a crawl visits thousands of pages.
package extract

import (
	"errors"
	"io"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Link is a raw reference found in a document, before resolution.
type Link struct {
	// URL is the attribute value as written in the document.
	URL string `json:"url"`
	// Tag is the element the reference was found on, for example "a" or "img".
	Tag string `json:"tag"`
	// Attr is the attribute holding the reference, for example "href".
	Attr string `json:"attr"`
}

// Result is everything Links learned from a document.
type Result struct {
	// Links in document order. Duplicates are preserved; deduplication
	// happens after resolution.
	Links []Link `json:"links"`
	// Base is the value of the first <base href> element, if any.
	Base string `json:"base,omitempty"`
	// NoFollow is true when a robots meta tag forbids following links.
	NoFollow bool `json:"nofollow,omitempty"`
}

// Links tokenizes r and returns the references it contains. It recognizes
// a[href], area[href], link[href], img[src|srcset], source[src|srcset],
// script[src], iframe[src], audio[src] and video[src|poster]. Errors from r
// other than io.EOF are returned along with any links found before the
// failure.
func Links(r io.Reader) (Result, error) {
	var res Result
	z := html.NewTokenizer(r)
	for {
		switch z.Next() {
		case html.ErrorToken:
			if err := z.Err(); !errors.Is(err, io.EOF) {
				return res, err
			}
			return res, nil
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if !hasAttr {
				continue
			}
			a := atom.Lookup(name)
			if !interesting(a) {
				continue
			}
			res.collect(a, readAttrs(z))
		}
	}
}

func interesting(a atom.Atom) bool {
	switch a {
	case atom.A, atom.Area, atom.Img, atom.Link, atom.Script, atom.Iframe,
		atom.Source, atom.Video, atom.Audio, atom.Base, atom.Meta:
		return true
	}
	return false
}

type attrs map[string]string

func readAttrs(z *html.Tokenizer) attrs {
	m := make(attrs, 4)
	for {
		k, v, more := z.TagAttr()
		key := string(k)
		// First occurrence wins, matching browser behavior for duplicates.
		if _, dup := m[key]; !dup {
			m[key] = string(v)
		}
		if !more {
			return m
		}
	}
}

func (res *Result) collect(a atom.Atom, at attrs) {
	tag := a.String()
	switch a {
	case atom.Base:
		if href, ok := at["href"]; ok && res.Base == "" {
			res.Base = strings.TrimSpace(href)
		}
	case atom.Meta:
		if strings.EqualFold(at["name"], "robots") && hasToken(at["content"], "nofollow", "none") {
			res.NoFollow = true
		}
	case atom.A, atom.Area:
		res.add(tag, "href", at["href"])
	case atom.Link:
		if hasToken(at["rel"], "preconnect", "dns-prefetch") {
			// These name origins rather than resources; the bare origin
			// often 404s and checking it says nothing about the page.
			return
		}
		res.add(tag, "href", at["href"])
	case atom.Img, atom.Source:
		res.add(tag, "src", at["src"])
		for _, u := range parseSrcset(at["srcset"]) {
			res.add(tag, "srcset", u)
		}
	case atom.Video:
		res.add(tag, "src", at["src"])
		res.add(tag, "poster", at["poster"])
	case atom.Script, atom.Iframe, atom.Audio:
		res.add(tag, "src", at["src"])
	}
}

func (res *Result) add(tag, attr, v string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return
	}
	res.Links = append(res.Links, Link{URL: v, Tag: tag, Attr: attr})
}

func hasToken(list string, want ...string) bool {
	for f := range strings.FieldsFuncSeq(list, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t' || r == '\n'
	}) {
		for _, w := range want {
			if strings.EqualFold(f, w) {
				return true
			}
		}
	}
	return false
}

// parseSrcset returns the URLs in a srcset attribute value, a comma-separated
// list of "url [descriptor]" candidates.
func parseSrcset(s string) []string {
	var out []string
	for cand := range strings.SplitSeq(s, ",") {
		if fields := strings.Fields(cand); len(fields) > 0 {
			out = append(out, fields[0])
		}
	}
	return out
}
