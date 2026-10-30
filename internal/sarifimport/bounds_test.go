package sarifimport

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// logWith is a one-run log whose run also holds extra, JSON members.
func logWith(extra string) string {
	return `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},` + extra + `}]}`
}

// repeat joins n copies of s with commas.
func repeat(s string, n int) string {
	return strings.TrimSuffix(strings.Repeat(s+",", n), ",")
}

// repeatKeys is n object members k0…, each with value v.
func repeatKeys(v string, n int) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"k%d":%s`, i, v)
	}
	return b.String()
}

const oneResult = `{"ruleId":"r","locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.go"}}}]}`

// boundLogs are inputs whose repeated parts used to be decoded into memory
// many times their size before any limit applied.
func boundLogs(n int) map[string]string {
	return map[string]string{
		"empty results":     logWith(`"results":[` + repeat(`{}`, n) + `]`),
		"locations":         logWith(`"results":[{"ruleId":"r","locations":[` + repeat(`{}`, n) + `]}]`),
		"suppressions":      logWith(`"results":[{"ruleId":"r","suppressions":[` + repeat(`{}`, n) + `]}]`),
		"arguments":         logWith(`"results":[{"ruleId":"r","message":{"arguments":[` + repeat(`""`, n) + `]}}]`),
		"artifacts":         logWith(`"artifacts":[` + repeat(`{}`, n) + `]`),
		"base ids":          logWith(`"originalUriBaseIds":{` + repeatKeys(`{"uri":"file:///a/"}`, n) + `}`),
		"message strings":   `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t","rules":[{"id":"r","messageStrings":{` + repeatKeys(`{"text":""}`, n) + `}}]}}}]}`,
		"rules":             `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t","rules":[` + repeat(`{}`, n) + `]}}}]}`,
		"unknown nested":    logWith(`"x":[` + repeat(`{"a":[{}]}`, n) + `]`),
		"results in budget": logWith(`"results":[` + repeat(oneResult, min(n, MaxFindings)) + `]`),
	}
}

// TestParseBoundsMemory: what Parse allocates grows with what it keeps,
// never with how often the log repeats something. Before the reader
// streamed, 30 MiB of "results":[{},…] allocated 10.5 GiB.
func TestParseBoundsMemory(t *testing.T) {
	for name, log := range boundLogs(400_000) {
		t.Run(name, func(t *testing.T) {
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			_, err := Parse(strings.NewReader(log))
			runtime.ReadMemStats(&after)
			if err != nil {
				assert.True(t, errors.Is(err, ErrTooLarge), err.Error())
			}
			// Tokens are read one at a time, so even the total allocated,
			// not only what stays live, is a small multiple of the input.
			alloc := after.TotalAlloc - before.TotalAlloc
			limit := uint64(20*len(log)) + 32<<20
			assert.True(t, alloc < limit, fmt.Sprintf("%d bytes allocated for a %d byte log", alloc, len(log)))
		})
	}
}

// TestParseBoundsMessages: texts, templates and descriptions are cut before
// arguments are filled in and before redaction, so a huge "{0}" template no
// longer costs seconds and hundreds of megabytes per result.
func TestParseBoundsMessages(t *testing.T) {
	tmpl := strings.Repeat("{0}", 1<<20)
	arg := `"` + strings.Repeat("x ", MaxMessage/2) + `"`
	args := `[` + repeat(arg, maxArgs) + `]`
	big := strings.Repeat("word ", 600_000)
	templated := `{"ruleId":"r","message":{"id":"m","arguments":` + args + `},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.go"},"region":{"startLine":1}}}]}`
	log := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t","rules":[{"id":"r",
	  "shortDescription":{"text":"` + big + `"},
	  "messageStrings":{"m":{"text":"` + tmpl + `"}}}]}},
	  "results":[` + repeat(templated, 20) + `,
	    {"ruleId":"r","message":{"text":"` + big + `"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"b.go"}}}]},
	    {"ruleIndex":0,"locations":[{"physicalLocation":{"artifactLocation":{"uri":"c.go"}}}]}]}]}`
	start := time.Now()
	runs, err := Parse(strings.NewReader(log))
	require.NoError(t, err)
	assert.True(t, time.Since(start) < 10*time.Second, time.Since(start).String())
	require.Equal(t, len(runs), 1)
	assert.Equal(t, len(runs[0].Findings), 3, "the 20 templated results are one finding")
	for _, f := range runs[0].Findings {
		assert.True(t, f.Message != "" && len([]rune(f.Message)) <= MaxMessage, f.Message)
	}
}

func TestFill(t *testing.T) {
	assert.Equal(t, fill("a {0} b {1} {2} {x}", []string{"{1}", "y"}), "a {1} b y {2} {x}")
	long := fill(strings.Repeat("{0}", 10_000), []string{strings.Repeat("z", 100)})
	assert.True(t, len(long) <= maxRawMessage, fmt.Sprint(len(long)))
}

func TestClipWords(t *testing.T) {
	assert.Equal(t, clipWords("short", 10), "short")
	assert.Equal(t, clipWords("key ghp_abcdefghijklmnop", 12), "key", "a split word is dropped")
	assert.Equal(t, clipWords("abcdefghij", 4), "abcd", "a single word is cut")
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

type readLimitError struct{}

func (readLimitError) Error() string { return "request body too large" }

// TestParseKeepsReaderError: the caller can still tell a body cut off by its
// own limit (http.MaxBytesError) from a malformed log.
func TestParseKeepsReaderError(t *testing.T) {
	_, err := Parse(io.MultiReader(strings.NewReader(`{"version":"2.1.0","runs":[`), failingReader{readLimitError{}}))
	var limit readLimitError
	assert.True(t, errors.As(err, &limit), fmt.Sprint(err))
	assert.True(t, errors.Is(err, ErrInvalid), fmt.Sprint(err))
}

func TestParseRefusesDeepNesting(t *testing.T) {
	deep := strings.Repeat("[", 10_000) + strings.Repeat("]", 10_000)
	_, err := Parse(strings.NewReader(logWith(`"x":` + deep)))
	assert.True(t, errors.Is(err, ErrInvalid), fmt.Sprint(err))
	_, err = Parse(strings.NewReader(logWith(`"x":` + strings.Repeat("[", 60) + strings.Repeat("]", 60))))
	assert.NoError(t, err)
}
