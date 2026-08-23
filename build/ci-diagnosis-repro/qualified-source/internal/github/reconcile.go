package github

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	workerisolation "github.com/skyhuang233/workflow/internal/isolation"
	"github.com/skyhuang233/workflow/internal/store"
)

type DeliveredReconciler struct {
	Store    *store.Store
	Client   *Client
	Isolator ContainerIsolator
}

type ContainerIsolator interface {
	IsolateContainer(context.Context, string) error
}

func (r DeliveredReconciler) Reconcile(ctx context.Context, repository string) (int, error) {
	if r.Store == nil || r.Client == nil {
		return 0, fmt.Errorf("delivered reconciler dependencies are incomplete")
	}
	if err := ValidateRepository(repository); err != nil {
		return 0, err
	}
	deliveries, err := r.Store.PendingTicketDeliveries(ctx, repository)
	if err != nil {
		return 0, wrapPollStoreError(err)
	}
	marked := 0
	for _, delivery := range deliveries {
		state, err := r.reconcileTicket(ctx, delivery)
		if err != nil {
			return marked, err
		}
		if state == pullRequestDelivered {
			marked++
		}
	}
	return marked, nil
}

func (r DeliveredReconciler) ReconcileTicket(ctx context.Context, delivery store.TicketDelivery) (bool, error) {
	if r.Store == nil || r.Client == nil {
		return false, fmt.Errorf("delivered reconciler dependencies are incomplete")
	}
	if err := ValidateRepository(delivery.Repository); err != nil {
		return false, err
	}
	state, err := r.reconcileTicket(ctx, delivery)
	return state != pullRequestPending, err
}

func (r DeliveredReconciler) reconcileTicket(ctx context.Context, delivery store.TicketDelivery) (pullRequestState, error) {
	deliveryState, err := r.Client.pullRequestDelivery(ctx, delivery.Repository, delivery.PullRequestNumber, delivery.CandidateCommit)
	if err != nil {
		return pullRequestPending, err
	}
	switch deliveryState.State {
	case pullRequestDelivered:
		marked, err := r.markDelivered(ctx, delivery, deliveryState.MergeCommit)
		if err != nil {
			return pullRequestPending, wrapPollStoreError(err)
		}
		if !marked {
			return pullRequestPending, nil
		}
	case pullRequestClosedUnmerged:
		if err := r.freezeClosedPullRequest(ctx, delivery); err != nil {
			return pullRequestPending, wrapPollStoreError(err)
		}
	}
	return deliveryState.State, nil
}

func (r DeliveredReconciler) freezeClosedPullRequest(ctx context.Context, delivery store.TicketDelivery) error {
	now := time.Now().UTC()
	return workerisolation.RetryWorkerTransition(ctx, r.Store, r.Isolator, func(isolated []store.WorkerIsolationProof) error {
		_, err := r.Store.FreezePlanForClosedPullRequest(ctx, delivery.VersionID, delivery.IssueID, now, isolated...)
		return err
	})
}

func (r DeliveredReconciler) markDelivered(ctx context.Context, delivery store.TicketDelivery, mergeCommit string) (bool, error) {
	marked := false
	err := workerisolation.RetryWorkerTransition(ctx, r.Store, r.Isolator, func(isolated []store.WorkerIsolationProof) error {
		if len(isolated) == 0 {
			var err error
			marked, err = r.Store.MarkTicketDeliveredAtMerge(ctx, delivery.VersionID, delivery.IssueID, mergeCommit)
			return err
		}
		var err error
		marked, err = r.Store.MarkTicketDeliveredAtMergeAfterIsolation(ctx, delivery.VersionID, delivery.IssueID, mergeCommit, isolated[len(isolated)-1])
		return err
	})
	return marked, err
}

type pullRequestState int

const (
	pullRequestPending pullRequestState = iota
	pullRequestDelivered
	pullRequestClosedUnmerged
)

func (c *Client) pullRequestReachedMain(ctx context.Context, repository string, number int64, candidateCommit string) (bool, error) {
	deliveryState, err := c.pullRequestDelivery(ctx, repository, number, candidateCommit)
	return deliveryState.State == pullRequestDelivered, err
}

type pullRequestDeliveryResult struct {
	State       pullRequestState
	MergeCommit string
}

func (c *Client) pullRequestDelivery(ctx context.Context, repository string, number int64, candidateCommit string) (pullRequestDeliveryResult, error) {
	var pull struct {
		State          string       `json:"state"`
		MergedAt       string       `json:"merged_at"`
		MergeCommitSHA string       `json:"merge_commit_sha"`
		MergedBy       userResponse `json:"merged_by"`
		Base           struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := c.getJSON(ctx, "/repos/"+repository+"/pulls/"+strconv.FormatInt(number, 10), &pull); err != nil {
		return pullRequestDeliveryResult{}, err
	}
	if pull.State != "closed" {
		return pullRequestDeliveryResult{State: pullRequestPending}, nil
	}
	if pull.MergedAt == "" {
		return pullRequestDeliveryResult{State: pullRequestClosedUnmerged}, nil
	}
	if !actionableAuthor(c.RepositoryOwner, pull.MergedBy.Login, pull.MergedBy.Type) {
		return pullRequestDeliveryResult{State: pullRequestPending}, nil
	}
	if pull.MergeCommitSHA == "" || pull.Base.Ref != "main" || candidateCommit == "" {
		return pullRequestDeliveryResult{State: pullRequestPending}, nil
	}
	var comparison struct {
		Status string `json:"status"`
	}
	path := "/repos/" + repository + "/compare/" + url.PathEscape(candidateCommit) + "...main"
	if err := c.getJSON(ctx, path, &comparison); err != nil {
		return pullRequestDeliveryResult{}, err
	}
	if comparison.Status == "ahead" || comparison.Status == "identical" {
		return pullRequestDeliveryResult{State: pullRequestDelivered, MergeCommit: pull.MergeCommitSHA}, nil
	}
	return pullRequestDeliveryResult{State: pullRequestPending}, nil
}
