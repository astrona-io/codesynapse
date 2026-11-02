package fga

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	openfga "github.com/openfga/go-sdk"
	fgaclient "github.com/openfga/go-sdk/client"
	"github.com/openfga/go-sdk/credentials"
	fgatelemetry "github.com/openfga/go-sdk/telemetry"

	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// Environment variables. The token is a pre-shared key and comes from the
// environment only: never a flag, never logged.
const (
	EnvURL   = "CODESYNAPSE_OPENFGA_URL"
	EnvToken = "CODESYNAPSE_OPENFGA_TOKEN" //nolint:gosec // env var name, not a credential
)

// Bounds.
const (
	// DefaultTimeout bounds every call. It stays below OpenFGA's default
	// ListObjects deadline (3s): at its deadline OpenFGA answers with the
	// objects found so far and does not say so, and a client that gives up
	// first turns that into an error instead of a silently short list.
	DefaultTimeout = 2 * time.Second
	// ListLimit is the most objects a ListObjects answer may hold. It equals
	// OpenFGA's default list-objects-max-results (1000); an answer that
	// reaches it may have been cut and is refused.
	ListLimit = 1000
	// MaxBatchCheck is OpenFGA's default max-checks-per-batch-check.
	MaxBatchCheck = 50
	// MaxWrite is OpenFGA's default max-tuples-per-write; one call is one
	// transaction.
	MaxWrite = 100
	// maxStorePages bounds the store listing EnsureStore pages through.
	maxStorePages = 20
)

// Errors. Every error that comes with a decision means "not allowed".
var (
	ErrNotConfigured = errors.New(EnvURL + " is not set")
	ErrURL           = errors.New(EnvURL + " must be https://host[:port], or http:// to an internal service name (one label or *.svc[.cluster.local]), without credentials, path, query or fragment")
	ErrNoToken       = errors.New(EnvToken + " is not set")
	ErrTruncated     = errors.New("openfga: list answer may be truncated")
	ErrTooMany       = errors.New("openfga: too many items in one call")
	ErrNoStore       = errors.New("openfga: no store or model selected")
	ErrAmbiguous     = errors.New("openfga: more than one store has this name")
	ErrBadAnswer     = errors.New("openfga: incomplete answer")
)

// Config configures a Client.
type Config struct {
	URL   string
	Token string // pre-shared key; never logged
	// StoreID and ModelID select the store and model; EnsureStore and
	// EnsureModel find or create them.
	StoreID, ModelID string
	// Timeout bounds each call; DefaultTimeout when zero.
	Timeout time.Duration
}

// ConfigFromEnv reads CODESYNAPSE_OPENFGA_URL and CODESYNAPSE_OPENFGA_TOKEN.
// An unset URL answers ErrNotConfigured, so callers can tell "off" from
// "broken".
func ConfigFromEnv() (Config, error) {
	cfg := Config{URL: strings.TrimSpace(os.Getenv(EnvURL)), Token: os.Getenv(EnvToken)}
	if cfg.URL == "" {
		return cfg, ErrNotConfigured
	}
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	// Same rule as the alerts URL: https anywhere, plain http only to a
	// service name on the internal network, so the key never crosses an
	// outside network in clear text.
	if telemetry.CheckAlertsURL(c.URL) != nil {
		return ErrURL
	}
	if c.Token == "" {
		return ErrNoToken
	}
	return nil
}

// Tuple is one relationship: User has Relation on Object. User is
// "type:id" or a userset "type:id#relation", Object "type:id".
type Tuple struct {
	User, Relation, Object string
}

// Client wraps the OpenFGA SDK. Checks ask for HIGHER_CONSISTENCY, and every
// error, timeout or truncated answer is "not allowed" plus an error; there is
// no fallback to allow.
type Client struct {
	sdk     *fgaclient.OpenFgaClient
	timeout time.Duration
	store   string
	model   string
}

// New validates cfg and builds a client. The HTTP client follows no
// redirects (the key would go along) and ignores proxy settings.
func New(cfg Config) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	hc := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	sdk, err := fgaclient.NewSdkClient(&fgaclient.ClientConfiguration{
		ApiUrl:               strings.TrimRight(cfg.URL, "/"),
		StoreId:              cfg.StoreID,
		AuthorizationModelId: cfg.ModelID,
		Credentials: &credentials.Credentials{
			Method: credentials.CredentialsMethodApiToken,
			Config: &credentials.Config{ApiToken: cfg.Token},
		},
		UserAgent:  "codesynapse",
		HTTPClient: hc,
		// One retry inside the call's deadline: a lost answer does not
		// become a refusal, a slow server still ends in one.
		RetryParams: &openfga.RetryParams{MaxRetry: 1, MinWaitInMs: 50},
		// The SDK's own metrics carry store and model IDs and URLs; phase 5
		// adds bounded platform metrics instead.
		Telemetry: &fgatelemetry.Configuration{Metrics: &fgatelemetry.MetricsConfiguration{}},
	})
	if err != nil {
		return nil, fmt.Errorf("openfga client: %w", err)
	}
	return &Client{sdk: sdk, timeout: timeout, store: cfg.StoreID, model: cfg.ModelID}, nil
}

