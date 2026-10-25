package hugegraphstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Limited users (docs/PLAN.md, roadmap 9). HugeGraph checks the labels a
// Gremlin script touches against the user's targets, so a user that may read
// everything and write only some labels can be made with its REST auth API
// (users, groups, targets, accesses; standalone 1.7 layout). The admin runs
// ProvisionRole once, before the services start; the services then connect as
// the limited user and never create schema.

// Role is a limited user: it reads the whole graph and writes, updates and
// deletes only the vertex and edge labels it lists.
type Role struct {
	User, Password                     string
	WriteVertexLabels, WriteEdgeLabels []string
}

var (
	roleName  = regexp.MustCompile(`^[a-z][a-z0-9_]{2,31}$`)
	labelName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,47}$`)
)

const minRolePassword = 16

// MCPRole is what the MCP service needs beyond reading: agent memory, reviews,
// curated links and usage counters (the stores MCP tools write).
func MCPRole(user, password string) Role {
	return Role{User: user, Password: password,
		WriteVertexLabels: []string{"mem_entity", "review", "curated", "usage"},
		WriteEdgeLabels:   []string{"fact"}}
}

type resource struct {
	Type  string  `json:"type"`
	Label *string `json:"label,omitempty"`
}

// ProvisionRole creates or updates the role's user, group, targets and
// accesses. It is idempotent: a second run with other labels replaces what the
// first granted. s must be connected as an administrator.
func (s *Store) ProvisionRole(ctx context.Context, r Role) error {
	if !roleName.MatchString(r.User) || r.User == "admin" {
		return fmt.Errorf("hugegraph role: invalid user name %q", r.User)
	}
	if len(r.Password) < minRolePassword {
		return fmt.Errorf("hugegraph role: the password of %s needs at least %d characters", r.User, minRolePassword)
	}
	for _, l := range append(append([]string{}, r.WriteVertexLabels...), r.WriteEdgeLabels...) {
		if !labelName.MatchString(l) {
			return fmt.Errorf("hugegraph role: invalid label %q", l)
		}
	}
	space := "/graphspaces/" + defaultGraphSpace + "/auth"
	userID, err := s.upsertUser(ctx, space, r)
	if err != nil {
		return err
	}
	groupID, err := s.upsertGroup(ctx, "cs-"+r.User)
	if err != nil {
		return err
	}
	if err := s.revokeAll(ctx, space, groupID, "cs-"+r.User+"-"); err != nil {
		return err
	}
	var belong struct{ ID string }
	err = s.rest(ctx, http.MethodPost, space+"/belongs", map[string]any{"user": userID, "group": groupID}, &belong)
	if err != nil && !strings.Contains(err.Error(), "exist") {
		return fmt.Errorf("hugegraph role: joining %s to its group: %w", r.User, err)
	}
	all := func(types ...string) []resource {
		out := make([]resource, len(types))
		for i, t := range types {
			out[i] = resource{Type: t}
		}
		return out
	}
	labelled := func(t string, labels []string) []resource {
		out := make([]resource, len(labels))
		for i := range labels {
			out[i] = resource{Type: t, Label: &labels[i]}
		}
		return out
	}
	grants := []struct {
		name  string
		res   []resource
		perms []string
	}{
		{"read", all("VERTEX", "EDGE", "VERTEX_AGGR", "EDGE_AGGR", "VERTEX_LABEL", "EDGE_LABEL", "PROPERTY_KEY", "INDEX_LABEL", "STATUS"), []string{"READ"}},
		{"gremlin", all("GREMLIN"), []string{"EXECUTE"}},
	}
	if own := append(labelled("VERTEX", r.WriteVertexLabels), labelled("EDGE", r.WriteEdgeLabels)...); len(own) > 0 {
		grants = append(grants, struct {
			name  string
			res   []resource
			perms []string
		}{"write", own, []string{"WRITE", "DELETE"}})
	}
	for _, g := range grants {
		var t struct{ ID string }
		name := "cs-" + r.User + "-" + g.name
		if err := s.rest(ctx, http.MethodPost, space+"/targets", map[string]any{
			"target_name": name, "target_graph": s.graph, "target_url": "hugegraph", "target_resources": g.res}, &t); err != nil {
			return fmt.Errorf("hugegraph role: target %s: %w", name, err)
		}
		for _, p := range g.perms {
			if err := s.rest(ctx, http.MethodPost, space+"/accesses", map[string]any{"group": groupID, "target": t.ID, "access_permission": p}, nil); err != nil {
				return fmt.Errorf("hugegraph role: granting %s on %s: %w", p, name, err)
			}
		}
	}
	return s.verifyRole(ctx, r)
}

// verifyRole reads as the new user, so a missing grant fails the provisioning
// instead of the first request of a service. The count comes first on purpose:
// a limited user's first label read otherwise fails with "Not allowed to access
// thread group via Gremlin" (the server builds a rate limiter, which starts a
// thread, in the script's security context); after the count it does not.
// A refusal is retried for a few seconds.
func (s *Store) verifyRole(ctx context.Context, r Role) error {
	s.http.CloseIdleConnections()
	var err error
	for range 10 {
		var as *Store
		if as, err = New(ctx, Config{URL: s.base, Graph: s.graph, User: r.User, Password: r.Password, QueryTimeout: s.timeout}); err == nil {
			var n []int
			if err = as.read(ctx, `g.V().count()`, nil, &n); err == nil {
				var maps []map[string]any
				err = as.read(ctx, props+`g.V().hasLabel('node').limit(1).toList().collect { P(it) }`, nil, &maps)
			}
			_ = as.Close(ctx)
		}
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if err != nil {
		return fmt.Errorf("hugegraph role: %s cannot read the graph: %w", r.User, err)
	}
	return nil
}

// upsertUser creates the user or sets the given password, and returns its ID.
// Users are found by listing: the server keys them by an ID that is not the name.
func (s *Store) upsertUser(ctx context.Context, space string, r Role) (string, error) {
	var list struct {
		Users []struct {
			ID   string `json:"id"`
			Name string `json:"user_name"`
		} `json:"users"`
	}
	if err := s.rest(ctx, http.MethodGet, space+"/users", nil, &list); err != nil {
		return "", fmt.Errorf("hugegraph role: listing users: %w", err)
	}
	for _, u := range list.Users {
		if u.Name == r.User {
			if err := s.rest(ctx, http.MethodPut, space+"/users/"+url.PathEscape(u.ID), map[string]any{"user_name": r.User, "user_password": r.Password}, nil); err != nil {
				return "", fmt.Errorf("hugegraph role: updating %s: %w", r.User, err)
			}
			return u.ID, nil
		}
	}
	var u struct {
		ID string `json:"id"`
	}
	if err := s.rest(ctx, http.MethodPost, space+"/users", map[string]any{"user_name": r.User, "user_password": r.Password}, &u); err != nil {
		return "", fmt.Errorf("hugegraph role: creating %s: %w", r.User, err)
	}
	return u.ID, nil
}

// upsertGroup returns the ID of the group, creating it. Groups are at the
// root of the auth API in 1.7, not under the graph space.
func (s *Store) upsertGroup(ctx context.Context, name string) (string, error) {
	var list struct {
		Groups []struct {
			ID   string `json:"id"`
			Name string `json:"group_name"`
		} `json:"groups"`
	}
	if err := s.rest(ctx, http.MethodGet, "/auth/groups", nil, &list); err != nil {
		return "", fmt.Errorf("hugegraph role: listing groups: %w", err)
	}
	for _, g := range list.Groups {
		if g.Name == name && g.ID != "" {
			return g.ID, nil
		}
	}
	var g struct{ ID string }
	if err := s.rest(ctx, http.MethodPost, "/auth/groups", map[string]any{"group_name": name}, &g); err != nil {
		return "", fmt.Errorf("hugegraph role: creating group %s: %w", name, err)
	}
	return g.ID, nil
}

// revokeAll removes the group's accesses and the targets named for it, so a
// run replaces what the last one granted.
func (s *Store) revokeAll(ctx context.Context, space, groupID, targetPrefix string) error {
	var acc struct {
		Accesses []struct {
			ID    string `json:"id"`
			Group string `json:"group"`
		} `json:"accesses"`
	}
	if err := s.rest(ctx, http.MethodGet, space+"/accesses", nil, &acc); err != nil {
		return fmt.Errorf("hugegraph role: listing accesses: %w", err)
	}
	for _, a := range acc.Accesses {
		if a.Group == groupID {
			if err := s.rest(ctx, http.MethodDelete, space+"/accesses/"+url.PathEscape(a.ID), nil, nil); err != nil {
				return fmt.Errorf("hugegraph role: revoking: %w", err)
			}
		}
	}
	var tg struct {
		Targets []struct {
			ID   string `json:"id"`
			Name string `json:"target_name"`
		} `json:"targets"`
	}
	if err := s.rest(ctx, http.MethodGet, space+"/targets", nil, &tg); err != nil {
		return fmt.Errorf("hugegraph role: listing targets: %w", err)
	}
	for _, t := range tg.Targets {
		if strings.HasPrefix(t.Name, targetPrefix) {
			if err := s.rest(ctx, http.MethodDelete, space+"/targets/"+url.PathEscape(t.ID), nil, nil); err != nil {
				return fmt.Errorf("hugegraph role: removing target: %w", err)
			}
		}
	}
	return nil
}

var errRestNotFound = errors.New("not found")

// rest calls the server's REST API as the store's user. Answers are bounded;
// an error carries the server's first message line, never the request body.
func (s *Store) rest(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.user != "" {
		req.SetBasicAuth(s.user, s.password)
	}
	res, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close() //nolint:errcheck // bounded read
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode == http.StatusNotFound {
		return errRestNotFound
	}
	if res.StatusCode >= 300 {
		var e struct{ Message string }
		_ = json.Unmarshal(raw, &e) //nolint:errcheck // the status is the error
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, firstLine(e.Message))
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}
