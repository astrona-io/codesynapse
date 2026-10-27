package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/redact"
)

// Ansible: playbooks, role files, variable files and inventories are read
// as structure for IT-operations questions (which plays run on a host
// group, which tasks use a module, where a package or service is managed)
// and checked for a few risky patterns. Only names are stored, never
// values; a templated name ("{{ item }}") is not guessed. Entries use the
// existing Config and ConfigEntry kinds, told apart by Signature:
// play, role, task, handler, package, service, var, group, host, collection.

const ansibleLanguage = "ansible"

// maxAnsibleSubjects bounds package and service entries made by one task.
const maxAnsibleSubjects = 20

var (
	roleFilePath  = regexp.MustCompile(`(^|/)roles/([^/]+)/(tasks|handlers|defaults|vars|meta)/`)
	roleMainPath  = regexp.MustCompile(`(^|/)roles/([^/]+)/tasks/main\.ya?ml$`)
	varsFilePath  = regexp.MustCompile(`(^|/)(group_vars|host_vars)/`)
	inventoryPath = regexp.MustCompile(`(^|/)(inventory|inventories)(/|$)|(^|/)(hosts|inventory)(\.ya?ml|\.ini)?$`)

	ansibleSecretKey = regexp.MustCompile(`(?i)(passw(or)?d|passwd|secret|token|api[_-]?key|apikey|private[_-]?key|access[_-]?key|credential|(^|_)pass$)`)
	// ansibleNotSecret are options that name a policy, file or length, not a secret.
	ansibleNotSecret = regexp.MustCompile(`(?i)(update_password|password_lock|password_expire\w*|(_file|_path|_length|_src|_dest|_name|_user|_id|_arn|_type|_mode|_state|_policy|_command|_prompt)$)`)

	ansibleTemplate = regexp.MustCompile(`\{\{.*?\}\}`)
	chmodWorld      = regexp.MustCompile(`\bchmod\s+(?:-\S+\s+)*0?[0-7]?[0-7][0-7][2367]\b`)
	insecureFlag    = regexp.MustCompile(`\b(?:curl|wget)\b[^|;&\n]*\s(?:--insecure|-k|--no-check-certificate)\b`)
	worldWritable   = regexp.MustCompile(`^0?o?[0-7]?[0-7][0-7][2367]$|^(?:[ugo]*a|o)[+=][rwxXst]*w`)
)

// ansibleTaskKeywords are task keys that are not module names.
var ansibleTaskKeywords = map[string]bool{
	"name": true, "when": true, "tags": true, "become": true, "become_user": true, "become_method": true,
	"become_flags": true, "register": true, "loop": true, "notify": true, "vars": true, "environment": true,
	"args": true, "changed_when": true, "failed_when": true, "ignore_errors": true, "ignore_unreachable": true,
	"no_log": true, "delegate_to": true, "delegate_facts": true, "run_once": true, "until": true, "retries": true,
	"delay": true, "listen": true, "async": true, "poll": true, "check_mode": true, "diff": true,
	"connection": true, "any_errors_fatal": true, "block": true, "rescue": true, "always": true,
	"loop_control": true, "throttle": true, "module_defaults": true, "collections": true, "remote_user": true,
	"port": true, "timeout": true, "debugger": true, "local_action": true, "action": true, "no_proxy": true,
}

