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

var phpFixture = map[string]string{
	"app/Http/Controllers/UserController.php": `<?php

namespace App\Http\Controllers;

use App\Models\User;
use App\Services\{Mailer, Audit as AuditLog};
use Illuminate\Http\Request;
use Symfony\Component\Routing\Attribute\Route;

/**
 * Handles users.
 */
#[Route('/users')]
#[IsGranted('ROLE_USER')]
class UserController extends Controller
{
    public function __construct(private Mailer $mailer) {}

    #[Route('/{id}', methods: ['GET'])]
    public function show(Request $request, int $id)
    {
        $user = User::find($id);
        $this->notify($user);
        $text = "it's {$user->name} }";
        return view('user', ['user' => $user]);
    }

    #[Route('/', methods: ['POST', 'PUT'])]
    public function store(Request $request)
    {
        $name = $request->input('name');
        DB::select("SELECT * FROM users WHERE name = '$name'");
        shell_exec('convert ' . $request->input('file'));
        AuditLog::record($name);
        return helper_format($name);
    }

    private function notify(User $user): void
    {
        $body = <<<EOT
        Hello {$user->name} {
        EOT;
        $this->mailer->send($body); # a comment with { brace
    }
}
`,
	"app/Http/Controllers/helpers.php": `<?php

namespace App\Http\Controllers;

function helper_format(string $s): string
{
    return $s;
}
`,
	"app/Models/User.php": `<?php
namespace App\Models;

class User extends Model
{
    const API_TOKEN = 'sk_live_9f8e7d6c5b4a3928';

    public static function find(int $id): ?User
    {
        return null;
    }
}
`,
	"app/Services/Mailer.php": `<?php
namespace App\Services;

class Mailer
{
    public function send(string $body): void {}
}
`,
	"app/Services/Audit.php": `<?php
namespace App\Services;

final class Audit
{
    public static function record(string $what): void
    {
        $token = md5(uniqid());
    }
}
`,
	"routes/web.php": `<?php

use Illuminate\Support\Facades\Route;
use App\Http\Controllers\UserController;

Route::get('dashboard', [UserController::class, 'show'])->middleware('auth');
Route::post('/login', function () {
    return 'ok';
});
?>
<html><body>{ not php }</body></html>
`,
	"tests/Feature/UserTest.php": `<?php

namespace Tests\Feature;

use App\Models\User;
use PHPUnit\Framework\TestCase;

class UserTest extends TestCase
{
    public function test_finds_user(): void
    {
        $this->assertNull(User::find(1));
    }
}
`,
	"tests/Pest/ExampleTest.php": `<?php

test('formats', function () {
    expect(helper_format('x'))->toBe('x');
});
`,
	"resources/views/home.blade.php": `<p>{{ $name }}</p>`,
}

func TestPHP(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range phpFixture {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
	res, err := Extract(context.Background(), "r", dir)
	require.NoError(t, err)
	g := res.Graph
	assert.Equal(t, res.Languages[PHP], 8, "Blade templates are not PHP code")
	id := func(k model.NodeKind, rel, name string) string { return model.NodeID("r", k, rel+"::"+name) }
	node := func(nid string) model.Node {
		t.Helper()
		n, ok := g.Node(nid)
		require.True(t, ok, "missing "+nid)
		return *n
	}
	ctl := "app/Http/Controllers/UserController.php"
	users := node(id(model.KindType, ctl, "UserController"))
	assert.Equal(t, users.Summary, "Handles users.")
	assert.Equal(t, users.QualifiedName, `App\Http\Controllers.UserController`)
	show := node(id(model.KindMethod, ctl, "UserController.show"))
	assert.Equal(t, show.EndLine, 26, "a string with a brace does not break the body")
	notify := node(id(model.KindMethod, ctl, "UserController.notify"))
	assert.Equal(t, notify.EndLine, 44, "neither does a heredoc or a # comment")
	assert.False(t, notify.Exported)
	test := node(id(model.KindMethod, "tests/Feature/UserTest.php", "UserTest.test_finds_user"))
	assert.True(t, test.IsTest)

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
	find := id(model.KindMethod, "app/Models/User.php", "User.find")
	helper := id(model.KindFunction, "app/Http/Controllers/helpers.php", "helper_format")
	store := id(model.KindMethod, ctl, "UserController.store")
	calls(show.ID, find)
	calls(show.ID, notify.ID)
	calls(store, id(model.KindMethod, "app/Services/Audit.php", "Audit.record"))
	calls(store, helper)
	calls(notify.ID, id(model.KindMethod, "app/Services/Mailer.php", "Mailer.send"))
	calls(test.ID, find)
	calls(id(model.KindFunction, "tests/Pest/ExampleTest.php", "<tests>"), helper)

	var found []string
	for _, n := range g.Nodes() {
		if n.Kind == model.KindFinding {
			found = append(found, n.Rule)
		}
	}
	slices.Sort(found)
	assert.Equal(t, found, []string{"exec-shell", "hardcoded-secret", "insecure-rand", "sql-concat", "weak-hash"})
	assert.True(t, slices.Contains(node(store).SecurityTags, "exec"))

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
		"GET /users/{id}": show.ID,
		"POST /users":     store,
		"PUT /users":      store,
		"GET /dashboard":  show.ID, // [UserController::class, 'show'] through the use import
		"POST /login":     "",
	})
	assert.Equal(t, guards["POST /users"], "IsGranted('ROLE_USER')")
	assert.Equal(t, guards["GET /dashboard"], "middleware(auth)")
}

