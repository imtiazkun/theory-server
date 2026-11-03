package theory

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// AUTH-3
func TestMemberEmailIsUniqueIgnoringCase(t *testing.T) {
	e := newEnv(t)
	if err := e.store.AddMember("Other Alice", "ALICE@example.com"); err == nil {
		t.Fatal("a second member with the same email, in other case, was added")
	}
	m, err := e.store.MemberByEmail("Alice@Example.com")
	if err != nil || m.Name != "Alice" {
		t.Fatalf("lookup ignoring case: %v %v", m, err)
	}
}

// AUTH-5, AUTH-6
func TestTokenFormatAndOnlyHashStored(t *testing.T) {
	e := newEnv(t)
	tok := e.tokens["alice@example.com"]
	if !regexp.MustCompile(`^theory_[0-9a-f]{64}$`).MatchString(tok) {
		t.Fatalf("token %q isn't theory_ and 32 random bytes", tok)
	}
	e.store.Close()
	for _, name := range []string{"theory.db", "theory.db-wal"} {
		raw, err := os.ReadFile(filepath.Join(e.dataDir, name))
		if err != nil {
			continue
		}
		if bytes.Contains(raw, []byte(tok)) || bytes.Contains(raw, []byte(strings.TrimPrefix(tok, TokenPrefix))) {
			t.Fatalf("%s contains the token itself", name)
		}
		if bytes.Contains(raw, []byte(hashToken(tok))) {
			return
		}
	}
	t.Fatal("the database doesn't contain the token's SHA-256 hash")
}

// AUTH-8
func TestRequestsWithoutValidTokenGet401AndNoContent(t *testing.T) {
	e := newEnv(t)
	for _, token := range []string{"", "theory_nope", "nope"} {
		status, body := e.apiRaw(token, "GET", "/api/v1/me", nil)
		if status != http.StatusUnauthorized || len(body) != 0 {
			t.Fatalf("API with token %q: %d %q", token, status, body)
		}
		req, _ := http.NewRequest("GET", e.vaultURL()+"/info/refs?service=git-upload-pack", nil)
		if token != "" {
			req.SetBasicAuth("anyone", token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		buf.ReadFrom(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized || buf.Len() != 0 {
			t.Fatalf("git with token %q: %d %q", token, res.StatusCode, buf.String())
		}
	}
	// Git takes the token as the Basic password only, with any user name.
	req, _ := http.NewRequest("GET", e.vaultURL()+"/info/refs?service=git-upload-pack", nil)
	req.SetBasicAuth("whoever", e.tokens["alice@example.com"])
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("git with a valid token: %d", res.StatusCode)
	}
}

// API-5: GET /me
func TestMe(t *testing.T) {
	e := newEnv(t)
	status, out := e.api("bob@example.com", "GET", "/api/v1/me", nil)
	if status != 200 || out["name"] != "Bob" || out["email"] != "bob@example.com" {
		t.Fatalf("%d %v", status, out)
	}
}

// AUTH-7
func TestRevokeTokens(t *testing.T) {
	e := newEnv(t)
	second, _ := e.store.CreateToken("alice@example.com")
	if n, err := e.store.RevokeTokens("alice@example.com"); err != nil || n != 2 {
		t.Fatalf("revoked %d, %v", n, err)
	}
	for _, tok := range []string{e.tokens["alice@example.com"], second} {
		if status, _ := e.apiRaw(tok, "GET", "/api/v1/me", nil); status != http.StatusUnauthorized {
			t.Fatalf("revoked token still works: %d", status)
		}
	}
	if status, _ := e.api("bob@example.com", "GET", "/api/v1/me", nil); status != 200 {
		t.Fatal("revoking Alice's tokens revoked Bob's")
	}
}

// AUTH-4
func TestRemoveMemberRevokesTokensAndKeepsCommits(t *testing.T) {
	e := newEnv(t)
	alice := e.clone("alice@example.com")
	c := alice.commit("Index.md", "hello", "alice@example.com")
	alice.mustPush("main")
	if err := e.store.RemoveMember("alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if status, _ := e.api("alice@example.com", "GET", "/api/v1/me", nil); status != http.StatusUnauthorized {
		t.Fatalf("removed member's token still works: %d", status)
	}
	if _, err := git(e.vaultPath(), "cat-file", "-e", c); err != nil {
		t.Fatal("removed member's commit is gone")
	}
}

// AUTH-1, GIT-3: two engineers share a vault.
func TestTwoEngineersShareAVault(t *testing.T) {
	e := newEnv(t)
	alice := e.clone("alice@example.com")
	alice.commit("Index.md", "from alice", "alice@example.com")
	alice.mustPush("main")
	bob := e.clone("bob@example.com")
	if got, _ := os.ReadFile(filepath.Join(bob.dir, "Index.md")); string(got) != "from alice" {
		t.Fatalf("Bob sees %q", got)
	}
	bob.commit("Bob.md", "from bob", "bob@example.com")
	bob.mustPush("main")
	alice.mustGit("pull", "--quiet", "--rebase", "origin", "main")
	if _, err := os.Stat(filepath.Join(alice.dir, "Bob.md")); err != nil {
		t.Fatal("Alice doesn't see Bob's note")
	}
}

// GIT-1
func TestVaultNames(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"Team", "my_vault", "a.b", "", "../x", "a b"} {
		if err := CreateVault(dir, bad); err == nil {
			t.Fatalf("vault %q was created", bad)
		}
	}
	if err := CreateVault(dir, "my-vault-2"); err != nil {
		t.Fatal(err)
	}
	if err := CreateVault(dir, "my-vault-2"); err == nil {
		t.Fatal("a vault was created twice")
	}
	if _, err := os.Stat(filepath.Join(dir, "vaults", "my-vault-2.git", "HEAD")); err != nil {
		t.Fatal("the vault isn't a bare repository at vaults/<name>.git")
	}
}

func TestUnknownVaultIs404(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest("GET", e.server.URL+"/git/nope.git/info/refs?service=git-upload-pack", nil)
	req.SetBasicAuth("x", e.tokens["alice@example.com"])
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("git on a missing vault: %d", res.StatusCode)
	}
	if status, out := e.api("alice@example.com", "GET", "/api/v1/vaults/nope/lock", nil); status != 404 || out["error"] == nil {
		t.Fatalf("API on a missing vault: %d %v", status, out)
	}
}
