// Package markdown parses notes without changing their source text.
package markdown

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Document struct {
	Fields                            map[string]any
	Body                              string
	BodyLine                          int
	node                              *yaml.Node
	header, opening, closing, newline string
}

func lines(text string) []string {
	if text == "" {
		return nil
	}
	out := strings.SplitAfter(text, "\n")
	if out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func Parse(text string) (Document, error) {
	d := Document{Fields: map[string]any{}, Body: text, BodyLine: 1, newline: "\n", node: &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}}
	ls := lines(text)
	if len(ls) == 0 || strings.TrimSpace(strings.TrimPrefix(ls[0], "\ufeff")) != "---" {
		return d, nil
	}
	if strings.HasSuffix(ls[0], "\r\n") {
		d.newline = "\r\n"
	}
	end := 1
	for end < len(ls) && strings.TrimSpace(ls[end]) != "---" && strings.TrimSpace(ls[end]) != "..." {
		end++
	}
	if end == len(ls) {
		return d, errors.New("invalid YAML frontmatter: missing closing delimiter")
	}
	d.opening, d.closing = ls[0], ls[end]
	d.header = strings.Join(ls[1:end], "")
	d.Body, d.BodyLine = strings.Join(ls[end+1:], ""), end+2
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(d.header), &node); err != nil {
		return d, fmt.Errorf("invalid YAML frontmatter: %w", err)
	}
	if len(node.Content) == 0 {
		return d, nil
	}
	d.node = node.Content[0]
	if d.node.Kind == yaml.ScalarNode && d.node.Tag == "!!null" && d.node.Value == "" {
		d.node = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		return d, nil
	}
	if d.node.Kind != yaml.MappingNode {
		return d, errors.New("invalid YAML frontmatter: expected a mapping")
	}
	seen := map[string]bool{}
	for i := 0; i < len(d.node.Content); i += 2 {
		key := d.node.Content[i]
		if key.Kind != yaml.ScalarNode || (key.Tag != "!!str" && key.Tag != "!!merge") || seen[key.Value] {
			return d, errors.New("invalid YAML frontmatter: keys must be unique strings")
		}
		seen[key.Value] = true
	}
	if err := textTimestamps(d.node).Decode(&d.Fields); err != nil {
		return d, fmt.Errorf("invalid YAML frontmatter: %w", err)
	}
	if _, err := json.Marshal(d.Fields); err != nil {
		return d, errors.New("invalid YAML frontmatter: values must use JSON-compatible mapping keys and numbers")
	}
	return d, nil
}

