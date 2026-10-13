package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/xyzjace/terraplane/pkg/storage/models"
	"github.com/xyzjace/terraplane/pkg/storage/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const leaseExpiredRunningMsg = "lease expired while job was running"

type jobRepository struct {
	db *DB
}

func NewJobRepository(db *DB) repository.JobRepository {
	return &jobRepository{db: db}
}

func (r *jobRepository) Create(ctx context.Context, job *models.Job) error {
	return r.db.pool.WithContext(ctx).Create(job).Error
}

func (r *jobRepository) UpsertPendingJob(ctx context.Context, repo string, prNumber int, stackName string, action string, payload map[string]interface{}, agent string) (*models.Job, error) {
	payloadJSON, err := marshalJobPayload(payload)
	if err != nil {
		return nil, err
	}

	var job models.Job
	err = r.db.pool.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"repo = ? AND pr_number = ? AND stack_name = ? AND action = ? AND status = ?",
				repo, prNumber, stackName, action, models.JobStatusPending,
			).
			Limit(1).
			Find(&job)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			job = models.Job{
				ID:        uuid.NewString(),
				Repo:      repo,
				PRNumber:  int32(prNumber),
				StackName: stackName,
				Dir:       stringFromPayload(payload, "dir"),
				CommitSHA: stringFromPayload(payload, "commit_sha"),
				AgentID:   agent,
				Action:    models.JobAction(action),
				Payload:   payloadJSON,
				Status:    models.JobStatusPending,
			}
			return tx.Create(&job).Error
		}

		job.AgentID = agent
		job.Payload = payloadJSON
		if dir := stringFromPayload(payload, "dir"); dir != "" {
			job.Dir = dir
		}
		if sha := stringFromPayload(payload, "commit_sha"); sha != "" {
			job.CommitSHA = sha
		}
		job.Output = ""
		job.ErrorMsg = ""
		job.LeaseExpiresAt = nil
		return tx.Save(&job).Error
	})
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func marshalJobPayload(payload map[string]interface{}) (string, error) {
	if payload == nil {
		return "{}", nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal job payload: %w", err)
	}
	return string(b), nil
}

