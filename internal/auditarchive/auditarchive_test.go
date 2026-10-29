package auditarchive

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// The GET Object example of the AWS Signature Version 4 documentation.
func TestSignMatchesAWSExample(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	require.NoError(t, err)
	req.Header.Set("Range", "bytes=0-9")
	sign(req, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", emptySHA256,
		time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	assert.Equal(t, req.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, "+
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41")
}

func TestKeys(t *testing.T) {
	assert.Equal(t, segmentKey(1, 20000), "00000000000000000001-00000000000000020000.jsonl.gz")
	for _, bad := range []string{"", "../x", "1-2.jsonl.gz", "00000000000000000001-00000000000000020000.jsonl.gz/..", "/etc/passwd"} {
		assert.Error(t, checkKey(bad), bad)
	}
}

func TestDir(t *testing.T) {
	ctx := context.Background()
	d, err := NewDir(filepath.Join(t.TempDir(), "audit"))
	require.NoError(t, err)
	key := segmentKey(1, 2)
	require.NoError(t, d.Put(ctx, key, []byte("one"), time.Now()))
	require.NoError(t, d.Put(ctx, key, []byte("two"), time.Now())) // a retried seal replaces the file
	b, err := d.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, string(b), "two")
	info, err := os.Stat(filepath.Join(d.root, key))
	require.NoError(t, err)
	assert.Equal(t, info.Mode().Perm(), os.FileMode(0o400))
	require.NoError(t, d.Delete(ctx, key))
	require.NoError(t, d.Delete(ctx, key))
	_, err = d.Get(ctx, key)
	assert.True(t, errors.Is(err, fs.ErrNotExist))
	assert.Error(t, d.Put(ctx, "../escape", nil, time.Now()))
}

// fakeS3 is a bucket that checks signatures are present and checksums match.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	headers http.Header
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=key/") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(b)
		if r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) || r.Header.Get("X-Amz-Checksum-Sha256") != base64.StdEncoding.EncodeToString(sum[:]) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.objects[r.URL.Path], f.headers = b, r.Header.Clone()
	case http.MethodGet:
		b, ok := f.objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(b)
	case http.MethodDelete:
		delete(f.objects, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}
}

func TestS3(t *testing.T) {
	ctx := context.Background()
	f := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	s := &S3{Endpoint: srv.URL, Bucket: "audit-bucket", Region: "eu-north-1", Prefix: "cs/audit/", AccessKey: "key", SecretKey: "secret", LockMode: "COMPLIANCE"}
	require.NoError(t, s.Check(true))
	assert.Error(t, s.Check(false)) // plain http needs the explicit switch

	key := segmentKey(1, 2)
	until := time.Date(2027, 11, 30, 12, 0, 0, 0, time.UTC)
	require.NoError(t, s.Put(ctx, key, []byte("sealed"), until))
	assert.Equal(t, f.headers.Get("X-Amz-Object-Lock-Mode"), "COMPLIANCE")
	assert.Equal(t, f.headers.Get("X-Amz-Object-Lock-Retain-Until-Date"), "2027-11-30T12:00:00Z")
	_, ok := f.objects["/audit-bucket/cs/audit/"+key]
	assert.True(t, ok)
	b, err := s.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, string(b), "sealed")
	require.NoError(t, s.Delete(ctx, key))
	_, err = s.Get(ctx, key)
	assert.True(t, errors.Is(err, fs.ErrNotExist))

	bad := *s
	bad.AccessKey = "other"
	assert.Error(t, bad.Put(ctx, key, []byte("x"), until))
}

func TestS3RefusesRedirects(t *testing.T) {
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer target.Close()
	srv := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusTemporaryRedirect))
	defer srv.Close()
	s := &S3{Endpoint: srv.URL, Bucket: "audit-bucket", Region: "us-east-1", AccessKey: "key", SecretKey: "secret"}
	assert.Error(t, s.Put(context.Background(), segmentKey(1, 1), []byte("x"), time.Now()))
	assert.Equal(t, hits, 0)
}

