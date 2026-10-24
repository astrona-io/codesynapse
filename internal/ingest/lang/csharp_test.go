package lang

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/rules"
)

var csFixture = map[string]string{
	"src/Api/Program.cs": `using Acme.Api.Services;

var builder = WebApplication.CreateBuilder(args);
var app = builder.Build();
app.MapGet("/health", Health).AllowAnonymous();
app.MapPost("/users", (UserDto u) => Results.Ok()).RequireAuthorization();
app.Run();

static string Health() => "ok";
`,
	"src/Api/Controllers/UsersController.cs": `using System.Diagnostics;
using Microsoft.AspNetCore.Mvc;
using Acme.Api.Services;
using Newtonsoft.Json;

namespace Acme.Api.Controllers
{
    /// <summary>
    /// Manages users.
    /// </summary>
    [ApiController]
    [Route("api/[controller]")]
    [Authorize]
    public class UsersController : ControllerBase
    {
        private readonly UserService _users = new UserService();

        [HttpGet("{id}")]
        public IActionResult Get(int id)
        {
            var u = _users.Find(id);
            return Ok(u);
        }

        [HttpPost, AllowAnonymous]
        public async Task<IActionResult> Create([FromBody] string body)
        {
            Process.Start("cmd.exe", $"/c echo {body}");
            return Ok(JsonConvert.DeserializeObject(body));
        }

        [HttpDelete("/admin/users/{id}")]
        public void Remove(int id) => UserService.Purge(id);
    }
}
`,
	"src/Api/Services/UserService.cs": "\ufeff" + `namespace Acme.Api.Services;

public record UserDto(string Name);

public sealed class UserService
{
    private const string ApiKey = "sk_live_9f8e7d6c5b4a3928";

    public UserService() { }

    /// <summary>Finds a user.</summary>
    public UserDto? Find(int id)
    {
        var sql = @"SELECT ""name"" {
                    FROM users";
        using var cmd = new SqlCommand($"SELECT * FROM users WHERE id = {id}", conn);
        return Load(sql);
    }

    private UserDto? Load(string sql) => null;

    public static void Purge(int id)
    {
        var md5 = MD5.Create();
    }
}
`,
	"tests/Acme.Api.Tests/UserServiceTests.cs": `using Acme.Api.Services;
using Xunit;
using Dto = Acme.Api.Services.UserDto;

namespace Acme.Api.Tests;

public class UserServiceTests
{
    [Fact]
    public void FindsUser()
    {
        var s = new UserService();
        Assert.Null(s.Find(1));
    }
}
`,
}

func TestCSharp(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range csFixture {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
	res, err := Extract(context.Background(), "r", dir)
	require.NoError(t, err)
	g := res.Graph
	assert.Equal(t, res.Languages[CSharp], 4)
	id := func(k model.NodeKind, rel, name string) string { return model.NodeID("r", k, rel+"::"+name) }
	node := func(nid string) model.Node {
		t.Helper()
		n, ok := g.Node(nid)
		require.True(t, ok, "missing "+nid)
		return *n
	}
	ctl, svc, tst := "src/Api/Controllers/UsersController.cs", "src/Api/Services/UserService.cs", "tests/Acme.Api.Tests/UserServiceTests.cs"
	users := node(id(model.KindType, ctl, "UsersController"))
	assert.Equal(t, users.Summary, "Manages users.")
	assert.Equal(t, users.Package, "Acme.Api.Controllers", "a block namespace is the package, and does not nest the class")
	assert.Equal(t, users.QualifiedName, "Acme.Api.Controllers.UsersController")
	node(id(model.KindType, svc, "UserDto"))
	find := node(id(model.KindMethod, svc, "UserService.Find"))
	assert.Equal(t, find.Summary, "Finds a user.")
	assert.Equal(t, find.EndLine, 18, "the verbatim string with a brace and quotes does not break the body")
	assert.True(t, find.Exported)
	assert.False(t, node(id(model.KindMethod, svc, "UserService.Load")).Exported)
	assert.Equal(t, node(id(model.KindMethod, ctl, "UsersController.Remove")).EndLine, 33, "an expression-bodied method ends on its line")
	fact := node(id(model.KindMethod, tst, "UserServiceTests.FindsUser"))
	assert.True(t, fact.IsTest)
	assert.Equal(t, fact.QualifiedName, "Acme.Api.Tests.UserServiceTests.FindsUser")

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
	calls(id(model.KindMethod, ctl, "UsersController.Get"), find.ID)
	calls(find.ID, id(model.KindMethod, svc, "UserService.Load"))
	calls(id(model.KindMethod, ctl, "UsersController.Remove"), id(model.KindMethod, svc, "UserService.Purge"))
	calls(fact.ID, id(model.KindMethod, svc, "UserService.UserService"))
	calls(fact.ID, find.ID)

	var rules []string
	for _, n := range g.Nodes() {
		if n.Kind == model.KindFinding {
			rules = append(rules, n.Rule)
		}
	}
	slices.Sort(rules)
	assert.Equal(t, rules, []string{"exec-shell", "hardcoded-secret", "sql-concat", "weak-hash"})
	assert.True(t, slices.Contains(node(id(model.KindMethod, ctl, "UsersController.Create")).SecurityTags, "exec"))
	for _, ns := range []string{"Newtonsoft.Json", "Microsoft.AspNetCore.Mvc", "Xunit"} {
		assert.True(t, len(res.ExternalImports[ns]) > 0, ns)
	}
	for _, ns := range []string{"Acme.Api.Services", "Acme.Api"} {
		_, local := res.ExternalImports[ns]
		assert.False(t, local, ns+" is a namespace of the repository (one file starts with a byte order mark)")
	}

	handled := map[string]string{}
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeHandledBy {
			handled[e.From] = e.To
		}
	}
	routes := map[string]string{}
	guards := map[string]string{}
	for _, n := range g.Nodes() {
		if n.Kind == model.KindRoute {
			routes[n.Name] = handled[n.ID]
			guards[n.Name] = n.Summary
		}
	}
	assert.Equal(t, routes, map[string]string{
		"GET /api/users/{id}":      id(model.KindMethod, ctl, "UsersController.Get"),
		"POST /api/users":          id(model.KindMethod, ctl, "UsersController.Create"),
		"DELETE /admin/users/{id}": id(model.KindMethod, ctl, "UsersController.Remove"),
		"GET /health":              "", // a local function of the top-level statements
		"POST /users":              "",
	})
	assert.Equal(t, guards["POST /api/users"], "Authorize; AllowAnonymous")
	assert.Equal(t, guards["POST /users"], "RequireAuthorization")
}

