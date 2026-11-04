package theory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

const goneMessage = "Your compile lock expired. The marker didn't move."

// LOCK-1
func TestLockHasOneHolder(t *testing.T) {
	e := newEnv(t)
	if status, out := e.api("bob@example.com", "GET", lockPath(), nil); status != 200 || out["holder"] != nil {
		t.Fatalf("free lock: %d %v", status, out)
	}
	e.takeLock("alice@example.com")
	status, out := e.api("bob@example.com", "POST", lockPath(), nil)
	if status != http.StatusConflict {
		t.Fatalf("second take: %d %v", status, out)
	}
	holder := out["holder"].(map[string]any)
	if holder["name"] != "Alice" || holder["email"] != "alice@example.com" || out["since"] != "2026-11-03T12:00:00Z" || out["error"] == nil {
		t.Fatalf("409 doesn't say who holds it and since when: %v", out)
	}
	status, out = e.api("bob@example.com", "GET", lockPath(), nil)
	if status != 200 || out["holder"].(map[string]any)["email"] != "alice@example.com" || out["since"] != "2026-11-03T12:00:00Z" {
		t.Fatalf("GET lock: %d %v", status, out)
	}
	// Holding it yourself doesn't let you take it twice.
	if status, _ := e.api("alice@example.com", "POST", lockPath(), nil); status != http.StatusConflict {
		t.Fatalf("holder took the lock again: %d", status)
	}
}

// LOCK-2
func TestOnlyLockIdRenewsOrReleases(t *testing.T) {
	e := newEnv(t)
	id := e.takeLock("alice@example.com")
	for _, action := range []string{"renew", "release"} {
		status, out := e.api("alice@example.com", "POST", lockPath("/wrong-id/", action), nil)
		if status != http.StatusGone || out["error"] != goneMessage {
			t.Fatalf("%s with a wrong id: %d %v", action, status, out)
		}
	}
	// Anyone with the id can use it; the id is the proof.
	if status, out := e.api("alice@example.com", "POST", lockPath("/", id, "/renew"), nil); status != 200 || out["expiresAt"] == nil {
		t.Fatalf("renew: %d %v", status, out)
	}
	if status, _ := e.api("alice@example.com", "POST", lockPath("/", id, "/release"), nil); status != 200 {
		t.Fatalf("release: %d", status)
	}
}

// LOCK-3
func TestLockLeaseExpiresUnlessRenewed(t *testing.T) {
	e := newEnv(t)
	status, out := e.api("alice@example.com", "POST", lockPath(), nil)
	if status != 200 || out["expiresAt"] != "2026-11-03T12:05:00Z" {
		t.Fatalf("a lock is a 5-minute lease: %d %v", status, out)
	}
	id := out["lockId"].(string)
	// Renewing every 60 seconds keeps it for longer than 5 minutes.
	for i := 0; i < 10; i++ {
		e.clock.Advance(60 * time.Second)
		if status, _ := e.api("alice@example.com", "POST", lockPath("/", id, "/renew"), nil); status != 200 {
			t.Fatalf("renew %d: %d", i, status)
		}
	}
	if status, _ := e.api("bob@example.com", "POST", lockPath(), nil); status != http.StatusConflict {
		t.Fatal("a renewed lock was taken")
	}
	e.clock.Advance(LockLease - time.Second)
	if status, _ := e.api("bob@example.com", "POST", lockPath(), nil); status != http.StatusConflict {
		t.Fatal("the lock expired early")
	}
	e.clock.Advance(time.Second)
	if status, out := e.api("bob@example.com", "GET", lockPath(), nil); status != 200 || out["holder"] != nil {
		t.Fatalf("expired lock still shown: %v", out)
	}
	e.takeLock("bob@example.com")
}

// A vault where main has two commits, c1 and c2.
func withHistory(t *testing.T) (e *env, c1, c2 string) {
	e = newEnv(t)
	alice := e.clone("alice@example.com")
	c1 = alice.commit("Index.md", "1", "alice@example.com")
	c2 = alice.commit("Index.md", "2", "alice@example.com")
	alice.mustPush("main")
	return e, c1, c2
}

// LOCK-4, LOCK-6
func TestExpiredLockDoesNotMoveMarker(t *testing.T) {
	e, c1, _ := withHistory(t)
	id := e.takeLock("alice@example.com")
	e.clock.Advance(LockLease)
	status, out := e.api("alice@example.com", "POST", lockPath("/", id, "/release"), map[string]any{"marker": c1})
	if status != http.StatusGone || out["error"] != goneMessage {
		t.Fatalf("release of an expired lock: %d %v", status, out)
	}
	if status, out := e.api("alice@example.com", "POST", lockPath("/", id, "/renew"), nil); status != http.StatusGone || out["error"] != goneMessage {
		t.Fatalf("renew of an expired lock: %d %v", status, out)
	}
	if m := Marker(e.vaultPath()); m != "" {
		t.Fatalf("marker moved to %s", m)
	}
}

