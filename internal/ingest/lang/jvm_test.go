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

var jvmFixture = map[string]string{
	"src/main/java/com/acme/web/UserController.java": `package com.acme.web;

import com.acme.service.UserService;
import org.springframework.web.bind.annotation.*;

/** Serves users. */
@RestController
@RequestMapping("/api/users")
public class UserController {
    private final UserService userService = new UserService();

    /** Returns one user. */
    @GetMapping("/{id}")
    public String get(@PathVariable String id) {
        return userService.find(id);
    }

    @PostMapping
    @PreAuthorize("hasRole('ADMIN')")
    public void create(String name) throws Exception {
        audit(name);
        Runtime.getRuntime().exec(name);
    }

    private void audit(String name) {
        System.out.println(name);
    }
}
`,
	"src/main/java/com/acme/service/UserService.java": `package com.acme.service;

import com.acme.util.Validation;
import java.security.MessageDigest;
import java.sql.Connection;

public class UserService {
    private Connection conn;
    private static final String API_TOKEN = "sk_live_9f8e7d6c5b4a39281706";

    public UserService() {
    }

    public String find(String id) {
        Validation.check(id);
        conn.createStatement().executeQuery("SELECT * FROM users WHERE id = '" + id + "'");
        return id;
    }

    String fingerprint(byte[] data) throws Exception {
        return new String(MessageDigest.getInstance("MD5").digest(data));
    }
}
`,
	"src/main/java/com/acme/util/Validation.java": `package com.acme.util;

public final class Validation {
    public static void check(String value) {
        if (value == null || value.isEmpty()) {
            throw new IllegalArgumentException("empty");
        }
    }
}
`,
	"src/main/kotlin/com/acme/util/Strings.kt": `package com.acme.util

fun slug(text: String): String = text.lowercase().replace(" ", "-")

class Formatter {
    fun format(title: String): String {
        val s = slug(title)
        return "Title: $s"
    }

    companion object {
        fun create() = Formatter()
    }
}

fun Application.module() {
    routing {
        get("/health") {
            call.respondText("ok")
        }
    }
}
`,
	"src/test/java/com/acme/service/UserServiceTest.java": `package com.acme.service;

import org.junit.jupiter.api.Test;

class UserServiceTest {
    @Test
    void findsUser() {
        new UserService().find("1");
    }
}
`,
}

func TestJavaKotlin(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range jvmFixture {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
	res, err := Extract(context.Background(), "r", dir)
	require.NoError(t, err)
	g := res.Graph
	assert.Equal(t, res.Languages[Java], 4)
	assert.Equal(t, res.Languages[Kotlin], 1)

	id := func(k model.NodeKind, rel, name string) string { return model.NodeID("r", k, rel+"::"+name) }
	ctl := "src/main/java/com/acme/web/UserController.java"
	svc := "src/main/java/com/acme/service/UserService.java"
	kt := "src/main/kotlin/com/acme/util/Strings.kt"
	node := func(nid string) model.Node {
		t.Helper()
		n, ok := g.Node(nid)
		require.True(t, ok, "missing "+nid)
		return *n
	}
	get := node(id(model.KindMethod, ctl, "UserController.get"))
	assert.Equal(t, get.QualifiedName, "com.acme.web.UserController.get")
	assert.Equal(t, get.Package, "com.acme.web", "the declared package is the module")
	assert.True(t, get.Exported)
	assert.Equal(t, get.Summary, "Returns one user.")
	assert.Equal(t, node(id(model.KindType, ctl, "UserController")).Summary, "Serves users.")
	assert.False(t, node(id(model.KindMethod, ctl, "UserController.audit")).Exported)
	node(id(model.KindFunction, kt, "slug"))
	node(id(model.KindMethod, kt, "Formatter.format"))
	node(id(model.KindMethod, kt, "Formatter.Companion.create"))
	_, field := g.Node(id(model.KindMethod, ctl, "UserController.UserService"))
	assert.False(t, field, "a field initialiser is no method")
	assert.True(t, node(id(model.KindMethod, "src/test/java/com/acme/service/UserServiceTest.java", "UserServiceTest.findsUser")).IsTest)

	edges := map[[2]string]model.Evidence{}
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeCalls {
			edges[[2]string{e.From, e.To}] = e.Evidence
		}
	}
	calls := func(from, to string) model.Evidence {
		t.Helper()
		ev, ok := edges[[2]string{from, to}]
		assert.True(t, ok, from+" -> "+to)
		return ev
	}
	assert.Equal(t, calls(get.ID, id(model.KindMethod, svc, "UserService.find")), model.EvidenceInferred, "a field of another class: by unique name")
	assert.Equal(t, calls(id(model.KindMethod, ctl, "UserController.create"), id(model.KindMethod, ctl, "UserController.audit")), model.EvidenceSyntactic, "implicit this")
	assert.Equal(t, calls(id(model.KindMethod, svc, "UserService.find"), id(model.KindMethod, "src/main/java/com/acme/util/Validation.java", "Validation.check")), model.EvidenceSyntactic, "an imported class's static method")
	assert.Equal(t, calls(id(model.KindMethod, kt, "Formatter.format"), id(model.KindFunction, kt, "slug")), model.EvidenceSyntactic)
	calls(id(model.KindMethod, "src/test/java/com/acme/service/UserServiceTest.java", "UserServiceTest.findsUser"), id(model.KindMethod, svc, "UserService.find"))

	var rules []string
	for _, n := range g.Nodes() {
		if n.Kind == model.KindFinding {
			rules = append(rules, n.Rule)
		}
	}
	slices.Sort(rules)
	assert.Equal(t, rules, []string{"exec-variable", "hardcoded-secret", "sql-concat", "weak-hash"})
	assert.True(t, slices.Contains(node(id(model.KindMethod, ctl, "UserController.create")).SecurityTags, "exec"))

	routes := map[string]string{}
	handled := map[string]string{}
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeHandledBy {
			handled[e.From] = e.To
		}
	}
	for _, n := range g.Nodes() {
		if n.Kind == model.KindRoute {
			routes[n.Signature+" "+n.Name] = handled[n.ID] + " | " + n.Summary
		}
	}
	assert.Equal(t, routes, map[string]string{
		"http GET /api/users/{id}": get.ID + " | ",
		"http POST /api/users":     id(model.KindMethod, ctl, "UserController.create") + ` | PreAuthorize("hasRole('ADMIN')")`,
		"http GET /health":         " | ",
	})
}

func TestJVMRules(t *testing.T) {
	tests := []struct {
		line string
		want []string
	}{
		{`jdbcTemplate.query("SELECT * FROM t WHERE a = '" + a + "'", mapper);`, []string{"sql-concat"}},
		{`em.createNativeQuery("DELETE FROM t WHERE id = $id")`, []string{"sql-concat"}},
		{`jdbcTemplate.query("SELECT * FROM t WHERE a = ?", mapper, a);`, nil},
		{`digester.update("$userName:$realm".toByteArray())`, nil},
		{`new ProcessBuilder("sh", "-c", "ls " + dir).start();`, []string{"exec-shell"}},
		{`new ProcessBuilder("git", "status").start();`, nil},
		{`Cipher.getInstance("AES/GCM/NoPadding")`, nil},
		{`Cipher.getInstance("DES")`, []string{"weak-cipher"}},
		{`val resetToken = Random().nextInt()`, []string{"insecure-rand"}},
		{`val count = Random().nextInt()`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			var got []string
			for _, f := range jvmRules(tt.line, maskBrace(tt.line, true, false), 1, "") {
				got = append(got, f.rule)
			}
			assert.Equal(t, got, tt.want)
		})
	}
}
