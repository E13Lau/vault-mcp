package markdown

import (
	"errors"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

type Heading struct {
	Level int    `json:"level"`
	Text  string `json:"text"`
	Line  int    `json:"line"`
}

func inlineText(node ast.Node, source []byte) string {
	var out strings.Builder
	_ = ast.Walk(node, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		if !enter {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Text:
			out.Write(n.Value(source))
			if n.SoftLineBreak() || n.HardLineBreak() {
				out.WriteByte(' ')
			}
		case *ast.String:
			out.Write(n.Value)
		case *ast.AutoLink:
			out.Write(n.Label(source))
		}
		return ast.WalkContinue, nil
	})
	return out.String()
}

func Outline(d Document) []Heading {
	out := []Heading{}
	source := []byte(d.Body)
	root := goldmark.DefaultParser().Parse(text.NewReader(source))
	_ = ast.Walk(root, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		if h, ok := n.(*ast.Heading); ok && enter {
			start := h.Lines().At(0).Start
			out = append(out, Heading{h.Level, inlineText(h, source), d.BodyLine + strings.Count(d.Body[:start], "\n")})
		}
		return ast.WalkContinue, nil
	})
	return out
}

func BodySummary(d Document) string {
	source := []byte(d.Body)
	root := goldmark.DefaultParser().Parse(text.NewReader(source))
	result := ""
	_ = ast.Walk(root, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		if _, ok := n.(*ast.Paragraph); ok && enter {
			result = strings.Join(strings.Fields(inlineText(n, source)), " ")
			if result != "" {
				return ast.WalkStop, nil
			}
		}
		return ast.WalkContinue, nil
	})
	runes := []rune(result)
	if len(runes) > 120 {
		result = string(runes[:120])
	}
	return result
}

type Link struct {
	Target     string `json:"target"`
	Heading    string `json:"heading,omitempty"`
	Alias      string `json:"alias,omitempty"`
	Embedded   bool   `json:"embedded"`
	Start, End int    `json:"-"`
}

func ParseLink(input string) (Link, error) {
	input = strings.TrimSpace(input)
	l := Link{}
	if strings.HasPrefix(input, "![[") {
		l.Embedded = true
		input = input[1:]
	}
	if strings.HasPrefix(input, "[[") {
		if !strings.HasSuffix(input, "]]") {
			return l, errors.New("wikilink is missing closing brackets")
		}
		input = input[2 : len(input)-2]
	}
	parts := strings.SplitN(input, "|", 2)
	if len(parts) == 2 {
		l.Alias = strings.TrimSpace(parts[1])
	}
	parts = strings.SplitN(parts[0], "#", 2)
	if len(parts) == 2 {
		l.Heading = strings.TrimSpace(parts[1])
	}
	l.Target = strings.TrimSpace(parts[0])
	if l.Target == "" && l.Heading == "" {
		return l, errors.New("wikilink target must not be empty")
	}
	if strings.ContainsAny(l.Target, "\\\x00") {
		return l, errors.New("invalid wikilink target")
	}
	return l, nil
}

var wikiPattern = regexp.MustCompile(`!?\[\[([^\]\n]+)\]\]`)

// visible keeps rendered text at its source offsets. Link destinations, image
// alt text, code, and raw HTML cannot create tags or wikilinks.
func visible(body string) []byte {
	source := []byte(body)
	masked := make([]byte, len(source))
	for i, b := range source {
		if b == '\n' {
			masked[i] = '\n'
		} else {
			masked[i] = ' '
		}
	}
	root := goldmark.DefaultParser().Parse(text.NewReader(source))
	_ = ast.Walk(root, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		if !enter {
			return ast.WalkContinue, nil
		}
		switch n.Kind() {
		case ast.KindFencedCodeBlock, ast.KindCodeBlock, ast.KindHTMLBlock, ast.KindCodeSpan, ast.KindImage:
			return ast.WalkSkipChildren, nil
		}
		if t, ok := n.(*ast.Text); ok {
			copy(masked[t.Segment.Start:t.Segment.Stop], source[t.Segment.Start:t.Segment.Stop])
		}
		return ast.WalkContinue, nil
	})
	return masked
}

func Links(body string) []Link {
	masked := visible(body)
	out := []Link{}
	for _, loc := range wikiPattern.FindAllIndex(masked, -1) {
		// Backslash-escaped delimiters are plain text.
		escapes := 0
		for i := loc[0] - 1; i >= 0 && body[i] == '\\'; i-- {
			escapes++
		}
		if escapes%2 == 1 {
			continue
		}
		l, err := ParseLink(body[loc[0]:loc[1]])
		if err != nil {
			continue
		}
		l.Start, l.End = loc[0], loc[1]
		out = append(out, l)
	}
	return out
}

