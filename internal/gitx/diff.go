package gitx

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// LineRange is an inclusive range of line numbers on the new side of a diff.
type LineRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// FileChange describes the changes to one file in a diff.
type FileChange struct {
	// Path is the repository-relative path on the new side (old side for deletions).
	Path    string      `json:"path"`
	OldPath string      `json:"old_path,omitempty"`
	Added   bool        `json:"added,omitempty"`
	Deleted bool        `json:"deleted,omitempty"`
	Ranges  []LineRange `json:"ranges,omitempty"`
}

// MaxDiffBytes bounds diff input before parsing and Git output before buffering.
const MaxDiffBytes = 32 << 20

// maxDiffLine bounds a single diff line. Oversized lines reject the whole diff;
// silently dropping a line could hide changed code from a review.
const maxDiffLine = 1 << 20

type diffFile struct {
	FileChange
	header               string
	oldSeen, newSeen     bool
	renameFrom, renameTo bool
}

func (f *diffFile) finish() (FileChange, error) {
	if f.oldSeen != f.newSeen || f.renameFrom != f.renameTo {
		return FileChange{}, fmt.Errorf("incomplete diff file header")
	}
	if f.Path == "" && f.header != "" {
		oldPath, newPath, err := diffHeaderPaths(f.header)
		if err != nil {
			return FileChange{}, err
		}
		f.OldPath, f.Path = oldPath, newPath
	}
	if f.header != "" {
		oldPath := f.OldPath
		if oldPath == "" {
			oldPath = f.Path
		}
		if err := validateDiffHeader(f.header, oldPath, f.Path); err != nil {
			return FileChange{}, err
		}
	}
	if f.Path == "" || (f.Added && f.Deleted) {
		return FileChange{}, fmt.Errorf("invalid diff file paths")
	}
	if f.Deleted {
		f.Path = f.OldPath
	}
	if f.OldPath == f.Path {
		f.OldPath = ""
	}
	return f.FileChange, nil
}

