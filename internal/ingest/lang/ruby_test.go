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

var rubyFixture = map[string]string{
	"app/models/user.rb": `# frozen_string_literal: true

# A person who signs in.
class User < ApplicationRecord
  has_many :posts
  before_save :normalize_email

  API_TOKEN = "sk_live_9f8e7d6c5b4a3928"

  def self.search(term)
    where("name LIKE '%#{term}%'")
  end

  def initialize(name)
    @name = name
  end

  def display_name
    format_name(@name)
  end

  private

  def normalize_email
    self.email = email.downcase if email
  end

  def format_name(n)
    n.to_s.strip
  end
end
`,
	"app/controllers/users_controller.rb": `class UsersController < ApplicationController
  before_action :load_user, only: [:show]

  def index
    @users = User.search(params[:q])
    render json: @users
  end

  def show
    data = <<~SQL
      SELECT * FROM users WHERE id = #{params[:id]} do
    SQL
    if @user.nil?
      redirect_to params[:return_to]
    end
    system("convert #{params[:file]} out.png")
    render json: { name: "#{@user.display_name} }", end: 1 }
  end

  def create
    user = User.new(params[:name])
    items.each do |item|
      item.save
    end
    send_file File.join("/srv", params[:path])
  end

  private

  def load_user
    @user = User.find(params[:id])
  end
end
`,
	"config/routes.rb": `Rails.application.routes.draw do
  root "home#index"
  resources :users, only: [:index, :show] do
    resources :posts, except: [:destroy, :edit, :new, :update]
  end
  namespace :admin do
    get "stats", to: "stats#show"
  end
  post "/login" => "sessions#create"
end
`,
	"app.rb": `require "sinatra"
require_relative "lib/helpers"

get "/hello" do
  greet(params[:name])
end

def greet(name)
  "hi #{name}"
end
`,
	"lib/helpers.rb": `module Helpers
  def self.slug(s) = s.downcase
end
`,
	"spec/models/user_spec.rb": `require "rails_helper"

RSpec.describe User do
  it "searches" do
    expect(User.search("a")).to be_empty
  end
end
`,
	"spec/rails_helper.rb": "RSpec.configure { |c| c.order = :random }\n",
	"test/models/user_test.rb": `class UserTest < ActiveSupport::TestCase
  def test_display_name
    assert_equal "a", User.new("a").display_name
  end
end
`,
}

