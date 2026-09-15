package cook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
	"github.com/redis/go-redis/v9"
)

// JobStore keeps job state in Redis with a TTL. Jobs are deliberately not
// durable: a lost job means the user presses the button again, which is cheaper
// than the machinery of an exactly-once queue for a local single-GPU model.
type JobStore struct {
	client *redis.Client
	ttl    time.Duration
}

func NewJobStore(client *redis.Client, ttl time.Duration) *JobStore {
	return &JobStore{client: client, ttl: ttl}
}

func jobKey(id uuid.UUID) string       { return "job:" + id.String() }
func eventChannel(id uuid.UUID) string { return "job-events:" + id.String() }

func idempotencyKey(userID uuid.UUID, key string) string {
	return fmt.Sprintf("idem:adapt:%s:%s", userID, key)
}

// jobRecord is the stored shape. UserID is persisted (unlike in the API view) so
// that SSE can verify ownership.
type jobRecord struct {
	Job
	OwnerID uuid.UUID `json:"owner_id"`
}

// Save writes the job and publishes it to subscribers of its event channel.
func (s *JobStore) Save(ctx context.Context, job Job) error {
	job.UpdatedAt = time.Now().UTC()
	payload, err := json.Marshal(jobRecord{Job: job, OwnerID: job.UserID})
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}
	if err := s.client.Set(ctx, jobKey(job.ID), payload, s.ttl).Err(); err != nil {
		return fmt.Errorf("store job: %w", err)
	}
	// A publish failure only costs latency: SSE readers also poll.
	_ = s.client.Publish(ctx, eventChannel(job.ID), payload).Err()
	return nil
}

func (s *JobStore) Get(ctx context.Context, id uuid.UUID) (Job, error) {
	raw, err := s.client.Get(ctx, jobKey(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return Job{}, fmt.Errorf("%w: job %s (it may have expired)", httpx.ErrNotFound, id)
	}
	if err != nil {
		return Job{}, fmt.Errorf("read job: %w", err)
	}
	var rec jobRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Job{}, fmt.Errorf("unmarshal job: %w", err)
	}
	rec.Job.UserID = rec.OwnerID
	return rec.Job, nil
}

// Subscribe returns a channel of job snapshots pushed on every Save.
func (s *JobStore) Subscribe(ctx context.Context, id uuid.UUID) (<-chan Job, func(), error) {
	sub := s.client.Subscribe(ctx, eventChannel(id))
	if _, err := sub.Receive(ctx); err != nil {
		return nil, nil, fmt.Errorf("subscribe to job events: %w", err)
	}
	out := make(chan Job, 8)
	go func() {
		defer close(out)
		for msg := range sub.Channel() {
			var rec jobRecord
			if err := json.Unmarshal([]byte(msg.Payload), &rec); err != nil {
				continue
			}
			rec.Job.UserID = rec.OwnerID
			select {
			case out <- rec.Job:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, func() { _ = sub.Close() }, nil
}

// ClaimIdempotencyKey reserves key for jobID. If the key was already used it
// returns the job id from the first request, so a retried POST returns the
// original job instead of queueing a second generation.
func (s *JobStore) ClaimIdempotencyKey(ctx context.Context, userID uuid.UUID, key string, jobID uuid.UUID) (uuid.UUID, bool, error) {
	redisKey := idempotencyKey(userID, key)
	ok, err := s.client.SetNX(ctx, redisKey, jobID.String(), s.ttl).Result()
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("claim idempotency key: %w", err)
	}
	if ok {
		return jobID, true, nil
	}
	existing, err := s.client.Get(ctx, redisKey).Result()
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("read idempotency key: %w", err)
	}
	parsed, err := uuid.Parse(existing)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("stored idempotency value is not a job id: %w", err)
	}
	return parsed, false, nil
}

// ReleaseIdempotencyKey drops the reservation so the user can retry immediately
// after a failure that never produced a result.
func (s *JobStore) ReleaseIdempotencyKey(ctx context.Context, userID uuid.UUID, key string) {
	if key == "" {
		return
	}
	_ = s.client.Del(ctx, idempotencyKey(userID, key)).Err()
}
