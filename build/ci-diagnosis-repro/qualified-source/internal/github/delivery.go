package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/delivery"
	"github.com/skyhuang233/workflow/internal/store"
)

// GitPusher is kept separate from the REST client because candidate commits
// are already present in the Ticket Workspace and must be pushed through the
// gateway's controlled credential boundary.
type GitPusher interface {
	Push(context.Context, string, string, string, string, bool) error
}

type DeliveryRemote struct {
	Client           *Client
	Pusher           GitPusher
	Store            *store.Store
	Token            string
	PushURL          string
	CredentialSource func(context.Context) (string, error)
	mu               sync.RWMutex
}

func (r *DeliveryRemote) CredentialAvailable(ctx context.Context) error {
	if r.CredentialSource == nil {
		client := r.client()
		if client == nil || strings.TrimSpace(client.Token) == "" {
			return delivery.ErrGatewayCredentialRejected
		}
		return nil
	}
	token, err := r.CredentialSource(ctx)
	if err != nil {
		if errors.Is(err, credential.ErrNotFound) || errors.Is(err, delivery.ErrGatewayCredentialRejected) {
			return delivery.ErrGatewayCredentialRejected
		}
		return fmt.Errorf("load Control Plane GitHub credential: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return delivery.ErrGatewayCredentialRejected
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Client == nil {
		return errors.New("GitHub client is missing")
	}
	r.Token = token
	r.Client = NewClient(r.Client.BaseURL, token, r.Client.HTTP).WithRepositoryOwner(r.Client.RepositoryOwner)
	return nil
}

func (r *DeliveryRemote) client() *Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.Client
}

func (r *DeliveryRemote) Observe(ctx context.Context, request store.DeliveryRequest) (delivery.Observation, error) {
	client := r.client()
	if client == nil {
		return delivery.Observation{}, fmt.Errorf("GitHub client is missing")
	}
	if err := requireOwnerGuardedRepository(ctx, client, request.Repository); err != nil {
		return delivery.Observation{}, err
	}
	if request.Operation == store.DeliveryProjectPlan || request.Operation == store.DeliveryProjectInbox || request.Operation == store.DeliveryAddIssueLabel {
		if request.Operation == store.DeliveryProjectInbox {
			applied, err := client.HasWorkflowInboxProjection(ctx, request.Repository, request.WorkflowQuestions)
			if err != nil {
				return delivery.Observation{}, err
			}
			return delivery.Observation{Applied: applied}, nil
		}
		if request.PlanProjection == nil {
			return delivery.Observation{}, fmt.Errorf("plan projection is missing")
		}
		if request.Operation == store.DeliveryAddIssueLabel {
			return delivery.Observation{}, nil
		}
		applied, err := client.HasPlanProjection(ctx, request.Repository, request.RootNumber, *request.PlanProjection)
		if err != nil {
			return delivery.Observation{}, err
		}
		return delivery.Observation{Applied: applied}, nil
	}
	head, exists, err := client.branchHead(ctx, request.Repository, request.Branch)
	if err != nil {
		return delivery.Observation{}, err
	}
	observation := delivery.Observation{RemoteHead: head, RemoteExists: exists}
	if request.Operation == store.DeliveryPushCandidate {
		observation.Applied = head == request.CommitSHA
		return observation, nil
	}
	if request.Operation == store.DeliveryUpsertPR {
		pull, found, findErr := findPullRequest(ctx, client, request)
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
	comments, err := client.listIssueComments(ctx, request.Repository, request.PullRequestNumber)
	if err != nil {
		return observation, err
	}
	if err := client.requireRepositoryOwner(); err != nil {
		return observation, err
	}
	marker := "workflow-idempotency:" + request.IdempotencyKey
	for _, comment := range comments {
		if actionableAuthor(client.RepositoryOwner, comment.User.Login, comment.User.Type) && strings.Contains(comment.Body, marker) {
			observation.Applied = true
			break
		}
	}
	return observation, nil
}

func (r *DeliveryRemote) Apply(ctx context.Context, request store.DeliveryRequest) (delivery.Observation, error) {
	client := r.client()
	if client == nil {
		return delivery.Observation{}, fmt.Errorf("GitHub client is missing")
	}
	if err := requireOwnerGuardedRepository(ctx, client, request.Repository); err != nil {
		return delivery.Observation{}, err
	}
	switch request.Operation {
	case store.DeliveryPushCandidate:
		pusher, err := r.pusher(ctx, request, client.Token)
		if err != nil {
			return delivery.Observation{}, err
		}
		if pusher == nil {
			return delivery.Observation{}, fmt.Errorf("candidate push adapter is missing")
		}
		if err := pusher.Push(ctx, request.Repository, request.Branch, request.CommitSHA, request.ExpectedRemoteHead, request.ExpectRemoteAbsent); err != nil {
			return delivery.Observation{}, err
		}
		return delivery.Observation{Applied: true, RemoteHead: request.CommitSHA, RemoteExists: true}, nil
	case store.DeliveryUpsertPR:
		pull, found, err := findPullRequest(ctx, client, request)
		if err != nil {
			return delivery.Observation{}, err
		}
		if !found {
			payload := map[string]string{"title": request.Title, "head": request.Branch, "base": "main", "body": request.Body}
			if err := client.requestJSON(ctx, http.MethodPost, "/repos/"+request.Repository+"/pulls", payload, &pull); err != nil {
				return delivery.Observation{}, err
			}
		} else {
			payload := map[string]string{"title": request.Title, "body": request.Body}
			if err := client.requestJSON(ctx, http.MethodPatch, "/repos/"+request.Repository+"/pulls/"+strconv.FormatInt(pull.Number, 10), payload, &pull); err != nil {
				return delivery.Observation{}, err
			}
		}
		if err := validatePullRequest(pull, request); err != nil {
			return delivery.Observation{}, fmt.Errorf("%w: %v", store.ErrDeliveryRejected, err)
		}
		if pull.Head.SHA != request.CommitSHA {
			return delivery.Observation{}, fmt.Errorf("%w: pull request head %q does not match accepted candidate %q", store.ErrDeliveryRejected, pull.Head.SHA, request.CommitSHA)
		}
		return delivery.Observation{Applied: true, RemoteHead: request.CommitSHA, PullRequestNumber: pull.Number, PullRequestNodeID: pull.NodeID}, nil
	case store.DeliveryReplyEvidence:
		body := fmt.Sprintf("%s\n\n<!-- workflow-idempotency:%s -->", request.Evidence, request.IdempotencyKey)
		payload := map[string]string{"body": body}
		if err := client.requestJSON(ctx, http.MethodPost, "/repos/"+request.Repository+"/issues/"+strconv.FormatInt(request.PullRequestNumber, 10)+"/comments", payload, nil); err != nil {
			return delivery.Observation{}, err
		}
		return delivery.Observation{Applied: true, PullRequestNumber: request.PullRequestNumber}, nil
	case store.DeliveryProjectPlan:
		if request.PlanProjection == nil {
			return delivery.Observation{}, fmt.Errorf("plan projection is missing")
		}
		if err := client.UpdatePlanProjection(ctx, request.Repository, request.RootNumber, *request.PlanProjection); err != nil {
			return delivery.Observation{}, err
		}
		return delivery.Observation{Applied: true}, nil
	case store.DeliveryProjectInbox:
		if err := client.ProjectWorkflowInbox(ctx, request.Repository, request.WorkflowQuestions); err != nil {
			return delivery.Observation{}, err
		}
		return delivery.Observation{Applied: true}, nil
	case store.DeliveryAddIssueLabel:
		if request.PlanProjection == nil || request.Label == "" {
			return delivery.Observation{}, fmt.Errorf("plan label is incomplete")
		}
		if err := client.AddIssueLabel(ctx, request.Repository, request.RootNumber, request.Label); err != nil {
			return delivery.Observation{}, err
		}
		return delivery.Observation{Applied: true}, nil
	default:
		return delivery.Observation{}, fmt.Errorf("unsupported delivery operation %q", request.Operation)
	}
}

func (r *DeliveryRemote) pusher(ctx context.Context, request store.DeliveryRequest, token string) (GitPusher, error) {
	if r.Pusher != nil {
		return r.Pusher, nil
	}
	if r.Store == nil || token == "" {
		return nil, nil
	}
	workspace, err := r.Store.WorkspaceForRun(ctx, request.RunID)
	if err != nil {
		return nil, err
	}
	return WorkspacePusher{WorkspacePath: workspace, Token: token, PushURL: r.PushURL}, nil
}

func requireOwnerGuardedRepository(ctx context.Context, client *Client, repository string) error {
	if err := ValidateRepository(repository); err != nil {
		return fmt.Errorf("%w: %w", store.ErrDeliveryRejected, err)
	}
	if err := client.RequireOwnerGuardedRepository(ctx, repository); err != nil {
		if errors.Is(err, ErrRepositoryOwnerMismatch) {
			return fmt.Errorf("%w: %w", store.ErrDeliveryRejected, err)
		}
		return err
	}
	return nil
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
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}

func findPullRequest(ctx context.Context, client *Client, request store.DeliveryRequest) (pullRequestResponse, bool, error) {
	if request.PullRequestNumber != 0 {
		var pull pullRequestResponse
		err := client.getJSON(ctx, "/repos/"+request.Repository+"/pulls/"+strconv.FormatInt(request.PullRequestNumber, 10), &pull)
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
	if err := client.getJSON(ctx, path, &pulls); err != nil {
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
