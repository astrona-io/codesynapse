// Package nebulastore implements store.Store on NebulaGraph 3.x through its
// Graph service (the official nebula-go driver).
//
// Query safety, the nGQL version of the Gremlin rule: every statement is
// built from constants in this package, and every value travels as a
// statement parameter ($name), never in the statement text. Labels (tags,
// edge types) are model allowlist values checked with Valid before they are
// written into a statement.
//
// The one exception is the vertex ID. nGQL does not accept a parameter in a
// vertex ID position (INSERT, GO FROM, FETCH, DELETE), so IDs are written
// into the text; they are always the output of vid, a fixed prefix and
// lower-case hex, so nothing a caller or a repository controls can reach the
// statement. literal refuses anything else.
//
// NebulaGraph specifics the code relies on:
//   - There are no multi-statement transactions. A statement is the unit of
//     atomicity, so a repository replacement upserts the new graph in place,
//     stamps it with a run, and deletes whatever carries another run after the
//     last batch (replace.go). A lease on the repository (a single conditional
//     UPSERT) keeps two publishers apart.
//   - Re-inserting a vertex or an edge with the same ID overwrites it, which
//     is how upserts work. An edge is identified by source, type, rank and
//     destination; the rank is 0 unless a store needs several parallel edges.
//   - Schema changes reach the Graph service on the next heartbeat, so
//     EnsureSchema waits until each tag, edge type and index answers.
//   - Roles are per space (GOD, ADMIN, DBA, USER, GUEST), not per tag, and
//     the code graph shares the space with reviews, memory, curated records
//     and usage, which the MCP service writes. So each service has its own
//     user: the API holds DBA and owns the schema (EnsureSchema), workers and
//     MCP hold USER (data, no DDL) and only check it (CheckSchema), and the
//     GOD account (root) stays with the init step, which creates the space.
package nebulastore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	nebula "github.com/vesoft-inc/nebula-go/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/astrona-io/codesynapse/internal/sessionmemory"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/vector"
)

// Config holds connection settings. Password is never logged.
type Config struct {
	// Addrs are the Graph service addresses, "host:port" (9669 by default).
	Addrs []string
	// Space is the NebulaGraph space the store owns. Default "codesynapse".
	Space    string
	User     string
	Password string
	// QueryTimeout bounds each read; writes get four times as long. 0 means 30s.
	QueryTimeout time.Duration
	// Vectors holds memory and symbol embeddings. Without one, recall by
	// meaning is off.
	Vectors vector.Index
	// Partitions and Replicas shape the space EnsureSchema creates. 0 means
	// 10 partitions and 1 replica.
	Partitions, Replicas int
	// SchemaWait bounds how long EnsureSchema waits for the Graph service to
	// see the schema it created. 0 means 90s.
	SchemaWait time.Duration
}

// Store is a NebulaGraph-backed store.Store.
type Store struct {
	sessionmemory.Engine // transient chat memory over the session document backend

	cfg     Config
	timeout time.Duration
	vec     vector.Index

	mu   sync.Mutex
	pool *nebula.SessionPool // bound to the space; nil until the space exists
}

