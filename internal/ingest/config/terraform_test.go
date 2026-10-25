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

func tfFindings(t *testing.T, content string) []string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.tf"), []byte(content), 0o600))
	g, err := Extract(context.Background(), "r", root)
	require.NoError(t, err)
	var out []string
	for _, n := range g.Nodes() {
		if n.Kind == model.KindFinding && strings.HasPrefix(n.Rule, "tf-") {
			assert.Equal(t, n.Language, "terraform")
			out = append(out, fmt.Sprintf("%s:%02d", n.Rule, n.StartLine))
		}
	}
	slices.Sort(out)
	return out
}

const tfBad = `# Everything here is wrong on purpose.
resource "aws_security_group" "ssh" {
  ingress {
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = ["10.0.0.0/8", "0.0.0.0/0"]
  }
  ingress {
    from_port   = 443
    to_port     = 443
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_s3_bucket_acl" "assets" {
  acl = "public-read"
}

resource "aws_s3_bucket_public_access_block" "assets" {
  block_public_acls = false
}

resource "aws_db_instance" "main" {
  engine              = "postgres"
  publicly_accessible = true
  password            = "Sup3rS3cretValue"
}

resource "aws_ebs_volume" "data" {
  size = 10
}

resource "google_storage_bucket_iam_member" "public" {
  member = "allUsers"
}

resource "google_compute_firewall" "db" {
  source_ranges = ["0.0.0.0/0"]
  allow {
    protocol = "tcp"
    ports    = ["5432"]
  }
}

resource "azurerm_network_security_rule" "rdp" {
  direction              = "Inbound"
  access                 = "Allow"
  source_address_prefix  = "*"
  destination_port_range = "3389"
}

variable "api_token" {
  default = "9xQ-not-a-real-one"
}
`

const tfGood = `/* Nothing here should fire. */
resource "aws_security_group" "web" {
  ingress {
    from_port   = 443
    to_port     = 443
    cidr_blocks = ["0.0.0.0/0"]
  }
  ingress {
    from_port   = 22
    to_port     = 22
    cidr_blocks = [var.admin_cidr]
  }
}

resource "aws_db_instance" "main" {
  engine              = "postgres"
  storage_encrypted   = true
  publicly_accessible = false
  password            = var.db_password
  secret_arn          = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:db"
}

resource "aws_ebs_volume" "data" {
  encrypted = true
}

resource "aws_s3_bucket" "logs" {
  bucket = "logs-${var.env}"
  acl    = "private"
}

resource "google_compute_firewall" "web" {
  source_ranges = ["0.0.0.0/0"]
  allow {
    protocol = "tcp"
    ports    = ["80", "443"]
  }
}

variable "db_password" {
  type      = string
  sensitive = true
}

locals {
  policy = <<EOT
{"Principal": "*", "password": "this is inside a heredoc"}
EOT
  token_name = "deploy-token-${var.env}"
}
`

func TestTerraformRules(t *testing.T) {
	assert.Equal(t, tfFindings(t, tfBad), []string{
		"tf-open-ingress:07", "tf-open-ingress:39", "tf-open-ingress:49",
		"tf-public-database:26",
		"tf-public-storage:17", "tf-public-storage:21", "tf-public-storage:35",
		"tf-secret-literal:27", "tf-secret-literal:54",
		"tf-unencrypted-storage:24", "tf-unencrypted-storage:30",
	})
	assert.Len(t, tfFindings(t, tfGood), 0)
}

func TestTerraformNode(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.tf"), []byte(tfGood), 0o600))
	g, err := Extract(context.Background(), "r", root)
	require.NoError(t, err)
	n, ok := g.Node(model.NodeID("r", model.KindConfig, "main.tf"))
	require.True(t, ok)
	assert.Equal(t, n.Language, "terraform")
	assert.Equal(t, n.Summary, "Terraform: 7 block(s), resources aws_db_instance, aws_ebs_volume, aws_s3_bucket, aws_security_group, google_compute_firewall")
}

func TestParseHCL(t *testing.T) {
	blocks := parseHCL("a \"x\" {\n  b = \"${c == \"d\" ? 1 : 2}\"\n  e = true # comment\n  f = [\n    \"g\",\n  ]\n  h { i = 1 }\n}\n")
	require.Len(t, blocks, 1)
	b := blocks[0]
	assert.Equal(t, b.Labels, []string{"x"})
	assert.False(t, b.Attrs["b"].Literal, "interpolated")
	assert.Equal(t, b.Attrs["e"].Bool, "true")
	assert.Equal(t, b.Attrs["f"].Strings, []string{"g"})
	assert.Equal(t, b.Attrs["f"].Line, 4)
	require.Len(t, b.Blocks, 1)
	assert.Equal(t, b.Blocks[0].Type, "h")
}

func FuzzParseHCL(f *testing.F) {
	f.Add([]byte(tfBad))
	f.Add([]byte(tfGood))
	f.Add([]byte("a {\n b = <<-EOT\n x\n EOT\n}\n/* unterminated"))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_ = parseHCL(string(data))
	})
}