func stringFromPayload(payload map[string]interface{}, key string) string {
	if payload == nil {
		return ""
	}
	v, ok := payload[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func (r *jobRepository) Get(ctx context.Context, jobID string) (*models.Job, error) {
	var job models.Job
	err := r.db.pool.WithContext(ctx).First(&job, "id = ?", jobID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: %s", repository.ErrJobNotFound, jobID)
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func (r *jobRepository) ClaimPendingJobForAgent(ctx context.Context, agentID string, status models.JobStatus, leaseExpiresAt *time.Time) (*models.Job, error) {
	if agentID == "" {
		return nil, fmt.Errorf("agentID is required")
	}

	var jobs []*models.Job
	err := r.db.pool.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ?", models.JobStatusPending).
			Where(
				"action IN ? AND agent_id = ? AND NOT EXISTS (SELECT 1 FROM jobs AS busy WHERE busy.repo = jobs.repo AND busy.stack_name = jobs.stack_name AND busy.status IN ?)",
				[]models.JobAction{models.JobActionPlan, models.JobActionApply},
				agentID,
				[]models.JobStatus{models.JobStatusClaimed, models.JobStatusRunning},
			)

		result := q.Order("created_at ASC, id ASC").Limit(1).Find(&jobs)
		if result.Error != nil {
			return result.Error
		}
		if len(jobs) == 0 {
			return nil
		}

		jobs[0].Status = status
		jobs[0].LeaseExpiresAt = leaseExpiresAt
		return tx.Save(jobs[0]).Error
	})
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, nil
	}
	return jobs[0], nil
}

func (r *jobRepository) ReleaseClaimedJob(ctx context.Context, jobID string) error {
	return r.db.pool.WithContext(ctx).
		Model(&models.Job{}).
		Where("id = ? AND status = ?", jobID, models.JobStatusClaimed).
		Updates(map[string]interface{}{
			"status":           models.JobStatusPending,
			"lease_expires_at": gorm.Expr("NULL"),
		}).Error
}

func (r *jobRepository) FailClaimedJob(ctx context.Context, jobID, errMsg string) error {
	return r.db.pool.WithContext(ctx).
		Model(&models.Job{}).
		Where("id = ? AND status = ?", jobID, models.JobStatusClaimed).
		Updates(map[string]interface{}{
			"status":           models.JobStatusFailed,
			"error_msg":        errMsg,
			"lease_expires_at": gorm.Expr("NULL"),
		}).Error
}

func (r *jobRepository) ReapExpiredClaims(ctx context.Context, now time.Time) (*repository.ReapExpiredClaimsResult, error) {
	result := &repository.ReapExpiredClaimsResult{}
	err := r.db.pool.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		expiredLease := "lease_expires_at IS NOT NULL AND lease_expires_at < ?"

		var running []*models.Job
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("status = ? AND "+expiredLease, models.JobStatusRunning, now).
			Find(&running).Error; err != nil {
			return err
		}
		if len(running) > 0 {
			ids := make([]string, len(running))
			for i, job := range running {
				ids[i] = job.ID
			}
			if err := tx.Model(&models.Job{}).
				Where("id IN ?", ids).
				Updates(map[string]interface{}{
					"status":           models.JobStatusFailed,
					"error_msg":        leaseExpiredRunningMsg,
					"lease_expires_at": gorm.Expr("NULL"),
				}).Error; err != nil {
				return err
			}
			result.RunningFailed = running
		}

		claimed := tx.Model(&models.Job{}).
			Where("status = ? AND "+expiredLease, models.JobStatusClaimed, now).
			Updates(map[string]interface{}{
				"status":           models.JobStatusPending,
				"lease_expires_at": gorm.Expr("NULL"),
			})
		if claimed.Error != nil {
			return claimed.Error
		}
		result.ClaimedReturned = int(claimed.RowsAffected)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (r *jobRepository) RefreshAgentClaims(ctx context.Context, agentID string, leaseExpiresAt *time.Time) error {
	return r.db.pool.WithContext(ctx).
		Model(&models.Job{}).
		Where(
			"agent_id = ? AND status IN ?",
			agentID,
			[]models.JobStatus{models.JobStatusClaimed, models.JobStatusRunning},
		).
		Update("lease_expires_at", leaseExpiresAt).Error
}

func (r *jobRepository) Update(ctx context.Context, job *models.Job) error {
	return r.db.pool.WithContext(ctx).Save(job).Error
}

func (r *jobRepository) UpdatePayload(ctx context.Context, job *models.Job, payload map[string]interface{}) error {
	payloadJSON, err := marshalJobPayload(payload)
	if err != nil {
		return err
	}
	job.Payload = payloadJSON
	return r.Update(ctx, job)
}

func (r *jobRepository) Delete(ctx context.Context, jobID string) error {
	return r.db.pool.WithContext(ctx).Delete(&models.Job{}, "id = ?", jobID).Error
}

func (r *jobRepository) DeleteByRepoPRAndStacks(ctx context.Context, repo string, prNumber int, stackNames []string) (int, error) {
	if len(stackNames) == 0 {
		return 0, nil
	}

	result := r.db.pool.WithContext(ctx).
		Where("repo = ? AND pr_number = ? AND stack_name IN ?", repo, prNumber, stackNames).
		Delete(&models.Job{})
	if result.Error != nil {
		return 0, result.Error
	}
	return int(result.RowsAffected), nil
}

func (r *jobRepository) CleanupExpiredJobs(ctx context.Context, cutoff time.Time) (int, error) {
	result := r.db.pool.WithContext(ctx).
		Model(&models.Job{}).
		Where("created_at < ? AND status IN ?", cutoff, []models.JobStatus{
			models.JobStatusSucceeded, models.JobStatusFailed,
		}).
		Delete(&models.Job{})
	if result.Error != nil {
		return 0, result.Error
	}
	return int(result.RowsAffected), nil
}
