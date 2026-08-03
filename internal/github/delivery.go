package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/skyhuang233/workflow/internal/delivery"
	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

// GitPusher is kept separate from the REST client because candidate commits
// are already present in the Ticket Workspace and must be pushed through the
// gateway's controlled credential boundary.
type GitPusher interface {
	Push(context.Context, string, string, string, string, bool) error
}

type DeliveryRemote struct {
	Client *Client
	Pusher GitPusher
}

func (r DeliveryRemote) Observe(ctx context.Context, request store.DeliveryRequest) (delivery.Observation, error) {
	if r.Client == nil {
		return delivery.Observation{}, fmt.Errorf("GitHub client is missing")
	}
	if request.Operation == store.DeliveryProjectPlan || request.Operation == store.DeliveryAddIssueLabel {
		if request.PlanProjection == nil {
			return delivery.Observation{}, fmt.Errorf("plan projection is missing")
		}
		issue, err := r.Client.getIssue(ctx, request.Repository, request.RootNumber)
		if err != nil {
			return delivery.Observation{}, err
		}
		if request.Operation == store.DeliveryAddIssueLabel {
			for _, label := range issue.Labels {
				if label == request.Label {
					return delivery.Observation{Applied: true}, nil
				}
			}
			return delivery.Observation{}, nil
		}
		expected, err := plan.RenderProjection(issue.Body, *request.PlanProjection)
		if err != nil {
			return delivery.Observation{}, err
		}
		return delivery.Observation{Applied: issue.Body == expected}, nil
	}
	head, exists, err := r.Client.branchHead(ctx, request.Repository, request.Branch)
	if err != nil {
		return delivery.Observation{}, err
	}
	observation := delivery.Observation{RemoteHead: head, RemoteExists: exists}
	if request.Operation == store.DeliveryPushCandidate {
		observation.Applied = head == request.CommitSHA
		return observation, nil
	}
	if request.Operation == store.DeliveryUpsertPR {
		pull, found, findErr := r.findPullRequest(ctx, request)
		if findErr != nil {
			return observation, findErr
		}
		if found {
			observation.PullRequestNumber = pull.Number
			observation.PullRequestNodeID = pull.NodeID
			observation.Applied = pull.Head.SHA == request.CommitSHA && pull.Title == request.Title && pull.Body == request.Body
		}
		return observation, nil
	}
	comments, err := r.listComments(ctx, request.Repository, request.PullRequestNumber)
	if err != nil {
		return observation, err
	}
	marker := "workflow-idempotency:" + request.IdempotencyKey
	for _, comment := range comments {
		if strings.Contains(comment.Body, marker) {
			observation.Applied = true
			break
		}
	}
	return observation, nil
}

func (r DeliveryRemote) Apply(ctx context.Context, request store.DeliveryRequest) (delivery.Observation, error) {
	if r.Client == nil {
		return delivery.Observation{}, fmt.Errorf("GitHub client is missing")
	}
	switch request.Operation {
	case store.DeliveryPushCandidate:
		if r.Pusher == nil {
			return delivery.Observation{}, fmt.Errorf("candidate push adapter is missing")
		}
		if err := r.Pusher.Push(ctx, request.Repository, request.Branch, request.CommitSHA, request.ExpectedRemoteHead, request.ExpectRemoteAbsent); err != nil {
			return delivery.Observation{}, err
		}
		return delivery.Observation{Applied: true, RemoteHead: request.CommitSHA, RemoteExists: true}, nil
	case store.DeliveryUpsertPR:
		pull, found, err := r.findPullRequest(ctx, request)
		if err != nil {
			return delivery.Observation{}, err
		}
		if !found {
			payload := map[string]string{"title": request.Title, "head": request.Branch, "base": "main", "body": request.Body}
			if err := r.Client.requestJSON(ctx, http.MethodPost, "/repos/"+request.Repository+"/pulls", payload, &pull); err != nil {
				return delivery.Observation{}, err
			}
		} else {
			payload := map[string]string{"title": request.Title, "body": request.Body}
			if err := r.Client.requestJSON(ctx, http.MethodPatch, "/repos/"+request.Repository+"/pulls/"+strconv.FormatInt(pull.Number, 10), payload, &pull); err != nil {
				return delivery.Observation{}, err
			}
		}
		return delivery.Observation{Applied: true, RemoteHead: request.CommitSHA, PullRequestNumber: pull.Number, PullRequestNodeID: pull.NodeID}, nil
	case store.DeliveryReplyEvidence:
		body := fmt.Sprintf("%s\n\n<!-- workflow-idempotency:%s -->", request.Evidence, request.IdempotencyKey)
		payload := map[string]string{"body": body}
		if err := r.Client.requestJSON(ctx, http.MethodPost, "/repos/"+request.Repository+"/issues/"+strconv.FormatInt(request.PullRequestNumber, 10)+"/comments", payload, nil); err != nil {
			return delivery.Observation{}, err
		}
		return delivery.Observation{Applied: true, PullRequestNumber: request.PullRequestNumber}, nil
	case store.DeliveryProjectPlan:
		if request.PlanProjection == nil {
			return delivery.Observation{}, fmt.Errorf("plan projection is missing")
		}
		if err := r.Client.UpdatePlanProjection(ctx, request.Repository, request.RootNumber, *request.PlanProjection); err != nil {
			return delivery.Observation{}, err
		}
		return delivery.Observation{Applied: true}, nil
	case store.DeliveryAddIssueLabel:
		if request.PlanProjection == nil || request.Label == "" {
			return delivery.Observation{}, fmt.Errorf("plan label is incomplete")
		}
		if err := r.Client.AddIssueLabel(ctx, request.Repository, request.RootNumber, request.Label); err != nil {
			return delivery.Observation{}, err
		}
		return delivery.Observation{Applied: true}, nil
	default:
		return delivery.Observation{}, fmt.Errorf("unsupported delivery operation %q", request.Operation)
	}
}

