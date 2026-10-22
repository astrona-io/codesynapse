package arch

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// parsed is what is read from one record file.
type parsed struct {
	Title   string
	Fields  map[string][]string // lower-case key -> values
	Format  string
	Summary string
	Lines   int
}

var (
	h1Re       = regexp.MustCompile(`^#\s+(.+?)\s*#*\s*$`)
	h2Re       = regexp.MustCompile(`^#{2,6}\s+(.+?)\s*#*\s*$`)
	bulletKV   = regexp.MustCompile(`^\s*[-*]?\s*\**([A-Za-z][A-Za-z -]{1,30}?)\**\s*:\**\s*(.+?)\s*$`)
	titlePre   = regexp.MustCompile(`^(?i)(adr|rfc)[\s_-]*#?\d+\s*[:.\-—–]\s*`)
	linkRe     = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	maxScan    = 40
	maxSummary = 240
)

// keyAliases folds the spellings seen in practice onto one field name.
var keyAliases = map[string]string{
	"state": "status", "status": "status",
	"date": "date", "decided": "date", "decision-date": "date", "last-updated": "date", "updated": "date",
	"owner": "owner", "owners": "owner", "deciders": "owner", "decision-makers": "owner", "author": "owner", "authors": "owner", "maintainer": "owner", "team": "owner",
	"component": "components", "components": "components", "scope": "components", "service": "components", "services": "components", "area": "components",
	"label": "labels", "labels": "labels", "tag": "labels", "tags": "labels", "keywords": "labels",
	"topic":      "topic",
	"supersedes": "supersedes", "replaces": "supersedes",
	"superseded-by": "superseded-by", "replaced-by": "superseded-by",
	"related": "relates", "relates": "relates", "relates-to": "relates", "see-also": "relates", "related-to": "relates",
	"conflicts-with": "conflicts", "conflicts": "conflicts",
	"implements": "implements", "code": "implements", "implemented-by": "implements", "implementation": "implements",
	"type": "type", "doc-type": "type", "kind": "type", "id": "id", "title": "title",
}

func normKey(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	k = strings.NewReplacer("_", "-", " ", "-").Replace(k)
	return keyAliases[k]
}

// parseDoc reads the metadata of a Markdown record. YAML front matter wins;
// without it the first lines are read for "- Status: accepted" bullets or
// "Status: accepted" lines, and a "## Status" section supplies the status.
func parseDoc(content []byte) parsed {
	p := parsed{Fields: map[string][]string{}, Lines: bytes.Count(content, []byte("\n")) + 1}
	body := content
	if rest, ok := bytes.CutPrefix(content, []byte("---\n")); ok {
		if end := bytes.Index(rest, []byte("\n---")); end >= 0 {
			if fm := frontMatter(rest[:end]); len(fm) > 0 {
				p.Fields, p.Format = fm, "front-matter"
				body = rest[end+4:]
			}
		}
	}
	var (
		sc        = bufio.NewScanner(bytes.NewReader(body))
		inCode    bool
		section   string
		scanned   int
		paraLines []string
		summaries = map[string]string{}
		first     string
	)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	flush := func() {
		if len(paraLines) == 0 {
			return
		}
		text := strings.Join(paraLines, " ")
		paraLines = nil
		if len(text) < 20 {
			return
		}
		key := strings.ToLower(section)
		if first == "" {
			first = text
		}
		if _, ok := summaries[key]; !ok {
			summaries[key] = text
		}
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inCode = !inCode
			flush()
			continue
		}
		if inCode {
			continue
		}
		if m := h1Re.FindStringSubmatch(line); m != nil && p.Title == "" {
			p.Title = strings.TrimSpace(m[1])
			scanned = 0
			continue
		}
		if m := h2Re.FindStringSubmatch(line); m != nil {
			flush()
			section = strings.TrimSpace(m[1])
			continue
		}
		t := strings.TrimSpace(line)
		if t == "" {
			flush()
			continue
		}
		// Metadata bullets sit right under the title; stop looking after a few lines.
		if scanned < maxScan && section == "" {
			scanned++
			if m := bulletKV.FindStringSubmatch(line); m != nil {
				if k := normKey(m[1]); k != "" {
					if p.Format == "" {
						p.Format = "bullets"
					}
					if _, have := p.Fields[k]; !have {
						p.Fields[k] = splitList(k, m[2])
					}
					continue
				}
			}
		}
		// "## Status" followed by the word: MADR and Nygard layouts.
		if strings.EqualFold(section, "status") || strings.EqualFold(section, "state") {
			if _, have := p.Fields["status"]; !have {
				p.Fields["status"] = []string{strings.TrimSpace(strings.Trim(t, "*_`"))}
				if p.Format == "" {
					p.Format = "section"
				}
			}
			// A line such as "Superseded by ADR-0005" says more than the word.
			if m := regexp.MustCompile(`(?i)supers[ie]ded by\s+(.+)`).FindStringSubmatch(t); m != nil {
				p.Fields["superseded-by"] = append(p.Fields["superseded-by"], splitList("superseded-by", m[1])...)
			}
			if m := regexp.MustCompile(`(?i)supersedes\s+(.+)`).FindStringSubmatch(t); m != nil {
				p.Fields["supersedes"] = append(p.Fields["supersedes"], splitList("supersedes", m[1])...)
			}
			continue
		}
		if strings.HasPrefix(t, "|") || strings.HasPrefix(t, ">") || strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "![") {
			flush()
			continue
		}
		paraLines = append(paraLines, t)
	}
	flush()
	if v := p.Fields["title"]; len(v) > 0 {
		p.Title = v[0]
	}
	p.Title = titlePre.ReplaceAllString(p.Title, "")
	for _, k := range []string{"decision", "decision outcome", "summary", "overview", "context", "purpose", "problem"} {
		if s, ok := summaries[k]; ok {
			p.Summary = s
			break
		}
	}
	if p.Summary == "" {
		p.Summary = first
	}
	p.Summary = clip(p.Summary, maxSummary)
	return p
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func frontMatter(b []byte) map[string][]string {
	var raw map[string]any
	if yaml.Unmarshal(b, &raw) != nil {
		return nil
	}
	out := map[string][]string{}
	for k, v := range raw {
		key := normKey(k)
		if key == "" {
			continue
		}
		switch x := v.(type) {
		case string:
			out[key] = splitList(key, x)
		case []any:
			for _, e := range x {
				out[key] = append(out[key], splitList(key, fmt.Sprint(e))...)
			}
		case nil:
		default:
			out[key] = []string{fmt.Sprint(x)}
		}
	}
	return out
}

// splitList turns "a, b; c" into values, taking the link text and target of
// Markdown links as candidates. Free text fields stay whole.
func splitList(key, s string) []string {
	s = strings.TrimSpace(s)
	switch key {
	case "status", "title", "date", "id", "type", "topic", "owner":
		return []string{strings.Trim(strings.TrimSpace(s), "*_`")}
	}
	var out []string
	for _, m := range linkRe.FindAllStringSubmatch(s, -1) {
		// A link to a Markdown file names that file; any other link is read by its text.
		if target := strings.TrimSpace(m[2]); strings.Contains(strings.ToLower(target), ".md") && !strings.Contains(target, "://") {
			out = append(out, target)
		} else {
			out = append(out, strings.TrimSpace(m[1]))
		}
	}
	s = linkRe.ReplaceAllString(s, " ")
	for _, part := range regexp.MustCompile(`[,;]`).Split(s, -1) {
		if part = strings.Trim(strings.TrimSpace(part), "`*_"); part != "" {
			out = append(out, part)
		}
	}
	return out
}