func TestRuby(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range rubyFixture {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
	res, err := Extract(context.Background(), "r", dir)
	require.NoError(t, err)
	g := res.Graph
	assert.Equal(t, res.Languages[Ruby], 8)
	id := func(k model.NodeKind, rel, name string) string { return model.NodeID("r", k, rel+"::"+name) }
	node := func(nid string) model.Node {
		t.Helper()
		n, ok := g.Node(nid)
		require.True(t, ok, "missing "+nid)
		return *n
	}
	um, uc := "app/models/user.rb", "app/controllers/users_controller.rb"
	user := node(id(model.KindType, um, "User"))
	assert.Equal(t, user.Summary, "A person who signs in.")
	assert.Equal(t, [2]int{user.StartLine, user.EndLine}, [2]int{4, 31})
	assert.Equal(t, node(id(model.KindMethod, um, "User.search")).EndLine, 12)
	assert.False(t, node(id(model.KindMethod, um, "User.format_name")).Exported, "after private")
	assert.True(t, node(id(model.KindMethod, um, "User.display_name")).Exported)
	show := node(id(model.KindMethod, uc, "UsersController.show"))
	assert.Equal(t, show.EndLine, 18, "a heredoc, an if block and a brace in an interpolated string")
	assert.Equal(t, node(id(model.KindMethod, uc, "UsersController.create")).EndLine, 26, "a do block")
	assert.Equal(t, node(id(model.KindType, uc, "UsersController")).EndLine, 33)
	slug := node(id(model.KindMethod, "lib/helpers.rb", "Helpers.slug"))
	assert.Equal(t, slug.EndLine, slug.StartLine, "an endless def")
	assert.True(t, node(id(model.KindMethod, "test/models/user_test.rb", "UserTest.test_display_name")).IsTest)

	edges := map[[2]string]string{}
	for _, e := range g.Edges() {
		edges[[2]string{e.From, e.To}] = string(e.Kind)
	}
	calls := func(from, to string) {
		t.Helper()
		assert.Equal(t, edges[[2]string{from, to}], string(model.EdgeCalls), from+" -> "+to)
	}
	search := id(model.KindMethod, um, "User.search")
	calls(id(model.KindMethod, uc, "UsersController.index"), search)
	calls(id(model.KindMethod, um, "User.display_name"), id(model.KindMethod, um, "User.format_name"))
	create := id(model.KindMethod, uc, "UsersController.create")
	calls(create, user.ID)
	calls(create, id(model.KindMethod, um, "User.initialize"))
	calls(id(model.KindFunction, "spec/models/user_spec.rb", "<tests>"), search)
	tdn := id(model.KindMethod, "test/models/user_test.rb", "UserTest.test_display_name")
	calls(tdn, id(model.KindMethod, um, "User.initialize"))
	calls(tdn, id(model.KindMethod, um, "User.display_name"))
	calls(model.NodeID("r", model.KindFile, "app.rb"), id(model.KindFunction, "app.rb", "greet"))

	// Rails calls actions and callbacks: they are entry points.
	refs := map[string]bool{}
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeReferences {
			refs[e.To] = true
		}
	}
	for _, m := range []string{uc + "::UsersController.index", uc + "::UsersController.load_user", um + "::User.normalize_email"} {
		k := model.KindMethod
		assert.True(t, refs[model.NodeID("r", k, m)], m)
	}
	assert.False(t, refs[id(model.KindMethod, um, "User.format_name")])

	var found []string
	for _, n := range g.Nodes() {
		if n.Kind == model.KindFinding {
			found = append(found, n.Rule)
		}
	}
	slices.Sort(found)
	assert.Equal(t, found, []string{"exec-shell", "hardcoded-secret", "open-redirect", "path-from-request", "sql-concat"})
	assert.True(t, len(res.ExternalImports["sinatra"]) > 0)
	for _, local := range []string{"rails_helper", "lib/helpers"} {
		_, ok := res.ExternalImports[local]
		assert.False(t, ok, local+" is a file of the repository")
	}

	var routes []string
	for _, n := range g.Nodes() {
		if n.Kind == model.KindRoute {
			routes = append(routes, n.Name)
		}
	}
	slices.Sort(routes)
	assert.Equal(t, routes, []string{
		"GET /", "GET /admin/stats", "GET /hello", "GET /users", "GET /users/:id",
		"GET /users/:user_id/posts", "GET /users/:user_id/posts/:id", "POST /login", "POST /users/:user_id/posts",
	})
}

