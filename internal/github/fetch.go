package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Snapshot is everything a sync reads from one repository.
type Snapshot struct {
	Repo        Repo
	Milestones  []Milestone
	Issues      []Issue // pull requests are not included
	Discussions []Discussion
	// NoBoardStatus is set when the token cannot read Projects boards (it
	// lacks the read:project scope); BoardStatus is then empty everywhere.
	NoBoardStatus bool
}

// Milestone is a GitHub milestone.
type Milestone struct {
	Number      int
	Title       string
	Description string
	State       string // OPEN or CLOSED
	URL         string
}

// Issue is a GitHub issue with what a sync needs about it.
type Issue struct {
	Number      int
	Title       string
	Body        string
	State       string // OPEN or CLOSED
	StateReason string // COMPLETED, NOT_PLANNED, DUPLICATE, REOPENED or ""
	URL         string
	Milestone   int // 0 when the issue has no milestone
	Labels      []string
	Assignees   []string
	// PullRequests are the pull requests linked to close this issue.
	PullRequests []PullRequest
	// BoardStatus is the value of the "Status" field on the Projects boards the
	// issue is on, such as "In Progress". Empty when it is on none.
	BoardStatus []string
}

// Closed reports whether the issue is closed.
func (i Issue) Closed() bool { return strings.EqualFold(i.State, "CLOSED") }

// ClosedAsCompleted reports whether the issue was closed as done rather than
// as not planned or a duplicate.
func (i Issue) ClosedAsCompleted() bool {
	if !i.Closed() {
		return false
	}
	switch strings.ToUpper(i.StateReason) {
	case "NOT_PLANNED", "DUPLICATE":
		return false
	}
	return true
}

// PullRequest is a pull request linked to an issue.
type PullRequest struct {
	Number      int
	Title       string
	URL         string
	State       string // OPEN, CLOSED or MERGED
	MergeCommit string // SHA, when merged
}

// Merged reports whether the pull request was merged.
func (p PullRequest) Merged() bool { return strings.EqualFold(p.State, "MERGED") }

// Open reports whether the pull request is still open.
func (p PullRequest) Open() bool { return strings.EqualFold(p.State, "OPEN") }

// Discussion is a GitHub discussion. Skald only links to them.
type Discussion struct {
	Number   int
	Title    string
	URL      string
	Category string
	Closed   bool
	Answered bool
}

// Limits on what one sync reads, so a huge repository cannot run away.
const (
	pageSize       = 50
	MaxIssues      = 5000
	MaxMilestones  = 500
	MaxDiscussions = 1000
)

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

const milestonesQuery = `query($owner:String!,$name:String!,$cursor:String){
  repository(owner:$owner,name:$name){
    milestones(first:50,after:$cursor,states:[OPEN,CLOSED],orderBy:{field:NUMBER,direction:ASC}){
      pageInfo{hasNextPage endCursor}
      nodes{number title description state url}
    }
  }
}`

const issuesQuery = `query($owner:String!,$name:String!,$cursor:String){
  repository(owner:$owner,name:$name){
    issues(first:50,after:$cursor,states:[OPEN,CLOSED],orderBy:{field:CREATED_AT,direction:ASC}){
      pageInfo{hasNextPage endCursor}
      nodes{
        number title body state stateReason url
        milestone{number}
        labels(first:20){nodes{name}}
        assignees(first:10){nodes{login}}
        closedByPullRequestsReferences(first:10,includeClosedPrs:true){
          nodes{number title url state mergeCommit{oid}}
        }
        %s
      }
    }
  }
}`

// boardFields reads the Status field of the Projects boards an issue is on.
// It needs the read:project scope, so FetchRepo drops it when that is missing.
const boardFields = `projectItems(first:10){
          nodes{fieldValueByName(name:"Status"){... on ProjectV2ItemFieldSingleSelectValue{name}}}
        }`

