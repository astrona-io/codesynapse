package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "embed"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/app"
)

// `codesynapse install` and `uninstall` put the Helm chart on a cluster in
// one step, like `cilium install`: the namespace, a Secret with random
// credentials (made once, never overwritten), and the chart, waiting until
// every service answers. They drive the helm and kubectl already on the
// machine; nothing new is linked into the CLI. Every command names its
// context explicitly, and the target is confirmed before anything changes.

// defaultChart is where releases publish the chart (docs/operations/releasing.md).
const defaultChart = "oci://ghcr.io/astrona-io/charts/codesynapse"

//go:embed installvalues/local.yaml
var localValues []byte

// secretKeys are the credentials the chart reads from its Secret, and how
// each is made: random bytes, as hex for tokens and passwords and as base64
// for the 32-byte key. NebulaGraph refuses passwords over 24 characters, so
// its get 12 bytes.
var secretKeys = []struct {
	name   string
	bytes  int
	base64 bool
}{
	{"CODESYNAPSE_API_TOKEN", 24, false}, {"CODESYNAPSE_MCP_TOKEN", 24, false}, {"POSTGRES_PASSWORD", 24, false},
	{"NEBULA_PASSWORD", 12, false}, {"NEBULA_API_PASSWORD", 12, false}, {"NEBULA_WORKER_PASSWORD", 12, false},
	{"NEBULA_MCP_PASSWORD", 12, false}, {"METRICS_PASSWORD", 24, false}, {"SECRET_KEY", 32, true},
}

// k8sName is a DNS-1123 label: what a namespace or release may be called.
var k8sName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

type installOptions struct {
	EC        *app.Env
	uninstall bool
	context   string
	namespace string
	release   string
	chart     string
	version   string
	local     bool
	values    []string
	sets      []string
	yes       bool
	dryRun    bool
	timeout   time.Duration
	keepData  bool
	status    bool
	wait      bool
	test      bool
	// run runs helm and kubectl; tests replace it. checkTools is set when
	// it is the real one, so the tools must be on PATH.
	run        func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
	checkTools bool
}

func (o *installOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	f.StringVar(&o.context, "context", "", "Kubernetes context to install into (default: kubectl's current context, confirmed before anything changes)")
	f.StringVarP(&o.namespace, "namespace", "n", "codesynapse", "Namespace")
	f.StringVar(&o.release, "release", "codesynapse", "Helm release name")
	f.BoolVarP(&o.yes, "yes", "y", false, "Do not ask for confirmation")
	f.DurationVar(&o.timeout, "timeout", 15*time.Minute, "How long to wait")
	if o.status {
		f.BoolVar(&o.wait, "wait", false, "Wait until every component is ready (up to --timeout)")
		f.BoolVar(&o.test, "test", false, "Also run the chart's tests: every service answers")
		return nil
	}
	if o.uninstall {
		f.BoolVar(&o.keepData, "keep-data", true, "Keep the Secret and the stores' volumes (set false to delete the namespace with everything in it)")
		return nil
	}
	f.StringVar(&o.chart, "chart", defaultChart, "Chart to install: the published OCI chart, or a path such as deploy/helm/codesynapse")
	f.StringVar(&o.version, "version", "", "Chart version (default: this CLI's version)")
	f.BoolVar(&o.local, "local", false, "Laptop preset: small resource requests and plain HTTP (kind, k3d, Docker Desktop, minikube)")
	f.StringSliceVarP(&o.values, "values", "f", nil, "Extra Helm values files")
	f.StringArrayVar(&o.sets, "set", nil, "Extra Helm values (key=value)")
	f.BoolVar(&o.dryRun, "dry-run", false, "Show what would run and render the chart without installing")
	return nil
}

func (o *installOptions) Complete(context.Context, *app.Env) error {
	if o.run == nil {
		o.run, o.checkTools = runTool, true
	}
	return nil
}

