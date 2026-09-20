package agent

import (
	"math"
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	xhtml "golang.org/x/net/html"
)

// webHTMLToMarkdown preserves document structure without running page scripts.
// Behavior reference: Crawl4AI markdown_generation_strategy.py; independent Go implementation.
func webHTMLToMarkdown(input string, base *url.URL) string {
	doc, err := xhtml.Parse(strings.NewReader(input))
	if err != nil {
		return htmlToText(input)
	}
	root := doc
	var main, article, body *xhtml.Node
	var find func(*xhtml.Node)
	find = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			switch n.Data {
			case "main":
				if main == nil {
					main = n
				}
			case "article":
				if article == nil {
					article = n
				}
			case "body":
				body = n
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	if main != nil {
		root = main
	} else if article != nil {
		root = article
	} else if body != nil {
		root = body
	}
	var render func(*xhtml.Node, int) string
	render = func(n *xhtml.Node, depth int) string {
		if depth > 512 {
			return ""
		}
		if n.Type == xhtml.TextNode {
			return webSpace(n.Data)
		}
		if n.Type != xhtml.ElementNode && n.Type != xhtml.DocumentNode {
			return ""
		}
		switch n.Data {
		case "head", "script", "style", "noscript", "nav", "footer", "aside", "form", "svg", "template":
			return ""
		}
		for _, a := range n.Attr {
			if a.Key == "hidden" || (a.Key == "aria-hidden" && a.Val == "true") {
				return ""
			}
		}
		if n.Data == "pre" {
			text := webNodeText(n)
			fence := "```"
			for strings.Contains(text, fence) {
				fence += "`"
			}
			return "\n\n" + fence + "\n" + strings.Trim(text, "\r\n") + "\n" + fence + "\n\n"
		}
		var b strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			b.WriteString(render(c, depth+1))
		}
		text := b.String()
		trimmed := strings.TrimSpace(text)
		switch n.Data {
		case "h1", "h2", "h3", "h4", "h5", "h6":
			return "\n\n" + strings.Repeat("#", int(n.Data[1]-'0')) + " " + trimmed + "\n\n"
		case "p", "div", "section", "article", "main", "ul", "ol", "table":
			return "\n\n" + trimmed + "\n\n"
		case "li":
			marker := "- "
			if n.Parent != nil && n.Parent.Data == "ol" {
				marker = "1. "
			}
			return marker + strings.ReplaceAll(trimmed, "\n", "\n  ") + "\n"
		case "br":
			return "\n"
		case "hr":
			return "\n\n---\n\n"
		case "strong", "b":
			return "**" + trimmed + "**"
		case "em", "i":
			return "*" + trimmed + "*"
		case "code":
			fence := "`"
			for strings.Contains(trimmed, fence) {
				fence += "`"
			}
			return fence + " " + trimmed + " " + fence
		case "blockquote":
			return "\n\n> " + strings.ReplaceAll(trimmed, "\n", "\n> ") + "\n\n"
		case "a":
			for _, a := range n.Attr {
				if a.Key == "href" {
					if target := webMarkdownURL(a.Val, base); target != "" && trimmed != "" {
						return "[" + strings.ReplaceAll(trimmed, "]", "\\]") + "](" + target + ")"
					}
				}
			}
		case "td", "th":
			return " " + strings.ReplaceAll(strings.Join(strings.Fields(trimmed), " "), "|", "\\|") + " |"
		case "tr":
			row := "|" + text + "\n"
			// Markdown tables need a separator after their first row, including td-only tables.
			first := true
			for s := n.PrevSibling; s != nil; s = s.PrevSibling {
				if s.Data == "tr" {
					first = false
				}
			}
			if n.Parent != nil && n.Parent.Data == "tbody" {
				for s := n.Parent.PrevSibling; s != nil; s = s.PrevSibling {
					if s.Data == "thead" {
						first = false
					}
				}
			}
			if first {
				row += "|"
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Data == "td" || c.Data == "th" {
						row += " --- |"
					}
				}
				row += "\n"
			}
			return row
		}
		return text
	}
	return strings.TrimSpace(render(root, 0))
}