// StoreID returns the selected store.
func (c *Client) StoreID() string { return c.store }

// ModelID returns the selected authorisation model.
func (c *Client) ModelID() string { return c.model }

func (c *Client) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.timeout)
}

func (c *Client) ids() (*string, *string, error) {
	if c.store == "" || c.model == "" {
		return nil, nil, ErrNoStore
	}
	s, m := c.store, c.model
	return &s, &m, nil
}

func higher() *openfga.ConsistencyPreference {
	p := openfga.CONSISTENCYPREFERENCE_HIGHER_CONSISTENCY
	return &p
}

// Check reports whether t.User has t.Relation on t.Object.
func (c *Client) Check(ctx context.Context, t Tuple) (bool, error) {
	store, model, err := c.ids()
	if err != nil {
		return false, err
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.sdk.Check(ctx).
		Body(fgaclient.ClientCheckRequest{User: t.User, Relation: t.Relation, Object: t.Object}).
		Options(fgaclient.ClientCheckOptions{StoreId: store, AuthorizationModelId: model, Consistency: higher()}).
		Execute()
	if err != nil {
		return false, fmt.Errorf("openfga check: %w", err)
	}
	if resp == nil || resp.Allowed == nil {
		return false, ErrBadAnswer
	}
	return *resp.Allowed, nil
}

// BatchCheck answers each check in order, in one request of at most
// MaxBatchCheck checks. Any error, including one for a single check, makes
// every answer false.
func (c *Client) BatchCheck(ctx context.Context, checks []Tuple) ([]bool, error) {
	out := make([]bool, len(checks))
	if len(checks) == 0 {
		return out, nil
	}
	if len(checks) > MaxBatchCheck {
		return out, ErrTooMany
	}
	store, model, err := c.ids()
	if err != nil {
		return out, err
	}
	items := make([]fgaclient.ClientBatchCheckItem, len(checks))
	for i, t := range checks {
		items[i] = fgaclient.ClientBatchCheckItem{User: t.User, Relation: t.Relation, Object: t.Object, CorrelationId: strconv.Itoa(i)}
	}
	one := int32(1)
	size := int32(MaxBatchCheck)
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.sdk.BatchCheck(ctx).
		Body(fgaclient.ClientBatchCheckRequest{Checks: items}).
		Options(fgaclient.BatchCheckOptions{StoreId: store, AuthorizationModelId: model, Consistency: higher(), MaxParallelRequests: &one, MaxBatchSize: &size}).
		Execute()
	if err != nil {
		return out, fmt.Errorf("openfga batch check: %w", err)
	}
	if resp == nil || resp.Result == nil {
		return out, ErrBadAnswer
	}
	res := *resp.Result
	got := make([]bool, len(checks))
	for i := range checks {
		r, ok := res[strconv.Itoa(i)]
		if !ok || r.Error != nil || r.Allowed == nil {
			return out, fmt.Errorf("%w: check %d", ErrBadAnswer, i)
		}
		got[i] = *r.Allowed
	}
	return got, nil
}

// ListObjects returns the objects of objectType on which user has relation.
// An answer of ListLimit objects or more may have been cut short and is
// refused with ErrTruncated; so is any error, with no objects.
func (c *Client) ListObjects(ctx context.Context, user, relation, objectType string) ([]string, error) {
	store, model, err := c.ids()
	if err != nil {
		return nil, err
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.sdk.ListObjects(ctx).
		Body(fgaclient.ClientListObjectsRequest{User: user, Relation: relation, Type: objectType}).
		Options(fgaclient.ClientListObjectsOptions{StoreId: store, AuthorizationModelId: model, Consistency: higher()}).
		Execute()
	if err != nil {
		return nil, fmt.Errorf("openfga list objects: %w", err)
	}
	if resp == nil {
		return nil, ErrBadAnswer
	}
	if len(resp.Objects) >= ListLimit {
		return nil, ErrTruncated
	}
	return resp.Objects, nil
}

// WriteTuples writes at most MaxWrite tuples in one transaction. Tuples that
// exist already are ignored, so a replay is harmless.
func (c *Client) WriteTuples(ctx context.Context, tuples []Tuple) error {
	keys := make([]fgaclient.ClientTupleKey, len(tuples))
	for i, t := range tuples {
		keys[i] = fgaclient.ClientTupleKey{User: t.User, Relation: t.Relation, Object: t.Object}
	}
	return c.write(ctx, fgaclient.ClientWriteRequest{Writes: keys}, len(tuples))
}

// DeleteTuples deletes at most MaxWrite tuples in one transaction. Tuples
// that are gone already are ignored.
func (c *Client) DeleteTuples(ctx context.Context, tuples []Tuple) error {
	keys := make([]fgaclient.ClientTupleKeyWithoutCondition, len(tuples))
	for i, t := range tuples {
		keys[i] = fgaclient.ClientTupleKeyWithoutCondition{User: t.User, Relation: t.Relation, Object: t.Object}
	}
	return c.write(ctx, fgaclient.ClientWriteRequest{Deletes: keys}, len(tuples))
}

func (c *Client) write(ctx context.Context, req fgaclient.ClientWriteRequest, n int) error {
	if n == 0 {
		return nil
	}
	if n > MaxWrite {
		return ErrTooMany
	}
	store, model, err := c.ids()
	if err != nil {
		return err
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	_, err = c.sdk.Write(ctx).Body(req).Options(fgaclient.ClientWriteOptions{
		StoreId:              store,
		AuthorizationModelId: model,
		// Transaction mode (the default) sends all tuples in one request.
		Conflict: fgaclient.ClientWriteConflictOptions{
			OnDuplicateWrites: fgaclient.CLIENT_WRITE_REQUEST_ON_DUPLICATE_WRITES_IGNORE,
			OnMissingDeletes:  fgaclient.CLIENT_WRITE_REQUEST_ON_MISSING_DELETES_IGNORE,
		},
	}).Execute()
	if err != nil {
		return fmt.Errorf("openfga write: %w", err)
	}
	return nil
}

// EnsureStore selects the store called name, creating it when none exists,
// and returns its ID. Two stores with the name are an error: picking one
// could split decisions between them.
func (c *Client) EnsureStore(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", errors.New("openfga: empty store name")
	}
	var found []string
	var token *string
	for page := 0; ; page++ {
		if page == maxStorePages {
			return "", ErrTooMany
		}
		cctx, cancel := c.call(ctx)
		resp, err := c.sdk.ListStores(cctx).Options(fgaclient.ClientListStoresOptions{Name: &name, ContinuationToken: token}).Execute()
		cancel()
		if err != nil {
			return "", fmt.Errorf("openfga list stores: %w", err)
		}
		for _, s := range resp.Stores {
			// The name filter is the server's; check it here too.
			if s.Name == name && s.DeletedAt == nil {
				found = append(found, s.Id)
			}
		}
		if resp.ContinuationToken == "" {
			break
		}
		t := resp.ContinuationToken
		token = &t
	}
	switch len(found) {
	case 0:
		cctx, cancel := c.call(ctx)
		defer cancel()
		resp, err := c.sdk.CreateStore(cctx).Body(fgaclient.ClientCreateStoreRequest{Name: name}).Execute()
		if err != nil {
			return "", fmt.Errorf("openfga create store: %w", err)
		}
		c.store = resp.Id
	case 1:
		c.store = found[0]
	default:
		return "", ErrAmbiguous
	}
	c.model = ""
	return c.store, nil
}

// EnsureModel selects the store's latest model when it equals Model(), and
// writes Model() as a new one otherwise. It returns the model ID.
func (c *Client) EnsureModel(ctx context.Context) (string, error) {
	if c.store == "" {
		return "", ErrNoStore
	}
	want, err := Model()
	if err != nil {
		return "", err
	}
	store := c.store
	cctx, cancel := c.call(ctx)
	latest, err := c.sdk.ReadLatestAuthorizationModel(cctx).Options(fgaclient.ClientReadLatestAuthorizationModelOptions{StoreId: &store}).Execute()
	cancel()
	if err != nil {
		return "", fmt.Errorf("openfga read model: %w", err)
	}
	if latest != nil && latest.AuthorizationModel != nil && sameModel(want, *latest.AuthorizationModel) {
		c.model = latest.AuthorizationModel.Id
		return c.model, nil
	}
	cctx, cancel = c.call(ctx)
	defer cancel()
	resp, err := c.sdk.WriteAuthorizationModel(cctx).Body(want).Options(fgaclient.ClientWriteAuthorizationModelOptions{StoreId: &store}).Execute()
	if err != nil {
		return "", fmt.Errorf("openfga write model: %w", err)
	}
	c.model = resp.AuthorizationModelId
	return c.model, nil
}
