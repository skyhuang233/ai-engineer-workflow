package github

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
)

type DeliveredReconciler struct {
	Store  *store.Store
	Client *Client
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
		return 0, err
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
	state, err := r.Client.pullRequestDeliveryState(ctx, delivery.Repository, delivery.PullRequestNumber, delivery.CandidateCommit)
	if err != nil {
		return pullRequestPending, err
	}
	switch state {
	case pullRequestDelivered:
		if err := r.Store.MarkTicketDelivered(ctx, delivery.VersionID, delivery.IssueID); err != nil {
			return pullRequestPending, err
		}
	case pullRequestClosedUnmerged:
		if _, err := r.Store.FreezePlanForClosedPullRequest(ctx, delivery.VersionID, delivery.IssueID, time.Now().UTC()); err != nil {
			return pullRequestPending, err
		}
	}
	return state, nil
}

type pullRequestState int

const (
	pullRequestPending pullRequestState = iota
	pullRequestDelivered
	pullRequestClosedUnmerged
)

func (c *Client) pullRequestReachedMain(ctx context.Context, repository string, number int64, candidateCommit string) (bool, error) {
	state, err := c.pullRequestDeliveryState(ctx, repository, number, candidateCommit)
	return state == pullRequestDelivered, err
}

func (c *Client) pullRequestDeliveryState(ctx context.Context, repository string, number int64, candidateCommit string) (pullRequestState, error) {
	var pull struct {
		State          string `json:"state"`
		MergedAt       string `json:"merged_at"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		Base           struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := c.getJSON(ctx, "/repos/"+repository+"/pulls/"+strconv.FormatInt(number, 10), &pull); err != nil {
		return pullRequestPending, err
	}
	if pull.State != "closed" {
		return pullRequestPending, nil
	}
	if pull.MergedAt == "" {
		return pullRequestClosedUnmerged, nil
	}
	if pull.MergeCommitSHA == "" || pull.Base.Ref != "main" || candidateCommit == "" {
		return pullRequestPending, nil
	}
	var comparison struct {
		Status string `json:"status"`
	}
	path := "/repos/" + repository + "/compare/" + url.PathEscape(candidateCommit) + "...main"
	if err := c.getJSON(ctx, path, &comparison); err != nil {
		return pullRequestPending, err
	}
	if comparison.Status == "ahead" || comparison.Status == "identical" {
		return pullRequestDelivered, nil
	}
	return pullRequestPending, nil
}
