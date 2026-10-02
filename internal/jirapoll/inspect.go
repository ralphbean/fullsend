package jirapoll

import (
	"context"
	"time"

	"github.com/fullsend-ai/fullsend/internal/forge/jira"
	"github.com/fullsend-ai/fullsend/internal/normevent"
)

// DiscoverIssueEvents reads Jira activity without writing Jira checkpoint or
// lock properties. The caller owns durable candidate and checkpoint commits.
func DiscoverIssueEvents(ctx context.Context, client JiraClient, issue jira.Issue, baseURL string, since time.Time) ([]JiraEvent, time.Time, error) {
	p := New(client, nil, Options{JiraBaseURL: baseURL})
	result, err := p.detectChanges(ctx, issue, since)
	return result.events, result.maxSeen, err
}

// NormalizeReference refetches actor authorization from Jira project roles
// and converts one already-refetched activity reference to a normalized event.
func NormalizeReference(ctx context.Context, client JiraClient, event JiraEvent, project, repo string) (normevent.Event, error) {
	p := New(client, nil, Options{JiraProject: project, TargetRepo: repo})
	p.roleMembership = make(map[string]string)
	p.roleGroups = make(map[string][]string)
	p.roleGroupsChecked = make(map[string]bool)
	self, err := client.GetMyself(ctx)
	if err != nil {
		return normevent.Event{}, err
	}
	p.selfAccountID = self.AccountID
	if err = p.loadRoleStructure(ctx, project); err != nil {
		return normevent.Event{}, err
	}
	if id := actorID(event); id != "" {
		if err = p.resolveActorRoles(ctx, []string{id}); err != nil {
			return normevent.Event{}, err
		}
	}
	return p.toNormalizedEvent(event), nil
}