func NoteTarget(target string) string {
	if strings.EqualFold(path.Ext(target), ".md") {
		return target[:len(target)-3]
	}
	return target
}

func Resolve(link Link, source string, paths []string) []string {
	return NewResolver(paths).Resolve(link, source)
}

type Resolver struct {
	exact  map[string][]string
	suffix map[string][]string
}

func NewResolver(paths []string) *Resolver {
	r := &Resolver{exact: map[string][]string{}, suffix: map[string][]string{}}
	for _, p := range paths {
		n := NoteTarget(p)
		r.exact[n] = append(r.exact[n], p)
		parts := strings.Split(n, "/")
		for i := range parts {
			key := strings.Join(parts[i:], "/")
			r.suffix[key] = append(r.suffix[key], p)
		}
	}
	return r
}

func (r *Resolver) Resolve(link Link, source string) []string {
	out := []string{}
	target := NoteTarget(link.Target)
	if target == "" {
		if source != "" {
			for _, p := range r.exact[NoteTarget(source)] {
				if p == source {
					return []string{p}
				}
			}
		}
		return out
	}
	if strings.HasPrefix(target, "./") || strings.HasPrefix(target, "../") {
		if source == "" {
			return out
		}
		target = path.Clean(path.Join(path.Dir(source), target))
		return append(out, r.exact[target]...)
	}
	target = strings.TrimPrefix(path.Clean(target), "/")
	if target == ".." || strings.HasPrefix(target, "../") {
		return out
	}
	if strings.Contains(target, "/") {
		if matches := r.exact[target]; len(matches) > 0 {
			return append(out, matches...)
		}
	}
	return append(out, r.suffix[target]...)
}

func NormalizeTag(tag string) (string, error) {
	tag = strings.TrimPrefix(strings.TrimSpace(tag), "#")
	hasLetter := false
	if tag == "" || strings.HasPrefix(tag, "/") || strings.HasSuffix(tag, "/") || strings.Contains(tag, "//") {
		return "", errors.New("invalid tag")
	}
	for _, r := range tag {
		if unicode.IsLetter(r) || r == '_' || r == '-' {
			hasLetter = true
		} else if !unicode.IsNumber(r) && r != '/' {
			return "", errors.New("invalid tag: use letters, digits, _, -, and / segments")
		}
	}
	if !hasLetter {
		return "", errors.New("tag must contain a non-numeric character")
	}
	return tag, nil
}

func FrontmatterTags(fields map[string]any) []string {
	out := []string{}
	add := func(s string) {
		for _, t := range strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == ',' }) {
			if tag, err := NormalizeTag(t); err == nil {
				out = append(out, tag)
			}
		}
	}
	switch tags := fields["tags"].(type) {
	case string:
		add(tags)
	case []any:
		for _, tag := range tags {
			if s, ok := tag.(string); ok {
				add(s)
			}
		}
	case []string:
		for _, s := range tags {
			add(s)
		}
	}
	return UniqueTags(out)
}

func Tags(d Document) []string {
	out := FrontmatterTags(d.Fields)
	masked := visible(d.Body)
	for _, l := range Links(d.Body) {
		for i := l.Start; i < l.End; i++ {
			masked[i] = ' '
		}
	}
	s := string(masked)
	for i, r := range s {
		if r != '#' {
			continue
		}
		if i > 0 {
			previous, _ := utf8.DecodeLastRuneInString(s[:i])
			if !unicode.IsSpace(previous) && !strings.ContainsRune("([{>", previous) {
				continue
			}
		}
		end := i + 1
		for end < len(s) {
			next, size := utf8.DecodeRuneInString(s[end:])
			if !unicode.IsLetter(next) && !unicode.IsNumber(next) && !strings.ContainsRune("_- /", next) {
				break
			}
			if next == ' ' {
				break
			}
			end += size
		}
		if tag, err := NormalizeTag(s[i+1 : end]); err == nil {
			out = append(out, tag)
		}
	}
	return UniqueTags(out)
}

func UniqueTags(tags []string) []string {
	set := map[string]bool{}
	for _, tag := range tags {
		set[tag] = true
	}
	out := make([]string, 0, len(set))
	for tag := range set {
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}

type SummaryLink struct {
	Link Link
	Text string
}

func IndexSummaries(body string) []SummaryLink {
	out := []SummaryLink{}
	for _, link := range Links(body) {
		end := strings.IndexByte(body[link.End:], '\n')
		if end < 0 {
			end = len(body) - link.End
		}
		suffix := strings.TrimSpace(body[link.End : link.End+end])
		suffix = strings.TrimLeftFunc(suffix, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune("—–-:：", r) })
		if suffix != "" {
			out = append(out, SummaryLink{link, suffix})
		}
	}
	return out
}
