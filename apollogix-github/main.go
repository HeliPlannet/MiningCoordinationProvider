package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var envFileFlag = flag.String("env-file", ".env", "Path to .env file to load token from (relative paths are resolved against the binary's folder)")

func resolveEnvPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), path)
	}
	return path
}

func loadEnvFile(path string) map[string]string {
	envVars := make(map[string]string)
	data, err := os.ReadFile(path)
	if err != nil {
		return envVars
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			v := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
			envVars[k] = v
		}
	}
	return envVars
}

type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *RPCError   `json:"error,omitempty"`
}

type RPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

type ToolCallParam struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ToolCallResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

var tools = []Tool{
	{
		Name:        "get_authenticated_user",
		Description: "Get the profile details of the authenticated GitHub user associated with the token.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	},
	{
		Name:        "search_repositories",
		Description: "Search for GitHub repositories by query string.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "The search query (e.g. 'org:Apollogix', 'language:go', 'repo:owner/repo')",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "Number of results to return (default: 10, max: 100)",
				},
			},
			"required": []string{"query"},
		},
	},
	{
		Name:        "get_repository",
		Description: "Get detailed information about a GitHub repository.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner": map[string]interface{}{"type": "string", "description": "Repository owner / organization"},
				"repo":  map[string]interface{}{"type": "string", "description": "Repository name"},
			},
			"required": []string{"owner", "repo"},
		},
	},
	{
		Name:        "get_file_contents",
		Description: "Get the content of a file in a repository.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner": map[string]interface{}{"type": "string", "description": "Repository owner"},
				"repo":  map[string]interface{}{"type": "string", "description": "Repository name"},
				"path":  map[string]interface{}{"type": "string", "description": "Path to the file in the repository"},
				"ref":   map[string]interface{}{"type": "string", "description": "Branch, tag, or commit SHA (optional, defaults to default branch)"},
			},
			"required": []string{"owner", "repo", "path"},
		},
	},
	{
		Name:        "create_or_update_file",
		Description: "Create or update a file in a repository.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner":   map[string]interface{}{"type": "string", "description": "Repository owner"},
				"repo":    map[string]interface{}{"type": "string", "description": "Repository name"},
				"path":    map[string]interface{}{"type": "string", "description": "Path to the file"},
				"message": map[string]interface{}{"type": "string", "description": "Commit message"},
				"content": map[string]interface{}{"type": "string", "description": "New file content (plain text)"},
				"branch":  map[string]interface{}{"type": "string", "description": "Branch name (optional, defaults to default branch)"},
				"sha":     map[string]interface{}{"type": "string", "description": "Blob SHA if updating an existing file (optional)"},
			},
			"required": []string{"owner", "repo", "path", "message", "content"},
		},
	},
	{
		Name:        "list_commits",
		Description: "List recent commits in a repository.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner":  map[string]interface{}{"type": "string", "description": "Repository owner"},
				"repo":   map[string]interface{}{"type": "string", "description": "Repository name"},
				"sha":    map[string]interface{}{"type": "string", "description": "SHA or branch to start listing commits from (optional)"},
				"path":   map[string]interface{}{"type": "string", "description": "Only commits containing this file path (optional)"},
				"limit":  map[string]interface{}{"type": "integer", "description": "Number of commits to return (default: 10, max: 100)"},
			},
			"required": []string{"owner", "repo"},
		},
	},
	{
		Name:        "list_branches",
		Description: "List branches in a repository.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner": map[string]interface{}{"type": "string", "description": "Repository owner"},
				"repo":  map[string]interface{}{"type": "string", "description": "Repository name"},
				"limit": map[string]interface{}{"type": "integer", "description": "Number of branches to return (default: 30)"},
			},
			"required": []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_branch",
		Description: "Create a new branch from an existing branch or commit SHA.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner":       map[string]interface{}{"type": "string", "description": "Repository owner"},
				"repo":        map[string]interface{}{"type": "string", "description": "Repository name"},
				"branch":      map[string]interface{}{"type": "string", "description": "Name of the new branch"},
				"from_branch": map[string]interface{}{"type": "string", "description": "Source branch to base new branch on (e.g. 'develop', 'main')"},
				"from_sha":    map[string]interface{}{"type": "string", "description": "Direct commit SHA to base new branch on (optional)"},
			},
			"required": []string{"owner", "repo", "branch"},
		},
	},
	{
		Name:        "list_pull_requests",
		Description: "List pull requests in a repository. Returns compact JSON: number, title, state (open/closed/merged), draft, head, base, author, updated_at.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner": map[string]interface{}{"type": "string", "description": "Repository owner"},
				"repo":  map[string]interface{}{"type": "string", "description": "Repository name"},
				"state": map[string]interface{}{"type": "string", "enum": []string{"open", "closed", "all"}, "description": "State filter (default: 'open')"},
				"head":  map[string]interface{}{"type": "string", "description": "Filter by head user/branch (e.g. 'owner:feature-branch')"},
				"base":  map[string]interface{}{"type": "string", "description": "Filter by base branch (e.g. 'develop')"},
				"limit": map[string]interface{}{"type": "integer", "description": "Number of results (default: 10)"},
			},
			"required": []string{"owner", "repo"},
		},
	},
	{
		Name:        "get_pull_request",
		Description: "Get a pull request by number. Returns compact JSON: number, title, state (open/closed/merged), draft, head, base, author, updated_at. Use get_pr_review_context for body, files, review threads and checks.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner":       map[string]interface{}{"type": "string", "description": "Repository owner"},
				"repo":        map[string]interface{}{"type": "string", "description": "Repository name"},
				"pull_number": map[string]interface{}{"type": "integer", "description": "Pull request number"},
			},
			"required": []string{"owner", "repo", "pull_number"},
		},
	},
	{
		Name:        "get_pr_review_context",
		Description: "Everything needed to review or address feedback on a PR, as compact markdown from one GraphQL query: PR meta and body, changed files (+/-), review decision and latest review per reviewer, review threads with isResolved/isOutdated (comment ids usable as in_reply_to), and check status of the head commit.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner":                     map[string]interface{}{"type": "string", "description": "Repository owner"},
				"repo":                      map[string]interface{}{"type": "string", "description": "Repository name"},
				"pull_number":               map[string]interface{}{"type": "integer", "description": "Pull request number"},
				"include_resolved_comments": map[string]interface{}{"type": "boolean", "description": "Also print the comments of resolved threads (default: false; resolved threads are listed as one line)"},
			},
			"required": []string{"owner", "repo", "pull_number"},
		},
	},
	{
		Name:        "get_pr_diff",
		Description: "Unified diff of a pull request, optionally limited to one file or directory, truncated at max_bytes.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner":       map[string]interface{}{"type": "string", "description": "Repository owner"},
				"repo":        map[string]interface{}{"type": "string", "description": "Repository name"},
				"pull_number": map[string]interface{}{"type": "integer", "description": "Pull request number"},
				"path":        map[string]interface{}{"type": "string", "description": "Only files equal to this path or under this directory (optional)"},
				"max_bytes":   map[string]interface{}{"type": "integer", "description": "Maximum output size in bytes (default: 50000, max: 200000)"},
			},
			"required": []string{"owner", "repo", "pull_number"},
		},
	},
	{
		Name:        "add_pr_review_comment",
		Description: "Add an inline review comment on a PR line (path + line), or reply to an existing review thread (in_reply_to). Comments are posted publicly as the token owner.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner":       map[string]interface{}{"type": "string", "description": "Repository owner"},
				"repo":        map[string]interface{}{"type": "string", "description": "Repository name"},
				"pull_number": map[string]interface{}{"type": "integer", "description": "Pull request number"},
				"body":        map[string]interface{}{"type": "string", "description": "Comment markdown text"},
				"in_reply_to": map[string]interface{}{"type": "integer", "description": "Review comment id to reply to (from get_pr_review_context); path/line are then ignored"},
				"path":        map[string]interface{}{"type": "string", "description": "File path in the PR (required unless in_reply_to)"},
				"line":        map[string]interface{}{"type": "integer", "description": "Line in the diff to comment on; last line for a multi-line comment (required unless in_reply_to)"},
				"side":        map[string]interface{}{"type": "string", "enum": []string{"RIGHT", "LEFT"}, "description": "RIGHT = new code, LEFT = removed code (default: RIGHT)"},
				"start_line":  map[string]interface{}{"type": "integer", "description": "First line of a multi-line comment (optional)"},
				"commit_id":   map[string]interface{}{"type": "string", "description": "Commit SHA to anchor on (default: current PR head)"},
			},
			"required": []string{"owner", "repo", "pull_number", "body"},
		},
	},
	{
		Name:        "create_pull_request",
		Description: "Create a new pull request.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner": map[string]interface{}{"type": "string", "description": "Repository owner"},
				"repo":  map[string]interface{}{"type": "string", "description": "Repository name"},
				"title": map[string]interface{}{"type": "string", "description": "Pull request title"},
				"head":  map[string]interface{}{"type": "string", "description": "Branch containing changes"},
				"base":  map[string]interface{}{"type": "string", "description": "Target branch to merge into (e.g. 'develop')"},
				"body":  map[string]interface{}{"type": "string", "description": "Pull request description / markdown body"},
				"draft": map[string]interface{}{"type": "boolean", "description": "Whether to create PR as draft (default: false)"},
			},
			"required": []string{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner":        map[string]interface{}{"type": "string", "description": "Repository owner"},
				"repo":         map[string]interface{}{"type": "string", "description": "Repository name"},
				"pull_number":  map[string]interface{}{"type": "integer", "description": "Pull request number"},
				"commit_title": map[string]interface{}{"type": "string", "description": "Title for merge commit (optional)"},
				"merge_method": map[string]interface{}{"type": "string", "enum": []string{"merge", "squash", "rebase"}, "description": "Merge method (default: 'merge')"},
			},
			"required": []string{"owner", "repo", "pull_number"},
		},
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner": map[string]interface{}{"type": "string", "description": "Repository owner"},
				"repo":  map[string]interface{}{"type": "string", "description": "Repository name"},
				"state": map[string]interface{}{"type": "string", "enum": []string{"open", "closed", "all"}, "description": "State filter (default: 'open')"},
				"limit": map[string]interface{}{"type": "integer", "description": "Number of issues (default: 10)"},
			},
			"required": []string{"owner", "repo"},
		},
	},
	{
		Name:        "get_issue",
		Description: "Get issue details by issue number.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner":        map[string]interface{}{"type": "string", "description": "Repository owner"},
				"repo":         map[string]interface{}{"type": "string", "description": "Repository name"},
				"issue_number": map[string]interface{}{"type": "integer", "description": "Issue number"},
			},
			"required": []string{"owner", "repo", "issue_number"},
		},
	},
	{
		Name:        "create_issue",
		Description: "Create a new issue.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner":  map[string]interface{}{"type": "string", "description": "Repository owner"},
				"repo":   map[string]interface{}{"type": "string", "description": "Repository name"},
				"title":  map[string]interface{}{"type": "string", "description": "Issue title"},
				"body":   map[string]interface{}{"type": "string", "description": "Issue description / body"},
				"labels": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Labels to apply"},
			},
			"required": []string{"owner", "repo", "title"},
		},
	},
	{
		Name:        "add_issue_comment",
		Description: "Add a comment to an issue or pull request.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner":        map[string]interface{}{"type": "string", "description": "Repository owner"},
				"repo":         map[string]interface{}{"type": "string", "description": "Repository name"},
				"issue_number": map[string]interface{}{"type": "integer", "description": "Issue or PR number"},
				"body":         map[string]interface{}{"type": "string", "description": "Comment markdown text"},
			},
			"required": []string{"owner", "repo", "issue_number", "body"},
		},
	},
}

