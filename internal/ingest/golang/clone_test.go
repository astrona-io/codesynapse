package golang

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

const cloneSrc = `package p

func helper(n int) int { return n }
func other(n int) int  { return n + 1 }

type A struct{ total int }
type B struct{ sum int }

func sumA(xs []int, limit int) int {
	total := 0
	for _, x := range xs {
		if x > limit {
			continue
		}
		total += helper(x)
	}
	return total
}

// Same shape, other names and literal values.
func addAll(values []int, max int) int {
	acc := 0
	for _, v := range values {
		if v > max {
			continue
		}
		acc += helper(v)
	}
	return acc
}

// Calls another function: not a clone.
func sumOther(xs []int, limit int) int {
	total := 0
	for _, x := range xs {
		if x > limit {
			continue
		}
		total += other(x)
	}
	return total
}

// Different operator: not a clone.
func sumLess(xs []int, limit int) int {
	total := 0
	for _, x := range xs {
		if x < limit {
			continue
		}
		total += helper(x)
	}
	return total
}

func small(a int) int {
	b := a
	return b
}

func (a *A) Grow(n int) int {
	a.total += n
	a.total *= 2
	if a.total > 100 {
		a.total = 100
	}
	return a.total
}

func (b *B) Grow(n int) int {
	b.sum += n
	b.sum *= 2
	if b.sum > 100 {
		b.sum = 100
	}
	return b.sum
}
`

func TestCloneHash(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", cloneSrc, 0)
	require.NoError(t, err)
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
	_, err = (&types.Config{}).Check("p", fset, []*ast.File{f}, info)
	require.NoError(t, err)

	hashes := map[string]string{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			name := fd.Name.Name
			if fd.Recv != nil {
				name = fd.Recv.List[0].Type.(*ast.StarExpr).X.(*ast.Ident).Name + "." + name
			}
			hashes[name], _ = cloneHash(info, fd)
		}
	}
	assert.True(t, hashes["sumA"] != "", "big enough")
	assert.Equal(t, hashes["addAll"], hashes["sumA"], "renamed locals and parameters")
	assert.True(t, hashes["sumOther"] != hashes["sumA"], "different callee")
	assert.True(t, hashes["sumLess"] != hashes["sumA"], "different operator")
	assert.Equal(t, hashes["small"], "", "below the size threshold")
	assert.Equal(t, hashes["helper"], "", "below the size threshold")
	assert.True(t, hashes["A.Grow"] != "", "methods are candidates")
	assert.True(t, hashes["A.Grow"] != hashes["B.Grow"], "different fields are different code")
}
