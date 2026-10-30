package telemetry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestCheckAlertsURL(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"http://vmalert:8880", true},
		{"http://codesynapse-vmalert:8880/", true},
		{"http://codesynapse-vmalert.observability.svc:8880", true},
		{"http://codesynapse-vmalert.observability.svc.cluster.local:8880", true},
		{"https://alerts.example.com", true},
		{"https://10.0.0.5:8880", true},
		{"http://alerts.example.com", false},
		{"http://10.0.0.5:8880", false},
		{"http://[::1]:8880", false},
		{"http://a.b.c.svc", false},
		{"http://user:pw@vmalert:8880", false},
		{"http://vmalert:8880/api", false},
		{"http://vmalert:8880?x=1", false},
		{"http://vmalert:8880#f", false},
		{"http://VMALERT:8880", false},
		{"ftp://vmalert", false},
		{"vmalert:8880", false},
		{"", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			assert.Equal(t, CheckAlertsURL(tc.url) == nil, tc.ok)
		})
	}
	c, err := NewAlertsClient("")
	assert.NoError(t, err)
	assert.True(t, c == nil)
	_, err = NewAlertsClient("http://example.com")
	assert.Error(t, err)
}

// TestAlertsDropOtherLabels: labels and annotations other than severity
// never leave the client, and unknown states or severities become "other".
func TestAlertsDropOtherLabels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/alerts":
			_, _ = w.Write([]byte(`{"status":"success","data":{"alerts":[
				{"name":"IngestStalled","state":"pending","value":"1","activeAt":"2026-10-29T10:00:00Z",
				 "labels":{"alertname":"IngestStalled","severity":"warning","repo":"acme/secret","tenant_id":"0123456789abcdef01234567"},
				 "annotations":{"summary":"acme/secret is stuck","runbook":"https://wiki.internal/x"}},
				{"name":"MCPDown","state":"firing","value":"0","activeAt":"2026-10-29T09:00:00Z",
				 "labels":{"severity":"page-me-now","instance":"10.1.2.3:8090"}}]}}`))
		case "/api/v1/rules":
			assert.Equal(t, r.URL.Query().Get("type"), "alert")
			_, _ = w.Write([]byte(`{"status":"success","data":{"groups":[{"name":"codesynapse","rules":[
				{"name":"MCPDown","type":"alerting","state":"firing","health":"err","lastError":"query failed: secret-host:9000","lastEvaluation":"2026-10-29T10:01:00Z","labels":{"severity":"critical","team":"x"}},
				{"name":"recorded","type":"recording","health":"ok"}]}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &AlertsClient{URL: srv.URL}

	alerts, err := c.Alerts(t.Context())
	require.NoError(t, err)
	require.Equal(t, len(alerts), 2)
	assert.Equal(t, alerts[0].Name, "MCPDown", "firing first")
	assert.Equal(t, alerts[0].Severity, "other")
	assert.Equal(t, alerts[1].Severity, "warning")
	raw, err := json.Marshal(alerts)
	require.NoError(t, err)
	for _, leak := range []string{"acme", "tenant", "wiki", "10.1.2.3", "instance", "summary"} {
		assert.False(t, strings.Contains(string(raw), leak), "%s passed on: %s", leak, raw)
	}

	rules, err := c.Rules(t.Context())
	require.NoError(t, err)
	require.Equal(t, len(rules), 1, "recording rules are not alerts")
	assert.Equal(t, rules[0].Group, "codesynapse")
	assert.Equal(t, rules[0].Severity, "critical")
	assert.True(t, rules[0].Failing)
	raw, err = json.Marshal(rules)
	require.NoError(t, err)
	for _, leak := range []string{"secret-host", "team"} {
		assert.False(t, strings.Contains(string(raw), leak), "%s passed on: %s", leak, raw)
	}
}

func TestAlertsRefuseRedirectsAndLargeAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/alerts" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		if r.URL.Path == "/elsewhere" {
			t.Error("redirect followed")
		}
		_, _ = w.Write([]byte(`{"data":{"groups":[{"name":"`))
		_, _ = w.Write([]byte(strings.Repeat("x", alertsMaxBody)))
		_, _ = w.Write([]byte(`"}]}}`))
	}))
	defer srv.Close()
	c := &AlertsClient{URL: srv.URL, HTTP: srv.Client()}
	_, err := c.Alerts(t.Context())
	assert.Error(t, err)
	_, err = c.Rules(t.Context())
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "too large"))
}
