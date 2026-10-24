package lang

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

var rustFixture = map[string]string{
	"app/Cargo.toml": "[package]\nname = \"app\"\n",
	"app/src/main.rs": `mod db;
mod handlers;

use axum::{routing::get, Router};
use crate::handlers::{list_users, create_user as create};
use db::User;

#[tokio::main]
async fn main() {
    let app = Router::new()
        .route("/users", get(list_users).post(create))
        .route("/health", get(|| async { "ok" }));
    db::open("postgres://localhost");
}
`,
	"app/src/db.rs": `use sqlx::PgPool;

/// A user row.
pub struct User<'a> {
    pub name: &'a str,
}

pub trait Store {
    fn load(&self, id: u32) -> String;
}

impl<'a> Store for User<'a> {
    fn load(&self, id: u32) -> String {
        self.helper(id)
    }
}

impl<'a> User<'a> {
    pub fn new(name: &'a str) -> Self {
        User { name }
    }

    fn helper(&self, id: u32) -> String {
        let quote = '\'';
        format!("{}{}", id, quote)
    }
}

/// Opens the pool.
pub fn open(url: &str) {
    let r = r#"SELECT "x" FROM t"#;
    let _ = (url, r);
}

pub async fn find(pool: &PgPool, name: &str) {
    sqlx::query(&format!("SELECT * FROM users WHERE name = '{}'", name)).execute(pool).await.unwrap();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn opens() {
        open("x");
    }
}
`,
	"app/src/handlers.rs": `use crate::db::{self, User};
use std::process::Command;

const API_TOKEN: &str = "sk_live_9f8e7d6c5b4a39281706";

pub async fn list_users() -> String {
    let u = User::new("ada");
    db::open("x");
    u.name.to_string()
}

pub async fn create(body: String) {
    Command::new("sh").arg("-c").arg(format!("echo {}", body)).status().unwrap();
}

#[actix_web::get("/legacy")]
async fn legacy() -> String {
    String::new()
}
`,
	"app/tests/api.rs": `#[test]
fn health_works() {
    assert!(true);
}
`,
}

func TestRust(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range rustFixture {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
	res, err := Extract(context.Background(), "r", dir)
	require.NoError(t, err)
	g := res.Graph
	assert.Equal(t, res.Languages[Rust], 4)
	id := func(k model.NodeKind, rel, name string) string { return model.NodeID("r", k, rel+"::"+name) }
	node := func(nid string) model.Node {
		t.Helper()
		n, ok := g.Node(nid)
		require.True(t, ok, "missing "+nid)
		return *n
	}
	db, h := "app/src/db.rs", "app/src/handlers.rs"
	user := node(id(model.KindType, db, "User"))
	assert.Equal(t, user.Summary, "A user row.")
	assert.Equal(t, user.Package, "app::db")
	assert.True(t, user.Exported)
	node(id(model.KindType, db, "Store"))
	node(id(model.KindMethod, db, "Store.load"))
	load := node(id(model.KindMethod, db, "User.load"))
	assert.Equal(t, load.StartLine, 13, "impl Store for User: the method belongs to User")
	assert.False(t, node(id(model.KindMethod, db, "User.helper")).Exported)
	open := node(id(model.KindFunction, db, "open"))
	assert.Equal(t, open.Summary, "Opens the pool.")
	assert.Equal(t, open.EndLine, 33, "the raw string with quotes does not break the body")
	assert.True(t, node(id(model.KindMethod, db, "tests.opens")).IsTest)
	assert.True(t, node(id(model.KindFunction, "app/tests/api.rs", "health_works")).IsTest)

	edges := map[[2]string]bool{}
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeCalls {
			edges[[2]string{e.From, e.To}] = true
		}
	}
	calls := func(from, to string) {
		t.Helper()
		assert.True(t, edges[[2]string{from, to}], from+" -> "+to)
	}
	calls(load.ID, id(model.KindMethod, db, "User.helper"))
	list := id(model.KindFunction, h, "list_users")
	calls(list, id(model.KindMethod, db, "User.new"))
	calls(list, open.ID)
	calls(id(model.KindFunction, "app/src/main.rs", "main"), open.ID)
	calls(id(model.KindMethod, db, "tests.opens"), open.ID)

	var rules []string
	for _, n := range g.Nodes() {
		if n.Kind == model.KindFinding {
			rules = append(rules, n.Rule)
		}
	}
	slices.Sort(rules)
	assert.Equal(t, rules, []string{"exec-shell", "hardcoded-secret", "sql-concat"})
	assert.True(t, slices.Contains(node(id(model.KindFunction, h, "create")).SecurityTags, "exec"))
	assert.True(t, len(res.ExternalImports["sqlx"]) > 0 && len(res.ExternalImports["axum"]) > 0, "crates for Cargo reachability")
	for _, local := range []string{"std", "db", "crate"} {
		_, ok := res.ExternalImports[local]
		assert.False(t, ok, local+" is not a crate")
	}

	handled := map[string]string{}
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeHandledBy {
			handled[e.From] = e.To
		}
	}
	routes := map[string]string{}
	for _, n := range g.Nodes() {
		if n.Kind == model.KindRoute {
			routes[n.Name] = handled[n.ID]
		}
	}
	assert.Equal(t, routes, map[string]string{
		"GET /users":  id(model.KindFunction, h, "list_users"), // imported by use crate::handlers::{…}
		"POST /users": id(model.KindFunction, h, "create"),
		"GET /legacy": id(model.KindFunction, h, "legacy"),
	})
}
