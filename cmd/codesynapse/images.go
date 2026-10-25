package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/imagescan"
)

// `codesynapse images upload` sends the Trivy or Grype JSON report of the
// images a CI run built to the API, which keeps the parsed findings for the
// organisation (services/api/internal/api/imagescan.go). Reports hold package
// names, versions and advisory IDs, never source.

type imagesOptions struct {
	EC     *app.Env
	upload bool
	repo   string
	apiURL string
	root   string
	report string
}

func (o *imagesOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	f.StringVar(&o.repo, "repo", "", "Repository name in CodeSynapse (default: matched from the checkout's origin remote)")
	f.StringVar(&o.apiURL, "api-url", app.Getenv("CODESYNAPSE_API_URL", "http://127.0.0.1:8080"), "CodeSynapse API")
	return nil
}

func (o *imagesOptions) Complete(_ context.Context, _ *app.Env) error {
	o.root = "."
	abs, err := filepath.Abs(o.root)
	if err != nil {
		return err
	}
	o.root = abs
	if o.upload && len(o.EC.Args) > 0 {
		o.report = o.EC.Args[0]
	}
	return nil
}

func (o *imagesOptions) Validate(context.Context, *app.Env) error {
	if o.upload && o.report == "" {
		return errors.New("name the report: codesynapse images upload trivy.json")
	}
	return nil
}

func (o *imagesOptions) Run(ctx context.Context, _ *app.Env) error {
	ctx = app.Ctx(ctx, o.EC)
	c := newAPIClient(o.apiURL)
	c.http.Timeout = runtimeUploadTimeout
	repo := o.repo
	if repo == "" {
		var err error
		if repo, err = c.resolveRepo(ctx, o.root); err != nil {
			return err
		}
	}
	path := "/api/v1/repos/" + url.PathEscape(repo) + "/images"
	var sum imageSummary
	if !o.upload {
		if err := c.do(ctx, http.MethodGet, path, nil, &sum); err != nil {
			return err
		}
		return o.print(repo, sum)
	}
	body, err := gzipReport(o.report)
	if err != nil {
		return err
	}
	if err := c.send(ctx, http.MethodPut, path, body, &sum); err != nil {
		return err
	}
	return o.print(repo, sum)
}

// gzipReport reads the report, checking its size first, into a gzipped body.
func gzipReport(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() > imagescan.MaxReportBytes {
		return nil, fmt.Errorf("%s: %w", path, imagescan.ErrTooLarge)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // a report the user named
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// imageSummary is the API's answer (api.ImageScanSummary).
type imageSummary struct {
	UploadedAt time.Time           `json:"uploaded_at"`
	Images     []string            `json:"images"`
	Counts     map[string]int      `json:"counts"`
	Total      int                 `json:"total"`
	Truncated  bool                `json:"truncated"`
	Findings   []imagescan.Finding `json:"findings"`
}

func (o *imagesOptions) print(repo string, s imageSummary) error {
	if o.EC.Output == app.OutputJSON {
		return json.NewEncoder(o.EC.Stdout).Encode(s)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d image findings from %s (%s)\n", repo, s.Total, strings.Join(s.Images, ", "), s.UploadedAt.Format(time.RFC3339))
	for _, sev := range []string{"critical", "high", "medium", "low", "unknown"} {
		if n := s.Counts[sev]; n > 0 {
			fmt.Fprintf(&b, "  %-8s %d\n", sev, n)
		}
	}
	for _, f := range s.Findings[:min(len(s.Findings), 10)] {
		fix := "no fix yet"
		if f.Fixed != "" {
			fix = "fixed in " + f.Fixed
		}
		fmt.Fprintf(&b, "    %s %s %s@%s (%s)\n", f.Severity, f.ID, f.Package, f.Version, fix)
	}
	_, err := io.WriteString(o.EC.Stdout, b.String())
	return err
}

func imagesCmd(ec *app.Env) *cobra.Command {
	parent := &cobra.Command{Use: "images", Short: "Upload container image scans from CI", GroupID: app.GroupBase}
	parent.AddCommand(app.NewSubCommand("upload <report.json>", &imagesOptions{EC: ec, upload: true}, ec).
		WithShortDesc("Send a Trivy or Grype image report to CodeSynapse").
		WithLongDesc("Sends the JSON report of an image scan (trivy image -f json, grype -o json), gzipped, to the API with the key from CODESYNAPSE_API_TOKEN. The API keeps the packages, versions, advisory IDs and severities for your organisation, replacing the previous upload; the security report then counts them and the repository's images page lists them.").
		WithExample("  trivy image -f json -o trivy.json ghcr.io/acme/api:1.2.3\n  codesynapse images upload trivy.json --repo acme/api").
		WithMaxArgs(1).Build())
	parent.AddCommand(app.NewSubCommand("show", &imagesOptions{EC: ec}, ec).
		WithShortDesc("Show the image scan CodeSynapse has for a repository").
		WithMaxArgs(0).Build())
	return parent
}