// missingProjectScope reports whether err is GitHub refusing the board fields
// for lack of the read:project scope.
func missingProjectScope(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	m := strings.ToLower(ae.Message)
	return strings.Contains(m, "read:project") || strings.Contains(m, "projectitems")
}

const discussionsQuery = `query($owner:String!,$name:String!,$cursor:String){
  repository(owner:$owner,name:$name){
    hasDiscussionsEnabled
    discussions(first:50,after:$cursor,orderBy:{field:CREATED_AT,direction:ASC}){
      pageInfo{hasNextPage endCursor}
      nodes{number title url closed isAnswered category{name}}
    }
  }
}`

// FetchRepo reads milestones, issues (with linked pull requests and board
// status) and discussions. Discussions are skipped when the repository has
// them turned off.
func (c *Client) FetchRepo(ctx context.Context, repo Repo) (Snapshot, error) {
	snap := Snapshot{Repo: repo}
	vars := func(cursor string) map[string]any {
		v := map[string]any{"owner": repo.Owner, "name": repo.Name}
		if cursor != "" {
			v["cursor"] = cursor
		}
		return v
	}

	for cursor := ""; ; {
		var out struct {
			Repository *struct {
				Milestones struct {
					PageInfo pageInfo `json:"pageInfo"`
					Nodes    []struct {
						Number      int    `json:"number"`
						Title       string `json:"title"`
						Description string `json:"description"`
						State       string `json:"state"`
						URL         string `json:"url"`
					} `json:"nodes"`
				} `json:"milestones"`
			} `json:"repository"`
		}
		if err := c.graphql(ctx, milestonesQuery, vars(cursor), &out); err != nil {
			return Snapshot{}, err
		}
		if out.Repository == nil {
			return Snapshot{}, &APIError{Status: http.StatusNotFound, Message: fmt.Sprintf("repository %s not found", repo)}
		}
		for _, m := range out.Repository.Milestones.Nodes {
			snap.Milestones = append(snap.Milestones, Milestone{Number: m.Number, Title: m.Title, Description: m.Description, State: m.State, URL: m.URL})
		}
		pi := out.Repository.Milestones.PageInfo
		if !pi.HasNextPage || len(snap.Milestones) >= MaxMilestones {
			break
		}
		cursor = pi.EndCursor
	}

	for cursor := ""; ; {
		var out struct {
			Repository *struct {
				Issues struct {
					PageInfo pageInfo    `json:"pageInfo"`
					Nodes    []issueNode `json:"nodes"`
				} `json:"issues"`
			} `json:"repository"`
		}
		board := boardFields
		if snap.NoBoardStatus {
			board = ""
		}
		if err := c.graphql(ctx, fmt.Sprintf(issuesQuery, board), vars(cursor), &out); err != nil {
			if !snap.NoBoardStatus && missingProjectScope(err) {
				snap.NoBoardStatus = true
				continue // same page again, without the board fields
			}
			return Snapshot{}, err
		}
		if out.Repository == nil {
			return Snapshot{}, &APIError{Status: http.StatusNotFound, Message: fmt.Sprintf("repository %s not found", repo)}
		}
		for _, n := range out.Repository.Issues.Nodes {
			snap.Issues = append(snap.Issues, n.issue())
		}
		pi := out.Repository.Issues.PageInfo
		if !pi.HasNextPage || len(snap.Issues) >= MaxIssues {
			break
		}
		cursor = pi.EndCursor
	}

	for cursor := ""; ; {
		var out struct {
			Repository *struct {
				Enabled     bool `json:"hasDiscussionsEnabled"`
				Discussions struct {
					PageInfo pageInfo `json:"pageInfo"`
					Nodes    []struct {
						Number   int    `json:"number"`
						Title    string `json:"title"`
						URL      string `json:"url"`
						Closed   bool   `json:"closed"`
						Answered bool   `json:"isAnswered"`
						Category *struct {
							Name string `json:"name"`
						} `json:"category"`
					} `json:"nodes"`
				} `json:"discussions"`
			} `json:"repository"`
		}
		if err := c.graphql(ctx, discussionsQuery, vars(cursor), &out); err != nil {
			// Discussions are optional: a repository (or GitHub Enterprise
			// version) without them must not fail the whole sync.
			break
		}
		if out.Repository == nil || !out.Repository.Enabled {
			break
		}
		for _, d := range out.Repository.Discussions.Nodes {
			cat := ""
			if d.Category != nil {
				cat = d.Category.Name
			}
			snap.Discussions = append(snap.Discussions, Discussion{Number: d.Number, Title: d.Title, URL: d.URL, Category: cat, Closed: d.Closed, Answered: d.Answered})
		}
		pi := out.Repository.Discussions.PageInfo
		if !pi.HasNextPage || len(snap.Discussions) >= MaxDiscussions {
			break
		}
		cursor = pi.EndCursor
	}
	return snap, nil
}