func TestS3Check(t *testing.T) {
	ok := S3{Endpoint: "https://s3.example.com", Bucket: "audit", Region: "us-east-1", Prefix: "audit/", AccessKey: "k", SecretKey: "s"}
	require.NoError(t, ok.Check(false))
	for name, change := range map[string]func(*S3){
		"credentials in url": func(s *S3) { s.Endpoint = "https://u:p@s3.example.com" },
		"path in url":        func(s *S3) { s.Endpoint = "https://s3.example.com/bucket" },
		"bucket":             func(s *S3) { s.Bucket = "Bad_Bucket" },
		"prefix escapes":     func(s *S3) { s.Prefix = "../" },
		"prefix without /":   func(s *S3) { s.Prefix = "audit" },
		"no secret":          func(s *S3) { s.SecretKey = "" },
		"lock mode":          func(s *S3) { s.LockMode = "forever" },
	} {
		t.Run(name, func(t *testing.T) {
			s := ok
			change(&s)
			assert.Error(t, s.Check(true))
		})
	}
}

func TestFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	a, err := FromEnv(env(nil))
	require.NoError(t, err)
	assert.True(t, a == nil)
	a, err = FromEnv(env(map[string]string{EnvDir: t.TempDir()}))
	require.NoError(t, err)
	assert.Equal(t, a.Backend(), "dir")
	_, err = FromEnv(env(map[string]string{EnvDir: t.TempDir(), EnvS3Endpoint: "https://s3.example.com"}))
	assert.Error(t, err)
	a, err = FromEnv(env(map[string]string{EnvS3Endpoint: "https://s3.example.com", EnvS3Bucket: "audit", EnvS3AccessKey: "k", EnvS3SecretKey: "s"}))
	require.NoError(t, err)
	s3 := a.(*S3)
	assert.Equal(t, s3.Region, "us-east-1")
	assert.Equal(t, s3.Prefix, "audit/")
}

func addEvents(ctx context.Context, t *testing.T, st accounts.Store, from time.Time, n int) {
	t.Helper()
	for i := range n {
		_, err := st.AddEvent(ctx, accounts.Event{At: from.Add(time.Duration(i) * time.Hour), Action: "user.login", ActorEmail: "ada@example.com"})
		require.NoError(t, err)
	}
}

func TestSealExpireVerify(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	arch, err := NewDir(t.TempDir())
	require.NoError(t, err)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	addEvents(ctx, t, st, start, 10)

	// Entries before hour 6 are sealed; nothing is left to seal the second time.
	segs, err := Seal(ctx, st, arch, start.Add(6*time.Hour), 400*24*time.Hour, start.Add(48*time.Hour))
	require.NoError(t, err)
	require.Len(t, segs, 1)
	assert.Equal(t, segs[0].Count, 6)
	assert.Equal(t, segs[0].LastAt, start.Add(5*time.Hour))
	again, err := Seal(ctx, st, arch, start.Add(6*time.Hour), time.Hour, start)
	require.NoError(t, err)
	assert.Len(t, again, 0)

	// New entries still link to the sealed ones.
	addEvents(ctx, t, st, start.Add(10*time.Hour), 2)
	v, err := DeepVerify(ctx, st, arch)
	require.NoError(t, err)
	assert.True(t, v.OK, v.Reason)
	assert.Equal(t, v.Segments, 1)
	assert.Equal(t, v.Sealed, 6)
	assert.Equal(t, v.Archives, 1)
	assert.Equal(t, v.Checked, 6)
	assert.Equal(t, v.FirstID, int64(1))

	_, err = Seal(ctx, st, arch, start.Add(9*time.Hour), time.Hour, start)
	require.NoError(t, err)
	expired, err := Expire(ctx, st, arch, start.Add(6*time.Hour))
	require.NoError(t, err)
	require.Len(t, expired, 1)
	_, err = arch.Get(ctx, expired[0].Key)
	assert.True(t, errors.Is(err, fs.ErrNotExist))
	v, err = DeepVerify(ctx, st, arch)
	require.NoError(t, err)
	assert.True(t, v.OK, v.Reason)
	assert.Equal(t, v.FirstID, int64(7)) // the next segment is the anchor now
	assert.Equal(t, v.Archives, 1)
}