type pullRequestResponse struct {
	Number int64  `json:"number"`
	NodeID string `json:"node_id"`
	State  string `json:"state"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Head   struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type commentResponse struct {
	Body string `json:"body"`
}

func (r DeliveryRemote) findPullRequest(ctx context.Context, request store.DeliveryRequest) (pullRequestResponse, bool, error) {
	if request.PullRequestNumber != 0 {
		var pull pullRequestResponse
		err := r.Client.getJSON(ctx, "/repos/"+request.Repository+"/pulls/"+strconv.FormatInt(request.PullRequestNumber, 10), &pull)
		if err != nil {
			return pullRequestResponse{}, false, err
		}
		if err := validatePullRequest(pull, request); err != nil {
			return pullRequestResponse{}, false, err
		}
		return pull, true, nil
	}
	var pulls []pullRequestResponse
	path := "/repos/" + request.Repository + "/pulls?state=all&head=" + url.QueryEscape(strings.Split(request.Repository, "/")[0]+":"+request.Branch) + "&per_page=100"
	if err := r.Client.getJSON(ctx, path, &pulls); err != nil {
		return pullRequestResponse{}, false, err
	}
	for _, pull := range pulls {
		if pull.Head.Ref == request.Branch {
			if err := validatePullRequest(pull, request); err != nil {
				return pullRequestResponse{}, false, err
			}
			return pull, true, nil
		}
	}
	return pullRequestResponse{}, false, nil
}

func validatePullRequest(pull pullRequestResponse, request store.DeliveryRequest) error {
	if pull.Head.Ref != request.Branch {
		return fmt.Errorf("mapped pull request head does not match ticket branch")
	}
	if pull.State != "open" {
		return fmt.Errorf("mapped pull request is not open")
	}
	if pull.Base.Ref != "main" {
		return fmt.Errorf("mapped pull request does not target main")
	}
	return nil
}

func (r DeliveryRemote) listComments(ctx context.Context, repository string, number int64) ([]commentResponse, error) {
	var comments []commentResponse
	for page := 1; ; page++ {
		var batch []commentResponse
		path := "/repos/" + repository + "/issues/" + strconv.FormatInt(number, 10) + "/comments?per_page=100&page=" + strconv.Itoa(page)
		if err := r.Client.getJSON(ctx, path, &batch); err != nil {
			return nil, err
		}
		comments = append(comments, batch...)
		if len(batch) < 100 {
			return comments, nil
		}
	}
}

func (c *Client) branchHead(ctx context.Context, repository, branch string) (string, bool, error) {
	var response struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	path := "/repos/" + repository + "/git/ref/heads/" + url.PathEscape(branch)
	if err := c.getJSON(ctx, path, &response); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	if response.Object.SHA == "" {
		return "", false, fmt.Errorf("GitHub returned an empty branch head")
	}
	return response.Object.SHA, true, nil
}
