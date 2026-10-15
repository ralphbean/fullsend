package k8spoc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Dispatch reconciles existing Jobs before admitting new work. Reservations remain
// durable when a Job disappears or the API gives an ambiguous response.
func Dispatch(ctx context.Context) error {
	conn, err := connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	k, err := newKubeClient()
	if err != nil {
		return err
	}
	return dispatchWithClient(ctx, conn, k)
}

func dispatchWithClient(ctx context.Context, conn *pgx.Conn, k *kubeClient) error {
	if err := selectPending(ctx, conn); err != nil {
		return err
	}
	liveErr := selectPendingJira(ctx, conn)
	active, err := activeAttempts(ctx, conn)
	if err != nil {
		return err
	}
	var problems []string
	if liveErr != nil {
		problems = append(problems, liveErr.Error())
	}
	for _, a := range active {
		if err = reconcile(ctx, conn, k, a); err != nil {
			problems = append(problems, fmt.Sprintf("attempt %d: %v", a.id, err))
		}
	}
	for i := 0; i < 2; i++ {
		a, err := reserve(ctx, conn, 2, 2)
		if err != nil {
			return err
		}
		if a == nil {
			break
		}
		if err = reconcile(ctx, conn, k, *a); err != nil {
			problems = append(problems, fmt.Sprintf("attempt %d: %v", a.id, err))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("dispatch reconciliation: %s", strings.Join(problems, "; "))
	}
	return nil
}

func reconcile(ctx context.Context, conn *pgx.Conn, k *kubeClient, a attempt) error {
	job, exists, err := k.get(ctx, a.name)
	if err != nil {
		return err
	}
	if isGatewayAttempt(a) {
		return reconcileGatewayAttempt(ctx, conn, k, a, job, exists)
	}
	if !exists {
		if a.state == "creating" {
			job, err = k.create(ctx, a)
			if err != nil {
				// A lost create response can still mean the Job exists. Retain this
				// attempt and look up the same name on the next cycle.
				return err
			}
			_, err = conn.Exec(ctx, `UPDATE attempts SET job_uid=$2,state='active' WHERE id=$1 AND state='creating'`, a.id, job.Metadata.UID)
			return err
		}
		if a.state == "cleanup" {
			// The stub has no remote workload. Keep its reservation until
			// Kubernetes confirms the terminal Job has disappeared.
			tx, err := conn.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			var outcome string
			if err := tx.QueryRow(ctx, `SELECT outcome FROM attempts WHERE id=$1`, a.id).Scan(&outcome); err != nil {
				return err
			}
			if outcome != "succeeded" && outcome != "failed" {
				return fmt.Errorf("attempt %d has no terminal outcome for cleanup", a.id)
			}
			if _, err := tx.Exec(ctx, `UPDATE attempts SET cleanup_state='done',state='complete' WHERE id=$1`, a.id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE selected_runs SET state=$2 WHERE id=$1 AND state='reserved'`, a.runID, outcome); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		_, err = conn.Exec(ctx, `UPDATE attempts SET state='unknown',outcome='job_missing' WHERE id=$1 AND state='active'`, a.id)
		return err
	}
	if a.uid != "" && job.Metadata.UID != a.uid {
		return fmt.Errorf("Job UID mismatch for %s", a.name)
	}
	if a.state == "creating" {
		if _, err = conn.Exec(ctx, `UPDATE attempts SET job_uid=$2,state='active' WHERE id=$1`, a.id, job.Metadata.UID); err != nil {
			return err
		}
	}
	if job.Status.Succeeded == 0 && job.Status.Failed == 0 && a.state != "cleanup" {
		return nil
	}
	if a.state != "cleanup" {
		outcome := "succeeded"
		if job.Status.Failed > 0 {
			outcome = "failed"
		}
		if _, err = conn.Exec(ctx, `UPDATE attempts SET state='cleanup',outcome=$2,job_uid=$3 WHERE id=$1`, a.id, outcome, job.Metadata.UID); err != nil {
			return err
		}
	}
	if err = k.delete(ctx, a.name, job.Metadata.UID); err != nil {
		return err
	}
	// A successful DELETE response only accepts the request. A later cycle
	// confirms disappearance before marking cleanup complete.
	return nil
}

func reconcileGatewayAttempt(ctx context.Context, conn *pgx.Conn, k *kubeClient, a attempt, job kubeJob, exists bool) error {
	if !exists {
		switch a.state {
		case "creating":
			created, err := k.create(ctx, a)
			if err != nil {
				return err // A lost create response is reconciled by name next cycle.
			}
			_, err = conn.Exec(ctx, `UPDATE attempts SET job_uid=$2,state='active' WHERE id=$1 AND state='creating'`, a.id, created.Metadata.UID)
			return err
		case "active":
			// A missing Job cannot prove the run succeeded or its remote sandbox
			// stopped. Start independent cleanup and retain the reservation.
			if _, err := conn.Exec(ctx, `UPDATE attempts SET state='cleanup',outcome='failed' WHERE id=$1 AND state='active'`, a.id); err != nil {
				return err
			}
			return nil
		case "cleanup":
			verified, err := verifyGatewayStop(ctx, k, a)
			if err != nil || !verified {
				return err
			}
			if a.execution == "fullsend" {
				if err = k.deleteJiraEventConfigMap(ctx, a.runID); err != nil {
					return err
				}
			}
			return completeGatewayAttempt(ctx, conn, a)
		default:
			return fmt.Errorf("unexpected gateway attempt state %q", a.state)
		}
	}
	if a.uid != "" && job.Metadata.UID != a.uid {
		return fmt.Errorf("Job UID mismatch for %s", a.name)
	}
	if job.Metadata.Labels["fullsend.ai/attempt"] != fmt.Sprint(a.id) || job.Metadata.Labels["fullsend.ai/gateway-job"] != "demo" {
		return fmt.Errorf("Job %s does not belong to gateway attempt %d", a.name, a.id)
	}
	if a.state == "creating" {
		if _, err := conn.Exec(ctx, `UPDATE attempts SET job_uid=$2,state='active' WHERE id=$1`, a.id, job.Metadata.UID); err != nil {
			return err
		}
	}
	if job.Status.Succeeded == 0 && !jobFailed(job) {
		if a.state == "cleanup" {
			// An operator requested cancellation. Stop the Pod first; the
			// independent cleaner runs only after Kubernetes removes the Job.
			return k.delete(ctx, a.name, job.Metadata.UID)
		}
		return nil
	}
	if a.state != "cleanup" {
		outcome := "succeeded"
		if jobFailed(job) {
			outcome = "failed"
		}
		if _, err := conn.Exec(ctx, `UPDATE attempts SET state='cleanup',outcome=$2,job_uid=$3 WHERE id=$1`, a.id, outcome, job.Metadata.UID); err != nil {
			return err
		}
	}
	verified, err := verifyGatewayStop(ctx, k, a)
	if err != nil || !verified {
		return err
	}
	if a.execution == "fullsend" {
		if err = k.deleteJiraEventConfigMap(ctx, a.runID); err != nil {
			return err
		}
	}
	return k.delete(ctx, a.name, job.Metadata.UID)
}

func verifyGatewayStop(ctx context.Context, k *kubeClient, a attempt) (bool, error) {
	name := cleanupJobName(a.name)
	job, exists, err := k.get(ctx, name)
	if err != nil {
		return false, err
	}
	if !exists {
		_, err = k.createGatewayCleanup(ctx, a)
		return false, err
	}
	if job.Metadata.Labels["fullsend.ai/attempt"] != fmt.Sprint(a.id) || job.Metadata.Labels["fullsend.ai/gateway-job"] != "attempt-cleanup" {
		return false, fmt.Errorf("Job %s does not belong to gateway cleanup for attempt %d", name, a.id)
	}
	if jobFailed(job) {
		return false, fmt.Errorf("gateway cleanup Job %s failed; reservation retained", name)
	}
	if job.Status.Succeeded == 0 {
		return false, nil
	}
	return true, nil
}

func completeGatewayAttempt(ctx context.Context, conn *pgx.Conn, a attempt) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var outcome string
	if err := tx.QueryRow(ctx, `SELECT outcome FROM attempts WHERE id=$1`, a.id).Scan(&outcome); err != nil {
		return err
	}
	if outcome != "succeeded" && outcome != "failed" {
		return fmt.Errorf("attempt %d has no terminal outcome for cleanup", a.id)
	}
	tag, err := tx.Exec(ctx, `UPDATE attempts SET cleanup_state='done',state='complete' WHERE id=$1 AND state='cleanup'`, a.id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("attempt %d is no longer awaiting cleanup", a.id)
	}
	tag, err = tx.Exec(ctx, `UPDATE selected_runs SET state=$2 WHERE id=$1 AND state='reserved'`, a.runID, outcome)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("run %d no longer holds a reservation", a.runID)
	}
	return tx.Commit(ctx)
}

// Status prints durable work state without credentials or source content.
func Status(ctx context.Context) error {
	conn, err := connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	type summary struct {
		Checkpoints       int64           `json:"checkpoints"`
		Observations      int64           `json:"observations"`
		Waiting           int64           `json:"waiting"`
		Reserved          int64           `json:"reserved"`
		Succeeded         int64           `json:"succeeded"`
		Failed            int64           `json:"failed"`
		Dismissed         int64           `json:"dismissed"`
		Unknown           int64           `json:"unknown"`
		FailedRunIDs      json.RawMessage `json:"failed_run_ids"`
		UnknownAttemptIDs json.RawMessage `json:"unknown_attempt_ids"`
	}
	var s summary
	if err = conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM source_checkpoints),(SELECT count(*) FROM observations),(SELECT count(*) FROM selected_runs WHERE state='waiting'),(SELECT count(*) FROM selected_runs WHERE state='reserved'),(SELECT count(*) FROM selected_runs WHERE state='succeeded'),(SELECT count(*) FROM selected_runs WHERE state='failed'),(SELECT count(*) FROM selected_runs WHERE state='dismissed'),(SELECT count(*) FROM attempts WHERE state='unknown'),(SELECT coalesce(json_agg(id ORDER BY id),'[]'::json) FROM selected_runs WHERE state='failed'),(SELECT coalesce(json_agg(id ORDER BY id),'[]'::json) FROM attempts WHERE state='unknown')`).Scan(&s.Checkpoints, &s.Observations, &s.Waiting, &s.Reserved, &s.Succeeded, &s.Failed, &s.Dismissed, &s.Unknown, &s.FailedRunIDs, &s.UnknownAttemptIDs); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(s)
}
