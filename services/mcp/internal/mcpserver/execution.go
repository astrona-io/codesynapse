package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/parisnakitakejser/codesynapse/internal/execution"
)

const maxRuntimeTransports = 4096

type executionAccess interface {
	ResolveExecutionSession(context.Context, string, string, string) (execution.Identity, error)
}

// Runtime binds MCP transports to pre-registered primary execution identities.
// The same instance must wrap HTTP and the server's tool middleware so resumed
// event streams receive the same identity checks as fresh tool calls.
type Runtime struct {
	access     Access
	stdioToken string
	primary    string
	parent     string
	mu         sync.Mutex
	bindings   map[*mcp.ServerSession]execution.Identity
	protocol   map[string]execution.Identity
}

// NewRuntime accepts runtime-owned metadata. It never creates primary IDs.
func NewRuntime(access Access, stdioToken, primary, parent string) *Runtime {
	return &Runtime{access: access, stdioToken: stdioToken, primary: primary, parent: parent,
		bindings: map[*mcp.ServerSession]execution.Identity{}, protocol: map[string]execution.Identity{}}
}

func runtimeHeaders(headers http.Header) (string, string, error) {
	primary, parent := headers.Values(execution.SessionHeader), headers.Values(execution.ParentSessionHeader)
	if len(primary) != 1 || len(parent) != 1 || primary[0] == "" || parent[0] == "" {
		return "", "", fmt.Errorf("runtime must supply exactly one %s and %s header for the registered chat session", execution.SessionHeader, execution.ParentSessionHeader)
	}
	return primary[0], parent[0], nil
}

func (r *Runtime) resolve(ctx context.Context, caller *Caller, token, primary, parent string) (execution.Identity, error) {
	if r.access == nil {
		return execution.Identity{}, fmt.Errorf("tool execution requires accounts and a registered runtime session; configure a personal API key and register the runtime session with the API")
	}
	if primary == "" || parent == "" || primary != parent {
		return execution.Identity{}, fmt.Errorf("runtime must supply a registered primary session and an identical parent session; do not put session identity in tool arguments")
	}
	if caller == nil {
		var err error
		caller, err = r.access.Authenticate(ctx, token)
		if err != nil {
			return execution.Identity{}, fmt.Errorf("personal API key required for runtime session")
		}
	}
	if caller == nil || caller.UserID == "" || caller.OrgID == "" {
		return execution.Identity{}, fmt.Errorf("authenticated runtime subject required")
	}
	resolver, ok := r.access.(executionAccess)
	if !ok {
		return execution.Identity{}, fmt.Errorf("execution session registry is unavailable")
	}
	id, err := resolver.ResolveExecutionSession(ctx, caller.UserID, caller.OrgID, primary)
	if err != nil {
		return execution.Identity{}, fmt.Errorf("runtime session is unavailable; register it with the API using the same personal key")
	}
	if err := execution.Validate(id); err != nil || id.SessionID != primary || id.ParentSessionID != parent || id.SubjectID != caller.UserID || id.TenantID != caller.OrgID {
		return execution.Identity{}, fmt.Errorf("runtime session does not match the authenticated subject and tenant")
	}
	return id, nil
}

func (r *Runtime) bind(session *mcp.ServerSession, id execution.Identity) error {
	if session == nil {
		return fmt.Errorf("MCP transport session required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if previous, ok := r.bindings[session]; ok {
		if previous != id {
			return fmt.Errorf("primary runtime identity cannot change on an MCP transport; reconnect using the original chat identity")
		}
		return nil
	}
	if len(r.bindings) >= maxRuntimeTransports {
		return fmt.Errorf("runtime transport capacity reached")
	}
	r.bindings[session] = id
	protocolID := session.ID()
	if protocolID != "" {
		r.protocol[protocolID] = id
	}
	go func() {
		_ = session.Wait()
		r.mu.Lock()
		delete(r.bindings, session)
		delete(r.protocol, protocolID)
		r.mu.Unlock()
	}()
	return nil
}

func (r *Runtime) middleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			primary, parent := r.primary, r.parent
			var headerErr error
			if extra := req.GetExtra(); extra != nil && extra.Header != nil {
				primary, parent, headerErr = runtimeHeaders(extra.Header)
			}
			// Initialization may negotiate capabilities without executing tools.
			// Metadata supplied then pins the transport before its first call.
			if method != "tools/call" && (method != "initialize" || primary == "") {
				return next(ctx, method, req)
			}
			id, err := r.resolve(ctx, callerFrom(ctx), r.stdioToken, primary, parent)
			if headerErr != nil {
				err = headerErr
			}
			if err == nil {
				session, _ := req.GetSession().(*mcp.ServerSession)
				err = r.bind(session, id)
			}
			if err == nil {
				ctx, err = execution.With(ctx, id)
			}
			if err != nil {
				if r.access != nil {
					if auditErr := r.access.Denied(ctx, callerFrom(ctx), "runtime_session"); auditErr != nil {
						err = fmt.Errorf("runtime session validation and audit are unavailable")
					}
				}
				if method != "tools/call" {
					return nil, err
				}
				result := &mcp.CallToolResult{}
				result.SetError(err)
				return result, nil
			}
			return next(ctx, method, req)
		}
	}
}

// HTTP protects transport resumption as well as tool execution. It must run
// inside bearer authentication; no client header supplies tenant or subject.
func (r *Runtime) HTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		primary, parent, err := runtimeHeaders(req.Header)
		protocolID := req.Header.Get("Mcp-Session-Id")
		if err != nil && protocolID == "" && req.Header.Get(execution.SessionHeader) == "" && req.Method == http.MethodPost {
			// The tool middleware still refuses a tool call without metadata.
			next.ServeHTTP(w, req)
			return
		}
		var token string
		if info := auth.TokenInfoFromContext(req.Context()); info != nil {
			token, _ = info.Extra[ExtraToken].(string)
		}
		var id execution.Identity
		if err == nil {
			id, err = r.resolve(req.Context(), nil, token, primary, parent)
		}
		if err == nil && protocolID != "" {
			r.mu.Lock()
			previous, ok := r.protocol[protocolID]
			r.mu.Unlock()
			if ok && previous != id || !ok && req.Method != http.MethodPost {
				err = fmt.Errorf("MCP transport does not match the runtime session")
			}
		}
		if err != nil {
			if r.access != nil {
				var caller *Caller
				if info := auth.TokenInfoFromContext(req.Context()); info != nil {
					org, _ := info.Extra[ExtraOrgID].(string)
					key, _ := info.Extra[ExtraKeyID].(string)
					caller = &Caller{UserID: info.UserID, OrgID: org, KeyID: key}
				}
				if auditErr := r.access.Denied(req.Context(), caller, "runtime_session"); auditErr != nil {
					err = fmt.Errorf("runtime session validation and audit are unavailable")
				}
			}
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		ctx, err := execution.With(req.Context(), id)
		if err != nil {
			http.Error(w, "runtime session context conflict", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, req.WithContext(ctx))
	})
}