// LOCK-5, LOCK-6
func TestBrokenLock(t *testing.T) {
	e, c1, _ := withHistory(t)
	id := e.takeLock("alice@example.com")
	var stdout, stderr bytes.Buffer
	t.Setenv("THEORY_DATA", e.dataDir)
	if code := Main([]string{"lock", "break", vault}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("lock break: %d %s", code, stderr.String())
	}
	status, out := e.api("alice@example.com", "POST", lockPath("/", id, "/release"), map[string]any{"marker": c1})
	if status != http.StatusGone || out["error"] != goneMessage {
		t.Fatalf("release of a broken lock: %d %v", status, out)
	}
	if status, _ := e.api("alice@example.com", "POST", lockPath("/", id, "/renew"), nil); status != http.StatusGone {
		t.Fatalf("renew of a broken lock: %d", status)
	}
	if m := Marker(e.vaultPath()); m != "" {
		t.Fatalf("marker moved to %s", m)
	}
	e.takeLock("bob@example.com")
}

// LOCK-7, LOCK-8
func TestReleaseMovesMarker(t *testing.T) {
	e, c1, c2 := withHistory(t)
	id := e.takeLock("alice@example.com")
	status, out := e.api("alice@example.com", "POST", lockPath("/", id, "/release"), map[string]any{"marker": c1})
	if status != 200 || out["marker"] != c1 {
		t.Fatalf("release: %d %v", status, out)
	}
	if status, out := e.api("bob@example.com", "GET", lockPath(), nil); status != 200 || out["holder"] != nil {
		t.Fatalf("the lock is still held after release: %v", out)
	}
	// Members read the marker by fetching tags.
	bob := e.clone("bob@example.com")
	bob.mustGit("fetch", "--quiet", "--tags", "origin")
	if got := bob.mustGit("rev-parse", "theory/compiled^{commit}"); got != c1 {
		t.Fatalf("fetched marker %s, want %s", got, c1)
	}

	// A release that names no commit leaves the marker.
	id = e.takeLock("bob@example.com")
	status, out = e.api("bob@example.com", "POST", lockPath("/", id, "/release"), map[string]any{})
	if status != 200 || out["marker"] != c1 || Marker(e.vaultPath()) != c1 {
		t.Fatalf("release without marker: %d %v", status, out)
	}

	id = e.takeLock("bob@example.com")
	if status, out := e.api("bob@example.com", "POST", lockPath("/", id, "/release"), map[string]any{"marker": c2}); status != 200 || out["marker"] != c2 {
		t.Fatalf("second release: %d %v", status, out)
	}
}

// LOCK-8: when the marker can't move, the lock isn't released either.
func TestFailedMarkerKeepsLock(t *testing.T) {
	e, _, _ := withHistory(t)
	id := e.takeLock("alice@example.com")
	report := map[string]any{"status": "succeeded"}
	status, _ := e.api("alice@example.com", "POST", lockPath("/", id, "/release"), map[string]any{"marker": strings.Repeat("a", 40), "report": report})
	if status != http.StatusConflict {
		t.Fatalf("release to an unknown commit: %d", status)
	}
	if status, out := e.api("bob@example.com", "GET", lockPath(), nil); out["holder"] == nil {
		t.Fatalf("the lock was released though the marker didn't move: %d %v", status, out)
	}
	if _, out := e.api("bob@example.com", "GET", "/api/v1/vaults/"+vault+"/reports", nil); len(out["reports"].([]any)) != 0 {
		t.Fatal("the report was stored though the release failed")
	}
	if status, _ := e.api("alice@example.com", "POST", lockPath("/", id, "/release"), nil); status != 200 {
		t.Fatalf("the lock can still be released: %d", status)
	}
}