var (
	ansiblePackageModules = moduleSet("package", "apt", "yum", "dnf", "dnf5", "zypper", "apk", "pacman", "pip", "npm",
		"gem", "homebrew", "brew", "pkgng", "openbsd_pkg", "win_chocolatey", "win_package", "snap", "flatpak")
	ansibleServiceModules = moduleSet("service", "systemd", "systemd_service", "sysvinit", "win_service", "launchd", "supervisorctl")
	ansibleShellModules   = moduleSet("shell", "command", "raw", "win_shell", "win_command", "script")
	// ansibleKnownModules is enough to tell a task file from other YAML lists.
	ansibleKnownModules = moduleSet("copy", "template", "file", "lineinfile", "blockinfile", "replace", "debug", "set_fact",
		"include_vars", "include_tasks", "import_tasks", "include_role", "import_role", "import_playbook", "fail", "assert",
		"stat", "find", "user", "group", "cron", "git", "get_url", "uri", "unarchive", "wait_for", "pause", "meta",
		"setup", "gather_facts", "reboot", "hostname", "mount", "sysctl", "firewalld", "ufw", "iptables", "docker_container",
		"mysql_db", "mysql_user", "postgresql_db", "postgresql_user", "authorized_key", "known_hosts", "synchronize",
		"add_host", "group_by", "slurp", "fetch", "tempfile", "archive", "apt_repository", "apt_key", "yum_repository",
		"rpm_key", "k8s", "helm", "ec2_instance", "win_copy", "win_file", "win_feature", "win_regedit", "win_reboot",
	).add(ansiblePackageModules).add(ansibleServiceModules).add(ansibleShellModules)
)

// moduleSet holds module short names; lookups strip the collection prefix.
type moduleNames map[string]bool

func moduleSet(names ...string) moduleNames {
	m := moduleNames{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func (m moduleNames) add(o moduleNames) moduleNames {
	for k := range o {
		m[k] = true
	}
	return m
}

// has reports whether module (short or fully qualified) is in the set.
func (m moduleNames) has(module string) bool { return m[shortModule(module)] }

func shortModule(module string) string {
	return module[strings.LastIndexByte(module, '.')+1:]
}

// ansibleState collects what must be resolved once every file is read.
type ansibleState struct {
	roleMain map[string]string // role name -> its tasks/main file
	uses     []roleUse
}

type roleUse struct{ entry, role string }

func (b *builder) ansibleSt() *ansibleState {
	if b.ansible == nil {
		b.ansible = &ansibleState{roleMain: map[string]string{}}
	}
	return b.ansible
}

// isAnsibleConfig is ansible.cfg, which is INI and lists settings only.
func isAnsibleConfig(name string) bool { return name == "ansible.cfg" }

// isAnsibleINI is an INI inventory by name: hosts, inventory, *.ini under inventory dirs.
func isAnsibleINI(rel string) bool {
	base := path.Base(rel)
	ext := path.Ext(base)
	return (ext == "" || ext == ".ini") && inventoryPath.MatchString(rel)
}

// ansibleYAML indexes a YAML file as Ansible when its path or content says
// so and reports false otherwise (the caller then reads it as plain YAML).
func (b *builder) ansibleYAML(rel string, content []byte) bool {
	pathKind := ansiblePathKind(rel)
	dec := yaml.NewDecoder(bytes.NewReader(content))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false
		}
		docs = append(docs, &doc)
	}
	if len(docs) == 0 {
		return false
	}
	root := docRoot(docs[0])
	kind := pathKind
	switch {
	case root == nil:
		return false
	case kind == "" || kind == "inventory" || kind == "requirements":
		kind = ansibleContentKind(root, kind)
	}
	if kind == "" {
		return false
	}

	lines := bytes.Count(content, []byte("\n")) + 1
	fileID := b.id(model.KindConfig, rel)
	file := b.g.AddNode(model.Node{
		ID: fileID, Kind: model.KindConfig, Repo: b.repo, Name: baseName(rel), File: rel, Language: ansibleLanguage,
		StartLine: 1, EndLine: lines, LOC: lines, Bytes: len(content),
	})
	b.g.AddEdge(model.Edge{From: model.NodeID(b.repo, model.KindRepository, b.repo), To: fileID, Kind: model.EdgeContains})

	a := &ansibleFile{b: b, rel: rel, fileID: fileID}
	switch kind {
	case "playbook":
		for _, doc := range docs {
			a.playbook(docRoot(doc))
		}
	case "tasks", "handlers":
		a.tasks(fileID, root, kind == "handlers")
		if m := roleMainPath.FindStringSubmatch(rel); m != nil {
			b.ansibleSt().roleMain[m[2]] = rel
			a.entry(fileID, "role "+m[2], "role", 1, lines, "role:"+m[2])
		}
	case "vars":
		a.vars(fileID, root)
	case "meta":
		a.meta(fileID, root)
	case "inventory":
		a.inventory(root)
	case "requirements":
		a.requirements(fileID, root)
	}
	file.Summary = redact.Summary(fmt.Sprintf("Ansible %s", kind), 120)
	if a.summary != "" {
		file.Summary = redact.Summary(fmt.Sprintf("Ansible %s: %s", kind, a.summary), 240)
	}
	return true
}

