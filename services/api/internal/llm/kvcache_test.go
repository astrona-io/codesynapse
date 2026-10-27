package llm

import (
	"reflect"
	"testing"
)

func TestParseKVCacheTTL(t *testing.T) {
	for _, tc := range []struct {
		in      string
		forever bool
		ok      bool
	}{
		{"15m", false, true}, {"0", false, true}, {"forever", true, true}, {"24h", false, true},
		{"25h", false, false}, {"-1m", false, false}, {"soon", false, false},
	} {
		_, forever, err := parseKVCacheTTL(tc.in)
		if (err == nil) != tc.ok || forever != tc.forever {
			t.Errorf("%q: forever=%v err=%v", tc.in, forever, err)
		}
	}
}

func TestKVCacheOptions(t *testing.T) {
	local := func(url, server, ttl, mode string) ProviderConfig {
		return ProviderConfig{BaseURL: url, KVCacheServer: server, KVCacheTTL: ttl, KVCache: mode}
	}
	for _, tc := range []struct {
		name string
		cfg  ProviderConfig
		want map[string]any
	}{
		{"ollama by port", local("http://localhost:11434/v1", "", "15m", ""), map[string]any{"keep_alive": "15m0s"}},
		{"ollama forever", local("http://localhost:11434/v1", "", "forever", ""), map[string]any{"keep_alive": "-1"}},
		{"ollama zero", local("http://localhost:11434/v1", "", "0", ""), map[string]any{"keep_alive": "0"}},
		{"llamacpp", local("http://localhost:8080/v1", "llamacpp", "15m", ""), map[string]any{"cache_prompt": true}},
		{"llamacpp zero", local("http://localhost:8080/v1", "llamacpp", "0", ""), map[string]any{"cache_prompt": false}},
		{"vllm", local("http://localhost:8000/v1", "vllm", "15m", ""), nil},
		{"off", local("http://localhost:11434/v1", "", "15m", KVCacheOff), nil},
		{"remote", local("https://api.example.com/v1", "ollama", "15m", ""), nil},
	} {
		if got := kvCacheOptions(tc.cfg); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestKVCacheDefaults(t *testing.T) {
	c := Config{KVCacheTTL: "1h", Providers: []ProviderConfig{
		{Name: "a", BaseURL: "http://localhost:11434/v1"},
		{Name: "b", BaseURL: "http://localhost:11434/v1", KVCacheTTL: "5m"},
	}}
	if err := c.normalise(func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	if c.Providers[0].KVCacheTTL != "1h" || c.Providers[1].KVCacheTTL != "5m" || c.Providers[0].KVCache != KVCacheAuto {
		t.Errorf("%+v", c.Providers)
	}
	d := Config{Providers: []ProviderConfig{{Name: "a", BaseURL: "http://localhost:1/v1"}}}
	if err := d.normalise(func(string) string { return "" }); err != nil || d.Providers[0].KVCacheTTL != DefaultKVCacheTTL {
		t.Errorf("default: %v %+v", err, d.Providers[0])
	}
	bad := Config{Providers: []ProviderConfig{{Name: "a", BaseURL: "http://localhost:1/v1", KVCacheTTL: "99h"}}}
	if err := bad.normalise(func(string) string { return "" }); err == nil {
		t.Error("99h accepted")
	}
}
