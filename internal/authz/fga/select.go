package fga

import (
	"context"
	"errors"
	"fmt"

	fgaclient "github.com/openfga/go-sdk/client"
)

// findStores lists the IDs of the live stores called name.
func (c *Client) findStores(ctx context.Context, name string) ([]string, error) {
	if name == "" {
		return nil, errors.New("openfga: empty store name")
	}
	var found []string
	var token *string
	for page := 0; ; page++ {
		if page == maxStorePages {
			return nil, ErrTooMany
		}
		cctx, cancel := c.call(ctx)
		resp, err := c.sdk.ListStores(cctx).Options(fgaclient.ClientListStoresOptions{Name: &name, ContinuationToken: token}).Execute()
		cancel()
		if err != nil {
			return nil, fmt.Errorf("openfga list stores: %w", err)
		}
		for _, s := range resp.Stores {
			// The name filter is the server's; check it here too.
			if s.Name == name && s.DeletedAt == nil {
				found = append(found, s.Id)
			}
		}
		if resp.ContinuationToken == "" {
			return found, nil
		}
		t := resp.ContinuationToken
		token = &t
	}
}

// Select returns a client for the one store called name and its latest
// model, which must equal Model(). Unlike EnsureStore and EnsureModel it
// creates nothing and leaves c as it is, so a process that only decides
// never writes to OpenFGA and the client it returns is safe to share.
// Without the store, or before the sync wrote this model, it answers
// ErrNoStore.
func (c *Client) Select(ctx context.Context, name string) (*Client, error) {
	found, err := c.findStores(ctx, name)
	if err != nil {
		return nil, err
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("%w: no store %q yet", ErrNoStore, name)
	case 1:
	default:
		return nil, ErrAmbiguous
	}
	want, err := Model()
	if err != nil {
		return nil, err
	}
	store := found[0]
	cctx, cancel := c.call(ctx)
	defer cancel()
	latest, err := c.sdk.ReadLatestAuthorizationModel(cctx).Options(fgaclient.ClientReadLatestAuthorizationModelOptions{StoreId: &store}).Execute()
	if err != nil {
		return nil, fmt.Errorf("openfga read model: %w", err)
	}
	if latest == nil || latest.AuthorizationModel == nil || !sameModel(want, *latest.AuthorizationModel) {
		return nil, fmt.Errorf("%w: this model is not written yet", ErrNoStore)
	}
	return &Client{sdk: c.sdk, timeout: c.timeout, store: store, model: latest.AuthorizationModel.Id}, nil
}