func getToken() string {
	token := os.Getenv("APOLLOGIX_GHPA_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_PERSONAL_ACCESS_TOKEN")
	}
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token == "" {
		envVars := loadEnvFile(resolveEnvPath(*envFileFlag))
		token = envVars["APOLLOGIX_GHPA_TOKEN"]
	}
	return token
}

func githubRequest(method, endpoint string, bodyData interface{}) (string, error) {
	token := getToken()
	if token == "" {
		return "", fmt.Errorf("GitHub token is missing. Please set GITHUB_PERSONAL_ACCESS_TOKEN or GITHUB_TOKEN environment variable")
	}

	urlStr := "https://api.github.com" + endpoint
	var bodyReader io.Reader
	if bodyData != nil {
		jsonBytes, err := json.Marshal(bodyData)
		if err != nil {
			return "", err
		}
		bodyReader = bytes.NewReader(jsonBytes)
	}

	req, err := http.NewRequest(method, urlStr, bodyReader)
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "Go-GitHub-MCP-Server/1.0")
	if bodyData != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("GitHub API error %d: %s", resp.StatusCode, string(respBytes))
	}

	return string(respBytes), nil
}

func getStringArg(args map[string]interface{}, key string) string {
	if val, ok := args[key]; ok && val != nil {
		if s, ok := val.(string); ok {
			return s
		}
		return fmt.Sprintf("%v", val)
	}
	return ""
}

