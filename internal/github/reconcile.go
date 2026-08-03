package github

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

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
		delivered, err := r.Client.pullRequestReachedMain(ctx, repository, delivery.PullRequestNumber)
		if err != nil {
			return marked, err
		}
		if !delivered {
			continue
		}
		if err := r.Store.MarkTicketDelivered(ctx, delivery.VersionID, delivery.IssueID); err != nil {
			return marked, err
		}
		marked++
	}
	return marked, nil
}

func (c *Client) pullRequestReachedMain(ctx context.Context, repository string, number int64) (bool, error) {
	var pull struct {
		State          string `json:"state"`
		MergedAt       string `json:"merged_at"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		Base           struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := c.getJSON(ctx, "/repos/"+repository+"/pulls/"+strconv.FormatInt(number, 10), &pull); err != nil {
		return false, err
	}
	if pull.State != "closed" || pull.MergedAt == "" || pull.MergeCommitSHA == "" || pull.Base.Ref != "main" {
		return false, nil
	}
	var comparison struct {
		Status string `json:"status"`
	}
	path := "/repos/" + repository + "/compare/" + url.PathEscape(pull.MergeCommitSHA) + "...main"
	if err := c.getJSON(ctx, path, &comparison); err != nil {
		return false, err
	}
	return comparison.Status == "behind" || comparison.Status == "identical", nil
}
