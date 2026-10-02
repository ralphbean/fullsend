package jirapoll

import (
	"context"
	"testing"
	"time"

	"github.com/fullsend-ai/fullsend/internal/forge/jira"
	"github.com/fullsend-ai/fullsend/internal/normevent"
)

func TestDiscoveryAndNormalizationWithoutJiraMutation(t *testing.T) {
	client := newMockClient()
	client.roleMembership = map[string]string{"writer": "Developers"}
	created := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	issue := jira.Issue{ID: "123", Key: "FSENDAI-123", Fields: jira.IssueFields{Created: created, Labels: []string{"fullsend-dispatcher-test"}, Reporter: jira.User{AccountID: "writer", Active: true}}}
	events, covered, err := DiscoverIssueEvents(context.Background(), client, issue, "https://jira.example", time.Time{})
	if err != nil || len(events) != 1 || events[0].Type != "opened" || covered.IsZero() {
		t.Fatalf("discovery: %d events, covered %v: %v", len(events), covered, err)
	}
	parsed, err := ParseJiraTimestamp(created)
	if err != nil || !parsed.Equal(events[0].UpdatedAt) {
		t.Fatalf("timestamp: %v, %v", parsed, err)
	}
	normalized, err := NormalizeReference(context.Background(), client, events[0], "FSENDAI", "owner/repo")
	if err != nil || normalized.Repo != "owner/repo" || !normevent.IsWriteAuthorized(normalized.Actor.Role) {
		t.Fatalf("normalized event: %+v, %v", normalized, err)
	}
	if len(client.properties) != 0 {
		t.Fatal("discovery wrote Jira properties")
	}
}
