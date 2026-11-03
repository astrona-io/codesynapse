package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// jobAccessClient is an accounts.RemoteAccess for a worker: it holds no OpenFGA
// key and proves which job it runs by that job's lease. The API (which alone
// holds the key) rebuilds the job's requester from its record and answers for
// that requester only. Without a job in ctx it cannot answer, so it refuses
// (docs/plans/OPENFGA_PLAN.md, phase 6).
type jobAccessClient struct {
	base  string
	owner string
	http  *http.Client
}

func (c *jobAccessClient) AccessIn(ctx context.Context, p accounts.Principal, q accounts.AccessQuery) (accounts.Access, error) {
	id := jobIDFrom(ctx)
	if id == "" || c.base == "" {
		return accounts.Access{}, accounts.ErrForbidden // a job outside the queue has no lease to prove
	}
	body, err := json.Marshal(jobAccessRequest{UserID: p.User.ID, Via: p.Via, KeyID: p.KeyID, AccessQuery: q})
	if err != nil {
		return accounts.Access{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/internal/jobs/"+url.PathEscape(id)+"/access", bytes.NewReader(body))
	if err != nil {
		return accounts.Access{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(JobLeaseHeader, c.owner)
	resp, err := c.http.Do(req)
	if err != nil {
		return accounts.Access{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return accounts.Access{}, err
	}
	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case http.StatusNotFound:
			return accounts.Access{}, accounts.ErrNotFound
		case http.StatusForbidden, http.StatusUnauthorized:
			return accounts.Access{}, accounts.ErrForbidden
		}
		return accounts.Access{}, fmt.Errorf("access question refused: %d", resp.StatusCode)
	}
	var a accounts.Access
	if err := json.Unmarshal(data, &a); err != nil {
		return accounts.Access{}, err
	}
	return a, nil
}

// jobAccessHTTP is the client a worker uses; no redirects, no cookies, bounded.
func jobAccessHTTP(base, owner string) *jobAccessClient {
	transport := &http.Transport{
		Proxy:               nil,
		MaxIdleConns:        2,
		IdleConnTimeout:     30 * time.Second,
		MaxIdleConnsPerHost: 2,
	}
	return &jobAccessClient{base: base, owner: owner, http: &http.Client{
		Transport: transport,
		Timeout:   20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}
