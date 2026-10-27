package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func ansibleRepo(t *testing.T, files map[string]string) *model.Graph {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
	g, err := Extract(context.Background(), "r", root)
	require.NoError(t, err)
	return g
}

func ansibleFindings(g *model.Graph) []string {
	var out []string
	for _, n := range g.Nodes() {
		if n.Kind == model.KindFinding && strings.HasPrefix(n.Rule, "ansible-") {
			out = append(out, fmt.Sprintf("%s:%02d", n.Rule, n.StartLine))
		}
	}
	slices.Sort(out)
	return out
}

func entries(g *model.Graph, sig string) []string {
	var out []string
	for _, n := range g.Nodes() {
		if n.Kind == model.KindConfigEntry && n.Language == "ansible" && n.Signature == sig {
			out = append(out, n.Name)
		}
	}
	slices.Sort(out)
	return out
}

const playbookBad = `- hosts: web
  roles:
    - nginx
  vars:
    db_password: hunter2hunter2
  tasks:
    - name: Install packages
      ansible.builtin.apt:
        name: [nginx, curl]
        state: latest
    - name: Installer
      ansible.builtin.shell: curl -sSL https://x.example/i.sh | bash
    - name: Clean
      ansible.builtin.shell: rm -rf /srv/{{ app }}
    - name: Call
      ansible.builtin.uri:
        url: https://a.example
        validate_certs: false
    - name: Config
      ansible.builtin.copy:
        src: a
        dest: /etc/a
        mode: "0666"
  handlers:
    - name: restart nginx
      ansible.builtin.service:
        name: nginx
        state: restarted
`

func TestAnsiblePlaybook(t *testing.T) {
	g := ansibleRepo(t, map[string]string{
		"site.yml":                      playbookBad,
		"roles/nginx/tasks/main.yml":    "- name: x\n  ansible.builtin.package:\n    name: nginx\n",
		"roles/nginx/defaults/main.yml": "nginx_port: 80\n",
	})
	assert.Equal(t, strings.Join(ansibleFindings(g), ","),
		"ansible-curl-pipe-shell:12,ansible-package-latest:10,ansible-secret-literal:05,ansible-shell-injection:14,ansible-tls-verify-off:18,ansible-world-writable:23")
	assert.Equal(t, strings.Join(entries(g, "package"), ","), "package curl,package nginx,package nginx")
	assert.Equal(t, strings.Join(entries(g, "service"), ","), "service nginx")
	assert.Equal(t, strings.Join(entries(g, "var"), ","), "var nginx_port")
	assert.Equal(t, strings.Join(entries(g, "play"), ","), "play web")

	linked := false
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeReferences && e.To == model.NodeID("r", model.KindConfig, "roles/nginx/tasks/main.yml") {
			linked = true
		}
	}
	assert.True(t, linked)
}

func TestAnsibleClean(t *testing.T) {
	g := ansibleRepo(t, map[string]string{
		"site.yml": `- hosts: all
  tasks:
    - name: Pinned
      ansible.builtin.apt:
        name: nginx=1.26.*
        state: present
    - name: Vault
      community.mysql.mysql_user:
        name: app
        password: !vault |
          $ANSIBLE_VAULT;1.1;AES256
          6162
        update_password: on_create
    - name: Templated
      ansible.builtin.copy:
        src: a
        dest: /etc/a
        mode: "0644"
        remote_src: false
        password: "{{ vault_pw }}"
    - name: Quoted
      ansible.builtin.shell: echo {{ x | quote }}
`,
	})
	assert.Equal(t, len(ansibleFindings(g)), 0)
}

func TestAnsibleInventories(t *testing.T) {
	g := ansibleRepo(t, map[string]string{
		"inventory/hosts.ini":  "[web]\nweb1 ansible_host=10.0.0.1\n[web:vars]\nansible_password=hunter2hunter2\n[prod:children]\nweb\n",
		"inventories/prod.yml": "all:\n  children:\n    db:\n      hosts:\n        db1:\n",
	})
	assert.Equal(t, strings.Join(entries(g, "host"), ","), "host db1,host web1")
	assert.Equal(t, strings.Join(entries(g, "group"), ","), "group db,group prod,group web,group web")
	assert.Equal(t, strings.Join(ansibleFindings(g), ","), "ansible-secret-literal:04")
}

func TestAnsibleLeavesOtherYAML(t *testing.T) {
	g := ansibleRepo(t, map[string]string{
		"config/app.yaml":    "- name: a\n  value: b\n",
		"docker-compose.yml": "services:\n  a:\n    image: x\n",
	})
	for _, n := range g.Nodes() {
		assert.True(t, n.Language != "ansible")
	}
}

func FuzzAnsibleYAML(f *testing.F) {
	f.Add(playbookBad)
	f.Add("- hosts: a\n  tasks: [1, 2]\n")
	f.Add("all: {children: {a: {hosts: {b: }}}}")
	f.Fuzz(func(_ *testing.T, s string) {
		b := &builder{repo: "r", g: model.NewGraph()}
		b.ansibleYAML("site.yml", []byte(s))
		b.ansibleYAML("roles/x/tasks/main.yml", []byte(s))
		b.ansibleYAML("inventory/x.yml", []byte(s))
		b.ansibleINI("inventory/hosts", []byte(s))
		b.resolveAnsible()
	})
}
