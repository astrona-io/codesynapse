package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/astrona-io/codesynapse/internal/fetchsource"
)

// fetch_source (internal/fetchsource) is answered here, on the developer's
// machine: the server never fetches a URL an agent names, so the tool reaches
// nothing the developer's own browser could not. Pages are cached in the
// user cache folder. The call is reported like the git tools, without the
// URL, its host or the checkout's name.

// fetchSourceTimeout bounds one answer: the fetch's own timeout plus the
// report and the cache.
const fetchSourceTimeout = fetchsource.Timeout + 15*time.Second

// answerFetchSource answers a tools/call for fetch_source and reports
// whether it did.
func (b *bridge) answerFetchSource(ctx context.Context, msg []byte, m rpcMessage) bool {
	var c toolCall
	if json.Unmarshal(msg, &c) != nil || c.Params.Name != fetchsource.Tool || len(m.ID) == 0 || !b.mayAnswer(c.Params.Name) {
		return false
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, fetchSourceTimeout)
	defer cancel()

	result, failed := b.fetchSource(ctx, c)
	out, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	if err != nil {
		out = errorResponse(m.ID, err)
		failed = true
	}
	if !b.reportLocal(ctx, c.Params.Name, time.Since(start), len(out)/4, failed) {
		return false
	}
	b.write(out)
	return true
}

func (b *bridge) fetchSource(ctx context.Context, c toolCall) (map[string]any, bool) {
	ans, err := b.sourceFetcher().Run(ctx, c.Params.Arguments)
	if err != nil {
		return localFail(err)
	}
	raw, err := json.Marshal(ans)
	if err != nil {
		return localFail(err)
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(raw)}},
		"structuredContent": json.RawMessage(raw),
		"_meta":             map[string]any{"codesynapse/source": "local-fetch"},
	}, false
}

// sourceTTLs reads the per-host TTLs from the user's config folder. A file
// that cannot be read or is invalid is reported once and ignored, so the
// built-in documentation list applies; the hosts it names are not logged.
func (b *bridge) sourceTTLs() *fetchsource.TTLs {
	path, err := fetchsource.DefaultTTLFile()
	if err != nil {
		return nil
	}
	ttls, err := fetchsource.LoadTTLs(path)
	if err != nil {
		b.warn("fetch_source TTL file ignored; the built-in TTLs apply", "error", err)
		return nil
	}
	return ttls
}

// sourceFetcher is the bridge's fetcher, made on first use. Without a usable
// cache folder pages are still fetched, just not kept.
func (b *bridge) sourceFetcher() *fetchsource.Fetcher {
	b.fetchOnce.Do(func() {
		if b.fetcher != nil {
			return
		}
		var cache *fetchsource.Cache
		dir, err := fetchsource.DefaultDir()
		if err == nil {
			cache, err = fetchsource.OpenCache(dir, fetchsource.DefaultCacheBytes)
		}
		if err != nil {
			b.warn("fetch_source cache unavailable; pages are not kept", "error", err)
		}
		b.fetcher = fetchsource.New(cache).WithTTLs(b.sourceTTLs())
	})
	return b.fetcher
}
