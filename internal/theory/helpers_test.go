package theory

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test binary doubles as theory-server, so git can run it as the
// pre-receive hook (GIT-9) without building anything.
func TestMain(m *testing.M) {
	if os.Getenv("THEORY_TEST_RUN_MAIN") == "1" {
		os.Exit(Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	t       *testing.T
	dataDir string
	store   *Store
	server  *httptest.Server
	clock   *clock
	tokens  map[string]string // email -> token
}

const vault = "team"

var people = map[string]string{
	"alice@example.com": "Alice",
	"bob@example.com":   "Bob",
}

// newEnv starts a server on localhost with a fresh data folder, the vault
// "team" and two members, Alice and Bob.
func newEnv(t *testing.T) *env {
	t.Helper()
	dataDir := t.TempDir()
	store, err := OpenStore(filepath.Join(dataDir, "theory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := CreateVault(dataDir, vault); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dataDir: dataDir, store: store, clock: &clock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}, tokens: map[string]string{}}
	for email, name := range people {
		if err := store.AddMember(name, email); err != nil {
			t.Fatal(err)
		}
		tok, err := store.CreateToken(email)
		if err != nil {
			t.Fatal(err)
		}
		e.tokens[email] = tok
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{DataDir: dataDir, Store: store, HookBin: bin, HookEnv: []string{"THEORY_TEST_RUN_MAIN=1"}, Now: e.clock.Now}
	e.server = httptest.NewServer(s.Handler())
	t.Cleanup(e.server.Close)
	return e
}

func (e *env) vaultPath() string { return VaultPath(e.dataDir, vault) }

func (e *env) vaultURL() string { return e.server.URL + "/git/" + vault + ".git" }

// gitEnv keeps git away from the engineer's own config.
func gitEnv(home string) []string {
	return append(os.Environ(),
		"HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
	)
}

// runGit runs git in dir and returns combined output and whether it succeeded.
func runGit(t *testing.T, dir string, args ...string) (string, bool) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(dir)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, ok := runGit(t, dir, args...)
	if !ok {
		t.Fatalf("git %s failed:\n%s", strings.Join(args, " "), out)
	}
	return strings.TrimSpace(out)
}

// engineer is one engineer's clone of the vault.
type engineer struct {
	e     *env
	dir   string
	email string
}

func (e *env) clone(email string) *engineer {
	e.t.Helper()
	dir := e.t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte("x:" + e.tokens[email]))
	mustGit(e.t, dir, "init", "--quiet", "--initial-branch=main")
	mustGit(e.t, dir, "config", "http.extraHeader", "Authorization: Basic "+auth)
	mustGit(e.t, dir, "config", "user.name", people[email])
	mustGit(e.t, dir, "config", "user.email", email)
	mustGit(e.t, dir, "remote", "add", "origin", e.vaultURL())
	if _, ok := runGit(e.t, dir, "pull", "--quiet", "origin", "main"); ok {
		mustGit(e.t, dir, "branch", "--set-upstream-to=origin/main", "main")
	}
	return &engineer{e: e, dir: dir, email: email}
}

func (g *engineer) git(args ...string) (string, bool) { return runGit(g.e.t, g.dir, args...) }

func (g *engineer) mustGit(args ...string) string { return mustGit(g.e.t, g.dir, args...) }

// commit writes a file and commits it, authored by author, and returns the hash.
func (g *engineer) commit(path, content, author string) string {
	g.e.t.Helper()
	full := filepath.Join(g.dir, path)
	os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		g.e.t.Fatal(err)
	}
	g.mustGit("add", "--", path)
	g.mustGit("-c", "user.email="+author, "commit", "--quiet", "-m", "Edit "+path)
	return g.mustGit("rev-parse", "HEAD")
}

func (g *engineer) push(args ...string) (string, bool) {
	return g.git(append([]string{"push", "origin"}, args...)...)
}

func (g *engineer) mustPush(args ...string) {
	g.e.t.Helper()
	if out, ok := g.push(args...); !ok {
		g.e.t.Fatalf("push failed:\n%s", out)
	}
}

// api calls the API as the member with email, and decodes the answer into a map.
func (e *env) api(email, method, path string, body any) (int, map[string]any) {
	e.t.Helper()
	status, raw := e.apiRaw(e.tokens[email], method, path, body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			e.t.Fatalf("%s %s answered %d with invalid JSON: %s", method, path, status, raw)
		}
	}
	return status, out
}

func (e *env) apiRaw(token, method, path string, body any) (int, []byte) {
	e.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		r = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, e.server.URL+path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw
}

func lockPath(parts ...string) string {
	return "/api/v1/vaults/" + vault + "/lock" + strings.Join(parts, "")
}

// takeLock takes the lock as email and returns the lock id.
func (e *env) takeLock(email string) string {
	e.t.Helper()
	status, out := e.api(email, "POST", lockPath(), nil)
	if status != http.StatusOK {
		e.t.Fatalf("take lock: %d %v", status, out)
	}
	return out["lockId"].(string)
}