func (o *installOptions) Validate(context.Context, *app.Env) error {
	if !k8sName.MatchString(o.namespace) || !k8sName.MatchString(o.release) {
		return errors.New("--namespace and --release must be lower-case DNS labels")
	}
	if !o.yes && !o.dryRun && !o.status && !o.EC.IsTerminal {
		return errors.New("not a terminal: pass --yes to proceed without confirmation")
	}
	return nil
}

// runTool runs helm or kubectl without a shell; stdin carries anything
// secret, never the arguments.
func runTool(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed tool names, arguments without a shell
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, firstArg(args), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func (o *installOptions) Run(ctx context.Context, _ *app.Env) error {
	ctx = app.Ctx(ctx, o.EC)
	for _, tool := range []string{"helm", "kubectl"} {
		if _, err := exec.LookPath(tool); err != nil && o.checkTools {
			return fmt.Errorf("%s is not installed: see https://helm.sh/docs/intro/install/ and https://kubernetes.io/docs/tasks/tools/", tool)
		}
	}
	if o.context == "" {
		out, err := o.run(ctx, nil, "kubectl", "config", "current-context")
		if err != nil || strings.TrimSpace(string(out)) == "" {
			return errors.New("no current Kubernetes context: pass --context")
		}
		o.context = strings.TrimSpace(string(out))
	}
	switch {
	case o.status:
		return o.runStatus(ctx)
	case o.uninstall:
		return o.runUninstall(ctx)
	}
	return o.runInstall(ctx)
}

func (o *installOptions) runInstall(ctx context.Context) error {
	version := o.version
	if version == "" && !isPath(o.chart) {
		if version = strings.TrimPrefix(version, "v"); version == "" || version == "dev" {
			return errors.New("this is a development build: pass --version (a released chart version) or --chart deploy/helm/codesynapse")
		}
	}
	w := o.EC.Stderr
	fmt.Fprintf(w, "Installing CodeSynapse %s into context %q, namespace %q (release %q).\n", cmpOr(version, "from "+o.chart), o.context, o.namespace, o.release) //nolint:errcheck // terminal
	if !o.yes && !o.dryRun && !askYes(w, o.EC.Stdin, "Continue?") {
		return errors.New("cancelled")
	}
	k := []string{"--context", o.context}
	if !o.dryRun {
		if _, err := o.run(ctx, nil, "kubectl", append(k, "get", "namespace", o.namespace)...); err != nil {
			if _, err := o.run(ctx, nil, "kubectl", append(k, "create", "namespace", o.namespace)...); err != nil {
				return err
			}
			fmt.Fprintf(w, "  ✓ namespace %s\n", o.namespace) //nolint:errcheck // terminal
		}
		created, err := o.ensureSecret(ctx, k)
		if err != nil {
			return err
		}
		if created {
			fmt.Fprintln(w, "  ✓ secret codesynapse with random credentials (kept on reinstall; back it up: losing SECRET_KEY loses sealed tokens)") //nolint:errcheck // terminal
		} else {
			fmt.Fprintln(w, "  ✓ secret codesynapse already there, kept") //nolint:errcheck // terminal
		}
	}

	// The same values either way; a dry run renders instead of installing.
	args := []string{"upgrade", "--install", o.release, o.chart, "--wait", "--timeout", o.timeout.String()}
	if o.dryRun {
		args = []string{"template", o.release, o.chart}
	}
	args = append(args, "--kube-context", o.context, "--namespace", o.namespace, "--set", "global.existingSecret=codesynapse")
	if version != "" {
		args = append(args, "--version", version)
	}
	if o.local {
		f, err := os.CreateTemp("", "codesynapse-local-*.yaml")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name()) //nolint:errcheck // temporary
		if _, err := f.Write(localValues); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		args = append(args, "--values", f.Name())
	}
	for _, v := range o.values {
		args = append(args, "--values", v)
	}
	for _, s := range o.sets {
		args = append(args, "--set", s)
	}
	if o.dryRun {
		fmt.Fprintf(w, "Would run: helm %s\n", strings.Join(args, " ")) //nolint:errcheck // terminal
		out, err := o.run(ctx, nil, "helm", args...)
		if err == nil {
			_, err = o.EC.Stdout.Write(out)
		}
		return err
	}
	fmt.Fprintln(w, "  … installing the chart and waiting for every service (a few minutes the first time)") //nolint:errcheck // terminal
	if _, err := o.run(ctx, nil, "helm", args...); err != nil {
		return err
	}
	fmt.Fprintln(w, "  ✓ chart installed") //nolint:errcheck // terminal
	if _, err := o.run(ctx, nil, "helm", "test", o.release, "--kube-context", o.context, "--namespace", o.namespace); err != nil {
		fmt.Fprintf(w, "  ! helm test failed: %v\n", err) //nolint:errcheck // terminal
	} else {
		fmt.Fprintln(w, "  ✓ every service answers") //nolint:errcheck // terminal
	}
	ns, ctxFlag := o.namespace, "--context "+o.context
	if o.release != "codesynapse" {
		ctxFlag += " --release " + o.release
	}
	_, _ = fmt.Fprintf(w, `
Next:
  kubectl %[1]s -n %[2]s port-forward svc/%[3]s-web 3000:3000    # dashboard: http://localhost:3000
  kubectl %[1]s -n %[2]s port-forward svc/%[3]s-mcp 8090:8090    # MCP for agents
The first account you sign up becomes the installation's root account and hands over to an organisation owner.
Then connect Claude Code: create a personal key in the dashboard and run
  codesynapse mcp install --url http://localhost:8090/mcp
Check it any time with: codesynapse status %[1]s -n %[2]s
Remove it again with:   codesynapse uninstall %[1]s -n %[2]s
`, ctxFlag, ns, o.release) //nolint:errcheck // terminal
	return nil
}

