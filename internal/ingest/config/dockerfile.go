package config

import (
	"bytes"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/redact"
)

// Dockerfiles (security roadmap item 9): instructions are read with the
// Dockerfile syntax (line continuations, the escape directive, comments,
// heredocs) and checked for a root final stage, ADD from a URL, a script
// piped from the network into a shell, an unpinned base image and a
// literal secret in ENV or ARG. Values are never stored: a secret finding
// names the variable.

// IsDockerfile reports the names Docker and Podman build from by default
// or by convention: Dockerfile, Containerfile, Dockerfile.<x>, <x>.Dockerfile.
func IsDockerfile(name string) bool {
	lower := strings.ToLower(name)
	return lower == "dockerfile" || lower == "containerfile" ||
		strings.HasPrefix(lower, "dockerfile.") || strings.HasSuffix(lower, ".dockerfile") || strings.HasSuffix(lower, ".containerfile")
}

// instruction is one Dockerfile instruction with its first line.
type instruction struct {
	line int
	cmd  string // upper case
	args string // continuation lines joined with spaces, heredoc bodies with newlines
}

const maxInstructions = 5000

var (
	escapeDirective = regexp.MustCompile(`(?i)^#\s*escape\s*=\s*([\\` + "`" + `])\s*$`)
	heredocStart    = regexp.MustCompile(`<<-?\s*["']?([A-Za-z_][A-Za-z0-9_]*)["']?`)
)