func TestCSharpRules(t *testing.T) {
	for _, tt := range []struct {
		line string
		want []string
	}{
		{`Process.Start("bash", "-c " + cmd);`, []string{"exec-shell"}},
		{`Process.Start(fileName);`, []string{"exec-variable"}},
		{`Process.Start("notepad.exe");`, nil},
		{`var rows = conn.Query<User>("SELECT * FROM users WHERE name = '" + name + "'");`, []string{"sql-concat"}},
		{`var rows = conn.Query<User>("SELECT * FROM users WHERE name = @name", new { name });`, nil},
		{`db.Users.FromSqlRaw($"SELECT * FROM users WHERE id = {id}");`, []string{"sql-concat"}},
		{`db.Users.FromSqlInterpolated($"SELECT * FROM users WHERE id = {id}");`, nil},
		{`var f = new BinaryFormatter();`, []string{"deserialize"}},
		{`var s = new JsonSerializerSettings { TypeNameHandling = TypeNameHandling.All };`, []string{"deserialize"}},
		{`using var sha = SHA1.Create();`, []string{"weak-hash"}},
		{`using var sha = SHA256.Create();`, nil},
		{`var des = DES.Create();`, []string{"weak-cipher"}},
		{`aes.Mode = CipherMode.ECB;`, []string{"weak-cipher"}},
		{`handler.ServerCertificateCustomValidationCallback = (m, c, ch, e) => true;`, []string{"tls-insecure"}},
		{`handler.ServerCertificateCustomValidationCallback = HttpClientHandler.DangerousAcceptAnyServerCertificateValidator;`, []string{"tls-insecure"}},
		{`var token = new Random().Next().ToString();`, []string{"insecure-rand"}},
		{`var index = new Random().Next(10);`, nil},
		{`private const string Password = "hunter2hunter2";`, []string{"hardcoded-secret"}},
		{`private const string PasswordHeader = "X-Password-Hash";`, nil},
		{`// Process.Start(fileName);`, nil},
	} {
		t.Run(tt.line, func(t *testing.T) {
			var got []string
			for _, f := range csRules(tt.line, maskBrace(tt.line, true, true), 1, "") {
				got = append(got, f.rule)
			}
			assert.Equal(t, got, tt.want)
		})
	}
}

func TestCSharpMask(t *testing.T) {
	src := "var a = @\"x \"\" { y\nz\";\nvar b = $@\"{q} }\";\nvar c = \"\"\"\n{ raw\n\"\"\";\nf();"
	m := maskBrace(src, true, true)
	assert.Equal(t, strings.Count(m, "{")+strings.Count(m, "}"), 0)
	assert.True(t, strings.HasSuffix(m, "f();"))
}

// The C# examples of the rule catalogue are what csRules detects.
func TestCSharpRuleExamples(t *testing.T) {
	for _, r := range rules.All() {
		for _, e := range r.Examples {
			if e.Language != CSharp {
				continue
			}
			t.Run(r.ID, func(t *testing.T) {
				found := func(line string) bool {
					for _, f := range csRules(line, maskBrace(line, true, true), 1, "") {
						if f.rule == r.ID {
							return true
						}
					}
					return false
				}
				assert.True(t, found(e.Vulnerable), "vulnerable example not detected")
				assert.False(t, found(e.Fixed), "fixed example detected")
			})
		}
	}
}