// ensureSecret makes the credentials Secret once. Its content goes to
// kubectl on stdin, never in arguments other processes can read, and an
// existing value is never replaced: the stores keep their passwords. A Secret
// made by an older CLI only gets the keys it lacks.
func (o *installOptions) ensureSecret(ctx context.Context, k []string) (bool, error) {
	ns := append(append([]string{}, k...), "--namespace", o.namespace)
	if names, err := o.run(ctx, nil, "kubectl", append(ns, "get", "secret", "codesynapse",
		"--output", `go-template={{range $k, $v := .data}}{{$k}}{{"\n"}}{{end}}`)...); err == nil {
		have := map[string]bool{}
		for _, n := range strings.Fields(string(names)) {
			have[n] = true
		}
		data, err := newSecretValues(func(name string) bool { return !have[name] })
		if err != nil || len(data) == 0 {
			return false, err
		}
		patch, err := json.Marshal(map[string]any{"stringData": data})
		if err != nil {
			return false, err
		}
		_, err = o.run(ctx, patch, "kubectl", append(ns, "patch", "secret", "codesynapse", "--type", "merge", "--patch-file", "/dev/stdin")...)
		return false, err
	}
	data, err := newSecretValues(func(string) bool { return true })
	if err != nil {
		return false, err
	}
	manifest, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": "codesynapse", "namespace": o.namespace, "labels": map[string]string{"app.kubernetes.io/part-of": "codesynapse"}},
		"stringData": data,
	})
	if err != nil {
		return false, err
	}
	_, err = o.run(ctx, manifest, "kubectl", append(ns, "create", "-f", "-")...)
	return err == nil, err
}

// newSecretValues makes a random value for each of secretKeys that want
// selects.
func newSecretValues(want func(name string) bool) (map[string]string, error) {
	data := map[string]string{}
	for _, key := range secretKeys {
		if !want(key.name) {
			continue
		}
		b := make([]byte, key.bytes)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		v := hex.EncodeToString(b)
		if key.base64 {
			v = base64.StdEncoding.EncodeToString(b)
		}
		data[key.name] = v
	}
	return data, nil
}

