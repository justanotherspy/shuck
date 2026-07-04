package ingest

import (
	"encoding/json"
	"fmt"
)

// Decision is Filter's verdict on one verified delivery.
type Decision struct {
	// Envelope is the work to enqueue. Filter leaves DeliveryID empty; the
	// handler stamps it from the X-GitHub-Delivery header.
	Envelope Envelope
	// Enqueue reports whether the delivery becomes work at all.
	Enqueue bool
	// Reason says why a delivery was dropped (for logs); empty when Enqueue.
	Reason string
}

// webhookPayload is the small subset of a GitHub webhook payload the filter
// inspects. Everything else in the payload is deliberately ignored — workers
// re-fetch what they need from the API.
type webhookPayload struct {
	Action string `json:"action"`
	Number int    `json:"number"` // pull_request events

	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	WorkflowRun struct {
		ID           int64  `json:"id"`
		Conclusion   string `json:"conclusion"`
		HeadSHA      string `json:"head_sha"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"workflow_run"`
	// PullRequest is the nested PR object review events carry (their PR
	// number is NOT the top-level "number" field).
	PullRequest struct {
		Number int `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
	Comment struct {
		ID   int64       `json:"id"`
		User webhookUser `json:"user"`
	} `json:"comment"`
	Review struct {
		ID   int64       `json:"id"`
		User webhookUser `json:"user"`
	} `json:"review"`
}

// webhookUser is the author of a review comment or review. The numeric ID
// is the authoritative identity (logins are mutable).
type webhookUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// drillableConclusions are the workflow_run conclusions that become
// ci_failure work. Cancelled runs are included for parity with the CLI,
// which drills cancelled jobs best-effort.
var drillableConclusions = map[string]bool{
	"failure":   true,
	"cancelled": true,
	"timed_out": true,
}

// Filter decides whether a verified delivery becomes work. event is the
// X-GitHub-Event header; body is the raw payload. The event table is the
// extension point for new kinds. An unparseable payload is an error;
// everything the table doesn't match is a silent drop with a reason.
func Filter(event string, body []byte) (Decision, error) {
	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return Decision{}, fmt.Errorf("parse %s payload: %w", event, err)
	}
	if p.Repository.FullName == "" {
		return drop("payload has no repository"), nil
	}
	switch event {
	case "workflow_run":
		return filterWorkflowRun(p), nil
	case "pull_request":
		return filterPullRequest(p), nil
	case "pull_request_review_comment":
		return filterReviewComment(p), nil
	case "pull_request_review":
		return filterReview(p), nil
	default:
		return drop(fmt.Sprintf("event %q not routed", event)), nil
	}
}

func filterWorkflowRun(p webhookPayload) Decision {
	if p.Action != "completed" {
		return drop(fmt.Sprintf("workflow_run action %q ignored", p.Action))
	}
	if !drillableConclusions[p.WorkflowRun.Conclusion] {
		return drop(fmt.Sprintf("workflow_run conclusion %q ignored", p.WorkflowRun.Conclusion))
	}
	if p.WorkflowRun.ID <= 0 {
		return drop("workflow_run payload has no run id")
	}
	// Fan-out is keyed repo#pr; a run with no PR association (e.g. a push
	// to main, or a fork PR where GitHub omits the link) has no subscribers
	// to reach.
	pr := 0
	for _, ref := range p.WorkflowRun.PullRequests {
		if ref.Number > 0 {
			pr = ref.Number
			break
		}
	}
	if pr == 0 {
		return drop("workflow_run not associated with a pull request")
	}
	return Decision{
		Enqueue: true,
		Envelope: Envelope{
			Schema:         EnvelopeSchema,
			Kind:           KindCIFailure,
			Repo:           p.Repository.FullName,
			PR:             pr,
			RunID:          p.WorkflowRun.ID,
			HeadSHA:        p.WorkflowRun.HeadSHA,
			InstallationID: p.Installation.ID,
		},
	}
}

func filterPullRequest(p webhookPayload) Decision {
	if p.Action != "closed" {
		return drop(fmt.Sprintf("pull_request action %q ignored", p.Action))
	}
	if p.Number <= 0 {
		return drop("pull_request payload has no number")
	}
	return Decision{
		Enqueue: true,
		Envelope: Envelope{
			Schema:         EnvelopeSchema,
			Kind:           KindPRClosed,
			Repo:           p.Repository.FullName,
			PR:             p.Number,
			InstallationID: p.Installation.ID,
		},
	}
}

// filterReviewComment routes pull_request_review_comment.created. Edits and
// deletions are ignored: the created comment is the notification, and the
// worker re-fetches the live object anyway. Missing identifiers fail closed —
// the deliver contract requires author.github_user_id on review kinds.
func filterReviewComment(p webhookPayload) Decision {
	if p.Action != "created" {
		return drop(fmt.Sprintf("pull_request_review_comment action %q ignored", p.Action))
	}
	if p.PullRequest.Number <= 0 {
		return drop("pull_request_review_comment payload has no pull request number")
	}
	if p.Comment.ID <= 0 {
		return drop("pull_request_review_comment payload has no comment id")
	}
	if p.Comment.User.ID <= 0 {
		return drop("pull_request_review_comment payload has no author id")
	}
	return Decision{
		Enqueue: true,
		Envelope: Envelope{
			Schema:         EnvelopeSchema,
			Kind:           KindReviewComment,
			Repo:           p.Repository.FullName,
			PR:             p.PullRequest.Number,
			HeadSHA:        p.PullRequest.Head.SHA,
			InstallationID: p.Installation.ID,
			CommentID:      p.Comment.ID,
			AuthorID:       p.Comment.User.ID,
			AuthorLogin:    p.Comment.User.Login,
		},
	}
}

// filterReview routes pull_request_review.submitted. Dismissals and edits
// are ignored. Missing identifiers fail closed, as for review comments.
func filterReview(p webhookPayload) Decision {
	if p.Action != "submitted" {
		return drop(fmt.Sprintf("pull_request_review action %q ignored", p.Action))
	}
	if p.PullRequest.Number <= 0 {
		return drop("pull_request_review payload has no pull request number")
	}
	if p.Review.ID <= 0 {
		return drop("pull_request_review payload has no review id")
	}
	if p.Review.User.ID <= 0 {
		return drop("pull_request_review payload has no author id")
	}
	return Decision{
		Enqueue: true,
		Envelope: Envelope{
			Schema:         EnvelopeSchema,
			Kind:           KindReview,
			Repo:           p.Repository.FullName,
			PR:             p.PullRequest.Number,
			HeadSHA:        p.PullRequest.Head.SHA,
			InstallationID: p.Installation.ID,
			ReviewID:       p.Review.ID,
			AuthorID:       p.Review.User.ID,
			AuthorLogin:    p.Review.User.Login,
		},
	}
}

func drop(reason string) Decision {
	return Decision{Reason: reason}
}
