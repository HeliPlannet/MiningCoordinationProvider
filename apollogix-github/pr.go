package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

type slimPR struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Draft     bool   `json:"draft"`
	Head      string `json:"head"`
	Base      string `json:"base"`
	Author    string `json:"author"`
	UpdatedAt string `json:"updated_at"`
}

type restPR struct {
	Number    int     `json:"number"`
	Title     string  `json:"title"`
	State     string  `json:"state"`
	Draft     bool    `json:"draft"`
	MergedAt  *string `json:"merged_at"`
	UpdatedAt string  `json:"updated_at"`
	Head      struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (p restPR) slim() slimPR {
	state := p.State
	// REST reports merged PRs as "closed"; merged_at is what tells them apart.
	if p.MergedAt != nil {
		state = "merged"
	}
	return slimPR{p.Number, p.Title, state, p.Draft, p.Head.Ref, p.Base.Ref, p.User.Login, p.UpdatedAt}
}

func slimPullRequests(raw string) (string, error) {
	var prs []restPR
	if err := json.Unmarshal([]byte(raw), &prs); err != nil {
		return "", fmt.Errorf("failed to parse pull requests: %w", err)
	}
	out := make([]slimPR, 0, len(prs))
	for _, p := range prs {
		out = append(out, p.slim())
	}
	data, err := json.Marshal(out)
	return string(data), err
}

func slimPullRequest(raw string) (string, error) {
	var p restPR
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return "", fmt.Errorf("failed to parse pull request: %w", err)
	}
	data, err := json.Marshal(p.slim())
	return string(data), err
}