func (o *installOptions) runUninstall(ctx context.Context) error {
	w := o.EC.Stderr
	what := "the release (the Secret and the stores' volumes stay)"
	if !o.keepData {
		what = "the release and the whole namespace with its data"
	}
	fmt.Fprintf(w, "Removing %s: release %q in context %q, namespace %q.\n", what, o.release, o.context, o.namespace) //nolint:errcheck // terminal
	if !o.yes && !askYes(w, o.EC.Stdin, "Continue?") {
		return errors.New("cancelled")
	}
	if _, err := o.run(ctx, nil, "helm", "uninstall", o.release, "--kube-context", o.context, "--namespace", o.namespace, "--wait", "--timeout", o.timeout.String()); err != nil {
		return err
	}
	fmt.Fprintln(w, "  ✓ release removed") //nolint:errcheck // terminal
	if !o.keepData {
		if _, err := o.run(ctx, nil, "kubectl", "--context", o.context, "delete", "namespace", o.namespace, "--wait=true", "--timeout", o.timeout.String()); err != nil {
			return err
		}
		fmt.Fprintf(w, "  ✓ namespace %s deleted\n", o.namespace) //nolint:errcheck // terminal
	}
	return nil
}

func askYes(w io.Writer, in io.Reader, q string) bool {
	fmt.Fprintf(w, "%s [y/N] ", q)                  //nolint:errcheck // terminal
	line, _ := bufio.NewReader(in).ReadString('\n') //nolint:errcheck // EOF means no
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

func isPath(chart string) bool {
	if strings.HasPrefix(chart, "oci://") || strings.Contains(chart, "://") {
		return false
	}
	_, err := os.Stat(filepath.Clean(chart))
	return err == nil
}

func installCmd(ec *app.Env) *cobra.Command {
	return app.NewSubCommand("install", &installOptions{EC: ec}, ec).
		WithShortDesc("Install CodeSynapse on a Kubernetes cluster with its Helm chart").
		WithLongDesc("Creates the namespace, a Secret with random credentials (once; never replaced) and installs the chart, then waits until every service answers. Uses the helm and kubectl on this machine and asks before changing the cluster.").
		WithExample("  codesynapse install --local                       # a laptop cluster (kind, k3d, Docker Desktop)\n  codesynapse install --context prod -f values.yaml --set ingress.enabled=true\n  codesynapse install --chart deploy/helm/codesynapse --local   # from a checkout").
		WithGroupID(app.GroupBase).
		WithNoArgs().
		Build()
}

func uninstallCmd(ec *app.Env) *cobra.Command {
	return app.NewSubCommand("uninstall", &installOptions{EC: ec, uninstall: true}, ec).
		WithShortDesc("Remove CodeSynapse installed with `codesynapse install`").
		WithExample("  codesynapse uninstall                      # the release; data stays\n  codesynapse uninstall --keep-data=false    # and the namespace with all data").
		WithGroupID(app.GroupBase).
		WithNoArgs().
		Build()
}

// componentStatus is one Deployment or StatefulSet of the release.
type componentStatus struct {
	Kind, Name     string
	Ready, Desired int
}

// runStatus reports the release and every component's ready replicas, and
// fails when something is not ready (after --wait, when given).
func (o *installOptions) runStatus(ctx context.Context) error {
	w := o.EC.Stdout
	out, err := o.run(ctx, nil, "helm", "list", "--kube-context", o.context, "--namespace", o.namespace, "--filter", "^"+regexp.QuoteMeta(o.release)+"$", "-o", "json")
	var rels []struct {
		Revision   string `json:"revision"`
		Status     string `json:"status"`
		Chart      string `json:"chart"`
		AppVersion string `json:"app_version"`
	}
	if err != nil || json.Unmarshal(out, &rels) != nil || len(rels) == 0 {
		return fmt.Errorf("release %q not found in context %q, namespace %q: install it with `codesynapse install`", o.release, o.context, o.namespace)
	}
	rel := rels[0]
	fmt.Fprintf(w, "CodeSynapse %s (%s), release %q revision %s: %s\ncontext %q, namespace %q\n\n", //nolint:errcheck // terminal
		cmpOr(rel.AppVersion, "?"), rel.Chart, o.release, rel.Revision, rel.Status, o.context, o.namespace)

	deadline := time.Now().Add(o.timeout)
	var comps []componentStatus
	for {
		if comps, err = o.components(ctx); err != nil {
			return err
		}
		if !o.wait || allReady(comps) || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	notReady := 0
	for _, c := range comps {
		mark := "✓"
		if c.Ready < c.Desired || c.Desired == 0 {
			mark = "✗"
			notReady++
		}
		fmt.Fprintf(w, "  %s %-28s %-12s %d/%d ready\n", mark, c.Name, c.Kind, c.Ready, c.Desired) //nolint:errcheck // terminal
	}
	if len(comps) == 0 {
		return errors.New("no components found for the release")
	}
	if o.test {
		if _, err := o.run(ctx, nil, "helm", "test", o.release, "--kube-context", o.context, "--namespace", o.namespace); err != nil {
			fmt.Fprintf(w, "\n  ✗ helm test: %v\n", err) //nolint:errcheck // terminal
			return errStatus
		}
		fmt.Fprintln(w, "\n  ✓ helm test: every service answers") //nolint:errcheck // terminal
	}
	if notReady > 0 || rel.Status != "deployed" {
		fmt.Fprintf(w, "\n%d component(s) not ready. Look closer with: kubectl --context %s -n %s get pods\n", notReady, o.context, o.namespace) //nolint:errcheck // terminal
		return errStatus
	}
	fmt.Fprintln(w, "\nEverything is running.") //nolint:errcheck // terminal
	return nil
}

// errStatus makes `codesynapse status` exit 1 for scripts; the report
// above says why.
var errStatus = errors.New("codesynapse is not fully running")

func allReady(cs []componentStatus) bool {
	for _, c := range cs {
		if c.Ready < c.Desired || c.Desired == 0 {
			return false
		}
	}
	return len(cs) > 0
}

// components lists the release's Deployments and StatefulSets with their
// ready and desired replicas.
func (o *installOptions) components(ctx context.Context) ([]componentStatus, error) {
	out, err := o.run(ctx, nil, "kubectl", "--context", o.context, "--namespace", o.namespace, "get", "deployments,statefulsets",
		"-l", "app.kubernetes.io/instance="+o.release, "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int `json:"replicas"`
			} `json:"spec"`
			Status struct {
				ReadyReplicas int `json:"readyReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("reading the components: %w", err)
	}
	cs := make([]componentStatus, 0, len(list.Items))
	for _, it := range list.Items {
		desired := 1
		if it.Spec.Replicas != nil {
			desired = *it.Spec.Replicas
		}
		cs = append(cs, componentStatus{Kind: it.Kind, Name: it.Metadata.Name, Ready: it.Status.ReadyReplicas, Desired: desired})
	}
	return cs, nil
}

func statusCmd(ec *app.Env) *cobra.Command {
	return app.NewSubCommand("status", &installOptions{EC: ec, status: true}, ec).
		WithShortDesc("Check that CodeSynapse is running on the cluster").
		WithLongDesc("Shows the Helm release and every component's ready replicas (API, workers, MCP, web and the stores). Exits 1 when something is not ready, for scripts. --wait waits for it, --test also runs the chart's tests.").
		WithExample("  codesynapse status\n  codesynapse status --context kind-dev --wait --test").
		WithGroupID(app.GroupBase).
		WithNoArgs().
		Build()
}