func TestPHPRules(t *testing.T) {
	for _, tt := range []struct {
		line string
		want []string
	}{
		{`exec("tar xzf " . $path);`, []string{"exec-shell"}},
		{`exec('tar xzf ' . escapeshellarg($path));`, nil},
		{`exec('uptime');`, nil},
		{`$pdo->exec("DELETE FROM t WHERE id = $id");`, []string{"sql-concat"}},
		{"$out = `ls $dir`;", []string{"exec-shell"}},
		{`$rows = $pdo->query("SELECT * FROM users WHERE id = " . $id);`, []string{"sql-concat"}},
		{`$rows = $pdo->prepare('SELECT * FROM users WHERE id = ?');`, nil},
		{`User::whereRaw("name = '$name'")->get();`, []string{"sql-concat"}},
		{`return Genre::query()->where('name', 'like', "%{$name}%")->first();`, nil},
		{`eval('$x = ' . $code . ';');`, []string{"code-injection"}},
		{`$data = unserialize($_COOKIE['cart']);`, []string{"deserialize"}},
		{`$data = unserialize($raw, ['allowed_classes' => false]);`, nil},
		{`include $_GET['page'] . '.php';`, []string{"path-from-request"}},
		{`return file_get_contents(storage_path($request->input('file')));`, []string{"path-from-request"}},
		{`$hash = md5($password);`, []string{"weak-hash"}},
		{`$key = md5($url);`, nil},
		{`openssl_encrypt($data, 'des-ede3', $key);`, []string{"weak-cipher"}},
		{`openssl_encrypt($data, 'aes-256-gcm', $key, 0, $iv, $tag);`, nil},
		{`curl_setopt($ch, CURLOPT_SSL_VERIFYPEER, false);`, []string{"tls-insecure"}},
		{`$client = new Client(['verify' => false]);`, []string{"tls-insecure"}},
		{`$token = bin2hex(mt_rand());`, []string{"insecure-rand"}},
		{`$secret = 'hunter2hunter2';`, []string{"hardcoded-secret"}},
		{`// exec($cmd);`, nil},
	} {
		t.Run(tt.line, func(t *testing.T) {
			var got []string
			for _, f := range phpRules(tt.line, maskPHP("<?php " + tt.line)[6:], 1, "") {
				got = append(got, f.rule)
			}
			assert.Equal(t, got, tt.want)
		})
	}
}

// The PHP examples of the rule catalogue are what phpRules detects.
func TestPHPRuleExamples(t *testing.T) {
	for _, r := range rules.All() {
		for _, e := range r.Examples {
			if e.Language != PHP {
				continue
			}
			t.Run(r.ID, func(t *testing.T) {
				found := func(line string) bool {
					for _, f := range phpRules(line, maskPHP("<?php " + line)[6:], 1, "") {
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

func TestMaskPHP(t *testing.T) {
	src := "<html>{</html>\n<?php $a = 'x{'; # {\n$b = <<<'EOT'\n{ raw\nEOT;\n?>\n<p>}</p>\n<?= f() ?>"
	m := maskPHP(src)
	assert.Equal(t, strings.Count(m, "{")+strings.Count(m, "}"), 0)
	assert.Equal(t, strings.Count(m, "\n"), strings.Count(src, "\n"))
	assert.Contains(t, m, "f()")
	assert.Contains(t, m, "$b =")
}
