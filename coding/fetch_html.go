package coding

import (
	"bytes"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// htmlText reduces an HTML page to readable text: headings marked with #,
// list items with -, links as [text](href), preformatted text kept as is,
// and scripts, styles and other non-content dropped. A page that marks its
// main content (<main>, role="main", or a single <article>) is reduced to
// that; otherwise site navigation, headers and footers are left out.
//
// Links are resolved against base, the page's URL, so the model can fetch
// them as they stand.
func htmlText(body []byte, base *url.URL) string {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return string(body)
	}
	w := &htmlWriter{base: base}
	if title := findTitle(doc); title != "" {
		w.text("Title: " + title)
		w.block()
	}
	if main := mainContent(doc); main != nil {
		w.walk(main)
	} else {
		w.chrome = true
		w.walk(doc)
	}
	return w.String()
}

// mainContent finds the element a page marks as its main content.
func mainContent(doc *html.Node) *html.Node {
	var mains, articles []*html.Node
	var find func(*html.Node)
	find = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch {
			case n.DataAtom == atom.Main || attr(n, "role") == "main":
				mains = append(mains, n)
				return
			case n.DataAtom == atom.Article:
				articles = append(articles, n)
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	switch {
	case len(mains) == 1:
		return mains[0]
	case len(mains) == 0 && len(articles) == 1:
		return articles[0]
	}
	return nil
}

// chromeRoles are the ARIA landmarks that are a site's frame, not a page's
// content.
var chromeRoles = map[string]bool{"navigation": true, "banner": true, "contentinfo": true}

var skipped = map[atom.Atom]bool{
	atom.Head: true, atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true,
	atom.Svg: true, atom.Iframe: true, atom.Object: true, atom.Canvas: true, atom.Button: true,
	atom.Select: true,
}

var blocks = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true, atom.Main: true,
	atom.Header: true, atom.Footer: true, atom.Aside: true, atom.Nav: true, atom.Ul: true, atom.Ol: true,
	atom.Table: true, atom.Tr: true, atom.Blockquote: true, atom.Figure: true, atom.Figcaption: true,
	atom.Dl: true, atom.Dt: true, atom.Dd: true, atom.Form: true, atom.Hr: true, atom.Details: true,
	atom.Summary: true,
}

var headings = map[atom.Atom]string{
	atom.H1: "# ", atom.H2: "## ", atom.H3: "### ", atom.H4: "#### ", atom.H5: "##### ", atom.H6: "###### ",
}

type htmlWriter struct {
	b   strings.Builder
	pre int
	// li is the depth of list items: a block inside one stays on its line.
	li int
	// chrome drops navigation, headers and footers, for a page without a
	// marked main element.
	chrome bool
	base   *url.URL
}

// resolve makes href absolute against the page's URL.
func (w *htmlWriter) resolve(href string) string {
	if w.base == nil {
		return href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	return w.base.ResolveReference(u).String()
}

func (w *htmlWriter) String() string {
	s := trailing.ReplaceAllString(w.b.String(), "\n")
	s = blankRuns.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

var (
	blankRuns = regexp.MustCompile(`\n[ \t]*(\n[ \t]*)+`)
	trailing  = regexp.MustCompile(`[ \t]+\n`)
	spaces    = regexp.MustCompile(`\s+`)
)

// block ends the current block with a blank line, or a space inside a list
// item.
func (w *htmlWriter) block() {
	if w.li > 0 {
		if !strings.HasSuffix(w.b.String(), " ") {
			w.b.WriteString(" ")
		}
		return
	}
	w.b.WriteString("\n\n")
}

func (w *htmlWriter) line() { w.b.WriteString("\n") }

func (w *htmlWriter) text(s string) {
	if w.pre > 0 {
		w.b.WriteString(s)
		return
	}
	s = spaces.ReplaceAllString(s, " ")
	cur := w.b.String()
	if strings.HasSuffix(cur, "\n") || cur == "" {
		s = strings.TrimLeft(s, " ")
	} else if strings.HasSuffix(cur, " ") {
		s = strings.TrimLeft(s, " ")
	}
	w.b.WriteString(s)
}

func (w *htmlWriter) walk(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		w.text(n.Data)
		return
	case html.ElementNode:
		if skipped[n.DataAtom] || hiddenNode(n) {
			return
		}
		if w.chrome && (n.DataAtom == atom.Nav || n.DataAtom == atom.Header || n.DataAtom == atom.Footer ||
			chromeRoles[attr(n, "role")]) {
			return
		}
	case html.CommentNode, html.DoctypeNode:
		return
	}
	children := func() {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			w.walk(c)
		}
	}
	switch a := n.DataAtom; {
	case n.Type != html.ElementNode:
		children()
	case headings[a] != "" && w.li == 0:
		w.block()
		w.b.WriteString(headings[a])
		children()
		w.block()
	case a == atom.Pre:
		w.block()
		w.b.WriteString("```\n")
		w.pre++
		children()
		w.pre--
		w.b.WriteString("\n```")
		w.block()
	case a == atom.Br:
		w.line()
	case a == atom.Li:
		switch {
		case w.li > 0:
			w.b.WriteString(" ")
		case !strings.HasSuffix(w.b.String(), "\n"):
			w.line()
		}
		w.b.WriteString("- ")
		w.li++
		children()
		w.li--
	case a == atom.Td || a == atom.Th:
		children()
		w.b.WriteString(" | ")
	case a == atom.A:
		href := strings.TrimSpace(attr(n, "href"))
		inner := htmlWriter{pre: w.pre, li: w.li, base: w.base}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			inner.walk(c)
		}
		label := strings.TrimSpace(inner.String())
		switch {
		case href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(strings.ToLower(href), "javascript:"):
			w.text(label)
		case label == "":
			w.text("[" + w.resolve(href) + "]")
		default:
			w.text("[" + label + "](" + w.resolve(href) + ")")
		}
	case a == atom.Img:
		if alt := strings.TrimSpace(attr(n, "alt")); alt != "" {
			w.text("[image: " + alt + "]")
		}
	case a == atom.Code && w.pre == 0:
		w.b.WriteString("`")
		children()
		w.b.WriteString("`")
	case blocks[a]:
		w.block()
		children()
		w.block()
	default:
		children()
	}
}

func hiddenNode(n *html.Node) bool {
	if _, ok := attrOK(n, "hidden"); ok {
		return true
	}
	if attr(n, "aria-hidden") == "true" {
		return true
	}
	style := strings.ReplaceAll(strings.ToLower(attr(n, "style")), " ", "")
	return strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden")
}

func attr(n *html.Node, key string) string {
	v, _ := attrOK(n, key)
	return v
}

func attrOK(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func findTitle(n *html.Node) string {
	if n.Type == html.ElementNode && n.DataAtom == atom.Title {
		var b strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.TextNode {
				b.WriteString(c.Data)
			}
		}
		return strings.TrimSpace(spaces.ReplaceAllString(b.String(), " "))
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if t := findTitle(c); t != "" {
			return t
		}
	}
	return ""
}
