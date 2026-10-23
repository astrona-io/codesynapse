package fixes

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

func TestValidateRevision(t *testing.T) {
	base := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name  string
		rv    reviews.Review
		base  string
		valid bool
	}{
		{"commit", reviews.Review{Commit: base}, base, true},
		{"head", reviews.Review{Head: base}, base, true},
		{"diff base differs", reviews.Review{Base: "main", Head: base, Commit: base}, base, true},
		{"sha256", reviews.Review{Commit: strings.Repeat("a", 64)}, strings.Repeat("a", 64), true},
		{"missing", reviews.Review{}, base, false},
		{"old commit", reviews.Review{Commit: strings.Repeat("b", 40)}, base, false},
		{"ambiguous", reviews.Review{Head: strings.Repeat("b", 40), Commit: base}, base, false},
		{"symbolic", reviews.Review{Head: "main"}, "main", false},
		{"short", reviews.Review{Commit: "abcdef0"}, "abcdef0", false},
		{"nonhex", reviews.Review{Commit: strings.Repeat("z", 40)}, strings.Repeat("z", 40), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRevision(tc.rv, tc.base)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestExtractFile(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		valid            bool
	}{
		{"go", "```go\npackage app\n```\n", "package app\n", true},
		{"CRLF", "```go\r\npackage app\r\n```\r\n", "package app\r\n", true},
		{"long fence", "````go\n// ```\npackage app\n````", "// ```\npackage app\n", true},
		{"unicode", "```go\n// 日本語\npackage app\n```", "// 日本語\npackage app\n", true},
		{"prose", "Here you go:\n```go\npackage app\n```", "", false},
		{"trailing prose", "```go\npackage app\n```\nExplanation", "", false},
		{"two blocks", "```go\npackage app\n```\n```go\npackage other\n```", "", false},
		{"no end", "```go\npackage app\n", "", false},
		{"wrong language", "```python\npass\n```", "", false},
		{"oversized", "```go\n" + strings.Repeat("x", maxFileBytes+1) + "\n```", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := extractFile(tc.text)
			if tc.valid {
				require.NoError(t, err)
				assert.Equal(t, out, tc.want)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestRunRefusesOldPolicyAndUnknownComments(t *testing.T) {
	for _, which := range []string{"policy", "unknown", "duplicate", "unpinned"} {
		t.Run(which, func(t *testing.T) {
			ctx := fixContext(t)
			f := setup(ctx, t, fixed, http.StatusCreated)
			switch which {
			case "policy":
				f.req.ValidationVersion = 0
			case "unknown":
				f.req.CommentIDs = append(f.req.CommentIDs, "missing")
			case "duplicate":
				f.req.CommentIDs = append(f.req.CommentIDs, f.req.CommentIDs[0])
			case "unpinned":
				rv, err := f.st.GetReview(ctx, f.req.ReviewID)
				require.NoError(t, err)
				rv.Commit = ""
				require.NoError(t, f.st.SaveReview(ctx, rv))
			}
			_, err := f.r.Run(ctx, f.req)
			require.Error(t, err)
			assert.Len(t, f.pushed, 0)
		})
	}
}

func TestRunRefusesCloudEvenWithSourceOverride(t *testing.T) {
	ctx := fixContext(t)
	f := setup(ctx, t, fixed, http.StatusCreated)
	cfg, err := llm.Load("", func(k string) string {
		if k == "CODESYNAPSE_LLM_BASE_URL" {
			return "https://provider.example/v1"
		}
		return ""
	})
	require.NoError(t, err)
	allow := true
	cfg.Providers[0].SendSource = &allow
	f.r.LLM = llm.NewRegistry(cfg)
	_, err = f.r.Run(ctx, f.req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "use a local model")
	assert.Len(t, f.pushed, 0)
}

func TestRunRechecksAfterModel(t *testing.T) {
	for _, which := range []string{"revision", "comment", "decision", "file", "symlink", "permission", "repository head", "indexed head"} {
		t.Run(which, func(t *testing.T) {
			ctx := fixContext(t)
			f := setup(ctx, t, fixed, http.StatusCreated)
			var revoked atomic.Bool
			var indexChanged atomic.Bool
			f.r.CurrentRevision = func(context.Context) (string, error) {
				if indexChanged.Load() {
					return strings.Repeat("b", 40), nil
				}
				return f.req.Base, nil
			}
			f.r.Authorize = func(context.Context) error {
				if revoked.Load() {
					return context.Canceled
				}
				return nil
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				switch which {
				case "revision", "comment", "decision":
					rv, err := f.st.GetReview(ctx, f.req.ReviewID)
					require.NoError(t, err)
					switch which {
					case "revision":
						rv.Commit = strings.Repeat("b", 40)
					case "comment":
						rv.Comments[0].Message = "New task"
					case "decision":
						rv.Comments[0].Status = reviews.Rejected
					}
					require.NoError(t, f.st.SaveReview(ctx, rv))
				case "file", "symlink":
					entries, err := os.ReadDir(f.r.Workdir)
					require.NoError(t, err)
					require.Len(t, entries, 1)
					name := filepath.Join(f.r.Workdir, entries[0].Name(), "app/run.go")
					if which == "file" {
						require.NoError(t, os.WriteFile(name, []byte("package changed\n"), 0600))
					} else {
						require.NoError(t, os.Rename(name, name+".original"))
						require.NoError(t, os.Symlink("run.go.original", name))
					}
				case "permission":
					revoked.Store(true)
				case "indexed head":
					indexChanged.Store(true)
				case "repository head":
					_, err := gitx.Run(ctx, f.req.Root, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "--allow-empty", "-qm", "new head")
					require.NoError(t, err)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake", "choices": []any{map[string]any{"message": map[string]string{"content": fixed}, "finish_reason": "stop"}}})
			}))
			t.Cleanup(srv.Close)
			cfg, err := llm.Load("", func(k string) string {
				if k == "CODESYNAPSE_LLM_BASE_URL" {
					return srv.URL + "/v1"
				}
				return ""
			})
			require.NoError(t, err)
			f.r.LLM = llm.NewRegistry(cfg)
			_, err = f.r.Run(ctx, f.req)
			require.Error(t, err)
			assert.Len(t, f.pushed, 0)
		})
	}
}

func FuzzExtractFile(f *testing.F) {
	f.Add("```go\npackage app\n```\n")
	f.Add("```go\r\npackage app\r\n```\r\n")
	f.Fuzz(func(t *testing.T, s string) {
		out, err := extractFile(s)
		if err == nil {
			assert.True(t, len(out) <= maxFileBytes)
		}
	})
}

func TestCommittedFilesMatchValidatedChange(t *testing.T) {
	for _, which := range []string{"valid", "extra path", "wrong parent", "mode", "contents"} {
		t.Run(which, func(t *testing.T) {
			ctx := fixContext(t)
			f := setup(ctx, t, fixed, http.StatusCreated)
			root, err := os.OpenRoot(f.req.Root)
			require.NoError(t, err)
			defer func() { require.NoError(t, root.Close()) }()
			_, mode, err := readRegular(root, "app/run.go")
			require.NoError(t, err)
			output := []byte(strings.Replace(original, `exec("sh", "-c", cmd)`, `exec(cmd)`, 1))
			require.NoError(t, root.WriteFile("app/run.go", output, mode.Perm()))
			files := []string{"app/run.go"}
			switch which {
			case "extra path":
				require.NoError(t, root.WriteFile("extra.txt", []byte("unexpected"), 0600))
				files = append(files, "extra.txt")
			case "wrong parent":
				_, err = gitx.Run(ctx, f.req.Root, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "--allow-empty", "-qm", "intermediate")
				require.NoError(t, err)
			case "mode":
				require.NoError(t, os.Chmod(filepath.Join(f.req.Root, "app/run.go"), 0755))
			}
			commit, err := gitx.CommitFiles(ctx, f.req.Root, files, "test", f.r.Author)
			require.NoError(t, err)
			expected := checkedFile{name: "app/run.go", hash: sha256.Sum256(output), mode: mode}
			if which == "contents" {
				expected.hash = sha256.Sum256([]byte(original))
			}
			err = checkCommittedFiles(ctx, f.req.Root, f.req.Base, commit, []checkedFile{expected})
			if which == "valid" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