// ansiblePathKind is what the path says: tasks, handlers, vars, meta, or a
// hint ("inventory", "requirements") that the content must confirm.
func ansiblePathKind(rel string) string {
	if m := roleFilePath.FindStringSubmatch(rel); m != nil {
		switch m[3] {
		case "tasks":
			return "tasks"
		case "handlers":
			return "handlers"
		case "meta":
			return "meta"
		default:
			return "vars"
		}
	}
	if varsFilePath.MatchString(rel) {
		return "vars"
	}
	base := path.Base(rel)
	if base == "requirements.yml" || base == "requirements.yaml" {
		return "requirements"
	}
	if inventoryPath.MatchString(rel) {
		return "inventory"
	}
	return ""
}

// ansibleContentKind decides from the document alone. A hint of inventory
// or requirements stays unless the content disagrees.
func ansibleContentKind(root *yaml.Node, hint string) string {
	switch root.Kind {
	case yaml.SequenceNode:
		if hint == "requirements" {
			return "requirements" // legacy list of roles
		}
		if isPlaybook(root) {
			return "playbook"
		}
		if isTaskList(root) {
			return "tasks"
		}
	case yaml.MappingNode:
		switch hint {
		case "requirements":
			if mapGet(root, "collections") != nil || mapGet(root, "roles") != nil {
				return "requirements"
			}
		case "inventory":
			if isInventory(root) {
				return "inventory"
			}
		}
	}
	return ""
}

func isPlaybook(seq *yaml.Node) bool {
	for _, item := range seq.Content {
		if item.Kind != yaml.MappingNode {
			return false
		}
		if mapGet(item, "import_playbook") != nil {
			continue
		}
		if mapGet(item, "hosts") == nil {
			return false
		}
		if !slices.ContainsFunc([]string{"tasks", "roles", "pre_tasks", "post_tasks", "handlers"}, func(k string) bool { return mapGet(item, k) != nil }) {
			return false
		}
	}
	return len(seq.Content) > 0
}

func isTaskList(seq *yaml.Node) bool {
	for _, item := range seq.Content {
		if item.Kind != yaml.MappingNode {
			return false
		}
		if mapGet(item, "block") != nil {
			continue
		}
		module, _ := taskModule(item)
		if module == "" || !ansibleKnownModules.has(module) && strings.Count(module, ".") < 2 {
			return false
		}
	}
	return len(seq.Content) > 0
}

// isInventory is a YAML inventory: groups holding hosts, children or vars.
func isInventory(root *yaml.Node) bool {
	if all := mapGet(root, "all"); all != nil && all.Kind == yaml.MappingNode {
		return true
	}
	groups := 0
	for i := 0; i+1 < len(root.Content); i += 2 {
		v := root.Content[i+1]
		if v.Kind != yaml.MappingNode || mapGet(v, "hosts") == nil && mapGet(v, "children") == nil && mapGet(v, "vars") == nil {
			return false
		}
		groups++
	}
	return groups > 0
}

