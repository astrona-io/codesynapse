package auditarchive

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// S3 keeps segment files in an S3-compatible bucket (AWS S3, MinIO, Ceph,
// Garage, ...), signed with Signature Version 4 from the standard library.
// With LockMode set, every file carries an object lock until its retention
// ends, so not even these credentials can change or remove it early; the
// bucket must have object lock enabled.
type S3 struct {
	Endpoint     string // https://s3.eu-central-1.amazonaws.com or a MinIO URL
	Bucket       string
	Region       string
	Prefix       string // e.g. audit/
	AccessKey    string
	SecretKey    string // never logged
	SessionToken string
	LockMode     string // COMPLIANCE, GOVERNANCE or empty
	HTTP         *http.Client
	now          func() time.Time
}

var (
	bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	prefixPattern = regexp.MustCompile(`^([A-Za-z0-9_.-]+/)*$`)
	regionPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
)

// Check validates the settings. Plain http is refused unless allowHTTP (a
// MinIO in the same compose project).
func (s *S3) Check(allowHTTP bool) error {
	u, err := url.Parse(s.Endpoint)
	switch {
	case err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || strings.Trim(u.Path, "/") != "":
		return errors.New("audit S3 endpoint must be http(s)://host[:port] without credentials, path, query or fragment")
	case u.Scheme != "https" && (u.Scheme != "http" || !allowHTTP):
		return errors.New("audit S3 endpoint must use https")
	case !bucketPattern.MatchString(s.Bucket):
		return errors.New("audit S3 bucket name is invalid")
	case !regionPattern.MatchString(s.Region):
		return errors.New("audit S3 region is invalid")
	case !prefixPattern.MatchString(s.Prefix) || len(s.Prefix) > 200 || strings.Contains("/"+s.Prefix, "/./") || strings.Contains("/"+s.Prefix, "/../"):
		return errors.New("audit S3 prefix must be empty or path segments ending in / (no . or ..)")
	case s.AccessKey == "" || s.SecretKey == "":
		return errors.New("audit S3 access key and secret key are required")
	case s.LockMode != "" && s.LockMode != "COMPLIANCE" && s.LockMode != "GOVERNANCE":
		return errors.New("audit S3 lock mode must be COMPLIANCE, GOVERNANCE or empty")
	}
	return nil
}

// Backend implements Archive.
func (s *S3) Backend() string { return "s3" }

// Put implements Archive.
func (s *S3) Put(ctx context.Context, key string, body []byte, retainUntil time.Time) error {
	sum := sha256.Sum256(body)
	h := http.Header{}
	h.Set("Content-Type", "application/gzip")
	// The checksum makes S3 refuse a body that changed on the way; object
	// lock needs one anyway.
	h.Set("X-Amz-Checksum-Sha256", base64.StdEncoding.EncodeToString(sum[:]))
	if s.LockMode != "" {
		h.Set("X-Amz-Object-Lock-Mode", s.LockMode)
		h.Set("X-Amz-Object-Lock-Retain-Until-Date", retainUntil.UTC().Format(time.RFC3339))
	}
	res, err := s.do(ctx, http.MethodPut, key, h, body, hex.EncodeToString(sum[:]))
	if err != nil {
		return err
	}
	defer drain(res)
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("audit S3 put: HTTP %d", res.StatusCode)
	}
	return nil
}

// Get implements Archive. A missing file answers fs.ErrNotExist.
func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	res, err := s.do(ctx, http.MethodGet, key, http.Header{}, nil, emptySHA256)
	if err != nil {
		return nil, err
	}
	defer drain(res)
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("audit S3 get %s: %w", key, fs.ErrNotExist)
	default:
		return nil, fmt.Errorf("audit S3 get: HTTP %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, MaxFileBytes+1))
	if err == nil && len(b) > MaxFileBytes {
		err = errors.New("audit archive file is too large")
	}
	return b, err
}

// Delete implements Archive. In a versioned (object lock) bucket this adds
// a delete marker; a lifecycle rule for noncurrent versions removes the
// locked data once its retention ended.
func (s *S3) Delete(ctx context.Context, key string) error {
	res, err := s.do(ctx, http.MethodDelete, key, http.Header{}, nil, emptySHA256)
	if err != nil {
		return err
	}
	defer drain(res)
	if res.StatusCode != http.StatusNoContent && res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNotFound {
		return fmt.Errorf("audit S3 delete: HTTP %d", res.StatusCode)
	}
	return nil
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (s *S3) do(ctx context.Context, method, key string, h http.Header, body []byte, payloadHash string) (*http.Response, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	// Path style works with every S3-compatible store; bucket, prefix and
	// key are validated, so nothing in the path needs escaping.
	u := strings.TrimRight(s.Endpoint, "/") + "/" + s.Bucket + "/" + s.Prefix + key
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = h
	if body == nil {
		req.Body, req.ContentLength = http.NoBody, 0
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	if s.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.SessionToken)
	}
	sign(req, s.AccessKey, s.SecretKey, s.Region, payloadHash, now())
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	// A redirect would carry the signed request elsewhere: never follow one.
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c.Do(req)
}

func drain(res *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16)) //nolint:errcheck // drained for keep-alive
	res.Body.Close()                                            //nolint:errcheck,gosec // read-only body
}

// sign adds AWS Signature Version 4 headers for service s3. Every header
// already set on req is signed, with the host.
func sign(req *http.Request, accessKey, secretKey, region, payloadHash string, t time.Time) {
	amzDate := t.UTC().Format("20060102T150405Z")
	day := amzDate[:8]
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ",")
	}
	names := slices.Sorted(maps.Keys(headers))
	var canon strings.Builder
	for _, k := range names {
		canon.WriteString(k + ":" + strings.Join(strings.Fields(headers[k]), " ") + "\n")
	}
	signed := strings.Join(names, ";")
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	request := strings.Join([]string{req.Method, path, req.URL.Query().Encode(), canon.String(), signed, payloadHash}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	reqHash := sha256.Sum256([]byte(request))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(reqHash[:])
	key := []byte("AWS4" + secretKey)
	for _, part := range []string{day, region, "s3", "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
		", SignedHeaders="+signed+", Signature="+hex.EncodeToString(hmacSHA256(key, toSign)))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