func TestRubyRules(t *testing.T) {
	for _, tt := range []struct {
		line string
		want []string
	}{
		{"system(\"tar xzf #{path}\")", []string{"exec-shell"}},
		{"system(\"tar\", \"xzf\", path)", nil},
		{"out = `ls #{dir}`", []string{"exec-shell"}},
		{"system(cmd)", []string{"exec-variable"}},
		{"def system_reports", nil},
		{"@executor = executor", nil},
		{"IO.popen([\"sendmail\", \"-f\", \"nobody@#{domain}\"], \"w\")", nil},
		{"redis.exists?(\"subscribed:#{id}\")", nil},
		{"where('name LIKE ?', \"#{value}%\")", nil},
		{"@m = @m.where(\"#{type}_id\": nil)", nil},
		{"execute(\"ALTER TABLE \" + table)", []string{"sql-concat"}},
		{"KEY_FORMAT_TOKEN = 'user.%d.sign_in_token'", nil},
		{"Open3.capture2(\"git log #{ref}\")", []string{"exec-shell"}},
		{"User.where(\"name = '#{name}'\")", []string{"sql-concat"}},
		{"User.where(\"name = ?\", name)", nil},
		{"User.where(name: name)", nil},
		{"scope :named, ->(n) { where(\"name = '#{n}'\") }", []string{"sql-concat"}},
		{"Post.order(\"created_at #{params[:dir]}\")", []string{"sql-concat"}},
		{"eval(code)", []string{"code-injection"}},
		{"params[:type].constantize.new", []string{"code-injection"}},
		{"klass.class_eval { attr_accessor :x }", nil},
		{"obj = Marshal.load(data)", []string{"deserialize"}},
		{"cfg = YAML.safe_load(File.read(path))", nil},
		{"send_file params[:file]", []string{"path-from-request"}},
		{"redirect_to params[:next]", []string{"open-redirect"}},
		{"redirect_to root_path", nil},
		{"http.verify_mode = OpenSSL::SSL::VERIFY_NONE", []string{"tls-insecure"}},
		{"digest = Digest::MD5.hexdigest(password)", []string{"weak-hash"}},
		{"etag = Digest::MD5.hexdigest(body)", nil},
		{"cipher = OpenSSL::Cipher.new('des-ede3-cbc')", []string{"weak-cipher"}},
		{"token = rand(36**8).to_s(36)", []string{"insecure-rand"}},
		{"token = SecureRandom.hex(32)", nil},
		{"SECRET_KEY = \"hunter2hunter2\"", []string{"hardcoded-secret"}},
		{"# system(cmd)", nil},
	} {
		t.Run(tt.line, func(t *testing.T) {
			var got []string
			for _, f := range rubyRules(tt.line, maskRuby(tt.line), 1, "") {
				got = append(got, f.rule)
			}
			assert.Equal(t, got, tt.want)
		})
	}
}

// The Ruby examples of the rule catalogue are what rubyRules detects.
func TestRubyRuleExamples(t *testing.T) {
	for _, r := range rules.All() {
		for _, e := range r.Examples {
			if e.Language != Ruby {
				continue
			}
			t.Run(r.ID, func(t *testing.T) {
				found := func(line string) bool {
					for _, f := range rubyRules(line, maskRuby(line), 1, "") {
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

func TestMaskRuby(t *testing.T) {
	src := "a = \"#{h[\"k\"]} end\"\nb = %w[do end]\n=begin\ndef x\n=end\nc = <<~EOS\n  end do\nEOS\nd = x / 2 # end\ne = s.split(/ end /)\nf = :\"end\"\n"
	m := maskRuby(src)
	assert.Equal(t, strings.Count(m, "end")+strings.Count(m, "do")+strings.Count(m, "def"), 0, m)
	assert.Equal(t, strings.Count(m, "\n"), strings.Count(src, "\n"))
	assert.Contains(t, m, "d = x / 2")
}

// The masker keeps every byte position and line; the parser never panics
// and keeps symbols inside the file.
func FuzzParseRuby(f *testing.F) {
	for _, src := range rubyFixture {
		f.Add(src)
	}
	f.Add("x = <<~A\n#{\"\n%w(\n=begin\n")
	f.Fuzz(func(t *testing.T, src string) {
		m := maskRuby(src)
		if len(m) != len(src) || strings.Count(m, "\n") != strings.Count(src, "\n") {
			t.Fatalf("mask changed the layout of %q", src)
		}
		lines := strings.Count(src, "\n") + 1
		for _, s := range parseRuby([]byte(src), "config/routes.rb").symbols {
			if s.start < 1 || s.end < s.start || s.end > lines {
				t.Fatalf("%s spans %d-%d of %d lines", s.name, s.start, s.end, lines)
			}
		}
	})
}