func getIntArg(args map[string]interface{}, key string, def int) int {
	if val, ok := args[key]; ok && val != nil {
		switch v := val.(type) {
		case float64:
			return int(v)
		case int:
			return v
		case string:
			if parsed, err := strconv.Atoi(v); err == nil {
				return parsed
			}
		}
	}
	return def
}

func getBoolArg(args map[string]interface{}, key string, def bool) bool {
	if val, ok := args[key]; ok && val != nil {
		if b, ok := val.(bool); ok {
			return b
		}
	}
	return def
}

func executeTool(name string, args map[string]interface{}) (string, error) {
	switch name {
	case "get_authenticated_user":
		return githubRequest("GET", "/user", nil)

	case "search_repositories":
		q := getStringArg(args, "query")
		limit := getIntArg(args, "limit", 10)
		endpoint := fmt.Sprintf("/search/repositories?q=%s&per_page=%d", url.QueryEscape(q), limit)
		return githubRequest("GET", endpoint, nil)

	case "get_repository":
		owner := getStringArg(args, "owner")
		repo := getStringArg(args, "repo")
		return githubRequest("GET", fmt.Sprintf("/repos/%s/%s", owner, repo), nil)

	case "get_file_contents":
		owner := getStringArg(args, "owner")
		repo := getStringArg(args, "repo")
		path := getStringArg(args, "path")
		ref := getStringArg(args, "ref")
		endpoint := fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repo, strings.TrimPrefix(path, "/"))
		if ref != "" {
			endpoint += "?ref=" + url.QueryEscape(ref)
		}
		raw, err := githubRequest("GET", endpoint, nil)
		if err != nil {
			return "", err
		}
		// If base64 encoded content is present, decode it for readability
		var fileMeta struct {
			Encoding string `json:"encoding"`
			Content  string `json:"content"`
			Name     string `json:"name"`
			Path     string `json:"path"`
			SHA      string `json:"sha"`
			Size     int    `json:"size"`
		}
		if err := json.Unmarshal([]byte(raw), &fileMeta); err == nil && fileMeta.Encoding == "base64" {
			cleanBase64 := strings.ReplaceAll(fileMeta.Content, "\n", "")
			decoded, decodeErr := base64.StdEncoding.DecodeString(cleanBase64)
			if decodeErr == nil {
				return fmt.Sprintf("File: %s\nSHA: %s\nSize: %d bytes\n\nContent:\n%s", fileMeta.Path, fileMeta.SHA, fileMeta.Size, string(decoded)), nil
			}
		}
		return raw, nil

	case "create_or_update_file":
		owner := getStringArg(args, "owner")
		repo := getStringArg(args, "repo")
		path := getStringArg(args, "path")
		message := getStringArg(args, "message")
		content := getStringArg(args, "content")
		branch := getStringArg(args, "branch")
		sha := getStringArg(args, "sha")

		payload := map[string]interface{}{
			"message": message,
			"content": base64.StdEncoding.EncodeToString([]byte(content)),
		}
		if branch != "" {
			payload["branch"] = branch
		}
		if sha != "" {
			payload["sha"] = sha
		}
		endpoint := fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repo, strings.TrimPrefix(path, "/"))
		return githubRequest("PUT", endpoint, payload)

	case "list_commits":
		owner := getStringArg(args, "owner")
		repo := getStringArg(args, "repo")
		sha := getStringArg(args, "sha")
		path := getStringArg(args, "path")
		limit := getIntArg(args, "limit", 10)

		params := url.Values{}
		params.Set("per_page", strconv.Itoa(limit))
		if sha != "" {
			params.Set("sha", sha)
		}
		if path != "" {
			params.Set("path", path)
		}
		return githubRequest("GET", fmt.Sprintf("/repos/%s/%s/commits?%s", owner, repo, params.Encode()), nil)

	case "list_branches":
		owner := getStringArg(args, "owner")
		repo := getStringArg(args, "repo")
		limit := getIntArg(args, "limit", 30)
		return githubRequest("GET", fmt.Sprintf("/repos/%s/%s/branches?per_page=%d", owner, repo, limit), nil)

	case "create_branch":
		owner := getStringArg(args, "owner")
		repo := getStringArg(args, "repo")
		branch := getStringArg(args, "branch")
		fromBranch := getStringArg(args, "from_branch")
		fromSHA := getStringArg(args, "from_sha")

		targetSHA := fromSHA
		if targetSHA == "" {
			if fromBranch == "" {
				fromBranch = "develop"
			}
			refData, err := githubRequest("GET", fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", owner, repo, fromBranch), nil)
			if err != nil {
				return "", fmt.Errorf("failed to get base branch '%s': %w", fromBranch, err)
			}
			var refObj struct {
				Object struct {
					SHA string `json:"sha"`
				} `json:"object"`
			}
			if err := json.Unmarshal([]byte(refData), &refObj); err != nil {
				return "", fmt.Errorf("failed to parse ref data: %w", err)
			}
			targetSHA = refObj.Object.SHA
		}

		payload := map[string]interface{}{
			"ref": "refs/heads/" + branch,
			"sha": targetSHA,
		}
		return githubRequest("POST", fmt.Sprintf("/repos/%s/%s/git/refs", owner, repo), payload)

	case "list_pull_requests":
		owner := getStringArg(args, "owner")
		repo := getStringArg(args, "repo")
		state := getStringArg(args, "state")
		if state == "" {
			state = "open"
		}
		head := getStringArg(args, "head")
		base := getStringArg(args, "base")
		limit := getIntArg(args, "limit", 10)

		params := url.Values{}
		params.Set("state", state)
		params.Set("per_page", strconv.Itoa(limit))
		if head != "" {
			params.Set("head", head)
		}
		if base != "" {
			params.Set("base", base)
		}
		raw, err := githubRequest("GET", fmt.Sprintf("/repos/%s/%s/pulls?%s", owner, repo, params.Encode()), nil)
		if err != nil {
			return "", err
		}
		return slimPullRequests(raw)

	case "get_pull_request":
		owner := getStringArg(args, "owner")
		repo := getStringArg(args, "repo")
		pullNumber := getIntArg(args, "pull_number", 0)
		raw, err := githubRequest("GET", fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, pullNumber), nil)
		if err != nil {
			return "", err
		}
		return slimPullRequest(raw)

	case "get_pr_review_context":
		return getPRReviewContext(getStringArg(args, "owner"), getStringArg(args, "repo"),
			getIntArg(args, "pull_number", 0), getBoolArg(args, "include_resolved_comments", false))

	case "get_pr_diff":
		return getPRDiff(getStringArg(args, "owner"), getStringArg(args, "repo"),
			getIntArg(args, "pull_number", 0), getStringArg(args, "path"), getIntArg(args, "max_bytes", 50000))

	case "add_pr_review_comment":
		return addPRReviewComment(args)

	case "create_pull_request":
		owner := getStringArg(args, "owner")
		repo := getStringArg(args, "repo")
		title := getStringArg(args, "title")
		head := getStringArg(args, "head")
		base := getStringArg(args, "base")
		body := getStringArg(args, "body")
		draft := getBoolArg(args, "draft", false)

		payload := map[string]interface{}{
			"title": title,
			"head":  head,
			"base":  base,
			"body":  body,
			"draft": draft,
		}
		return githubRequest("POST", fmt.Sprintf("/repos/%s/%s/pulls", owner, repo), payload)

	case "merge_pull_request":
		owner := getStringArg(args, "owner")
		repo := getStringArg(args, "repo")
		pullNumber := getIntArg(args, "pull_number", 0)
		commitTitle := getStringArg(args, "commit_title")
		mergeMethod := getStringArg(args, "merge_method")
		if mergeMethod == "" {
			mergeMethod = "merge"
		}
		payload := map[string]interface{}{
			"merge_method": mergeMethod,
		}
		if commitTitle != "" {
			payload["commit_title"] = commitTitle
		}
		return githubRequest("PUT", fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", owner, repo, pullNumber), payload)

	case "list_issues":
		owner := getStringArg(args, "owner")
		repo := getStringArg(args, "repo")
		state := getStringArg(args, "state")
		if state == "" {
			state = "open"
		}
		limit := getIntArg(args, "limit", 10)
		return githubRequest("GET", fmt.Sprintf("/repos/%s/%s/issues?state=%s&per_page=%d", owner, repo, state, limit), nil)

	case "get_issue":
		owner := getStringArg(args, "owner")
		repo := getStringArg(args, "repo")
		issueNumber := getIntArg(args, "issue_number", 0)
		return githubRequest("GET", fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, issueNumber), nil)

	case "create_issue":
		owner := getStringArg(args, "owner")
		repo := getStringArg(args, "repo")
		title := getStringArg(args, "title")
		body := getStringArg(args, "body")
		payload := map[string]interface{}{
			"title": title,
			"body":  body,
		}
		if labelsVal, ok := args["labels"].([]interface{}); ok {
			payload["labels"] = labelsVal
		}
		return githubRequest("POST", fmt.Sprintf("/repos/%s/%s/issues", owner, repo), payload)

	case "add_issue_comment":
		owner := getStringArg(args, "owner")
		repo := getStringArg(args, "repo")
		issueNumber := getIntArg(args, "issue_number", 0)
		body := getStringArg(args, "body")
		payload := map[string]interface{}{
			"body": body,
		}
		return githubRequest("POST", fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, issueNumber), payload)

	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func main() {
	flag.Parse()

	scanner := bufio.NewScanner(os.Stdin)
	// Allow large line buffer up to 16MB for file diffs / content
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var req JSONRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			sendError(nil, -32700, "Parse error: "+err.Error())
			continue
		}

		handleRequest(&req)
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		fmt.Fprintf(os.Stderr, "Scanner error: %v\n", err)
	}
}

