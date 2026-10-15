package k8spoc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/fullsend-ai/fullsend/internal/forge/jira"
	"github.com/fullsend-ai/fullsend/internal/harness"
	"github.com/fullsend-ai/fullsend/internal/jirapoll"
	"github.com/jackc/pgx/v5"
)

var (
	projectKeyPattern  = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)
	labelPattern       = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,99}$`)
	repoPattern        = regexp.MustCompile(`^[^/\s]+/[^/\s]+$`)
	gitRevisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	repoBundlePattern  = regexp.MustCompile(`^fullsend-live-repo-[0-9a-f]{16}$`)
)

// LiveConfig is the validated, immutable selection snapshot for one Jira source.
type LiveConfig struct {
	JiraBaseURL       string        `json:"jira_base_url"`
	Project           string        `json:"project"`
	Label             string        `json:"label"`
	Repo              string        `json:"repo"`
	Harnesses         []LiveHarness `json:"harnesses"`
	FullsendHarnesses []string      `json:"fullsend_harnesses,omitempty"`
	RepoBundle        string        `json:"repo_bundle,omitempty"`
	RepoRevision      string        `json:"repo_revision,omitempty"`
	GCPAudience       string        `json:"gcp_audience,omitempty"`
	VertexProject     string        `json:"vertex_project,omitempty"`
}

// LiveHarness is a validated projection of a repository harness used by the stub runner.
type LiveHarness struct {
	Name    string `json:"name"`
	Role    string `json:"role"`
	Trigger string `json:"trigger"`
}

func readLiveConfig(path string) (LiveConfig, []byte, string, error) {
	var cfg LiveConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, nil, "", err
	}
	if err = json.Unmarshal(b, &cfg); err != nil {
		return cfg, nil, "", err
	}
	if err = cfg.validate(); err != nil {
		return cfg, nil, "", err
	}
	digest := sha256.Sum256(b)
	return cfg, b, hex.EncodeToString(digest[:]), nil
}

func (c LiveConfig) validate() error {
	if err := jira.ValidateBaseURL(c.JiraBaseURL); err != nil {
		return err
	}
	if !projectKeyPattern.MatchString(c.Project) || !labelPattern.MatchString(c.Label) || !repoPattern.MatchString(c.Repo) {
		return fmt.Errorf("live config requires a valid Jira project, label, and owner/repo")
	}
	if len(c.Harnesses) == 0 {
		return fmt.Errorf("live config requires at least one harness")
	}
	seen := make(map[string]bool, len(c.Harnesses))
	for _, h := range c.Harnesses {
		if h.Name == "" || h.Role == "" || strings.TrimSpace(h.Trigger) == "" || seen[h.Name] {
			return fmt.Errorf("live config has an incomplete or duplicate harness %q", h.Name)
		}
		if err := harness.ValidateTriggerExpression(h.Trigger); err != nil {
			return fmt.Errorf("harness %s trigger: %w", h.Name, err)
		}
		seen[h.Name] = true
	}
	seenFullsend := make(map[string]bool, len(c.FullsendHarnesses))
	for _, name := range c.FullsendHarnesses {
		if !seen[name] || seenFullsend[name] || name != "triage" {
			return fmt.Errorf("live config opts into an unknown or duplicate Fullsend harness %q", name)
		}
		seenFullsend[name] = true
	}
	if len(c.FullsendHarnesses) > 0 {
		if !repoBundlePattern.MatchString(c.RepoBundle) || !gitRevisionPattern.MatchString(c.RepoRevision) {
			return fmt.Errorf("real Fullsend execution requires a pinned repository bundle and revision")
		}
		if c.GCPAudience == "" || !strings.HasPrefix(c.GCPAudience, "//iam.googleapis.com/projects/") || c.VertexProject == "" {
			return fmt.Errorf("real Fullsend execution requires configured GCP workload identity and Vertex project")
		}
	}
	return nil
}

// PollJira discovers the bounded Jira scope and commits observations and its
// covered watermark together. It does not mutate Jira or use Jira checkpoints.
func PollJira(ctx context.Context, configPath, tokenPath, emailPath string) error {
	cfg, raw, digest, err := readLiveConfig(configPath)
	if err != nil {
		return err
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return err
	}
	email, err := os.ReadFile(emailPath)
	if err != nil {
		return err
	}
	client, err := jira.New(strings.TrimSpace(string(token)), jira.WithBaseURL(cfg.JiraBaseURL), jira.WithEmail(strings.TrimSpace(string(email))))
	if err != nil {
		return err
	}
	conn, err := connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	return pollJiraWithClient(ctx, conn, client, cfg, raw, digest)
}

func pollJiraWithClient(ctx context.Context, conn *pgx.Conn, client jirapoll.JiraClient, cfg LiveConfig, raw []byte, digest string) error {
	if _, err := client.GetMyself(ctx); err != nil {
		return fmt.Errorf("Jira authentication preflight: %w", err)
	}
	jql := fmt.Sprintf(`project = "%s" AND labels = "%s" ORDER BY updated ASC`, cfg.Project, cfg.Label)
	issues, err := client.SearchIssues(ctx, jql, 0)
	if err != nil {
		return fmt.Errorf("search Jira test scope: %w", err)
	}
	// The shared client caps pagination at 100 pages of 50. Refuse to
	// advance a watermark when that bound could have omitted issues.
	if len(issues) >= 5000 {
		return fmt.Errorf("Jira test scope reached the 5000-issue search cap")
	}
	source := "jira:" + cfg.Project + ":" + cfg.Label
	var sourceErrors []error
	for _, issue := range issues {
		if !strings.HasPrefix(issue.Key, cfg.Project+"-") || issue.ID == "" || !slices.Contains(issue.Fields.Labels, cfg.Label) {
			return fmt.Errorf("Jira search returned an issue outside the configured scope")
		}
		checkpointSource := source + ":" + issue.ID
		var cursor string
		err = conn.QueryRow(ctx, `SELECT cursor FROM source_checkpoints WHERE tenant=$1 AND source=$2`, cfg.Repo, checkpointSource).Scan(&cursor)
		if err != nil && err != pgx.ErrNoRows {
			return err
		}
		err = nil
		var since time.Time
		if cursor != "" {
			since, err = time.Parse(time.RFC3339Nano, cursor)
			if err != nil {
				return fmt.Errorf("invalid Jira checkpoint for %s: %w", issue.Key, err)
			}
		}
		events, covered, discoverErr := jirapoll.DiscoverIssueEvents(ctx, client, issue, cfg.JiraBaseURL, since)
		if discoverErr != nil {
			sourceErrors = append(sourceErrors, fmt.Errorf("%s: %w", issue.Key, discoverErr))
			continue
		}
		tx, beginErr := conn.Begin(ctx)
		if beginErr != nil {
			return beginErr
		}
		if _, err = tx.Exec(ctx, `INSERT INTO config_revisions(digest,content) VALUES($1,$2) ON CONFLICT DO NOTHING`, digest, raw); err != nil {
			tx.Rollback(ctx)
			return err
		}
		for _, event := range events {
			observationKey := event.Key() + "@" + event.UpdatedAt.UTC().Format(time.RFC3339Nano)
			_, err = tx.Exec(ctx, `INSERT INTO observations(tenant,source,observation_key,entity,revision,kind,behavior,authorized,config_digest)
				VALUES($1,$2,$3,$4,$5,$6,'success',false,$7) ON CONFLICT(tenant,source,observation_key) DO NOTHING`,
				cfg.Repo, source, observationKey, issue.Key, event.UpdatedAt.UTC().Format(time.RFC3339Nano), event.Type, digest)
			if err != nil {
				tx.Rollback(ctx)
				return err
			}
		}
		if !covered.IsZero() {
			_, err = tx.Exec(ctx, `INSERT INTO source_checkpoints(tenant,source,cursor) VALUES($1,$2,$3)
				ON CONFLICT(tenant,source) DO UPDATE SET cursor=CASE WHEN excluded.cursor::timestamptz > source_checkpoints.cursor::timestamptz THEN excluded.cursor ELSE source_checkpoints.cursor END,updated_at=now()`,
				cfg.Repo, checkpointSource, covered.UTC().Format(time.RFC3339Nano))
		}
		if err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
	}
	return errors.Join(sourceErrors...)
}