func TestSealSplitsLargeRuns(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	arch, err := NewDir(t.TempDir())
	require.NoError(t, err)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range SegmentEntries + 5 {
		_, err := st.AddEvent(ctx, accounts.Event{At: start.Add(time.Duration(i) * time.Second), Action: "x"})
		require.NoError(t, err)
	}
	segs, err := Seal(ctx, st, arch, start.Add(time.Hour*24), time.Hour, start)
	require.NoError(t, err)
	require.Len(t, segs, 2)
	assert.Equal(t, segs[0].Count, SegmentEntries)
	assert.Equal(t, segs[1].PrevHash, segs[0].LastHash)
	v, err := DeepVerify(ctx, st, arch)
	require.NoError(t, err)
	assert.True(t, v.OK, v.Reason)
	assert.Equal(t, v.Sealed, SegmentEntries+5)
}

func TestDeepVerifyFindsChangedArchives(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	setup := func(t *testing.T) (*memstore.Store, *Dir, accounts.AuditSegment) {
		st := memstore.New()
		arch, err := NewDir(t.TempDir())
		require.NoError(t, err)
		addEvents(ctx, t, st, start, 4)
		segs, err := Seal(ctx, st, arch, start.Add(3*time.Hour), time.Hour, start)
		require.NoError(t, err)
		require.Len(t, segs, 1)
		return st, arch, segs[0]
	}
	replace := func(t *testing.T, arch *Dir, key string, body []byte) {
		path := filepath.Join(arch.root, key)
		require.NoError(t, os.Chmod(path, 0o600))
		require.NoError(t, os.WriteFile(path, body, 0o600))
	}
	t.Run("missing file", func(t *testing.T) {
		st, arch, seg := setup(t)
		require.NoError(t, arch.Delete(ctx, seg.Key))
		v, err := DeepVerify(ctx, st, arch)
		require.NoError(t, err)
		assert.False(t, v.OK)
		assert.Contains(t, v.Reason, "cannot be read")
	})
	t.Run("changed bytes", func(t *testing.T) {
		st, arch, seg := setup(t)
		replace(t, arch, seg.Key, []byte("not the archive"))
		v, err := DeepVerify(ctx, st, arch)
		require.NoError(t, err)
		assert.False(t, v.OK)
		assert.Contains(t, v.Reason, "checksum")
	})
	t.Run("a shallow check only links hashes", func(t *testing.T) {
		st, arch, seg := setup(t)
		replace(t, arch, seg.Key, []byte("not the archive"))
		v, err := accounts.VerifyAudit(ctx, st)
		require.NoError(t, err)
		assert.True(t, v.OK)
		assert.Equal(t, v.Archives, 0)
	})
}

func FuzzDecode(f *testing.F) {
	body, err := encode([]accounts.Event{{ID: 1, Action: "x"}})
	require.NoError(f, err)
	f.Add(body)
	f.Add([]byte("not gzip"))
	f.Fuzz(func(_ *testing.T, b []byte) {
		_, _ = decode(b) //nolint:errcheck // must not panic or run away
	})
}

func TestSealCountsRuns(t *testing.T) {
	assert.Equal(t, sealOutcome(nil, nil), telemetry.SealNothing)
	assert.Equal(t, sealOutcome([]accounts.AuditSegment{{Count: 1}}, nil), telemetry.SealOK)
	assert.Equal(t, sealOutcome([]accounts.AuditSegment{{Count: 1}}, errors.New("put failed")), telemetry.SealError)

	ctx := context.Background()
	arch, err := NewDir(t.TempDir())
	require.NoError(t, err)
	_, err = Seal(ctx, memstore.New(), arch, time.Now(), time.Hour, time.Now())
	require.NoError(t, err)
	var b strings.Builder
	require.NoError(t, telemetry.Default.WriteText(&b))
	assert.Contains(t, b.String(), `platform_codesynapse_audit_seals_total{outcome="nothing_to_seal"}`)
}
