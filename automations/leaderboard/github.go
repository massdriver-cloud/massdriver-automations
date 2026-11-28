package leaderboard

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

//go:embed queries/pull_requests.graphql
var pullRequestsQuery string

const (
	pageSize = 50
	// maxPages bounds one query to 1,000 pull requests, so a busy repository with a long window can't use up the
	// token's rate limit.
	maxPages = 20
)

// Query selects the pull requests of one repository that were updated since a time.
type Query struct {
	Repo string
	// States is MERGED, or empty for every state.
	States []string
	Base   string
	// Files asks for the paths that each pull request changes.
	Files bool
	Since time.Time
}

// key is the same for queries that GitHub answers the same, so routes that share one run it once.
func (q Query) key() string {
	return fmt.Sprintf("%s|%v|%s|%t|%d", q.Repo, q.States, q.Base, q.Files, q.Since.Unix())
}

// GitHub reads pull requests.
type GitHub interface {
	PullRequests(ctx context.Context, q Query) ([]PullRequest, error)
}

// PullRequest is one pull request as GitHub returns it.
type PullRequest struct {
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	MergedAt  *time.Time `json:"mergedAt"`
	Additions int        `json:"additions"`
	Deletions int        `json:"deletions"`
	Labels    struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Author *Author `json:"author"`
	Files  *struct {
		Nodes []struct {
			Path string `json:"path"`
		} `json:"nodes"`
	} `json:"files"`
}

// Author is the account that opened a pull request. Name is empty for bots and for users without one.
type Author struct {
	Type      string `json:"__typename"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatarUrl"`
	Name      string `json:"name"`
}

// GraphQLGitHub calls the GitHub GraphQL API.
type GraphQLGitHub struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// PullRequests reads pages, most recently updated first, until a page reaches pull requests last updated before
// q.Since. A pull request merged or opened after q.Since was updated after it too, so the rest can't count.
func (g *GraphQLGitHub) PullRequests(ctx context.Context, q Query) ([]PullRequest, error) {
	owner, name, _ := strings.Cut(q.Repo, "/")
	vars := map[string]any{"owner": owner, "name": name, "first": pageSize, "files": q.Files}
	if len(q.States) > 0 {
		vars["states"] = q.States
	}
	if q.Base != "" {
		vars["base"] = q.Base
	}
	var out []PullRequest
	for range maxPages {
		page, err := g.page(ctx, vars)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Nodes...)
		n := len(page.Nodes)
		if !page.PageInfo.HasNextPage || n == 0 || page.Nodes[n-1].UpdatedAt.Before(q.Since) {
			return out, nil
		}
		vars["after"] = page.PageInfo.EndCursor
	}
	automation.Logger(ctx).Warn("github query truncated", "repo", q.Repo, "pull_requests", len(out))
	return out, nil
}

type pullRequestPage struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []PullRequest `json:"nodes"`
}

func (g *GraphQLGitHub) page(ctx context.Context, vars map[string]any) (*pullRequestPage, error) {
	body, err := json.Marshal(map[string]any{"query": pullRequestsQuery, "variables": vars})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+g.Token)

	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, automation.RedactURL(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: HTTP %d", resp.StatusCode)
	}

	var out struct {
		Data struct {
			Repository *struct {
				PullRequests pullRequestPage `json:"pullRequests"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("github: decode: %w", err)
	}
	if len(out.Errors) > 0 {
		msgs := make([]string, len(out.Errors))
		for i, e := range out.Errors {
			msgs[i] = e.Message
		}
		return nil, errors.New("github: " + strings.Join(msgs, "; "))
	}
	if out.Data.Repository == nil {
		return nil, errors.New("github: repository not found")
	}
	return &out.Data.Repository.PullRequests, nil
}
