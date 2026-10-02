package k8spoc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/fullsend-ai/fullsend/internal/forge/jira"
	"github.com/fullsend-ai/fullsend/internal/harness"
	"github.com/fullsend-ai/fullsend/internal/harnessdispatch"
	"github.com/fullsend-ai/fullsend/internal/jirapoll"
	"github.com/jackc/pgx/v5"
)

type liveCandidate struct {
	id             int64
	key, entity    string
	revision, kind string
	content        []byte
}

func pendingJira(ctx context.Context, conn *pgx.Conn) ([]liveCandidate, error) {
	rows, err := conn.Query(ctx, `SELECT o.id,o.observation_key,o.entity,o.revision,o.kind,c.content
		FROM observations o JOIN config_revisions c ON c.digest=o.config_digest
		WHERE o.selection_state='pending' AND o.source LIKE 'jira:%' ORDER BY o.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []liveCandidate
	for rows.Next() {
		var c liveCandidate
		if err = rows.Scan(&c.id, &c.key, &c.entity, &c.revision, &c.kind, &c.content); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func selectPendingJira(ctx context.Context, conn *pgx.Conn) error {
	candidates, err := pendingJira(ctx, conn)
	if err != nil || len(candidates) == 0 {
		return err
	}
	tokenPath := os.Getenv("JIRA_TOKEN_FILE")
	emailPath := os.Getenv("JIRA_EMAIL_FILE")
	if tokenPath == "" || emailPath == "" {
		// The fixture dispatcher shares this database but has no Jira
		// credentials. The live dispatcher mounts both paths explicitly.
		if tokenPath == "" && emailPath == "" {
			return nil
		}
		return fmt.Errorf("JIRA_TOKEN_FILE and JIRA_EMAIL_FILE must be set together")
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return err
	}
	email, err := os.ReadFile(emailPath)
	if err != nil {
		return err
	}
	clients := make(map[string]*jira.LiveClient)
	return selectJiraCandidates(ctx, conn, candidates, func(baseURL string) (jirapoll.JiraClient, error) {
		if client := clients[baseURL]; client != nil {
			return client, nil
		}
		client, err := jira.New(strings.TrimSpace(string(token)), jira.WithBaseURL(baseURL), jira.WithEmail(strings.TrimSpace(string(email))))
		if err == nil {
			clients[baseURL] = client
		}
		return client, err
	})
}

func selectJiraCandidates(ctx context.Context, conn *pgx.Conn, candidates []liveCandidate, clientFor func(string) (jirapoll.JiraClient, error)) error {
	var problems []error
	for _, candidate := range candidates {
		var cfg LiveConfig
		if err := json.Unmarshal(candidate.content, &cfg); err != nil {
			if markErr := markSelection(ctx, conn, candidate.id, "evaluation_error", err.Error(), nil); markErr != nil {
				problems = append(problems, fmt.Errorf("observation %d config: %w", candidate.id, markErr))
			}
			continue
		}
		if err := cfg.validate(); err != nil {
			if markErr := markSelection(ctx, conn, candidate.id, "evaluation_error", err.Error(), nil); markErr != nil {
				problems = append(problems, fmt.Errorf("observation %d config: %w", candidate.id, markErr))
			}
			continue
		}
		client, err := clientFor(cfg.JiraBaseURL)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if err = selectJiraCandidate(ctx, conn, client, candidate); err != nil {
			problems = append(problems, fmt.Errorf("observation %d: %w", candidate.id, err))
		}
	}
	return errors.Join(problems...)
}

func selectJiraCandidate(ctx context.Context, conn *pgx.Conn, client jirapoll.JiraClient, candidate liveCandidate) error {
	var cfg LiveConfig
	if err := json.Unmarshal(candidate.content, &cfg); err != nil {
		return markSelection(ctx, conn, candidate.id, "evaluation_error", err.Error(), nil)
	}
	if err := cfg.validate(); err != nil {
		return markSelection(ctx, conn, candidate.id, "evaluation_error", err.Error(), nil)
	}
	issue, err := client.GetIssue(ctx, candidate.entity)
	if err != nil {
		return err
	}
	if issue == nil || issue.Key != candidate.entity || !strings.HasPrefix(issue.Key, cfg.Project+"-") || !slices.Contains(issue.Fields.Labels, cfg.Label) {
		return markSelection(ctx, conn, candidate.id, "denied", "issue moved or left configured scope", nil)
	}
	revision, err := time.Parse(time.RFC3339Nano, candidate.revision)
	if err != nil {
		return markSelection(ctx, conn, candidate.id, "evaluation_error", err.Error(), nil)
	}
	var event *jirapoll.JiraEvent
	if candidate.kind == "opened" {
		created, parseErr := jirapoll.ParseJiraTimestamp(issue.Fields.Created)
		if parseErr != nil {
			return parseErr
		}
		if created.Equal(revision) {
			event = &jirapoll.JiraEvent{Type: "opened", IssueID: issue.ID, IssueKey: issue.Key,
				IssueURL:  strings.TrimRight(cfg.JiraBaseURL, "/") + "/browse/" + issue.Key,
				UpdatedAt: created, Labels: issue.Fields.Labels, Reporter: issue.Fields.Reporter}
		}
	} else {
		events, _, discoverErr := jirapoll.DiscoverIssueEvents(ctx, client, *issue, cfg.JiraBaseURL, revision.Add(-time.Nanosecond))
		if discoverErr != nil {
			return discoverErr
		}
		for i := range events {
			key := events[i].Key() + "@" + events[i].UpdatedAt.UTC().Format(time.RFC3339Nano)
			if key == candidate.key && events[i].Type == candidate.kind {
				event = &events[i]
				break
			}
		}
	}
	if event == nil {
		return markSelection(ctx, conn, candidate.id, "evaluation_error", "Jira event no longer refetchable", nil)
	}
	ne, err := jirapoll.NormalizeReference(ctx, client, *event, cfg.Project, cfg.Repo)
	if err != nil {
		return err
	}
	if !harnessdispatch.IsAuthorized(&ne) {
		return markSelection(ctx, conn, candidate.id, "denied", "actor lacks write authorization", nil)
	}
	eventMap, err := ne.ToMap()
	if err != nil {
		return markSelection(ctx, conn, candidate.id, "evaluation_error", err.Error(), nil)
	}
	var matches []string
	for _, h := range cfg.Harnesses {
		matched, evalErr := harness.EvaluateTrigger(h.Trigger, eventMap)
		if evalErr != nil {
			return markSelection(ctx, conn, candidate.id, "evaluation_error", evalErr.Error(), nil)
		}
		if matched {
			matches = append(matches, h.Name)
		}
	}
	if len(matches) == 0 {
		return markSelection(ctx, conn, candidate.id, "no_match", "", nil)
	}
	return markSelection(ctx, conn, candidate.id, "selected", "", matches)
}

func markSelection(ctx context.Context, conn *pgx.Conn, id int64, state, detail string, names []string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE observations SET selection_state=$2,selection_error=$3 WHERE id=$1 AND selection_state='pending'`, id, state, detail)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}
	for _, name := range names {
		if _, err = tx.Exec(ctx, `INSERT INTO selected_runs(observation_id,harness) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, name); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