type issueNode struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	State       string `json:"state"`
	StateReason string `json:"stateReason"`
	URL         string `json:"url"`
	Milestone   *struct {
		Number int `json:"number"`
	} `json:"milestone"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Assignees struct {
		Nodes []struct {
			Login string `json:"login"`
		} `json:"nodes"`
	} `json:"assignees"`
	ClosedBy struct {
		Nodes []struct {
			Number      int    `json:"number"`
			Title       string `json:"title"`
			URL         string `json:"url"`
			State       string `json:"state"`
			MergeCommit *struct {
				OID string `json:"oid"`
			} `json:"mergeCommit"`
		} `json:"nodes"`
	} `json:"closedByPullRequestsReferences"`
	ProjectItems struct {
		Nodes []struct {
			Status *struct {
				Name string `json:"name"`
			} `json:"fieldValueByName"`
		} `json:"nodes"`
	} `json:"projectItems"`
}

func (n issueNode) issue() Issue {
	is := Issue{Number: n.Number, Title: n.Title, Body: n.Body, State: n.State, StateReason: n.StateReason, URL: n.URL}
	if n.Milestone != nil {
		is.Milestone = n.Milestone.Number
	}
	for _, l := range n.Labels.Nodes {
		is.Labels = append(is.Labels, l.Name)
	}
	for _, a := range n.Assignees.Nodes {
		is.Assignees = append(is.Assignees, a.Login)
	}
	for _, p := range n.ClosedBy.Nodes {
		pr := PullRequest{Number: p.Number, Title: p.Title, URL: p.URL, State: p.State}
		if p.MergeCommit != nil {
			pr.MergeCommit = p.MergeCommit.OID
		}
		is.PullRequests = append(is.PullRequests, pr)
	}
	for _, it := range n.ProjectItems.Nodes {
		if it.Status != nil && it.Status.Name != "" {
			is.BoardStatus = append(is.BoardStatus, it.Status.Name)
		}
	}
	return is
}

// CloseIssue closes an issue with reason "completed" or "not_planned",
// posting comment first when it is not empty.
func (c *Client) CloseIssue(ctx context.Context, repo Repo, number int, reason, comment string) error {
	base := fmt.Sprintf("%s/repos/%s/%s/issues/%d", c.RESTURL, repo.Owner, repo.Name, number)
	if comment != "" {
		if err := c.do(ctx, http.MethodPost, base+"/comments", map[string]string{"body": comment}, nil); err != nil {
			return err
		}
	}
	return c.do(ctx, http.MethodPatch, base, map[string]string{"state": "closed", "state_reason": reason}, nil)
}

// CloseMilestone closes a milestone.
func (c *Client) CloseMilestone(ctx context.Context, repo Repo, number int) error {
	url := fmt.Sprintf("%s/repos/%s/%s/milestones/%d", c.RESTURL, repo.Owner, repo.Name, number)
	return c.do(ctx, http.MethodPatch, url, map[string]string{"state": "closed"}, nil)
}