// LOCK-9
func TestMarkerOnlyOnMainAndForward(t *testing.T) {
	e, c1, c2 := withHistory(t)
	alice := e.clone("alice@example.com")
	alice.mustGit("checkout", "--quiet", "-b", "side", c1)
	side := alice.commit("Side.md", "side", "alice@example.com")
	// Put the side commit's objects in the vault without it being on main.
	if _, err := git(e.vaultPath(), "fetch", "--quiet", alice.dir, "side:refs/heads/side"); err != nil {
		t.Fatal(err)
	}
	git(e.vaultPath(), "update-ref", "-d", "refs/heads/side")

	id := e.takeLock("alice@example.com")
	for _, bad := range []string{side, "c2", c2[:7]} {
		status, out := e.api("alice@example.com", "POST", lockPath("/", id, "/release"), map[string]any{"marker": bad})
		if status != http.StatusConflict || out["error"] == nil {
			t.Fatalf("marker %s: %d %v", bad, status, out)
		}
	}
	if status, _ := e.api("alice@example.com", "POST", lockPath("/", id, "/release"), map[string]any{"marker": c2}); status != 200 {
		t.Fatal("release to main's head failed")
	}

	for _, back := range []string{c1, c2} {
		id = e.takeLock("bob@example.com")
		status, out := e.api("bob@example.com", "POST", lockPath("/", id, "/release"), map[string]any{"marker": back})
		if status != http.StatusConflict || Marker(e.vaultPath()) != c2 {
			t.Fatalf("marker moved back or stayed to %s: %d %v", back, status, out)
		}
		if _, out := e.api("alice@example.com", "GET", lockPath(), nil); out["holder"] == nil {
			t.Fatal("refused release freed the lock")
		}
		e.api("bob@example.com", "POST", lockPath("/", id, "/release"), nil)
	}
}

// SHR-1, SHR-3, SHR-4
func TestReportStoredUnchanged(t *testing.T) {
	e, c1, _ := withHistory(t)
	refsBefore := vaultRefs(t, e)
	objectsBefore, _ := git(e.vaultPath(), "count-objects", "-v")
	id := e.takeLock("alice@example.com")
	report := `{ "status":"succeeded",  "questions": [ {"question":"Why?"} ], "future": {"x": 1.50} }`
	body := fmt.Sprintf(`{"marker":%q,"report":%s}`, c1, report)
	if status, raw := e.apiRaw(e.tokens["alice@example.com"], "POST", lockPath("/", id, "/release"), body); status != 200 {
		t.Fatalf("release: %d %s", status, raw)
	}
	// Bob reads Alice's report.
	status, raw := e.apiRaw(e.tokens["bob@example.com"], "GET", "/api/v1/vaults/"+vault+"/reports", nil)
	if status != 200 {
		t.Fatalf("reports: %d %s", status, raw)
	}
	var out struct {
		Reports []struct {
			ID        int64           `json:"id"`
			Member    person          `json:"member"`
			CreatedAt string          `json:"createdAt"`
			Report    json.RawMessage `json:"report"`
		} `json:"reports"`
	}
	json.Unmarshal(raw, &out)
	if len(out.Reports) != 1 || out.Reports[0].Member.Email != "alice@example.com" {
		t.Fatalf("reports: %s", raw)
	}
	stored, _ := e.store.Reports(vault, 0)
	if string(stored[0].JSON) != report {
		t.Fatalf("stored report changed:\n%s\nwant\n%s", stored[0].JSON, report)
	}
	// Nothing but the marker changed in the vault repository.
	if objectsAfter, _ := git(e.vaultPath(), "count-objects", "-v"); objectsAfter != objectsBefore {
		t.Fatalf("the vault got new objects:\n%s\n%s", objectsBefore, objectsAfter)
	}
	if after := vaultRefs(t, e); after != refsBefore+"\n"+MarkerRef+" "+c1 {
		t.Fatalf("vault refs: %s", after)
	}
}

// SHR-2
func TestReportsNewestFirstTwentyAtATime(t *testing.T) {
	e := newEnv(t)
	for i := 1; i <= 25; i++ {
		id := e.takeLock("alice@example.com")
		e.api("alice@example.com", "POST", lockPath("/", id, "/release"), map[string]any{"report": map[string]any{"n": i}})
	}
	page := func(query string) []int {
		_, out := e.api("bob@example.com", "GET", "/api/v1/vaults/"+vault+"/reports"+query, nil)
		var ns []int
		for _, r := range out["reports"].([]any) {
			ns = append(ns, int(r.(map[string]any)["report"].(map[string]any)["n"].(float64)))
		}
		return ns
	}
	first := page("")
	if len(first) != 20 || first[0] != 25 || first[19] != 6 {
		t.Fatalf("first page: %v", first)
	}
	_, out := e.api("bob@example.com", "GET", "/api/v1/vaults/"+vault+"/reports", nil)
	last := out["reports"].([]any)[19].(map[string]any)["id"].(float64)
	second := page(fmt.Sprintf("?before=%d", int64(last)))
	if fmt.Sprint(second) != "[5 4 3 2 1]" {
		t.Fatalf("second page: %v", second)
	}
}
