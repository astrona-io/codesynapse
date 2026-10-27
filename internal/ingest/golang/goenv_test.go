package golang

import (
	"slices"
	"testing"
)

// last is the value go sees: with duplicate keys the last one wins.
func last(env []string, key string) string {
	for _, e := range slices.Backward(env) {
		if len(e) > len(key) && e[:len(key)+1] == key+"=" {
			return e[len(key)+1:]
		}
	}
	return ""
}

func TestGoEnvNetworkModes(t *testing.T) {
	ctx := t.Context()
	off, err := GoEnv(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if last(off, "GOPROXY") != "off" {
		t.Errorf("offline GOPROXY = %q", last(off, "GOPROXY"))
	}
	proxy, err := GoEnv(WithModuleProxy(ctx), false)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"GOPROXY": publicProxy, "GOSUMDB": "sum.golang.org", "GONOSUMDB": "", "GOPRIVATE": "", "GONOPROXY": "",
		"GOVCS": "*:off", "GOFLAGS": "-mod=readonly", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0",
	} {
		if got := last(proxy, k); got != want {
			t.Errorf("proxy %s = %q, want %q", k, got, want)
		}
	}
	// Full network is the operator's choice and stays as it was.
	full, err := GoEnv(WithModuleProxy(ctx), true)
	if err != nil {
		t.Fatal(err)
	}
	if last(full, "GOPROXY") == publicProxy || last(full, "GOVCS") == "*:off" {
		t.Error("allowNetwork must not be narrowed by module proxy mode")
	}
}