func webNodeText(n *xhtml.Node) string {
	var b strings.Builder
	var visit func(*xhtml.Node)
	visit = func(n *xhtml.Node) {
		if n.Type == xhtml.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(n)
	return b.String()
}

func webSpace(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !space {
				b.WriteByte(' ')
			}
			space = true
		} else {
			b.WriteRune(r)
			space = false
		}
	}
	return b.String()
}

func webMarkdownURL(raw string, base *url.URL) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return strings.NewReplacer("(", "%28", ")", "%29", " ", "%20", "\n", "", "\r", "").Replace(u.String())
}

// Blocks keep fenced code intact when it contains blank lines.
func webMarkdownBlocks(s string) []string {
	var blocks []string
	var current []string
	fence := ""
	flush := func() {
		if v := strings.TrimSpace(strings.Join(current, "\n")); v != "" {
			blocks = append(blocks, v)
		}
		current = nil
	}
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if fence != "" {
			current = append(current, line)
			if strings.HasPrefix(t, fence) && strings.Trim(t, string(fence[0])) == "" {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			for _, r := range t {
				if byte(r) != t[0] {
					break
				}
				fence += string(r)
			}
		}
		if t == "" {
			flush()
		} else {
			current = append(current, line)
		}
	}
	flush()
	return blocks
}

// BM25-style block scoring: bounded input, no extra model call, original order restored.
func selectWebMarkdown(content, prompt string, maxChars int) string {
	content = strings.TrimSpace(content)
	if strings.TrimSpace(prompt) == "" {
		return truncateString(content, maxChars)
	}
	terms := keywordSet(prompt)
	// Include short Korean query terms (e.g. 설치); English keeps existing token rules.
	for _, w := range strings.FieldsFunc(strings.ToLower(prompt), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if utf8.RuneCountInString(w) >= 2 && strings.IndexFunc(w, func(r rune) bool { return r >= 0xAC00 && r <= 0xD7A3 }) >= 0 {
			terms[w] = true
		}
	}
	blocks := webMarkdownBlocks(content)
	if len(terms) == 0 || len(blocks) == 0 {
		return truncateString(content, maxChars)
	}
	lengths := make([]float64, len(blocks))
	avg := 0.0
	for i, b := range blocks {
		lengths[i] = float64(maxInt(1, utf8.RuneCountInString(b)))
		avg += lengths[i]
	}
	avg /= float64(len(blocks))
	scores := make([]float64, len(blocks))
	for term := range terms {
		counts := make([]int, len(blocks))
		df := 0
		for i, b := range blocks {
			counts[i] = strings.Count(strings.ToLower(b), term)
			if counts[i] > 0 {
				df++
			}
		}
		idf := math.Log(1 + (float64(len(blocks)-df)+0.5)/(float64(df)+0.5))
		for i, tf := range counts {
			if tf > 0 {
				f := float64(tf)
				scores[i] += idf * f * 2.2 / (f + 1.2*(0.25+0.75*lengths[i]/avg))
			}
		}
	}
	var ranked []int
	for i, score := range scores {
		if score > 0 {
			ranked = append(ranked, i)
		}
	}
	if len(ranked) == 0 {
		return truncateString(content, maxChars)
	}
	sort.SliceStable(ranked, func(i, j int) bool { return scores[ranked[i]] > scores[ranked[j]] })
	var selected []int
	total := 0
	for _, i := range ranked {
		n := utf8.RuneCountInString(blocks[i]) + 2
		if maxChars > 0 && total+n > maxChars && len(selected) > 0 {
			continue
		}
		selected = append(selected, i)
		total += n
	}
	sort.Ints(selected)
	var out []string
	for _, i := range selected {
		out = append(out, blocks[i])
	}
	return truncateString(strings.Join(out, "\n\n"), maxChars)
}