// parseDockerfile splits content into instructions.
func parseDockerfile(content []byte) []instruction {
	lines := strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n")
	escape := `\`
	// Parser directives come first, before any other line.
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if !strings.HasPrefix(t, "#") {
			break
		}
		if m := escapeDirective.FindStringSubmatch(t); m != nil {
			escape = m[1]
		}
	}
	var out []instruction
	for i := 0; i < len(lines) && len(out) < maxInstructions; i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		start := i + 1
		var b strings.Builder
		for {
			cont := strings.HasSuffix(t, escape)
			b.WriteString(strings.TrimSuffix(t, escape))
			if !cont || i+1 >= len(lines) {
				break
			}
			// Comment and empty lines inside a continuation are dropped.
			for i+1 < len(lines) {
				next := strings.TrimSpace(lines[i+1])
				if next != "" && !strings.HasPrefix(next, "#") {
					break
				}
				i++
			}
			if i+1 >= len(lines) {
				break
			}
			i++
			t = strings.TrimSpace(lines[i])
			b.WriteByte(' ')
		}
		text := b.String()
		cmd, args, _ := strings.Cut(text, " ")
		ins := instruction{line: start, cmd: strings.ToUpper(cmd), args: strings.TrimSpace(args)}
		// A heredoc's body runs to its delimiter line.
		for _, m := range heredocStart.FindAllStringSubmatch(text, 4) {
			for i+1 < len(lines) {
				i++
				if strings.TrimSpace(lines[i]) == m[1] {
					break
				}
				ins.args += "\n" + lines[i]
			}
		}
		out = append(out, ins)
	}
	return out
}

// Instruction is one Dockerfile instruction as other readers see it: its
// first line, the command in upper case and its arguments with continuation
// lines joined.
type Instruction struct {
	Line int
	Cmd  string
	Args string
}

// ParseDockerfile splits a Dockerfile into instructions with the same rules
// as the security checks, for readers outside this package (the architecture
// topology, which links image builds to the Go packages they compile).
func ParseDockerfile(content []byte) []Instruction {
	ins := parseDockerfile(content)
	out := make([]Instruction, len(ins))
	for i, in := range ins {
		out[i] = Instruction{Line: in.line, Cmd: in.cmd, Args: in.args}
	}
	return out
}

// stage is one FROM of a multi-stage build.
type stage struct {
	line  int
	image string // what FROM names: an image or an earlier stage
	name  string // AS name
	user  string // the last USER, "" when none
}

var (
	pipeToShell = regexp.MustCompile(`\b(?:curl|wget)\b[^|;&\n]*\|\s*(?:sudo\s+(?:-\S+\s+)*)?(?:/usr)?(?:/bin/)?(?:ba|z|da|k)?sh\b`)
	// Images built to run unprivileged by default.
	nonRootImage = regexp.MustCompile(`(?i)nonroot|rootless|unprivileged`)
)

// dockerFile indexes one Dockerfile and runs its rules.
func (b *builder) dockerFile(rel string, content []byte) {
	lines := bytes.Count(content, []byte("\n")) + 1
	fileID := b.id(model.KindConfig, rel)
	ins := parseDockerfile(content)
	var stages []stage
	byName := map[string]int{}
	for _, in := range ins {
		switch in.cmd {
		case "FROM":
			s := stage{line: in.line}
			fields := withoutFlags(strings.Fields(in.args))
			if len(fields) > 0 {
				s.image = fields[0]
			}
			if len(fields) >= 3 && strings.EqualFold(fields[1], "AS") {
				s.name = strings.ToLower(fields[2])
				byName[s.name] = len(stages)
			}
			stages = append(stages, s)
		case "USER":
			if len(stages) > 0 {
				stages[len(stages)-1].user = strings.TrimSpace(in.args)
			}
		}
	}
	images := make([]string, 0, len(stages))
	for _, s := range stages {
		if _, isStage := byName[strings.ToLower(s.image)]; !isStage && s.image != "" {
			images = append(images, s.image)
		}
	}
	summary := fmt.Sprintf("Dockerfile: %d stage(s)", len(stages))
	if len(images) > 0 {
		summary += ", from " + strings.Join(images[:min(len(images), 4)], ", ")
	}
	b.g.AddNode(model.Node{
		ID: fileID, Kind: model.KindConfig, Repo: b.repo, Name: path.Base(rel), File: rel, Language: "dockerfile",
		StartLine: 1, EndLine: lines, LOC: lines, Bytes: len(content), Summary: redact.Summary(summary, 200),
	})
	b.g.AddEdge(model.Edge{From: model.NodeID(b.repo, model.KindRepository, b.repo), To: fileID, Kind: model.EdgeContains})

	for _, s := range stages {
		if _, isStage := byName[strings.ToLower(s.image)]; isStage || strings.EqualFold(s.image, "scratch") {
			continue
		}
		if unpinnedImage(s.image) {
			b.reportIn(rel, fileID, "dockerfile-image-unpinned", s.line, fmt.Sprintf("FROM %s has no version tag or digest", redact.Summary(s.image, 80)), "dockerfile")
		}
	}
	if len(stages) > 0 {
		final := stages[len(stages)-1]
		if user, image := effectiveUser(stages, byName, len(stages)-1); isRoot(user) && !nonRootImage.MatchString(image) && !strings.EqualFold(image, "scratch") {
			detail := "The final stage sets no USER, so the container runs as root"
			if user != "" {
				detail = "The final stage runs as USER " + redact.Summary(user, 40)
			}
			b.reportIn(rel, fileID, "dockerfile-root-user", final.line, detail, "dockerfile")
		}
	}
	for _, in := range ins {
		switch in.cmd {
		case "ADD":
			if strings.Contains(in.args, "--checksum=") {
				continue
			}
			for _, f := range withoutFlags(strings.Fields(in.args)) {
				if strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
					b.reportIn(rel, fileID, "dockerfile-add-url", in.line, "ADD downloads a URL without --checksum", "dockerfile")
					break
				}
			}
		case "RUN":
			if pipeToShell.MatchString(in.args) {
				b.reportIn(rel, fileID, "dockerfile-curl-pipe-shell", in.line, "RUN pipes a download into a shell", "dockerfile")
			}
		case "ENV", "ARG":
			for _, kv := range envPairs(in.cmd, in.args) {
				if secretKey.MatchString(kv[0]) && looksSecret(kv[1]) {
					b.reportIn(rel, fileID, "dockerfile-secret-in-env", in.line, fmt.Sprintf("%s %s has a literal value", in.cmd, redact.Summary(kv[0], 60)), "dockerfile")
				}
			}
		}
	}
}

// effectiveUser follows FROM <stage> back to the stage that last set USER,
// returning that user and the image the chain starts from.
func effectiveUser(stages []stage, byName map[string]int, i int) (user, image string) {
	for range len(stages) { // a chain cannot be longer than the stages
		s := stages[i]
		if s.user != "" {
			return s.user, s.image
		}
		j, ok := byName[strings.ToLower(s.image)]
		if !ok || j >= i {
			return "", s.image
		}
		i = j
	}
	return "", ""
}

func isRoot(user string) bool {
	u, _, _ := strings.Cut(user, ":")
	return u == "" || u == "root" || u == "0"
}

// withoutFlags drops --name=value options (--platform, --chown, --link).
func withoutFlags(fields []string) []string {
	out := fields[:0:0]
	for _, f := range fields {
		if !strings.HasPrefix(f, "--") {
			out = append(out, f)
		}
	}
	return out
}

// envPairs reads ENV/ARG arguments: KEY=value pairs (quoted values
// allowed), or the legacy "ENV KEY value". An ARG without a default has
// no value.
func envPairs(cmd, args string) [][2]string {
	if cmd == "ENV" && !strings.Contains(strings.SplitN(args, " ", 2)[0], "=") {
		k, v, _ := strings.Cut(args, " ")
		return [][2]string{{k, unquote(strings.TrimSpace(v))}}
	}
	var out [][2]string
	for _, f := range splitQuoted(args) {
		if k, v, ok := strings.Cut(f, "="); ok {
			out = append(out, [2]string{k, unquote(v)})
		}
	}
	return out
}

// splitQuoted splits on spaces outside double or single quotes.
func splitQuoted(s string) []string {
	var out []string
	var b strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			b.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
			b.WriteRune(r)
		case r == ' ' || r == '\t':
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}
