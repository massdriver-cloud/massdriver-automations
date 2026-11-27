package prsync

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

// GitHub reads the pull requests of one repository.
type GitHub interface {
	PullRequests(ctx context.Context, repo string) ([]PullRequest, error)
}

// PullRequest is one pull request as GitHub returns it, open or merged.
type PullRequest struct {
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	URL            string     `json:"url"`
	IsDraft        bool       `json:"isDraft"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	MergedAt       *time.Time `json:"mergedAt"`
	ReviewDecision string     `json:"reviewDecision"`
	IsInMergeQueue bool       `json:"isInMergeQueue"`
	Author         *struct {
		Login string `json:"login"`
	} `json:"author"`
	AutoMergeRequest *struct {
		EnabledAt time.Time `json:"enabledAt"`
	} `json:"autoMergeRequest"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// checks is the combined state of the head commit's checks and statuses, such as SUCCESS or PENDING, or "" when
// the commit has none.
func (p PullRequest) checks() string {
	n := p.Commits.Nodes
	if len(n) == 0 || n[0].Commit.StatusCheckRollup == nil {
		return ""
	}
	return n[0].Commit.StatusCheckRollup.State
}

// GraphQLGitHub calls the GitHub GraphQL API.
type GraphQLGitHub struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// PullRequests runs one query for the repository's open pull requests and its most recently updated merged ones.
func (g *GraphQLGitHub) PullRequests(ctx context.Context, repo string) ([]PullRequest, error) {
	owner, name, _ := strings.Cut(repo, "/")
	body, err := json.Marshal(map[string]any{"query": pullRequestsQuery, "variables": map[string]any{
		"owner": owner, "name": name, "first": maxPRsLimit,
	}})
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
				Open   struct{ Nodes []PullRequest } `json:"open"`
				Merged struct{ Nodes []PullRequest } `json:"merged"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
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
	return append(out.Data.Repository.Open.Nodes, out.Data.Repository.Merged.Nodes...), nil
}