// ParseDiff parses unified diff output into per-file changes. Pure deletions
// within a file are recorded as a one-line range at the deletion point so the
// surrounding symbol is still considered changed. Header-only hunk summaries
// are accepted; this is an impact parser, not a patch-application validator.
func ParseDiff(diff []byte) ([]FileChange, error) {
	if len(diff) > MaxDiffBytes {
		return nil, fmt.Errorf("diff exceeds %d bytes", MaxDiffBytes)
	}
	var (
		out              []FileChange
		cur              *diffFile
		oldLeft, newLeft int
	)
	flush := func() error {
		if cur != nil {
			change, err := cur.finish()
			if err != nil {
				return err
			}
			out = append(out, change)
		}
		cur = nil
		oldLeft, newLeft = 0, 0
		return nil
	}
	sc := bufio.NewScanner(bytes.NewReader(diff))
	sc.Buffer(make([]byte, 0, 64*1024), maxDiffLine)
	for sc.Scan() {
		line := sc.Text()
		// Changed text can itself start with --- or +++. Consume it before
		// looking for file headers, using the hunk's old and new line counts.
		if len(line) > 0 {
			switch {
			case line[0] == '-' && oldLeft > 0:
				oldLeft--
				continue
			case line[0] == '+' && newLeft > 0:
				newLeft--
				continue
			case line[0] == ' ' && oldLeft > 0 && newLeft > 0:
				oldLeft--
				newLeft--
				continue
			}
		}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			if err := flush(); err != nil {
				return nil, err
			}
			header := strings.TrimPrefix(line, "diff --git ")
			if header == "" {
				return nil, fmt.Errorf("empty Git file header")
			}
			cur = &diffFile{header: header}
		case strings.HasPrefix(line, "--- "):
			if cur != nil && cur.newSeen {
				if err := flush(); err != nil {
					return nil, err
				}
			}
			if cur == nil {
				cur = &diffFile{}
			}
			p, err := diffPath(line[4:], true)
			if err != nil || cur.oldSeen || (cur.renameFrom && cur.OldPath != p) {
				return nil, fmt.Errorf("invalid old file header")
			}
			cur.oldSeen, cur.Added, cur.OldPath = true, p == "", p
		case strings.HasPrefix(line, "+++ "):
			if cur == nil || !cur.oldSeen || cur.newSeen {
				return nil, fmt.Errorf("new file header without old file header")
			}
			p, err := diffPath(line[4:], true)
			if err != nil {
				return nil, fmt.Errorf("invalid new file header: %w", err)
			}
			if cur.renameTo && cur.Path != p {
				return nil, fmt.Errorf("new file header disagrees with rename or copy")
			}
			cur.newSeen, cur.Deleted, cur.Path = true, p == "", p
			if cur.Deleted {
				cur.Path = cur.OldPath
			}
		case strings.HasPrefix(line, "rename from "), strings.HasPrefix(line, "rename to "),
			strings.HasPrefix(line, "copy from "), strings.HasPrefix(line, "copy to "):
			if cur == nil || cur.oldSeen {
				return nil, fmt.Errorf("rename or copy metadata outside Git file header")
			}
			_, rest, _ := strings.Cut(line, " ")
			direction, value, _ := strings.Cut(rest, " ")
			p, err := diffPath(value, false)
			if err != nil || p == "" {
				return nil, fmt.Errorf("invalid rename or copy path")
			}
			if (direction == "from" && cur.renameFrom) || (direction == "to" && cur.renameTo) {
				return nil, fmt.Errorf("duplicate rename or copy path")
			}
			if direction == "from" {
				cur.renameFrom, cur.OldPath = true, p
			} else {
				cur.renameTo, cur.Path = true, p
			}
		case strings.HasPrefix(line, "new file mode "):
			if cur != nil {
				cur.Added = true
			}
		case strings.HasPrefix(line, "deleted file mode "):
			if cur != nil {
				cur.Deleted = true
			}
		case strings.HasPrefix(line, "@@"):
			if cur == nil || !cur.oldSeen || !cur.newSeen {
				return nil, fmt.Errorf("hunk header before file headers")
			}
			r, oldCount, newCount, err := parseHunk(line)
			if err != nil {
				return nil, err
			}
			oldLeft, newLeft = oldCount, newCount
			if !cur.Deleted {
				cur.Ranges = append(cur.Ranges, r)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scanning diff: %w", err)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

// diffPath decodes Git's C-quoted names before removing a/ or b/. Extended
// rename/copy headers have no prefix or timestamp; their spaces are significant.
func diffPath(value string, fileHeader bool) (string, error) {
	if strings.HasPrefix(value, `"`) {
		end, err := quotedPathEnd(value)
		if err != nil {
			return "", err
		}
		if rest := value[end:]; rest != "" && (!fileHeader || rest[0] != '\t') {
			return "", fmt.Errorf("unexpected text after quoted path")
		}
		value, err = strconv.Unquote(value[:end])
		if err != nil {
			return "", fmt.Errorf("invalid quoted path: %w", err)
		}
	} else if fileHeader {
		value, _, _ = strings.Cut(value, "\t")
	}
	if value == "" || strings.ContainsRune(value, 0) {
		return "", fmt.Errorf("empty path or NUL in path")
	}
	if fileHeader {
		if value == "/dev/null" {
			return "", nil
		}
		if strings.HasPrefix(value, "a/") || strings.HasPrefix(value, "b/") {
			value = value[2:]
		}
	}
	if value == "" || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("path is not repository-relative")
	}
	for part := range strings.SplitSeq(value, "/") {
		if part == ".." || part == "." || part == "" {
			return "", fmt.Errorf("invalid repository path component")
		}
	}
	return value, nil
}

func quotedPathEnd(value string) (int, error) {
	for i := 1; i < len(value); i++ {
		switch value[i] {
		case '"':
			return i + 1, nil
		case '\\':
			i++
			if i == len(value) {
				break
			}
			if strings.ContainsRune(`abtnvfr\"`, rune(value[i])) {
				continue
			}
			if value[i] < '0' || value[i] > '3' || i+2 >= len(value) ||
				value[i+1] < '0' || value[i+1] > '7' || value[i+2] < '0' || value[i+2] > '7' {
				return 0, fmt.Errorf("invalid Git path escape")
			}
			i += 2
		}
	}
	return 0, fmt.Errorf("unterminated quoted path")
}

// Header paths are only needed for binary or mode-only changes. Text and rename
// headers provide unambiguous paths, including names containing spaces or " b/".
func diffHeaderPaths(header string) (string, string, error) {
	if strings.HasPrefix(header, `"`) {
		end, err := quotedPathEnd(header)
		if err != nil || end >= len(header) || header[end] != ' ' {
			return "", "", fmt.Errorf("invalid Git file header")
		}
		oldPath, oldErr := diffPath(header[:end], true)
		newPath, newErr := diffPath(header[end+1:], true)
		if oldErr != nil || newErr != nil || oldPath == "" || newPath == "" {
			return "", "", fmt.Errorf("invalid Git file header paths")
		}
		return oldPath, newPath, nil
	}
	// Unquoted names in a mode-only change are equal, including their byte
	// lengths. A single midpoint check keeps long space-filled names linear.
	mid := len(header) / 2
	if len(header)%2 == 1 && header[mid] == ' ' {
		oldPath, oldErr := diffPath(header[:mid], true)
		newPath, newErr := diffPath(header[mid+1:], true)
		if oldErr == nil && newErr == nil && oldPath != "" && oldPath == newPath {
			return oldPath, newPath, nil
		}
	}
	return "", "", fmt.Errorf("ambiguous or invalid Git file header")
}

func validateDiffHeader(header, oldPath, newPath string) error {
	ends := []int{len(oldPath) + 2, len(oldPath)}
	if strings.HasPrefix(header, `"`) {
		end, err := quotedPathEnd(header)
		if err != nil {
			return err
		}
		ends = []int{end}
	}
	for _, end := range ends {
		if end >= len(header) || header[end] != ' ' {
			continue
		}
		oldName, oldErr := diffPath(header[:end], true)
		newName, newErr := diffPath(header[end+1:], true)
		if oldErr == nil && newErr == nil && oldName == oldPath && newName == newPath {
			return nil
		}
	}
	return fmt.Errorf("git header disagrees with file paths")
}

func parseHunk(line string) (LineRange, int, int, error) {
	fields := strings.Fields(line)
	if len(fields) < 4 || fields[0] != "@@" || fields[3] != "@@" ||
		!strings.HasPrefix(fields[1], "-") || !strings.HasPrefix(fields[2], "+") {
		return LineRange{}, 0, 0, fmt.Errorf("malformed hunk header")
	}
	_, oldCount, oldErr := hunkRange(fields[1][1:])
	start, newCount, newErr := hunkRange(fields[2][1:])
	if oldErr != nil || newErr != nil {
		return LineRange{}, 0, 0, fmt.Errorf("invalid hunk range")
	}
	if newCount == 0 {
		// Git reports the line before a deleted block, or zero at file start.
		return LineRange{Start: max(start, 1), End: max(start, 1)}, oldCount, newCount, nil
	}
	return LineRange{Start: start, End: start + (newCount - 1)}, oldCount, newCount, nil
}

func hunkRange(spec string) (int, int, error) {
	startText, countText, hasCount := strings.Cut(spec, ",")
	if !decimalDigits(startText) || (hasCount && !decimalDigits(countText)) {
		return 0, 0, fmt.Errorf("invalid hunk number")
	}
	start, err := strconv.Atoi(startText)
	if err != nil {
		return 0, 0, err
	}
	count := 1
	if hasCount {
		count, err = strconv.Atoi(countText)
		if err != nil {
			return 0, 0, err
		}
	}
	maxInt := int(^uint(0) >> 1)
	if count > 0 && (start == 0 || start > maxInt-(count-1)) {
		return 0, 0, fmt.Errorf("hunk range overflows or starts at zero")
	}
	return start, count, nil
}

func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}