var spaceName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,47}$`)

// New checks the settings and that NebulaGraph answers. The space may not
// exist yet: EnsureSchema creates it.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if len(cfg.Addrs) == 0 {
		return nil, errors.New("nebula: at least one graph address (host:port) is required")
	}
	if cfg.Space == "" {
		cfg.Space = "codesynapse"
	}
	if !spaceName.MatchString(cfg.Space) {
		return nil, fmt.Errorf("invalid nebula space name %q", cfg.Space)
	}
	if cfg.Partitions <= 0 {
		cfg.Partitions = 10
	}
	if cfg.Replicas <= 0 {
		cfg.Replicas = 1
	}
	if cfg.SchemaWait <= 0 {
		cfg.SchemaWait = 90 * time.Second
	}
	timeout := cfg.QueryTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	s := &Store{cfg: cfg, timeout: timeout, vec: cfg.Vectors}
	s.Engine = sessionmemory.Engine{Backend: s}
	if _, err := hosts(cfg.Addrs); err != nil {
		return nil, err
	}
	// A Graph service that does not answer is an error here, so the services
	// retry while the database starts; a missing space is not.
	if err := s.ping(ctx); err != nil {
		return nil, fmt.Errorf("connecting to nebula at %s: %w", strings.Join(cfg.Addrs, ","), err)
	}
	if err := s.bind(); err != nil && !errors.Is(err, errNoSpace) {
		return nil, fmt.Errorf("connecting to nebula at %s: %w", strings.Join(cfg.Addrs, ","), err)
	}
	return s, nil
}

func hosts(addrs []string) ([]nebula.HostAddress, error) {
	out := make([]nebula.HostAddress, 0, len(addrs))
	for _, a := range addrs {
		h, p, ok := strings.Cut(a, ":")
		if !ok {
			h, p = a, "9669"
		}
		port, err := strconv.Atoi(p)
		if err != nil || h == "" || port < 1 || port > 65535 {
			return nil, fmt.Errorf("nebula: invalid graph address %q", a)
		}
		out = append(out, nebula.HostAddress{Host: h, Port: port})
	}
	return out, nil
}

// System names the database for metrics.
func (s *Store) System() string { return "nebulagraph" }

// CodeVectors is the index of symbol embeddings (internal/semantic); nil
// without Milvus.
func (s *Store) CodeVectors() vector.Index { return s.vec }

// Close implements store.Store.
func (s *Store) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.pool != nil {
		s.pool.Close()
		s.pool = nil
	}
	s.mu.Unlock()
	if s.vec != nil {
		return s.vec.Close(ctx)
	}
	return nil
}

var errNoSpace = errors.New("nebula: the space does not exist yet; run the schema step")

// bind opens the session pool on the space.
func (s *Store) bind() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pool != nil {
		return nil
	}
	addrs, err := hosts(s.cfg.Addrs)
	if err != nil {
		return err
	}
	conf, err := nebula.NewSessionPoolConf(s.cfg.User, s.cfg.Password, addrs, s.cfg.Space,
		nebula.WithMaxSize(16), nebula.WithMinSize(1), nebula.WithTimeOut(4*s.timeout), nebula.WithIdleTime(10*time.Minute))
	if err != nil {
		return err
	}
	pool, err := nebula.NewSessionPool(*conf, quiet{})
	if err != nil {
		if strings.Contains(err.Error(), "SpaceNotFound") || strings.Contains(strings.ToLower(err.Error()), "space not found") {
			return errNoSpace
		}
		return err
	}
	s.pool = pool
	return nil
}

// ping logs in once on a plain connection, which needs no space.
func (s *Store) ping(ctx context.Context) error {
	return s.admin(ctx, func(sess *nebula.Session) error {
		rs, err := sess.Execute("SHOW HOSTS")
		return check(rs, err)
	})
}

// admin runs f on a session that is not bound to the space (CREATE SPACE,
// space-level DDL before the pool exists).
func (s *Store) admin(ctx context.Context, f func(*nebula.Session) error) error {
	addrs, err := hosts(s.cfg.Addrs)
	if err != nil {
		return err
	}
	conf := nebula.GetDefaultConf()
	conf.TimeOut = 4 * s.timeout
	pool, err := nebula.NewConnectionPool(addrs, conf, quiet{})
	if err != nil {
		return err
	}
	// The goroutine owns the pool: closing it from here, while a call that
	// timed out is still writing on one of its connections, is a data race in
	// the driver. The session goes back and the pool closes before done is sent.
	done := make(chan error, 1)
	go func() {
		sess, err := pool.GetSession(s.cfg.User, s.cfg.Password)
		if err != nil {
			pool.Close()
			done <- err
			return
		}
		err = f(sess)
		sess.Release()
		pool.Close()
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// quiet drops the driver's own logging: it can include statement text.
type quiet struct{}

func (quiet) Info(string)  {}
func (quiet) Warn(string)  {}
func (quiet) Error(string) {}
func (quiet) Fatal(string) {}

// check turns a failed statement into an error. The statement text is left
// out: it is a constant, and the message already names what was wrong.
func check(rs *nebula.ResultSet, err error) error {
	if err != nil {
		return fmt.Errorf("nebula: %w", err)
	}
	if rs == nil {
		return errors.New("nebula: no result")
	}
	if !rs.IsSucceed() {
		return &Error{Code: int(rs.GetErrorCode()), Message: firstLine(rs.GetErrorMsg())}
	}
	return nil
}

// Error is a statement NebulaGraph refused.
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("nebula: %s (code %d)", e.Message, e.Code) }

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s[:min(len(s), 300)]
}

// Row is one result row by column name; NULL columns are nil.
type Row map[string]any

// read runs a constant read statement with parameters.
func (s *Store) read(ctx context.Context, stmt string, params map[string]any) ([]Row, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.timed(ctx, "read", stmt, params)
}

// write runs a constant write statement with parameters.
func (s *Store) write(ctx context.Context, stmt string, params map[string]any) ([]Row, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*s.timeout)
	defer cancel()
	return s.timed(ctx, "write", stmt, params)
}

// timed records the round trip and, for reads, how many rows came back.
func (s *Store) timed(ctx context.Context, op, stmt string, params map[string]any) ([]Row, error) {
	// The span names the operation, never the statement or its parameters.
	ctx, span := telemetry.Tracer().Start(ctx, "nebula "+op, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("db.system.name", "nebulagraph"), attribute.String("db.operation.name", op)))
	defer span.End()
	start := time.Now()
	rows, err := s.exec(ctx, stmt, params)
	if err != nil {
		span.SetStatus(codes.Error, op+" failed")
	}
	tenant := telemetry.Tenant(ctx)
	telemetry.DBDuration.ObserveContext(ctx, time.Since(start).Seconds(), tenant, "nebulagraph", op)
	if err != nil {
		telemetry.DBErrors.IncContext(ctx, tenant, "nebulagraph", op)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			telemetry.DBTimeouts.IncContext(ctx, tenant, "nebulagraph", op)
		}
		return nil, err
	}
	if op == "read" {
		telemetry.GraphTraversed.AddContext(ctx, float64(len(rows)), tenant, "nebulagraph")
		telemetry.GraphRowsReturned.AddContext(ctx, float64(len(rows)), tenant, "nebulagraph")
	}
	return rows, nil
}

// exec sends the statement. The driver cannot be cancelled, so a cancelled
// context abandons the call; the statement may still finish on the server.
func (s *Store) exec(ctx context.Context, stmt string, params map[string]any) ([]Row, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.bind(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	pool := s.pool
	s.mu.Unlock()
	if pool == nil {
		return nil, errNoSpace
	}
	type answer struct {
		rows []Row
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		var rs *nebula.ResultSet
		var err error
		if len(params) == 0 {
			rs, err = pool.Execute(stmt)
		} else {
			rs, err = pool.ExecuteWithParameter(stmt, param(params).(map[string]any))
		}
		if err := check(rs, err); err != nil {
			done <- answer{err: err}
			return
		}
		rows, err := decode(rs)
		done <- answer{rows, err}
	}()
	select {
	case a := <-done:
		return a.rows, a.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// decode turns a result set into rows of Go values.
func decode(rs *nebula.ResultSet) ([]Row, error) {
	cols := rs.GetColNames()
	out := make([]Row, 0, rs.GetRowSize())
	for i := range rs.GetRowSize() {
		rec, err := rs.GetRowValuesByIndex(i)
		if err != nil {
			return nil, err
		}
		row := make(Row, len(cols))
		for j, c := range cols {
			v, err := rec.GetValueByIndex(j)
			if err != nil {
				return nil, err
			}
			row[c], err = value(v)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, row)
	}
	return out, nil
}

func value(v *nebula.ValueWrapper) (any, error) {
	switch {
	case v.IsNull() || v.IsEmpty():
		return nil, nil
	case v.IsBool():
		return v.AsBool()
	case v.IsInt():
		return v.AsInt()
	case v.IsFloat():
		return v.AsFloat()
	case v.IsString():
		return v.AsString()
	case v.IsDateTime() || v.IsDate() || v.IsTime():
		// Only admin answers (SHOW JOBS) carry these; the graph stores text.
		return v.String(), nil
	case v.IsList() || v.IsSet():
		items, err := v.AsList()
		if v.IsSet() {
			items, err = v.AsDedupList()
		}
		if err != nil {
			return nil, err
		}
		out := make([]any, len(items))
		for i := range items {
			if out[i], err = value(&items[i]); err != nil {
				return nil, err
			}
		}
		return out, nil
	case v.IsMap():
		m, err := v.AsMap()
		if err != nil {
			return nil, err
		}
		out := make(map[string]any, len(m))
		for k, item := range m {
			if out[k], err = value(&item); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("nebula: unsupported value type %s", v.GetType())
}

// vid is the vertex ID of a model ID: a one-letter prefix that keeps kinds
// apart and 32 hex digits of its SHA-256. It is the only value written into
// statement text (see the package comment).
func vid(prefix, id string) string {
	h := sha256.Sum256([]byte(id))
	return prefix + hex.EncodeToString(h[:16])
}

func nodeVID(id string) string { return vid("n", id) }

var vidLiteral = regexp.MustCompile(`^[a-z][0-9a-f]{32}$`)

// literal quotes a vertex ID for statement text and panics on anything that
// is not the output of vid: that is a programmer error, never input.
func literal(v string) string {
	if !vidLiteral.MatchString(v) {
		panic("nebulastore: vertex ID " + strconv.Quote(v) + " was not made by vid")
	}
	return `"` + v + `"`
}

// literals quotes IDs for a list in statement text.
func literals(vids []string) string {
	q := make([]string, len(vids))
	for i, v := range vids {
		q[i] = literal(v)
	}
	return strings.Join(q, ",")
}

// ident checks a tag, edge type or property name before it is written into
// a statement. Callers pass allowlisted model values; this is the backstop.
var identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func ident(name string) string {
	if !identPattern.MatchString(name) {
		panic("nebulastore: " + strconv.Quote(name) + " is not a valid identifier")
	}
	return name
}

// anys converts a string list to the form the driver accepts as a parameter.
func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func lower(s string) string { return strings.ToLower(s) }

// param converts values to the types the driver accepts: it knows int, not
// int64 or int32, and lists only as []any.
func param(v any) any {
	switch x := v.(type) {
	case int64:
		return int(x)
	case int32:
		return int(x)
	case []string:
		return anys(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = param(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = param(e)
		}
		return out
	}
	return v
}