// taskModule returns the module a task calls and its argument node.
func taskModule(task *yaml.Node) (string, *yaml.Node) {
	for i := 0; i+1 < len(task.Content); i += 2 {
		k := task.Content[i].Value
		if ansibleTaskKeywords[k] || strings.HasPrefix(k, "with_") {
			continue
		}
		return k, task.Content[i+1]
	}
	if la := mapGet(task, "local_action"); la != nil {
		if la.Kind == yaml.MappingNode {
			return scalar(mapGet(la, "module")), la
		}
		if f := strings.Fields(scalar(la)); len(f) > 0 {
			return f[0], la
		}
	}
	return "", nil
}

type ansibleFile struct {
	b       *builder
	rel     string
	fileID  string
	summary string
}

// entry adds one ConfigEntry below parent.
func (a *ansibleFile) entry(parent, name, sig string, start, end int, key string) string {
	id := a.b.id(model.KindConfigEntry, a.rel+"#"+key)
	a.b.g.AddNode(model.Node{
		ID: id, Kind: model.KindConfigEntry, Repo: a.b.repo, Name: redact.Summary(name, 160), QualifiedName: redact.Summary(name, 160),
		File: a.rel, StartLine: start, EndLine: max(start, end), Signature: sig, Language: ansibleLanguage,
	})
	a.b.g.AddEdge(model.Edge{From: parent, To: id, Kind: model.EdgeDefines})
	return id
}

func (a *ansibleFile) report(rule string, line int, detail string) {
	a.b.reportIn(a.rel, a.fileID, rule, line, detail, ansibleLanguage)
}

// lastLine is the last line a node spans.
func lastLine(n *yaml.Node) int {
	end := n.Line
	for _, c := range n.Content {
		end = max(end, lastLine(c))
	}
	return end
}

func (a *ansibleFile) playbook(seq *yaml.Node) {
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return
	}
	var plays []string
	for _, play := range seq.Content {
		if play.Kind != yaml.MappingNode {
			continue
		}
		if imp := scalar(mapGet(play, "import_playbook")); imp != "" {
			a.entry(a.fileID, "import_playbook "+imp, "playbook", play.Line, lastLine(play), fmt.Sprintf("import:%d", play.Line))
			continue
		}
		hosts := hostPattern(mapGet(play, "hosts"))
		id := a.entry(a.fileID, "play "+hosts, "play", play.Line, lastLine(play), fmt.Sprintf("play:%d", play.Line))
		plays = append(plays, hosts)
		a.varsMap(mapGet(play, "vars"))
		if roles := mapGet(play, "roles"); roles != nil && roles.Kind == yaml.SequenceNode {
			for _, r := range roles.Content {
				a.roleUse(id, r)
			}
		}
		for _, section := range []string{"pre_tasks", "tasks", "post_tasks"} {
			if t := mapGet(play, section); t != nil {
				a.tasks(id, t, false)
			}
		}
		if h := mapGet(play, "handlers"); h != nil {
			a.tasks(id, h, true)
		}
	}
	a.summary = strings.Join(plays[:min(len(plays), 6)], "; ")
}

func hostPattern(n *yaml.Node) string {
	if n == nil {
		return "(unnamed)"
	}
	if n.Kind == yaml.SequenceNode {
		var hs []string
		for _, h := range n.Content {
			hs = append(hs, h.Value)
		}
		return strings.Join(hs, ",")
	}
	return n.Value
}

// roleUse records a role a play (or include_role) pulls in.
func (a *ansibleFile) roleUse(parent string, r *yaml.Node) {
	name := scalar(r)
	if r.Kind == yaml.MappingNode {
		name = scalar(mapGet(r, "role"))
		if name == "" {
			name = scalar(mapGet(r, "name"))
		}
		a.varsMap(mapGet(r, "vars"))
	}
	if name == "" || templated(name) {
		return
	}
	id := a.entry(parent, "role "+name, "role", r.Line, lastLine(r), fmt.Sprintf("use:%d:%s", r.Line, name))
	st := a.b.ansibleSt()
	st.uses = append(st.uses, roleUse{entry: id, role: path.Base(name)})
}