func handleRequest(req *JSONRPCRequest) {
	switch req.Method {
	case "initialize":
		result := map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{},
			},
			"serverInfo": map[string]interface{}{
				"name":    "github-mcp-go",
				"version": "1.0.0",
			},
		}
		sendResult(req.ID, result)

	case "notifications/initialized":
		// No response required for notifications

	case "ping":
		sendResult(req.ID, map[string]interface{}{})

	case "tools/list":
		sendResult(req.ID, map[string]interface{}{
			"tools": tools,
		})

	case "tools/call":
		var callParam ToolCallParam
		if err := json.Unmarshal(req.Params, &callParam); err != nil {
			sendError(req.ID, -32602, "Invalid params: "+err.Error())
			return
		}

		textResult, err := executeTool(callParam.Name, callParam.Arguments)
		if err != nil {
			sendResult(req.ID, ToolCallResult{
				Content: []ContentItem{
					{Type: "text", Text: "Error: " + err.Error()},
				},
				IsError: true,
			})
			return
		}

		sendResult(req.ID, ToolCallResult{
			Content: []ContentItem{
				{Type: "text", Text: textResult},
			},
			IsError: false,
		})

	default:
		sendError(req.ID, -32601, fmt.Sprintf("Method not found: %s", req.Method))
	}
}

func sendResult(id interface{}, result interface{}) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	data, _ := json.Marshal(resp)
	fmt.Fprintf(os.Stdout, "%s\n", data)
}

func sendError(id interface{}, code int, message string) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &RPCError{
			Code:    code,
			Message: message,
		},
	}
	data, _ := json.Marshal(resp)
	fmt.Fprintf(os.Stdout, "%s\n", data)
}
