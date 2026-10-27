package llm

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Local servers keep the attention state of a prompt they have read (the
// KV cache) and reuse it when the next prompt starts with the same tokens.
// Our system prompts are constant and the per-request part comes last, so
// this only needs switching on and a bound on how long it is kept.
const (
	KVCacheAuto = "auto"
	KVCacheOn   = "on"
	KVCacheOff  = "off"

	// KVCacheForever keeps the cache until the server evicts it.
	KVCacheForever = "forever"
	// DefaultKVCacheTTL covers a burst of related calls and then frees memory.
	DefaultKVCacheTTL = "15m"
	maxKVCacheTTL     = 24 * time.Hour

	ServerOllama   = "ollama"
	ServerLlamaCPP = "llamacpp"
	ServerVLLM     = "vllm"
	ServerOther    = "other"
)

// parseKVCacheTTL returns the expiry; forever is reported as (0, true).
func parseKVCacheTTL(s string) (d time.Duration, forever bool, err error) {
	switch s = strings.TrimSpace(s); s {
	case KVCacheForever:
		return 0, true, nil
	case "0":
		return 0, false, nil
	}
	d, err = time.ParseDuration(s)
	if err != nil || d < 0 || d > maxKVCacheTTL {
		return 0, false, fmt.Errorf("kv_cache_ttl must be a duration between 0 and %s, or %q", maxKVCacheTTL, KVCacheForever)
	}
	return d, false, nil
}

// kvServer is the server the cache options are written for. Detection by
// port is only a convenience; kv_cache_server names it exactly.
func kvServer(p ProviderConfig) string {
	if p.KVCacheServer != "" {
		return p.KVCacheServer
	}
	if u, err := url.Parse(p.BaseURL); err == nil && u.Port() == "11434" {
		return ServerOllama
	}
	return ServerOther
}

// kvCacheOptions are the extra request fields for a local server. Cloud
// providers and servers without options get none.
func kvCacheOptions(p ProviderConfig) map[string]any {
	if !p.Local() || p.KVCache == KVCacheOff {
		return nil
	}
	d, forever, err := parseKVCacheTTL(p.KVCacheTTL)
	if err != nil {
		return nil
	}
	switch kvServer(p) {
	case ServerOllama:
		keep := "0"
		switch {
		case forever:
			keep = "-1"
		case d > 0:
			keep = d.String()
		}
		return map[string]any{"keep_alive": keep}
	case ServerLlamaCPP:
		return map[string]any{"cache_prompt": d > 0 || forever}
	}
	return nil
}

func validateKVCache(p *ProviderConfig, fallbackTTL string) error {
	switch p.KVCache {
	case "":
		p.KVCache = KVCacheAuto
	case KVCacheAuto, KVCacheOn, KVCacheOff:
	default:
		return fmt.Errorf("kv_cache must be %s, %s or %s", KVCacheAuto, KVCacheOn, KVCacheOff)
	}
	switch p.KVCacheServer {
	case "", ServerOllama, ServerLlamaCPP, ServerVLLM, ServerOther:
	default:
		return fmt.Errorf("kv_cache_server must be %s, %s, %s or %s", ServerOllama, ServerLlamaCPP, ServerVLLM, ServerOther)
	}
	if p.KVCacheTTL == "" {
		p.KVCacheTTL = fallbackTTL
	}
	if p.KVCacheTTL == "" {
		p.KVCacheTTL = DefaultKVCacheTTL
	}
	_, _, err := parseKVCacheTTL(p.KVCacheTTL)
	return err
}