// Update replaces only changed top-level fields. Untouched YAML, comments,
// delimiters, CRLF, and the Markdown body retain their original bytes.
func Update(text string, fields map[string]any, remove []string) (string, error) {
	d, err := Parse(text)
	if err != nil {
		return "", err
	}
	if d.node.Style&yaml.FlowStyle != 0 {
		return "", errors.New("cannot edit a flow-style root mapping without reformatting it; use a block-style root mapping")
	}
	removed := map[string]bool{}
	for _, key := range remove {
		if _, ok := fields[key]; ok {
			return "", errors.New("a field cannot be updated and removed together")
		}
		removed[key] = true
	}
	type edit struct {
		start, end  int
		replacement string
	}
	edits := []edit{}
	ls := lines(d.header)
	offsets := make([]int, len(ls)+1)
	for i, line := range ls {
		offsets[i+1] = offsets[i] + len(line)
	}
	remaining := map[string]any{}
	for key, val := range fields {
		if key == "" {
			return "", errors.New("field names must not be empty")
		}
		remaining[key] = val
	}
	for i := 0; i < len(d.node.Content); i += 2 {
		key, old := d.node.Content[i], d.node.Content[i+1]
		value, changed := remaining[key.Value]
		if !changed && !removed[key.Value] {
			continue
		}
		start, end := key.Line-1, len(ls)
		if i+2 < len(d.node.Content) {
			end = d.node.Content[i+2].Line - 1
		}
		// Comments preceding the next field, or at the end of the header, are
		// kept outside the replacement. Inline comments stay with their value.
		for end > start+1 {
			trim := strings.TrimSpace(ls[end-1])
			if trim != "" && !strings.HasPrefix(trim, "#") {
				break
			}
			end--
		}
		replacement := ""
		if changed {
			replacement, err = encodeField(key.Value, value, key, old, d.newline)
			if err != nil {
				return "", err
			}
		}
		edits = append(edits, edit{offsets[start], offsets[end], replacement})
		delete(remaining, key.Value)
	}
	updated := d.header
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	for _, e := range edits {
		updated = updated[:e.start] + e.replacement + updated[e.end:]
	}
	keys := make([]string, 0, len(remaining))
	for k := range remaining {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		var alignment *yaml.Node
		if len(d.node.Content) > 0 {
			alignment = &yaml.Node{Column: d.node.Content[0].Column}
		}
		encoded, err := encodeField(key, remaining[key], alignment, nil, d.newline)
		if err != nil {
			return "", err
		}
		if updated != "" && !strings.HasSuffix(updated, "\n") {
			updated += d.newline
		}
		updated += encoded
	}
	if len(edits) == 0 && len(keys) == 0 {
		for _, key := range remove {
			if _, exists := d.Fields[key]; exists {
				return "", fmt.Errorf("cannot remove inherited YAML field %q without editing its merge source", key)
			}
		}
		return text, nil
	}
	opening, closing := d.opening, d.closing
	if opening == "" {
		opening = "---" + d.newline
		closing = "---" + d.newline
	}
	result := opening + updated + closing + d.Body
	checked, err := Parse(result)
	if err != nil {
		return "", fmt.Errorf("frontmatter update rejected: %w", err)
	}
	for key, want := range fields {
		got, exists := checked.Fields[key]
		a, err := json.Marshal(want)
		if err != nil {
			return "", errors.New("frontmatter values must be JSON-compatible")
		}
		b, err := json.Marshal(got)
		if !exists || err != nil || !bytes.Equal(a, b) {
			return "", fmt.Errorf("cannot preserve YAML semantics for field %q", key)
		}
	}
	for _, key := range remove {
		if _, exists := checked.Fields[key]; exists {
			return "", fmt.Errorf("cannot remove inherited YAML field %q without editing its merge source", key)
		}
	}
	return result, nil
}

// textTimestamps copies a node tree with YAML timestamps as plain strings, so
// dates such as 2026-09-25 are reported exactly as written.
func textTimestamps(n *yaml.Node) *yaml.Node {
	c := *n
	if c.Kind == yaml.ScalarNode && c.Tag == "!!timestamp" {
		c.Tag, c.Style = "!!str", yaml.DoubleQuotedStyle
	}
	if n.Alias != nil {
		c.Alias = textTimestamps(n.Alias)
	}
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		c.Content[i] = textTimestamps(child)
	}
	return &c
}

func encodeField(name string, value any, key, old *yaml.Node, newline string) (string, error) {
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}
	if key != nil {
		k.Style = key.Style
		k.LineComment = key.LineComment
	}
	var val yaml.Node
	if err := val.Encode(value); err != nil {
		return "", fmt.Errorf("cannot encode frontmatter field %q", name)
	}
	if text, ok := value.(string); ok && old != nil && old.Kind == yaml.ScalarNode && old.Tag == "!!timestamp" {
		var probe yaml.Node
		if yaml.Unmarshal([]byte(text), &probe) == nil && len(probe.Content) == 1 && probe.Content[0].Tag == "!!timestamp" {
			val = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!timestamp", Value: text}
		}
	}
	if old != nil {
		val.Anchor = old.Anchor
		if old.Kind == val.Kind && old.Tag == val.Tag {
			val.Style = old.Style
		}
		val.LineComment = old.LineComment
	}
	mapping := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{k, &val}}
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	if err := e.Encode(mapping); err != nil {
		return "", errors.New("cannot encode frontmatter")
	}
	_ = e.Close()
	encoded := strings.ReplaceAll(b.String(), "\n", newline)
	if key != nil && key.Column > 1 {
		padding := strings.Repeat(" ", key.Column-1)
		encoded = padding + strings.ReplaceAll(strings.TrimSuffix(encoded, newline), newline, newline+padding) + newline
	}
	return encoded, nil
}
