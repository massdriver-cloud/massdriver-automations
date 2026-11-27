package paramstogit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// githubError names the operation, not the URL: the path holds the repository and the file, which come from config.
type githubError struct {
	op      string
	status  int
	message string
	limited bool
}

func (e *githubError) Error() string {
	return fmt.Sprintf("github: %s: HTTP %d: %s", e.op, e.status, e.message)
}

// retryable reports whether sending the event again can succeed. A conflict means the branch moved under the
// write. A permission or validation error stays the same on every attempt.
func retryable(err error) bool {
	var ge *githubError
	if !errors.As(err, &ge) {
		return true
	}
	return ge.limited || ge.status == http.StatusConflict || ge.status == http.StatusTooManyRequests || ge.status >= 500
}

type github struct {
	url   string
	token string
	http  *http.Client
}

type person struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type file struct {
	exists  bool
	sha     string
	content []byte
}

func repoPath(repo string, parts ...string) string {
	p := "/repos/" + repo
	for _, part := range parts {
		p += "/" + escapePath(part)
	}
	return p
}

func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

func (g *github) getFile(ctx context.Context, repo, branch, path string) (file, error) {
	var out struct {
		SHA      string `json:"sha"`
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	err := g.do(ctx, "get file", http.MethodGet, repoPath(repo, "contents", path)+"?ref="+url.QueryEscape(branch), nil, &out)
	var ge *githubError
	if errors.As(err, &ge) && ge.status == http.StatusNotFound {
		return file{}, nil
	}
	if err != nil {
		return file{}, err
	}
	if out.Encoding != "base64" {
		return file{}, &githubError{op: "get file", status: http.StatusOK, message: "content is not base64"}
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return file{}, err
	}
	return file{exists: true, sha: out.SHA, content: b}, nil
}

func (g *github) putFile(ctx context.Context, repo, branch, path, sha, message string, content []byte, author *person) error {
	in := map[string]any{"message": message, "content": base64.StdEncoding.EncodeToString(content), "branch": branch}
	if sha != "" {
		in["sha"] = sha
	}
	if author != nil {
		in["author"] = author
	}
	return g.do(ctx, "put file", http.MethodPut, repoPath(repo, "contents", path), in, nil)
}

func (g *github) deleteFile(ctx context.Context, repo, branch, path, sha, message string, author *person) error {
	in := map[string]any{"message": message, "sha": sha, "branch": branch}
	if author != nil {
		in["author"] = author
	}
	return g.do(ctx, "delete file", http.MethodDelete, repoPath(repo, "contents", path), in, nil)
}

// branchSHA returns the commit at the head of branch, or "" when the branch does not exist.
func (g *github) branchSHA(ctx context.Context, repo, branch string) (string, error) {
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := g.do(ctx, "get ref", http.MethodGet, repoPath(repo, "git", "ref", "heads/"+branch), nil, &out)
	var ge *githubError
	if errors.As(err, &ge) && ge.status == http.StatusNotFound {
		return "", nil
	}
	return out.Object.SHA, err
}

func (g *github) createBranch(ctx context.Context, repo, branch, sha string) error {
	return g.do(ctx, "create ref", http.MethodPost, repoPath(repo, "git", "refs"),
		map[string]any{"ref": "refs/heads/" + branch, "sha": sha}, nil)
}

func (g *github) resetBranch(ctx context.Context, repo, branch, sha string) error {
	return g.do(ctx, "update ref", http.MethodPatch, repoPath(repo, "git", "refs", "heads/"+branch),
		map[string]any{"sha": sha, "force": true}, nil)
}

type pull struct {
	Number int `json:"number"`
}

// openPull returns the open pull request from head into base, or nil.
func (g *github) openPull(ctx context.Context, repo, head, base string) (*pull, error) {
	owner, _, _ := strings.Cut(repo, "/")
	q := url.Values{"state": {"open"}, "head": {owner + ":" + head}, "base": {base}}
	var out []pull
	if err := g.do(ctx, "list pulls", http.MethodGet, repoPath(repo, "pulls")+"?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

func (g *github) createPull(ctx context.Context, repo, head, base, title, body string) (*pull, error) {
	var out pull
	err := g.do(ctx, "create pull", http.MethodPost, repoPath(repo, "pulls"),
		map[string]any{"title": title, "head": head, "base": base, "body": body}, &out)
	return &out, err
}

func (g *github) requestReviewers(ctx context.Context, repo string, number int, users, teams []string) error {
	in := map[string]any{"reviewers": users, "team_reviewers": teams}
	if users == nil {
		in["reviewers"] = []string{}
	}
	if teams == nil {
		in["team_reviewers"] = []string{}
	}
	return g.do(ctx, "request reviewers", http.MethodPost, repoPath(repo, "pulls", strconv.Itoa(number), "requested_reviewers"), in, nil)
}

func (g *github) do(ctx context.Context, op, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.url+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "application/vnd.github+json")
	req.Header.Set("x-github-api-version", "2022-11-28")
	req.Header.Set("authorization", "Bearer "+g.token)
	if in != nil {
		req.Header.Set("content-type", "application/json")
	}

	resp, err := g.http.Do(req)
	if err != nil {
		// The error of the HTTP client quotes the URL.
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("github: %s: %w", op, ue.Err)
		}
		return fmt.Errorf("github: %s: %w", op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
		return &githubError{
			op:      op,
			status:  resp.StatusCode,
			message: e.Message,
			limited: resp.StatusCode == http.StatusForbidden && resp.Header.Get("x-ratelimit-remaining") == "0",
		}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("github: %s: decode: %w", op, err)
	}
	return nil
}
