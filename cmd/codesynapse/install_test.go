package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// fakeTools records helm and kubectl calls and answers from a script.
type fakeTools struct {
	calls  []string
	stdins [][]byte
	answer func(call string) (string, error)
}

func (f *fakeTools) run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	f.stdins = append(f.stdins, stdin)
	if strings.Contains(call, "--values /") {
		// The local preset is a temporary file: it must exist while helm runs.
		for i, a := range args {
			if a == "--values" && strings.Contains(args[i+1], "codesynapse-local") {
				raw, err := os.ReadFile(args[i+1])
				if err != nil || !bytes.Equal(raw, localValues) {
					return nil, errors.New("local values missing")
				}
			}
		}
	}
	out, err := f.answer(call)
	return []byte(out), err
}

func installEnv(stdin string) (*app.Env, *bytes.Buffer) {
	var out bytes.Buffer
	return &app.Env{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &out, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, &out
}

func TestInstall(t *testing.T) {
	missing := errors.New("NotFound")
	f := &fakeTools{answer: func(call string) (string, error) {
		switch {
		case strings.HasPrefix(call, "kubectl config current-context"):
			return "kind-dev\n", nil
		case strings.Contains(call, "get namespace"), strings.Contains(call, "get secret"):
			return "", missing
		}
		return "", nil
	}}
	env, out := installEnv("y\n")
	env.IsTerminal = true
	o := &installOptions{EC: env, namespace: "codesynapse", release: "codesynapse", chart: defaultChart, version: "1.2.0", local: true, timeout: time.Minute, run: f.run}
	require.NoError(t, o.Validate(context.Background(), env))
	require.NoError(t, o.Run(context.Background(), env))

	joined := strings.Join(f.calls, "\n")
	assert.Contains(t, joined, "kubectl --context kind-dev create namespace codesynapse")
	assert.Contains(t, joined, "kubectl --context kind-dev --namespace codesynapse create -f -")
	assert.Contains(t, joined, "helm upgrade --install codesynapse oci://ghcr.io/astrona-io/charts/codesynapse --wait --timeout 1m0s --kube-context kind-dev --namespace codesynapse --set global.existingSecret=codesynapse --version 1.2.0 --values ")
	assert.Contains(t, joined, "helm test codesynapse --kube-context kind-dev --namespace codesynapse")
	var secret []byte
	for i, call := range f.calls {
		if strings.Contains(call, "create -f -") {
			secret = f.stdins[i]
		}
	}
	require.True(t, len(secret) > 0)
	var made struct {
		StringData map[string]string `json:"stringData"`
	}
	require.NoError(t, json.Unmarshal(secret, &made))
	for _, k := range secretKeys {
		v := made.StringData[k.name]
		assert.True(t, v != "", "%s is generated", k.name)
		if strings.HasPrefix(k.name, "NEBULA_") {
			assert.Equal(t, len(v), 24, "%s fits NebulaGraph's 24-character limit", k.name)
		}
	}
	assert.False(t, strings.Contains(joined, "POSTGRES_PASSWORD"), "credentials travel on stdin, never in arguments")
	assert.Contains(t, out.String(), "codesynapse status --context kind-dev")

	// A second install keeps the Secret.
	f2 := &fakeTools{answer: func(string) (string, error) { return "", nil }}
	env2, _ := installEnv("")
	o2 := &installOptions{EC: env2, context: "kind-dev", namespace: "codesynapse", release: "codesynapse", chart: defaultChart, version: "1.2.0", yes: true, timeout: time.Minute, run: f2.run}
	require.NoError(t, o2.Run(context.Background(), env2))
	assert.False(t, strings.Contains(strings.Join(f2.calls, "\n"), "create -f -"), "an existing Secret is never replaced")

	// A Secret from an older CLI gets only the keys it lacks, on stdin.
	f3 := &fakeTools{answer: func(call string) (string, error) {
		if strings.Contains(call, "get secret codesynapse") {
			return "CODESYNAPSE_API_TOKEN\nCODESYNAPSE_MCP_TOKEN\nPOSTGRES_PASSWORD\nNEBULA_PASSWORD\nMETRICS_PASSWORD\nSECRET_KEY\n", nil
		}
		return "", nil
	}}
	env3, _ := installEnv("")
	o3 := &installOptions{EC: env3, context: "kind-dev", namespace: "codesynapse", release: "codesynapse", chart: defaultChart, version: "1.2.0", yes: true, timeout: time.Minute, run: f3.run}
	require.NoError(t, o3.Run(context.Background(), env3))
	var patch []byte
	for i, call := range f3.calls {
		if strings.Contains(call, "patch secret codesynapse --type merge --patch-file /dev/stdin") {
			patch = f3.stdins[i]
		}
	}
	var added struct {
		StringData map[string]string `json:"stringData"`
	}
	require.NoError(t, json.Unmarshal(patch, &added))
	assert.Len(t, added.StringData, 3, "only the missing keys")
	for _, k := range []string{"NEBULA_API_PASSWORD", "NEBULA_WORKER_PASSWORD", "NEBULA_MCP_PASSWORD"} {
		assert.Equal(t, len(added.StringData[k]), 24, k)
	}
	assert.False(t, strings.Contains(strings.Join(f3.calls, "\n"), "create -f -"))

	// A complete Secret is left alone.
	f4 := &fakeTools{answer: func(call string) (string, error) {
		if strings.Contains(call, "get secret codesynapse") {
			names := make([]string, 0, len(secretKeys))
			for _, k := range secretKeys {
				names = append(names, k.name)
			}
			return strings.Join(names, "\n"), nil
		}
		return "", nil
	}}
	env4, _ := installEnv("")
	o4 := &installOptions{EC: env4, context: "kind-dev", namespace: "codesynapse", release: "codesynapse", chart: defaultChart, version: "1.2.0", yes: true, timeout: time.Minute, run: f4.run}
	require.NoError(t, o4.Run(context.Background(), env4))
	assert.False(t, strings.Contains(strings.Join(f4.calls, "\n"), "patch secret"), "nothing to add")
}

func TestInstallRefusals(t *testing.T) {
	env, _ := installEnv("n\n")
	o := &installOptions{EC: env, namespace: "codesynapse", release: "codesynapse", chart: defaultChart, timeout: time.Minute}
	assert.Error(t, o.Validate(context.Background(), env), "not a terminal and no --yes")
	o.namespace = "Bad_NS"
	o.yes = true
	assert.Error(t, o.Validate(context.Background(), env), "namespace must be a DNS label")

	f := &fakeTools{answer: func(string) (string, error) { return "", nil }}
	dev := &installOptions{EC: env, context: "kind-dev", namespace: "codesynapse", release: "codesynapse", chart: defaultChart, yes: true, run: f.run}
	err := dev.Run(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "development build", "a dev CLI does not know which chart to install")

	env.IsTerminal = true
	no := &installOptions{EC: env, context: "kind-dev", namespace: "codesynapse", release: "codesynapse", chart: defaultChart, version: "1.2.0", run: f.run}
	assert.Error(t, no.Run(context.Background(), env), "answering no cancels")
	assert.Len(t, f.calls, 0, "nothing ran")
}

func TestUninstall(t *testing.T) {
	for _, keep := range []bool{true, false} {
		f := &fakeTools{answer: func(string) (string, error) { return "", nil }}
		env, _ := installEnv("")
		o := &installOptions{EC: env, uninstall: true, context: "kind-dev", namespace: "codesynapse", release: "codesynapse", yes: true, keepData: keep, timeout: time.Minute, run: f.run}
		require.NoError(t, o.Run(context.Background(), env))
		joined := strings.Join(f.calls, "\n")
		assert.Contains(t, joined, "helm uninstall codesynapse --kube-context kind-dev --namespace codesynapse")
		assert.Equal(t, strings.Contains(joined, "delete namespace"), !keep, "data goes only when asked")
	}
}

func TestStatus(t *testing.T) {
	components := func(webReady int) string {
		return `{"items":[
 {"kind":"Deployment","metadata":{"name":"codesynapse-api"},"spec":{"replicas":2},"status":{"readyReplicas":2}},
 {"kind":"Deployment","metadata":{"name":"codesynapse-web"},"spec":{"replicas":1},"status":{"readyReplicas":` + string(rune('0'+webReady)) + `}},
 {"kind":"StatefulSet","metadata":{"name":"codesynapse-nebula-graphd"},"spec":{"replicas":1},"status":{"readyReplicas":1}}]}`
	}
	for _, tc := range []struct {
		name     string
		webReady int
		ok       bool
	}{{"all ready", 1, true}, {"web down", 0, false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeTools{answer: func(call string) (string, error) {
				if strings.HasPrefix(call, "helm list") {
					return `[{"name":"codesynapse","revision":"3","status":"deployed","chart":"codesynapse-1.2.0","app_version":"1.2.0"}]`, nil
				}
				return components(tc.webReady), nil
			}}
			env, out := installEnv("")
			o := &installOptions{EC: env, status: true, context: "kind-dev", namespace: "codesynapse", release: "codesynapse", timeout: time.Minute, run: f.run}
			err := o.Run(context.Background(), env)
			assert.Equal(t, err == nil, tc.ok, out.String())
			assert.Contains(t, out.String(), "CodeSynapse 1.2.0 (codesynapse-1.2.0), release \"codesynapse\" revision 3: deployed")
			assert.Contains(t, out.String(), "codesynapse-nebula-graphd")
			if !tc.ok {
				assert.Contains(t, out.String(), "✗ codesynapse-web")
			}
		})
	}
}