func (a *ansibleFile) tasks(parent string, seq *yaml.Node, handlers bool) {
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return
	}
	for _, task := range seq.Content {
		if task.Kind == yaml.MappingNode {
			a.task(parent, task, handlers)
		}
	}
}

func (a *ansibleFile) task(parent string, task *yaml.Node, handler bool) {
	for _, section := range []string{"block", "rescue", "always"} {
		if inner := mapGet(task, section); inner != nil {
			a.tasks(parent, inner, handler)
		}
	}
	module, args := taskModule(task)
	if module == "" {
		return
	}
	title := scalar(mapGet(task, "name"))
	sig := "task"
	if handler {
		sig = "handler"
	}
	label := module
	if title != "" {
		label += " " + title
	}
	id := a.entry(parent, sig+" "+label, sig, task.Line, lastLine(task), fmt.Sprintf("%s:%d", sig, task.Line))

	a.varsMap(mapGet(task, "vars"))
	short := shortModule(module)
	switch short {
	case "include_role", "import_role":
		a.roleUse(id, args)
	}
	if ansiblePackageModules.has(module) || ansibleServiceModules.has(module) {
		kind := "package"
		if ansibleServiceModules.has(module) {
			kind = "service"
		}
		for _, name := range subjectNames(args) {
			a.entry(id, kind+" "+name, kind, task.Line, task.Line, fmt.Sprintf("%s:%d:%s", kind, task.Line, name))
		}
	}
	a.taskRules(module, args)
}

// subjectNames lists the literal package or service names of a task.
func subjectNames(args *yaml.Node) []string {
	if args == nil || args.Kind != yaml.MappingNode {
		return nil
	}
	n := mapGet(args, "name")
	if n == nil {
		n = mapGet(args, "pkg")
	}
	var out []string
	add := func(v string) {
		for _, s := range strings.Split(v, ",") {
			s = strings.TrimSpace(s)
			if s != "" && !templated(s) && len(out) < maxAnsibleSubjects {
				out = append(out, s)
			}
		}
	}
	switch {
	case n == nil:
	case n.Kind == yaml.SequenceNode:
		for _, c := range n.Content {
			add(c.Value)
		}
	default:
		add(n.Value)
	}
	return out
}

func (a *ansibleFile) taskRules(module string, args *yaml.Node) {
	short := shortModule(module)
	if args != nil && args.Kind == yaml.MappingNode {
		a.secretsIn(args)
		if v := mapGet(args, "validate_certs"); v != nil && falsy(v) {
			a.report("ansible-tls-verify-off", v.Line, module+" turns validate_certs off")
		}
		if m := mapGet(args, "mode"); m != nil && m.Kind == yaml.ScalarNode && worldWritable.MatchString(strings.TrimSpace(m.Value)) {
			a.report("ansible-world-writable", m.Line, module+" sets mode "+redact.Summary(m.Value, 12))
		}
		if st := mapGet(args, "state"); st != nil && st.Value == "latest" && ansiblePackageModules.has(module) {
			a.report("ansible-package-latest", st.Line, module+" installs with state latest")
		}
	}
	if !ansibleShellModules.has(module) {
		return
	}
	cmd, line := shellCommand(args)
	if cmd == "" {
		return
	}
	if pipeToShell.MatchString(cmd) {
		a.report("ansible-curl-pipe-shell", line, short+" pipes a download into a shell")
	}
	if insecureFlag.MatchString(cmd) {
		a.report("ansible-tls-verify-off", line, short+" downloads with certificate checks off")
	}
	if chmodWorld.MatchString(cmd) {
		a.report("ansible-world-writable", line, short+" makes a path writable by everyone")
	}
	if short == "shell" || short == "raw" || short == "win_shell" {
		for _, m := range ansibleTemplate.FindAllString(cmd, -1) {
			if !strings.Contains(m, "quote") && !strings.Contains(m, "| int") && !strings.Contains(m, "|int") {
				a.report("ansible-shell-injection", line, short+" interpolates a variable without | quote")
				break
			}
		}
	}
}