func githubGraphQL(query string, variables map[string]interface{}, out interface{}) error {
	raw, err := githubRequest("POST", "/graphql", map[string]interface{}{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return fmt.Errorf("failed to parse GraphQL response: %w", err)
	}
	// GraphQL reports errors with HTTP 200, so they must be checked in the body.
	if len(resp.Errors) > 0 {
		msgs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("GitHub GraphQL error: %s", strings.Join(msgs, "; "))
	}
	return json.Unmarshal(resp.Data, out)
}

const prReviewContextQuery = `query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      number title url state isDraft merged mergeable reviewDecision body
      author { login }
      baseRefName headRefName headRefOid createdAt updatedAt
      additions deletions changedFiles
      files(first: 100) {
        totalCount
        nodes { path additions deletions changeType }
      }
      reviews(last: 50) {
        nodes { author { login } state submittedAt }
      }
      reviewThreads(first: 100) {
        totalCount
        nodes {
          isResolved isOutdated path line originalLine startLine diffSide
          comments(first: 30) {
            totalCount
            nodes { databaseId author { login } body createdAt }
          }
        }
      }
      commits(last: 1) {
        nodes {
          commit {
            statusCheckRollup {
              state
              contexts(first: 100) {
                totalCount
                nodes {
                  __typename
                  ... on CheckRun { name status conclusion detailsUrl }
                  ... on StatusContext { context state targetUrl }
                }
              }
            }
          }
        }
      }
    }
  }
}`

type login struct {
	Login string `json:"login"`
}

func (l *login) name() string {
	if l == nil || l.Login == "" {
		return "ghost"
	}
	return l.Login
}

type prReviewContext struct {
	Repository struct {
		PullRequest *struct {
			Number         int    `json:"number"`
			Title          string `json:"title"`
			URL            string `json:"url"`
			State          string `json:"state"`
			IsDraft        bool   `json:"isDraft"`
			Merged         bool   `json:"merged"`
			Mergeable      string `json:"mergeable"`
			ReviewDecision string `json:"reviewDecision"`
			Body           string `json:"body"`
			Author         *login `json:"author"`
			BaseRefName    string `json:"baseRefName"`
			HeadRefName    string `json:"headRefName"`
			HeadRefOid     string `json:"headRefOid"`
			CreatedAt      string `json:"createdAt"`
			UpdatedAt      string `json:"updatedAt"`
			Additions      int    `json:"additions"`
			Deletions      int    `json:"deletions"`
			ChangedFiles   int    `json:"changedFiles"`
			Files          struct {
				TotalCount int `json:"totalCount"`
				Nodes      []struct {
					Path       string `json:"path"`
					Additions  int    `json:"additions"`
					Deletions  int    `json:"deletions"`
					ChangeType string `json:"changeType"`
				} `json:"nodes"`
			} `json:"files"`
			Reviews struct {
				Nodes []struct {
					Author      *login `json:"author"`
					State       string `json:"state"`
					SubmittedAt string `json:"submittedAt"`
				} `json:"nodes"`
			} `json:"reviews"`
			ReviewThreads struct {
				TotalCount int `json:"totalCount"`
				Nodes      []struct {
					IsResolved   bool   `json:"isResolved"`
					IsOutdated   bool   `json:"isOutdated"`
					Path         string `json:"path"`
					Line         *int   `json:"line"`
					OriginalLine *int   `json:"originalLine"`
					StartLine    *int   `json:"startLine"`
					DiffSide     string `json:"diffSide"`
					Comments     struct {
						TotalCount int `json:"totalCount"`
						Nodes      []struct {
							DatabaseID int64  `json:"databaseId"`
							Author     *login `json:"author"`
							Body       string `json:"body"`
							CreatedAt  string `json:"createdAt"`
						} `json:"nodes"`
					} `json:"comments"`
				} `json:"nodes"`
			} `json:"reviewThreads"`
			Commits struct {
				Nodes []struct {
					Commit struct {
						StatusCheckRollup *struct {
							State    string `json:"state"`
							Contexts struct {
								TotalCount int `json:"totalCount"`
								Nodes      []struct {
									Typename    string `json:"__typename"`
									Name        string `json:"name"`
									Status      string `json:"status"`
									Conclusion  string `json:"conclusion"`
									DetailsURL  string `json:"detailsUrl"`
									Context     string `json:"context"`
									StatusState string `json:"state"`
									TargetURL   string `json:"targetUrl"`
								} `json:"nodes"`
							} `json:"contexts"`
						} `json:"statusCheckRollup"`
					} `json:"commit"`
				} `json:"nodes"`
			} `json:"commits"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

func truncateText(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if len(s) <= max {
		return s
	}
	cut := utf8Cut(s, max)
	return s[:cut] + fmt.Sprintf("… [truncated %d bytes]", len(s)-cut)
}

// utf8Cut backs off to a rune boundary so truncation never splits a multi-byte character.
func utf8Cut(s string, max int) int {
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return max
}

func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}

func getPRReviewContext(owner, repo string, number int, includeResolved bool) (string, error) {
	var data prReviewContext
	vars := map[string]interface{}{"owner": owner, "repo": repo, "number": number}
	if err := githubGraphQL(prReviewContextQuery, vars, &data); err != nil {
		return "", err
	}
	pr := data.Repository.PullRequest
	if pr == nil {
		return "", fmt.Errorf("pull request %s/%s#%d not found", owner, repo, number)
	}

	var b strings.Builder
	state := strings.ToLower(pr.State)
	if pr.IsDraft {
		state += ", draft"
	}
	fmt.Fprintf(&b, "# PR #%d: %s\n", pr.Number, pr.Title)
	fmt.Fprintf(&b, "- %s · %s ← %s · author %s · %s\n", state, pr.BaseRefName, pr.HeadRefName, pr.Author.name(), pr.URL)
	fmt.Fprintf(&b, "- head %s · mergeable %s · review %s\n", pr.HeadRefOid, strings.ToLower(pr.Mergeable), orDash(pr.ReviewDecision))
	fmt.Fprintf(&b, "- created %s · updated %s · %d files +%d -%d\n", pr.CreatedAt, pr.UpdatedAt, pr.ChangedFiles, pr.Additions, pr.Deletions)
	if body := truncateText(pr.Body, 3000); body != "" {
		fmt.Fprintf(&b, "\n## Description\n%s\n", body)
	}

	fmt.Fprintf(&b, "\n## Files (%d)\n", pr.Files.TotalCount)
	for _, f := range pr.Files.Nodes {
		fmt.Fprintf(&b, "- %s +%d -%d (%s)\n", f.Path, f.Additions, f.Deletions, strings.ToLower(f.ChangeType))
	}
	if pr.Files.TotalCount > len(pr.Files.Nodes) {
		fmt.Fprintf(&b, "- … %d more files not listed; use get_pr_diff with path\n", pr.Files.TotalCount-len(pr.Files.Nodes))
	}

	// Keep only each reviewer's latest review, which is what GitHub counts.
	latest := map[string]string{}
	var reviewers []string
	for _, r := range pr.Reviews.Nodes {
		name := r.Author.name()
		if _, seen := latest[name]; !seen {
			reviewers = append(reviewers, name)
		} else if r.State == "COMMENTED" {
			continue
		}
		latest[name] = fmt.Sprintf("%s %s", strings.ToLower(r.State), r.SubmittedAt)
	}
	if len(reviewers) > 0 {
		b.WriteString("\n## Reviews\n")
		for _, name := range reviewers {
			fmt.Fprintf(&b, "- %s: %s\n", name, latest[name])
		}
	}

	threads := pr.ReviewThreads.Nodes
	unresolved := 0
	for _, t := range threads {
		if !t.IsResolved {
			unresolved++
		}
	}
	// Unresolved threads first: they are what still needs action.
	sort.SliceStable(threads, func(i, j int) bool { return !threads[i].IsResolved && threads[j].IsResolved })
	fmt.Fprintf(&b, "\n## Review threads (%d, %d unresolved)\n", pr.ReviewThreads.TotalCount, unresolved)
	for _, t := range threads {
		status := "UNRESOLVED"
		if t.IsResolved {
			status = "resolved"
		}
		if t.IsOutdated {
			status += ", outdated"
		}
		loc := t.Path
		if t.Line != nil {
			loc += fmt.Sprintf(":%d", *t.Line)
			if t.StartLine != nil && *t.StartLine != *t.Line {
				loc = fmt.Sprintf("%s:%d-%d", t.Path, *t.StartLine, *t.Line)
			}
		} else if t.OriginalLine != nil {
			loc += fmt.Sprintf(":%d (original)", *t.OriginalLine)
		}
		if t.DiffSide == "LEFT" {
			loc += " LEFT"
		}
		fmt.Fprintf(&b, "\n### [%s] %s\n", status, loc)
		if t.IsResolved && !includeResolved {
			if len(t.Comments.Nodes) > 0 {
				c := t.Comments.Nodes[0]
				fmt.Fprintf(&b, "- %d comments, first by %s (id %d)\n", t.Comments.TotalCount, c.Author.name(), c.DatabaseID)
			}
			continue
		}
		for _, c := range t.Comments.Nodes {
			fmt.Fprintf(&b, "- **%s** %s (id %d):\n%s\n", c.Author.name(), c.CreatedAt, c.DatabaseID, indent(truncateText(c.Body, 1500), "  "))
		}
		if t.Comments.TotalCount > len(t.Comments.Nodes) {
			fmt.Fprintf(&b, "- … %d more comments\n", t.Comments.TotalCount-len(t.Comments.Nodes))
		}
	}
	if pr.ReviewThreads.TotalCount > len(threads) {
		fmt.Fprintf(&b, "\n… %d more threads not fetched\n", pr.ReviewThreads.TotalCount-len(threads))
	}

	b.WriteString("\n## Checks\n")
	if len(pr.Commits.Nodes) == 0 || pr.Commits.Nodes[0].Commit.StatusCheckRollup == nil {
		b.WriteString("- none reported for head commit\n")
		return b.String(), nil
	}
	rollup := pr.Commits.Nodes[0].Commit.StatusCheckRollup
	passed := 0
	var other []string
	for _, c := range rollup.Contexts.Nodes {
		if c.Typename == "StatusContext" {
			if c.StatusState == "SUCCESS" {
				passed++
				continue
			}
			other = append(other, fmt.Sprintf("- %s: %s %s", c.Context, strings.ToLower(c.StatusState), c.TargetURL))
			continue
		}
		if c.Conclusion == "SUCCESS" || c.Conclusion == "SKIPPED" || c.Conclusion == "NEUTRAL" {
			passed++
			continue
		}
		result := c.Conclusion
		if result == "" {
			result = c.Status
		}
		other = append(other, fmt.Sprintf("- %s: %s %s", c.Name, strings.ToLower(result), c.DetailsURL))
	}
	fmt.Fprintf(&b, "- overall %s · %d of %d passed/skipped\n", strings.ToLower(rollup.State), passed, rollup.Contexts.TotalCount)
	for _, line := range other {
		b.WriteString(line + "\n")
	}
	return b.String(), nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return strings.ToLower(s)
}

type prFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Patch            string `json:"patch"`
}

func matchesPath(file, filter string) bool {
	filter = strings.Trim(filter, "/")
	return filter == "" || file == filter || strings.HasPrefix(file, filter+"/")
}

func getPRDiff(owner, repo string, number int, path string, maxBytes int) (string, error) {
	if maxBytes <= 0 {
		maxBytes = 50000
	}
	if maxBytes > 200000 {
		maxBytes = 200000
	}

	// The files endpoint caps at 3000 files (30 pages of 100).
	var files []prFile
	for page := 1; page <= 30; page++ {
		raw, err := githubRequest("GET", fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=100&page=%d", owner, repo, number, page), nil)
		if err != nil {
			return "", err
		}
		var batch []prFile
		if err := json.Unmarshal([]byte(raw), &batch); err != nil {
			return "", fmt.Errorf("failed to parse PR files: %w", err)
		}
		files = append(files, batch...)
		if len(batch) < 100 {
			break
		}
	}

	var b strings.Builder
	matched := 0
	for _, f := range files {
		if !matchesPath(f.Filename, path) && !(f.PreviousFilename != "" && matchesPath(f.PreviousFilename, path)) {
			continue
		}
		matched++
		oldName := f.Filename
		if f.PreviousFilename != "" {
			oldName = f.PreviousFilename
		}
		from, to := "a/"+oldName, "b/"+f.Filename
		if f.Status == "added" {
			from = "/dev/null"
		}
		if f.Status == "removed" {
			to = "/dev/null"
		}
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n# %s +%d -%d\n--- %s\n+++ %s\n", oldName, f.Filename, f.Status, f.Additions, f.Deletions, from, to)
		if f.Patch == "" {
			b.WriteString("(no textual patch: binary, rename only, or too large for the API)\n")
		} else {
			b.WriteString(f.Patch + "\n")
		}
	}
	if matched == 0 {
		if path != "" {
			return "", fmt.Errorf("no changed file matches path %q (%d files in PR)", path, len(files))
		}
		return "PR has no changed files", nil
	}

	out := b.String()
	if len(out) > maxBytes {
		cut := utf8Cut(out, maxBytes)
		out = out[:cut] + fmt.Sprintf("\n… [truncated %d bytes; narrow with path or raise max_bytes]", len(out)-cut)
	}
	return out, nil
}

func addPRReviewComment(args map[string]interface{}) (string, error) {
	owner := getStringArg(args, "owner")
	repo := getStringArg(args, "repo")
	number := getIntArg(args, "pull_number", 0)
	body := getStringArg(args, "body")
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("body is required")
	}

	var raw string
	var err error
	if replyTo := getIntArg(args, "in_reply_to", 0); replyTo != 0 {
		raw, err = githubRequest("POST", fmt.Sprintf("/repos/%s/%s/pulls/%d/comments/%d/replies", owner, repo, number, replyTo),
			map[string]interface{}{"body": body})
	} else {
		path := getStringArg(args, "path")
		line := getIntArg(args, "line", 0)
		if path == "" || line <= 0 {
			return "", fmt.Errorf("path and line are required unless in_reply_to is set")
		}
		side := strings.ToUpper(getStringArg(args, "side"))
		if side == "" {
			side = "RIGHT"
		}
		commitID := getStringArg(args, "commit_id")
		if commitID == "" {
			prRaw, err := githubRequest("GET", fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number), nil)
			if err != nil {
				return "", err
			}
			var pr struct {
				Head struct {
					SHA string `json:"sha"`
				} `json:"head"`
			}
			if err := json.Unmarshal([]byte(prRaw), &pr); err != nil {
				return "", fmt.Errorf("failed to parse pull request: %w", err)
			}
			commitID = pr.Head.SHA
		}
		payload := map[string]interface{}{"body": body, "commit_id": commitID, "path": path, "line": line, "side": side}
		if start := getIntArg(args, "start_line", 0); start > 0 && start < line {
			payload["start_line"] = start
			payload["start_side"] = side
		}
		raw, err = githubRequest("POST", fmt.Sprintf("/repos/%s/%s/pulls/%d/comments", owner, repo, number), payload)
	}
	if err != nil {
		return "", err
	}

	var c struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
		Path    string `json:"path"`
		Line    *int   `json:"line"`
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return raw, nil
	}
	out, err := json.Marshal(c)
	return string(out), err
}
