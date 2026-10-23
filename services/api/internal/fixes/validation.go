package fixes

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"

	"github.com/astrona-io/codesynapse/internal/reviews"
)

// validateRewrite permits changes only inside Go function bodies named by
// accepted comment ranges. Exact byte comparisons protect signatures, imports,
// licence headers and unrelated code without reformatting the model's answer.
// Syntax validation does not establish type correctness or fix correctness.
func validateRewrite(file string, orig, out []byte, cs []reviews.CommentItem) error {
	if filepath.Ext(file) != ".go" {
		return errors.New("automatic fixes currently require a Go file; edit other languages manually")
	}
	if len(orig) > maxFileBytes || len(out) > maxFileBytes {
		return errors.New("file exceeds the automatic fix size limit")
	}
	before, err := rewriteLayout(orig)
	if err != nil {
		return errors.New("original Go file has syntax errors; repair it manually before requesting a fix")
	}
	after, err := rewriteLayout(out)
	if err != nil {
		return errors.New("generated Go file has syntax errors")
	}
	allowed, err := rewriteTargets(file, before, cs)
	if err != nil {
		return err
	}
	if len(before) != len(after) {
		return errors.New("generated file adds or removes declarations")
	}
	oldEnd, newEnd := 0, 0
	for i, old := range before {
		next := after[i]
		if old.hasBody != next.hasBody || !bytes.Equal(orig[oldEnd:old.start], out[newEnd:next.start]) {
			return errors.New("generated file changes a header, signature or declaration outside the accepted scope")
		}
		if !allowed[i] && !bytes.Equal(orig[old.start:old.end], out[next.start:next.end]) {
			return errors.New("generated file changes code outside the accepted comment's function")
		}
		oldEnd, newEnd = old.end, next.end
	}
	if !bytes.Equal(orig[oldEnd:], out[newEnd:]) {
		return errors.New("generated file changes content outside the accepted scope")
	}
	return nil
}

type rewriteDecl struct {
	start, end          int
	firstLine, lastLine int
	hasBody             bool
}

func rewriteLayout(src []byte) ([]rewriteDecl, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.AllErrors|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	decls := make([]rewriteDecl, 0, len(f.Decls))
	for _, d := range f.Decls {
		// Physical positions prevent //line directives from moving an accepted
		// comment's scope to different source bytes.
		first, last := fset.PositionFor(d.Pos(), false), fset.PositionFor(d.End(), false)
		span := rewriteDecl{start: first.Offset, end: last.Offset, firstLine: first.Line, lastLine: last.Line}
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
			span.start = fset.PositionFor(fn.Body.Lbrace, false).Offset + 1
			span.end = fset.PositionFor(fn.Body.Rbrace, false).Offset
			span.hasBody = true
		}
		decls = append(decls, span)
	}
	return decls, nil
}

func rewriteTargets(file string, decls []rewriteDecl, cs []reviews.CommentItem) ([]bool, error) {
	if len(cs) == 0 {
		return nil, errors.New("automatic fixes require accepted comments with explicit line ranges")
	}
	allowed := make([]bool, len(decls))
	for _, c := range cs {
		end := c.EndLine
		if end == 0 {
			end = c.Line
		}
		if c.File != file || c.Status != reviews.Accepted || c.Line < 1 || end < c.Line {
			return nil, errors.New("fix comment has an invalid file, status or line range")
		}
		target := -1
		for i, d := range decls {
			if c.Line > d.lastLine || end < d.firstLine {
				continue
			}
			if target >= 0 || !d.hasBody || c.Line < d.firstLine || end > d.lastLine {
				return nil, errors.New("fix comment must identify exactly one Go function or method")
			}
			target = i
		}
		if target < 0 {
			return nil, errors.New("fix comment does not identify a Go function or method")
		}
		allowed[target] = true
	}
	return allowed, nil
}
