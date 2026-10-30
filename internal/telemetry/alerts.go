package telemetry

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Alerts reads what the internal vmalert evaluates
// (docs/plans/PLATFORM_STATS_PLAN.md, phase 6) for the Statistics page. Rule
// labels and annotations can carry anything an operator wrote, so only the
// rule name, state, the severity label, the value and when it became active
// are passed on.

// alertsMaxBody caps one vmalert answer.
const alertsMaxBody = 1 << 20

// MaxAlerts bounds the alerts and rules passed on.
const MaxAlerts = 200

// ErrAlertsURL rejects an alerts URL that is not https or an internal
// service name.
var ErrAlertsURL = errors.New("alerts url must be https://host[:port], or http:// to an internal service name (one label or *.svc[.cluster.local]), without credentials, query or fragment")

// CheckAlertsURL allows https anywhere, and plain http only to a service
// name: one DNS label (docker compose) or a cluster-internal *.svc name.
// IP literals over http are refused.
func CheckAlertsURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || u.Opaque != "" {
		return ErrAlertsURL
	}
	if p := strings.Trim(u.Path, "/"); p != "" {
		return ErrAlertsURL
	}
	host := u.Hostname()
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if net.ParseIP(host) != nil {
			return ErrAlertsURL
		}
		name := strings.TrimSuffix(strings.TrimSuffix(host, ".cluster.local"), ".svc")
		labels := strings.Split(name, ".")
		if name != host && len(labels) > 2 || name == host && len(labels) != 1 {
			return ErrAlertsURL
		}
		for _, l := range labels {
			if !dnsLabel(l) {
				return ErrAlertsURL
			}
		}
		return nil
	}
	return ErrAlertsURL
}

func dnsLabel(s string) bool {
	if s == "" || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// AlertsClient reads vmalert's API.
type AlertsClient struct {
	URL  string
	HTTP *http.Client
}

// NewAlertsClient answers nil for an empty URL and an error for a bad one.
func NewAlertsClient(raw string) (*AlertsClient, error) {
	if raw == "" {
		return nil, nil
	}
	if err := CheckAlertsURL(raw); err != nil {
		return nil, err
	}
	return &AlertsClient{URL: strings.TrimRight(raw, "/")}, nil
}

// Alert is one firing or pending alert, reduced to what is passed on.
type Alert struct {
	Name     string    `json:"name"`
	State    string    `json:"state"`
	Severity string    `json:"severity"`
	Value    string    `json:"value"`
	ActiveAt time.Time `json:"active_at"`
}

// AlertRule is one rule and how its last evaluation went. The error text
// is not passed on, only that there was one.
type AlertRule struct {
	Name           string    `json:"name"`
	Group          string    `json:"group"`
	State          string    `json:"state"`
	Severity       string    `json:"severity"`
	Health         string    `json:"health"`
	Failing        bool      `json:"failing"`
	LastEvaluation time.Time `json:"last_evaluation"`
}

// alertStates, ruleHealth and severities are the values passed on; others
// become "other".
var (
	alertStates = []string{"firing", "pending", "inactive"}
	ruleHealth  = []string{"ok", "err", "unknown"}
	severities  = []string{"critical", "warning", "info"}
)

func allowed(v string, list []string) string {
	if slices.Contains(list, v) {
		return v
	}
	if v == "" {
		return ""
	}
	return "other"
}

// clean keeps rule and group names short and printable.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

func (c *AlertsClient) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (c *AlertsClient) get(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+path, nil)
	if err != nil {
		return err
	}
	hc := *c.client()
	// Whatever client is used, a redirect is never followed.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("alerts: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck // read-only body
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16)) //nolint:errcheck // drained for keep-alive
		return fmt.Errorf("alerts: HTTP %d", res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, alertsMaxBody+1))
	if err != nil {
		return fmt.Errorf("alerts: %w", err)
	}
	if len(raw) > alertsMaxBody {
		return errors.New("alerts: answer too large")
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return errors.New("alerts: unreadable answer")
	}
	return nil
}

// Alerts lists the firing and pending alerts, firing first.
func (c *AlertsClient) Alerts(ctx context.Context) ([]Alert, error) {
	var env struct {
		Data struct {
			Alerts []struct {
				Name     string            `json:"name"`
				State    string            `json:"state"`
				Value    string            `json:"value"`
				ActiveAt time.Time         `json:"activeAt"`
				Labels   map[string]string `json:"labels"`
			} `json:"alerts"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/api/v1/alerts", &env); err != nil {
		return nil, err
	}
	out := make([]Alert, 0, min(len(env.Data.Alerts), MaxAlerts))
	for _, a := range env.Data.Alerts {
		if len(out) == MaxAlerts {
			break
		}
		out = append(out, Alert{
			Name: clean(a.Name), State: allowed(a.State, alertStates), Severity: allowed(a.Labels["severity"], severities),
			Value: clean(a.Value), ActiveAt: a.ActiveAt.UTC(),
		})
	}
	slices.SortStableFunc(out, func(a, b Alert) int {
		return cmp.Or(cmp.Compare(stateRank(a.State), stateRank(b.State)), cmp.Compare(a.Name, b.Name))
	})
	return out, nil
}

func stateRank(s string) int {
	if i := slices.Index(alertStates, s); i >= 0 {
		return i
	}
	return len(alertStates)
}

// Rules lists every alerting rule with its group and last evaluation.
func (c *AlertsClient) Rules(ctx context.Context) ([]AlertRule, error) {
	var env struct {
		Data struct {
			Groups []struct {
				Name  string `json:"name"`
				Rules []struct {
					Name           string            `json:"name"`
					Type           string            `json:"type"`
					State          string            `json:"state"`
					Health         string            `json:"health"`
					LastError      string            `json:"lastError"`
					LastEvaluation time.Time         `json:"lastEvaluation"`
					Labels         map[string]string `json:"labels"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/api/v1/rules?type=alert", &env); err != nil {
		return nil, err
	}
	var out []AlertRule
	for _, g := range env.Data.Groups {
		for _, r := range g.Rules {
			if r.Type != "" && r.Type != "alerting" {
				continue
			}
			if len(out) == MaxAlerts {
				return out, nil
			}
			out = append(out, AlertRule{
				Name: clean(r.Name), Group: clean(g.Name), State: allowed(r.State, alertStates),
				Severity: allowed(r.Labels["severity"], severities), Health: allowed(r.Health, ruleHealth),
				Failing: r.LastError != "", LastEvaluation: r.LastEvaluation.UTC(),
			})
		}
	}
	return out, nil
}