// shellCommand returns the command line of a shell-like task.
func shellCommand(args *yaml.Node) (string, int) {
	switch {
	case args == nil:
	case args.Kind == yaml.ScalarNode:
		return args.Value, args.Line
	case args.Kind == yaml.MappingNode:
		for _, k := range []string{"cmd", "_raw_params"} {
			if v := mapGet(args, k); v != nil {
				return v.Value, v.Line
			}
		}
	}
	return "", 0
}

func falsy(n *yaml.Node) bool {
	switch strings.ToLower(n.Value) {
	case "false", "no", "off", "0":
		return n.Kind == yaml.ScalarNode
	}
	return false
}

// varsMap reads a vars: block for literal secrets and names nothing else.
func (a *ansibleFile) varsMap(m *yaml.Node) {
	if m != nil && m.Kind == yaml.MappingNode {
		a.secretsIn(m)
	}
}

// secretsIn flags secret-named keys with a literal, unvaulted value.
func (a *ansibleFile) secretsIn(m *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		if v.Kind != yaml.ScalarNode || v.Tag == "!vault" || v.Tag == "!!bool" || v.Tag == "!!int" || v.Tag == "!!null" {
			continue
		}
		if ansibleSecretKey.MatchString(k.Value) && !ansibleNotSecret.MatchString(k.Value) && looksSecret(v.Value) {
			a.report("ansible-secret-literal", v.Line, fmt.Sprintf("%s holds a %d-character literal", redact.Summary(k.Value, 60), len(v.Value)))
		}
	}
}

func (a *ansibleFile) vars(parent string, root *yaml.Node) {
	if root.Kind != yaml.MappingNode {
		return
	}
	a.secretsIn(root)
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		end := lastLine(v)
		a.entry(parent, "var "+k.Value, "var", k.Line, end, "var:"+k.Value)
	}
}

func (a *ansibleFile) meta(parent string, root *yaml.Node) {
	deps := mapGet(root, "dependencies")
	if deps == nil || deps.Kind != yaml.SequenceNode {
		return
	}
	for _, d := range deps.Content {
		a.roleUse(parent, d)
	}
}

func (a *ansibleFile) requirements(parent string, root *yaml.Node) {
	var items []*yaml.Node
	switch root.Kind {
	case yaml.SequenceNode:
		items = root.Content
	case yaml.MappingNode:
		for _, key := range []string{"collections", "roles"} {
			if s := mapGet(root, key); s != nil && s.Kind == yaml.SequenceNode {
				items = append(items, s.Content...)
			}
		}
	}
	for _, it := range items {
		name := scalar(it)
		if it.Kind == yaml.MappingNode {
			name = scalar(mapGet(it, "name"))
			if name == "" {
				name = scalar(mapGet(it, "src"))
			}
		}
		if name != "" && !templated(name) {
			a.entry(parent, "collection "+redact.Summary(name, 100), "collection", it.Line, lastLine(it), fmt.Sprintf("req:%d", it.Line))
		}
	}
}

