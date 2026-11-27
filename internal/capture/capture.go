package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	markerStart    = "---KELOS_OUTPUTS_START---"
	markerEnd      = "---KELOS_OUTPUTS_END---"
	commandTimeout = 30 * time.Second
)

// Run streams the agent's JSON output from stdin to stdout, accumulating
// per-agent token usage in memory, then emits deterministic outputs
// (branch, commit, PRs, token usage, and a degenerate marker when Claude
// Code returned an unusable final message) between markers on stdout. It is
// intended to be the right-hand side of a pipe from the agent process so
// that no on-disk copy of the stream is required. It returns non-zero when
// the stream cannot be processed or Claude Code reports an incomplete result.
func Run() int {
	return run(os.Getenv("KELOS_AGENT_TYPE"), os.Stdin, os.Stdout, os.Stderr, realRunner{})
}

func run(agentType string, input io.Reader, stdout, stderr io.Writer, commandRunner runner) int {
	usage, err := StreamUsage(agentType, input, stdout)
	exitCode := 0
	if err != nil {
		fmt.Fprintf(stderr, "kelos-capture: %v\n", err)
		exitCode = 1
	}
	outputs := captureOutputs(commandRunner, usage)
	if len(outputs) == 0 {
		return exitCode
	}
	fmt.Fprintln(stdout, markerStart)
	for _, line := range outputs {
		fmt.Fprintln(stdout, line)
	}
	fmt.Fprintln(stdout, markerEnd)
	return exitCode
}

// runner abstracts command execution for testing.
type runner interface {
	run(name string, args ...string) (string, error)
}

type realRunner struct{}

func (realRunner) run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), err
}

func captureOutputs(r runner, usage map[string]string) []string {
	var outputs []string

	inGitRepo := isGitRepo(r)

	if inGitRepo {
		branch, err := r.run("git", "branch", "--show-current")
		if err == nil && branch != "" {
			outputs = append(outputs, "branch: "+branch)
			outputs = append(outputs, capturePRs(r, branch)...)
		}

		commit, err := r.run("git", "rev-parse", "HEAD")
		if err == nil && commit != "" {
			outputs = append(outputs, "commit: "+commit)
		}
	}

	if base := os.Getenv("KELOS_BASE_BRANCH"); base != "" {
		outputs = append(outputs, "base-branch: "+base)
	} else if inGitRepo {
		ref, err := r.run("git", "symbolic-ref", "refs/remotes/origin/HEAD")
		if err == nil && ref != "" {
			branch := strings.TrimPrefix(ref, "refs/remotes/origin/")
			if branch != "" {
				outputs = append(outputs, "base-branch: "+branch)
			}
		}
	}

	for _, key := range []string{"cost-usd", "input-tokens", "output-tokens", "response", "degenerate"} {
		if v, ok := usage[key]; ok {
			outputs = append(outputs, key+": "+v)
		}
	}

	return outputs
}

func isGitRepo(r runner) bool {
	_, err := r.run("git", "rev-parse", "--is-inside-work-tree")
	return err == nil
}

func capturePRs(r runner, branch string) []string {
	// The push goes to origin, so its owner and repo are the source of
	// truth. gh's own {owner} and {repo} placeholders are not used: on a
	// fork with an upstream remote gh fills them with the upstream owner.
	owner, repo, ok := originOwnerRepo(r)
	if !ok {
		return nil
	}

	// Check origin repo (current behavior)
	lines := queryPRs(r, branch, owner, owner+"/"+repo)

	// Also check upstream repo if set (fork workflow)
	if upstreamRepo := os.Getenv("KELOS_UPSTREAM_REPO"); upstreamRepo != "" {
		lines = append(lines, queryPRs(r, branch, owner, upstreamRepo)...)
	}

	return lines
}

// originURLRE matches the HTTPS, ssh:// and scp-like forms of a git remote
// URL and captures the owner and repository name.
var originURLRE = regexp.MustCompile(`^(?:[a-z+]+://(?:[^@/]+@)?[^/]+/|[^@/:]+@[^:/]+:)([^/]+)/([^/]+?)(?:\.git)?/?$`)

// originOwnerRepo returns the owner and repository of the origin remote.
func originOwnerRepo(r runner) (owner, repo string, ok bool) {
	out, err := r.run("git", "remote", "get-url", "origin")
	if err != nil {
		return "", "", false
	}
	m := originURLRE.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// queryPRs lists open PRs in target ("owner/repo") whose head is
// headOwner:branch. It uses the REST API because "gh pr list" calls GraphQL.
// The endpoint is literal and the filters are raw fields (-f), which gh
// sends as given. gh expands {owner}, :owner and similar only in the
// endpoint and in -F fields, so a branch named "owner-fix" is safe.
func queryPRs(r runner, branch, headOwner, target string) []string {
	output, err := r.run("gh", "api", "repos/"+target+"/pulls",
		"--method", "GET",
		"-f", "head="+headOwner+":"+branch,
		"-f", "state=open",
		"--jq", "[.[]|{url:.html_url}]")
	if err != nil || output == "" {
		return nil
	}
	var prs []struct {
		URL string `json:"url"`
	}
	if json.Unmarshal([]byte(output), &prs) != nil {
		return nil
	}
	var lines []string
	for _, pr := range prs {
		if pr.URL != "" {
			lines = append(lines, "pr: "+pr.URL)
		}
	}
	return lines
}
