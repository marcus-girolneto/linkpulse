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
			at := readAttrs(z)
			res.collect(a, &at)
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

// attrs holds the attribute values extract cares about. Reading into fixed
// fields instead of a map avoids allocating for every attribute of every tag.
type attrs struct {
	href, src, srcset, poster, rel, name, content string
	seen                                          uint8
}

const (
	seenHref uint8 = 1 << iota
	seenSrc
	seenSrcset
	seenPoster
	seenRel
	seenName
	seenContent
)

func readAttrs(z *html.Tokenizer) attrs {
	var a attrs
	for {
		k, v, more := z.TagAttr()
		// The conversion in a switch does not allocate. The first
		// occurrence of an attribute wins, matching browser behavior.
		switch string(k) {
		case "href":
			a.set(&a.href, seenHref, v)
		case "src":
			a.set(&a.src, seenSrc, v)
		case "srcset":
			a.set(&a.srcset, seenSrcset, v)
		case "poster":
			a.set(&a.poster, seenPoster, v)
		case "rel":
			a.set(&a.rel, seenRel, v)
		case "name":
			a.set(&a.name, seenName, v)
		case "content":
			a.set(&a.content, seenContent, v)
		}
		if !more {
			return a
		}
	}
}

func (a *attrs) set(dst *string, bit uint8, v []byte) {
	if a.seen&bit != 0 {
		return
	}
	a.seen |= bit
	*dst = string(v)
}

func (res *Result) collect(t atom.Atom, a *attrs) {
	tag := t.String()
	switch t {
	case atom.Base:
		if a.seen&seenHref != 0 && res.Base == "" {
			res.Base = strings.TrimSpace(a.href)
		}
	case atom.Meta:
		if strings.EqualFold(a.name, "robots") && hasToken(a.content, "nofollow", "none") {
			res.NoFollow = true
		}
	case atom.A, atom.Area:
		res.add(tag, "href", a.href)
	case atom.Link:
		if hasToken(a.rel, "preconnect", "dns-prefetch") {
			// These name origins rather than resources; the bare origin
			// often 404s and checking it says nothing about the page.
			return
		}
		res.add(tag, "href", a.href)
	case atom.Img, atom.Source:
		res.add(tag, "src", a.src)
		for _, u := range parseSrcset(a.srcset) {
			res.add(tag, "srcset", u)
		}
	case atom.Video:
		res.add(tag, "src", a.src)
		res.add(tag, "poster", a.poster)
	case atom.Script, atom.Iframe, atom.Audio:
		res.add(tag, "src", a.src)
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
