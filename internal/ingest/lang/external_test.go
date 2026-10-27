package lang

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestExternalCalls(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, rel), []byte(src), 0o644))
	}
	write("app/main.py", `import yaml
import requests as rq
from jinja2 import Template
from os import path

def load(text):
    yaml.safe_load(text)
    Template(text).render()
    rq.get("x")
    path.join("a", "b")
    local()

def local():
    pass
`)
	write("web/app.ts", `import _ from "lodash";
import { merge as mix } from "lodash";
import template from "lodash/template";
import * as ax from "axios";
import { Client } from "@scope/sdk/client";

export function run(x: string) {
  _.template(x);
  mix({}, {});
  template(x);
  ax.get("u");
  Client.create();
  helper();
}

function helper() {}
`)
	write("web/app.test.ts", `import { merge } from "lodash";
test("x", () => { merge({}, {}); });
`)

	res, err := Extract(context.Background(), "r", dir)
	require.NoError(t, err)

	fn := func(rel, name string) string { return model.NodeID("r", model.KindFunction, rel+"::"+name) }
	cases := []struct{ key, caller string }{
		{"yaml.safe_load", fn("app/main.py", "load")},
		{"jinja2.Template", fn("app/main.py", "load")},
		{"requests.get", fn("app/main.py", "load")}, // an alias still names the package
		{"lodash.template", fn("web/app.ts", "run")},
		{"lodash.merge", fn("web/app.ts", "run")},
		{"lodash.template", fn("web/app.ts", "run")},
		{"axios.get", fn("web/app.ts", "run")},
		{"@scope/sdk.client.Client.create", fn("web/app.ts", "run")},
	}
	for _, c := range cases {
		assert.Contains(t, res.ExternalCalls[c.key], c.caller, c.key)
	}
	_, got := res.ExternalCalls["local"]
	assert.False(t, got, "a function of the file is not a package member")
	for key, callers := range res.ExternalCalls {
		for _, id := range callers {
			assert.False(t, id == model.NodeID("r", model.KindFunction, "web/app.test.ts::test"), "test code is not a use: "+key)
		}
	}
}
