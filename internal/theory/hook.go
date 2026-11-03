package theory

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// PusherEnv carries the authenticated pusher's email from the request into the hook (GIT-9).
const PusherEnv = "THEORY_PUSHER_EMAIL"

// DataFilePath is the plugin's data file, which holds the member's token (GIT-8).
const DataFilePath = ".obsidian/plugins/theory-md/data.json"

type refUpdate struct{ old, new, ref string }

// PreReceive checks a push against the push rules (GIT-4 to GIT-8). It runs
// in the vault with git's pre-receive input. It returns the message of the
// first rule that fails, or "" when the whole push passes.
func PreReceive(gitDir, pusher string, input io.Reader) (string, error) {
	if pusher == "" {
		return "Rejected: push through theory-server.", nil
	}
	var updates []refUpdate
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		f := strings.Fields(scanner.Text())
		if len(f) == 3 {
			updates = append(updates, refUpdate{f[0], f[1], f[2]})
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	for _, u := range updates {
		if msg, err := checkUpdate(gitDir, pusher, u); msg != "" || err != nil {
			return msg, err
		}
	}
	return "", nil
}

func checkUpdate(gitDir, pusher string, u refUpdate) (string, error) {
	// GIT-7
	if strings.HasPrefix(u.ref, "refs/tags/") || strings.HasPrefix(u.ref, "refs/theory/") {
		return "Rejected: only the server moves tags.", nil
	}
	// GIT-5
	if u.ref != "refs/heads/main" {
		return "Rejected: only main can be pushed.", nil
	}
	// GIT-6
	if u.new == zeroHash {
		return "Rejected: main can't be rewritten. Pull, then push again.", nil
	}
	if u.old != zeroHash {
		if _, err := git(gitDir, "merge-base", "--is-ancestor", u.old, u.new); err != nil {
			return "Rejected: main can't be rewritten. Pull, then push again.", nil
		}
	}
	// A commit is new when no ref on the server reaches it yet.
	out, err := git(gitDir, "log", "--reverse", "--format=%H %ae", u.new, "--not", "--all")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		hash, author, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		// GIT-4
		if !strings.EqualFold(author, pusher) {
			return fmt.Sprintf("Rejected: commit %s is authored by %s, but you are %s. Push only your own commits.", hash[:7], author, pusher), nil
		}
		// GIT-8
		if changesDataFile(gitDir, hash) {
			return fmt.Sprintf("Rejected: %s contains the plugin's data file, which holds your token. Remove it from the commit.", hash[:7]), nil
		}
	}
	return "", nil
}

// changesDataFile reports whether commit adds the data file or changes it
// from every parent.
func changesDataFile(gitDir, commit string) bool {
	blob := blobAt(gitDir, commit)
	if blob == "" {
		return false
	}
	parents, _ := git(gitDir, "rev-parse", commit+"^@")
	for _, p := range strings.Fields(parents) {
		if blobAt(gitDir, p) == blob {
			return false
		}
	}
	return true
}

func blobAt(gitDir, commit string) string {
	out, err := git(gitDir, "rev-parse", "--verify", "--quiet", commit+":"+DataFilePath)
	if err != nil {
		return ""
	}
	return out
}