// inventory walks a YAML inventory: groups, their hosts and children.
func (a *ansibleFile) inventory(root *yaml.Node) {
	var walkGroup func(parent, name string, g *yaml.Node, depth int)
	walkGroup = func(parent, name string, g *yaml.Node, depth int) {
		if g == nil || g.Kind != yaml.MappingNode || depth > 8 {
			return
		}
		id := parent
		if name != "all" {
			id = a.entry(parent, "group "+name, "group", g.Line, lastLine(g), "group:"+name)
		}
		if hosts := mapGet(g, "hosts"); hosts != nil && hosts.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(hosts.Content); i += 2 {
				h := hosts.Content[i]
				a.entry(id, "host "+h.Value, "host", h.Line, lastLine(hosts.Content[i+1]), "host:"+name+":"+h.Value)
				a.varsMap(hosts.Content[i+1])
			}
		}
		a.varsMap(mapGet(g, "vars"))
		if ch := mapGet(g, "children"); ch != nil && ch.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(ch.Content); i += 2 {
				walkGroup(id, ch.Content[i].Value, ch.Content[i+1], depth+1)
			}
		}
	}
	if all := mapGet(root, "all"); all != nil {
		walkGroup(a.fileID, "all", all, 0)
		return
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		walkGroup(a.fileID, root.Content[i].Value, root.Content[i+1], 0)
	}
}

// ansibleINI indexes an INI inventory or ansible.cfg. Only group and host
// names are kept; a literal password in a vars section is reported.
func (b *builder) ansibleINI(rel string, content []byte) {
	lines := strings.Split(string(content), "\n")
	fileID := b.id(model.KindConfig, rel)
	file := b.g.AddNode(model.Node{
		ID: fileID, Kind: model.KindConfig, Repo: b.repo, Name: baseName(rel), File: rel, Language: ansibleLanguage,
		StartLine: 1, EndLine: len(lines), LOC: len(lines), Bytes: len(content), Summary: "Ansible settings",
	})
	b.g.AddEdge(model.Edge{From: model.NodeID(b.repo, model.KindRepository, b.repo), To: fileID, Kind: model.EdgeContains})
	if isAnsibleConfig(path.Base(rel)) {
		return
	}
	a := &ansibleFile{b: b, rel: rel, fileID: fileID}
	file.Summary = "Ansible inventory"
	group, section, groupID := "ungrouped", "hosts", fileID
	seen := map[string]bool{}
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' && strings.HasSuffix(line, "]") {
			name, kind, _ := strings.Cut(line[1:len(line)-1], ":")
			group, section = name, "hosts"
			switch kind {
			case "vars":
				section = "vars"
			case "children":
				section = "children"
			}
			if !seen[group] {
				seen[group] = true
				groupID = a.entry(fileID, "group "+group, "group", i+1, i+1, "group:"+group)
			}
			continue
		}
		switch section {
		case "vars":
			if k, v, ok := strings.Cut(line, "="); ok {
				k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
				if ansibleSecretKey.MatchString(k) && !ansibleNotSecret.MatchString(k) && looksSecret(v) {
					a.report("ansible-secret-literal", i+1, fmt.Sprintf("%s holds a %d-character literal", redact.Summary(k, 60), len(v)))
				}
			}
		case "children":
			if f := strings.Fields(line); len(f) > 0 {
				a.entry(groupID, "group "+f[0], "group", i+1, i+1, fmt.Sprintf("child:%s:%s", group, f[0]))
			}
		default:
			f := strings.Fields(line)
			if len(f) == 0 {
				continue
			}
			a.entry(groupID, "host "+f[0], "host", i+1, i+1, fmt.Sprintf("host:%s:%s", group, f[0]))
			for _, kv := range f[1:] {
				if k, v, ok := strings.Cut(kv, "="); ok && ansibleSecretKey.MatchString(k) && !ansibleNotSecret.MatchString(k) && looksSecret(strings.Trim(v, `"'`)) {
					a.report("ansible-secret-literal", i+1, fmt.Sprintf("%s holds a literal", redact.Summary(k, 60)))
				}
			}
		}
	}
}

// resolveAnsible links each role a play uses to that role's tasks/main file
// when the repository holds it.
func (b *builder) resolveAnsible() {
	if b.ansible == nil {
		return
	}
	for _, u := range b.ansible.uses {
		if file, ok := b.ansible.roleMain[u.role]; ok {
			b.g.AddEdge(model.Edge{From: u.entry, To: b.id(model.KindConfig, file), Kind: model.EdgeReferences})
		}
	}
}
