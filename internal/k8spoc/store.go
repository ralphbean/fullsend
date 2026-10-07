package k8spoc

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed schema.sql
var schema embed.FS

func connect(ctx context.Context) (*pgx.Conn, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return pgx.Connect(ctx, dsn)
}

func connectUntil(ctx context.Context, limit time.Duration) (*pgx.Conn, error) {
	if os.Getenv("DATABASE_URL") == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	deadline, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var last error
	for {
		conn, err := connect(deadline)
		if err == nil {
			return conn, nil
		}
		last = err
		timer := time.NewTimer(time.Second)
		select {
		case <-deadline.Done():
			timer.Stop()
			return nil, fmt.Errorf("database not ready within %s: %w", limit, last)
		case <-timer.C:
		}
	}
}

func Migrate(ctx context.Context) error {
	conn, err := connectUntil(ctx, 40*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	b, err := schema.ReadFile("schema.sql")
	if err != nil {
		return err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(408231)"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS schema_versions (version integer PRIMARY KEY)"); err != nil {
		return err
	}
	var version int
	err = tx.QueryRow(ctx, "SELECT version FROM schema_versions LIMIT 1").Scan(&version)
	if err != nil && err != pgx.ErrNoRows {
		return err
	}
	if version > 1 {
		return fmt.Errorf("unsupported schema version %d", version)
	}
	if version == 0 {
		if _, err = tx.Exec(ctx, string(b)); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_versions(version) VALUES(1)"); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Poll atomically records observations and the source checkpoint.
func Poll(ctx context.Context, fixturePath, selectionPath string) error {
	f, err := readFixture(fixturePath)
	if err != nil {
		return err
	}
	config, digest, err := readSelection(selectionPath)
	if err != nil {
		return err
	}
	conn, err := connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "INSERT INTO config_revisions(digest,content) VALUES($1,$2) ON CONFLICT DO NOTHING", digest, config); err != nil {
		return err
	}
	for _, o := range f.Observations {
		_, err = tx.Exec(ctx, `INSERT INTO observations(tenant,source,observation_key,entity,revision,kind,behavior,authorized,config_digest)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(tenant,source,observation_key) DO NOTHING`,
			f.Tenant, f.Source, o.Key, o.Entity, o.Revision, o.Kind, o.Behavior, o.Authorized, digest)
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO source_checkpoints(tenant,source,cursor) VALUES($1,$2,$3)
		ON CONFLICT(tenant,source) DO UPDATE SET cursor=excluded.cursor, updated_at=now()`, f.Tenant, f.Source, f.Cursor)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func selectPending(ctx context.Context, conn *pgx.Conn) error {
	rows, err := conn.Query(ctx, `SELECT o.id,o.kind,o.authorized,c.content FROM observations o
		JOIN config_revisions c ON c.digest=o.config_digest WHERE o.selection_state='pending' AND o.source NOT LIKE 'jira:%' ORDER BY o.id LIMIT 50`)
	if err != nil {
		return err
	}
	type candidate struct {
		id         int64
		kind       string
		authorized bool
		content    []byte
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.kind, &c.authorized, &c.content); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range candidates {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		state := "selected"
		detail := ""
		var names []string
		if !c.authorized {
			state = "denied"
		} else {
			var s Selection
			if err = json.Unmarshal(c.content, &s); err != nil {
				state = "evaluation_error"
				detail = err.Error()
			} else {
				names = s.ByKind[c.kind]
				if len(names) == 0 {
					state = "no_match"
				}
			}
		}
		if state == "selected" {
			for _, name := range names {
				if _, err = tx.Exec(ctx, `INSERT INTO selected_runs(observation_id,harness) VALUES($1,$2) ON CONFLICT DO NOTHING`, c.id, name); err != nil {
					tx.Rollback(ctx)
					return err
				}
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE observations SET selection_state=$2, selection_error=$3 WHERE id=$1 AND selection_state='pending'`, c.id, state, detail); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

type attempt struct {
	id, runID                                                   int64
	name, uid, state, behavior, tenant, source, entity, harness string
}

func activeAttempts(ctx context.Context, conn *pgx.Conn) ([]attempt, error) {
	rows, err := conn.Query(ctx, `SELECT a.id,a.run_id,a.job_name,a.job_uid,a.state,o.behavior,o.tenant,o.source,o.entity,r.harness
		FROM attempts a JOIN selected_runs r ON r.id=a.run_id JOIN observations o ON o.id=r.observation_id
		WHERE a.state IN ('creating','active','cleanup') ORDER BY a.id LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []attempt
	for rows.Next() {
		var a attempt
		if err = rows.Scan(&a.id, &a.runID, &a.name, &a.uid, &a.state, &a.behavior, &a.tenant, &a.source, &a.entity, &a.harness); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func reserve(ctx context.Context, conn *pgx.Conn, capGlobal, capTenant int) (*attempt, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(408232)"); err != nil {
		return nil, err
	}
	var global int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM selected_runs WHERE state='reserved'`).Scan(&global); err != nil {
		return nil, err
	}
	if global >= capGlobal {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT r.id,o.tenant,o.source,o.entity,o.behavior,r.harness FROM selected_runs r JOIN observations o ON o.id=r.observation_id WHERE r.state='waiting' ORDER BY r.id LIMIT 50`)
	if err != nil {
		return nil, err
	}
	var waiting []attempt
	for rows.Next() {
		var a attempt
		if err = rows.Scan(&a.runID, &a.tenant, &a.source, &a.entity, &a.behavior, &a.harness); err != nil {
			rows.Close()
			return nil, err
		}
		waiting = append(waiting, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, a := range waiting {
		var tenantCount, conflicts int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM selected_runs r JOIN observations o ON o.id=r.observation_id WHERE r.state='reserved' AND o.tenant=$1`, a.tenant).Scan(&tenantCount); err != nil {
			return nil, err
		}
		if tenantCount >= capTenant {
			continue
		}
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM selected_runs r JOIN observations o ON o.id=r.observation_id WHERE r.state='reserved' AND o.tenant=$1 AND o.source=$2 AND o.entity=$3`, a.tenant, a.source, a.entity).Scan(&conflicts); err != nil {
			return nil, err
		}
		if conflicts > 0 {
			continue
		}
		if _, err = tx.Exec(ctx, `UPDATE selected_runs SET state='reserved' WHERE id=$1`, a.runID); err != nil {
			return nil, err
		}
		var number int
		if err = tx.QueryRow(ctx, `SELECT coalesce(max(number),0)+1 FROM attempts WHERE run_id=$1`, a.runID).Scan(&number); err != nil {
			return nil, err
		}
		if err = tx.QueryRow(ctx, `INSERT INTO attempts(run_id,number,job_name) VALUES($1,$2,$3) RETURNING id,job_name`, a.runID, number, fmt.Sprintf("fs-poc-%d-%d", a.runID, number)).Scan(&a.id, &a.name); err != nil {
			return nil, err
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		a.state = "creating"
		return &a, nil
	}
	return nil, nil
}

func changeFailedRun(ctx context.Context, runID int64, target string) error {
	if runID <= 0 {
		return fmt.Errorf("run ID must be positive")
	}
	conn, err := connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(408232)"); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE selected_runs r SET state=$2 WHERE r.id=$1 AND r.state='failed'
		AND EXISTS (SELECT 1 FROM attempts a WHERE a.run_id=r.id AND a.state='complete' AND a.cleanup_state='done')`, runID, target)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("run %d is not a cleaned-up failure", runID)
	}
	return tx.Commit(ctx)
}

// Retry requeues one failed run. The next admission creates a fresh attempt.
func Retry(ctx context.Context, runID int64) error { return changeFailedRun(ctx, runID, "waiting") }

// Dismiss acknowledges one failed run after its Job has been cleaned up.
func Dismiss(ctx context.Context, runID int64) error { return changeFailedRun(ctx, runID, "dismissed") }

// Cancel requests an active gateway-demo attempt stop while retaining its reservation.
// Reconciliation removes the runner Job and verifies the sandbox is absent
// before the selected run becomes a cleaned-up failure.
func Cancel(ctx context.Context, runID int64) error {
	if runID <= 0 {
		return fmt.Errorf("run ID must be positive")
	}
	conn, err := connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	tag, err := conn.Exec(ctx, `UPDATE attempts a SET state='cleanup',outcome='failed'
		FROM selected_runs r JOIN observations o ON o.id=r.observation_id
		WHERE a.run_id=r.id AND r.id=$1 AND r.state='reserved'
		AND r.harness='gateway-demo' AND o.source='fixture-source'
		AND a.state='active'`, runID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("run %d has no cancellable gateway attempt", runID)
	}
	return nil
}
