package theory

import (
	"strings"
	"testing"
)

// assertRejected checks that the push failed with msg and that the vault's
// refs didn't change.
func assertRejected(t *testing.T, e *env, out string, ok bool, msg string, refsBefore string) {
	t.Helper()
	if ok {
		t.Fatalf("push was accepted, want %q:\n%s", msg, out)
	}
	if !strings.Contains(out, msg) {
		t.Fatalf("push output doesn't say %q:\n%s", msg, out)
	}
	if after := vaultRefs(t, e); after != refsBefore {
		t.Fatalf("refs changed on a rejected push:\nbefore:\n%s\nafter:\n%s", refsBefore, after)
	}
}

func vaultRefs(t *testing.T, e *env) string {
	t.Helper()
	out, _ := git(e.vaultPath(), "for-each-ref", "--format=%(refname) %(objectname)")
	return out
}

// A vault with one commit of Alice's on main.
func started(t *testing.T) (*env, *engineer) {
	e := newEnv(t)
	alice := e.clone("alice@example.com")
	alice.commit("Index.md", "start", "alice@example.com")
	alice.mustPush("main")
	return e, alice
}

// GIT-4
func TestPushRejectsCommitsByOthers(t *testing.T) {
	e, _ := started(t)
	bob := e.clone("bob@example.com")
	before := vaultRefs(t, e)
	c := bob.commit("Note.md", "pretending", "alice@example.com")
	out, ok := bob.push("main")
	assertRejected(t, e, out, ok,
		"Rejected: commit "+c[:7]+" is authored by alice@example.com, but you are bob@example.com. Push only your own commits.", before)
}

// GIT-4: only new commits are checked, so pulling a teammate's commits and
// pushing on top of them works.
func TestPushAllowsTeammateCommitsAlreadyOnServer(t *testing.T) {
	e, alice := started(t)
	bob := e.clone("bob@example.com")
	alice.commit("A.md", "a", "alice@example.com")
	alice.mustPush("main")
	bob.commit("B.md", "b", "bob@example.com")
	bob.mustGit("pull", "--quiet", "--no-rebase", "--no-edit", "origin", "main")
	bob.mustPush("main")
}

// GIT-4: ignoring case.
func TestPushAuthorEmailIgnoresCase(t *testing.T) {
	e, _ := started(t)
	bob := e.clone("bob@example.com")
	bob.commit("B.md", "b", "Bob@Example.COM")
	bob.mustPush("main")
}

// GIT-5
func TestPushOnlyMain(t *testing.T) {
	e, alice := started(t)
	before := vaultRefs(t, e)
	alice.mustGit("checkout", "--quiet", "-b", "draft")
	alice.commit("D.md", "d", "alice@example.com")
	out, ok := alice.push("draft")
	assertRejected(t, e, out, ok, "Rejected: only main can be pushed.", before)
}

// GIT-6
func TestPushMainOnlyMovesForward(t *testing.T) {
	e, alice := started(t)
	alice.commit("Second.md", "2", "alice@example.com")
	alice.mustPush("main")
	before := vaultRefs(t, e)

	alice.mustGit("reset", "--quiet", "--hard", "HEAD~1")
	alice.commit("Other.md", "x", "alice@example.com")
	out, ok := alice.push("--force", "main")
	assertRejected(t, e, out, ok, "Rejected: main can't be rewritten. Pull, then push again.", before)

	out, ok = alice.push(":main")
	assertRejected(t, e, out, ok, "Rejected: main can't be rewritten. Pull, then push again.", before)
}

// GIT-7
func TestPushNoTagsOrTheoryRefs(t *testing.T) {
	e, alice := started(t)
	before := vaultRefs(t, e)
	alice.mustGit("tag", "theory/compiled")
	out, ok := alice.push("theory/compiled")
	assertRejected(t, e, out, ok, "Rejected: only the server moves tags.", before)

	out, ok = alice.push("HEAD:refs/theory/anything")
	assertRejected(t, e, out, ok, "Rejected: only the server moves tags.", before)

	// A tag riding along with a good main push rejects the whole push.
	alice.commit("More.md", "m", "alice@example.com")
	out, ok = alice.push("main", "theory/compiled")
	assertRejected(t, e, out, ok, "Rejected: only the server moves tags.", before)
}

// GIT-8
func TestPushRejectsPluginDataFile(t *testing.T) {
	e, alice := started(t)
	before := vaultRefs(t, e)
	c := alice.commit(DataFilePath, `{"token":"theory_secret"}`, "alice@example.com")
	alice.commit("Later.md", "later", "alice@example.com")
	out, ok := alice.push("main")
	assertRejected(t, e, out, ok,
		"Rejected: "+c[:7]+" contains the plugin's data file, which holds your token. Remove it from the commit.", before)

	// Removing it from history lets the push through.
	alice.mustGit("reset", "--quiet", "--hard", "HEAD~2")
	alice.commit("Later.md", "later", "alice@example.com")
	alice.mustPush("main")
}

// GIT-8: changing the file is rejected too, not only adding it.
func TestPushRejectsChangedPluginDataFile(t *testing.T) {
	e, alice := started(t)
	// Put the file on the server behind the hook's back, as an old vault might have it.
	alice.commit(DataFilePath, `{}`, "alice@example.com")
	if _, err := git(e.vaultPath(), "fetch", "--quiet", alice.dir, "main:refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	alice.mustGit("fetch", "--quiet", "origin")

	alice.commit("Other.md", "unrelated", "alice@example.com")
	alice.mustPush("main") // keeping it unchanged is fine
	before := vaultRefs(t, e)

	c := alice.commit(DataFilePath, `{"token":"theory_x"}`, "alice@example.com")
	out, ok := alice.push("main")
	assertRejected(t, e, out, ok, "Rejected: "+c[:7]+" contains the plugin's data file", before)
}

// GIT-9: a push that bypasses the server is rejected.
func TestHookRejectsPushNotThroughServer(t *testing.T) {
	msg, err := PreReceive(".", "", strings.NewReader(""))
	if err != nil || msg == "" {
		t.Fatalf("a push without a pusher passed: %q %v", msg, err)
	}
}
