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

func marshalJob(job Job) ([]byte, error) {
	job.UpdatedAt = time.Now().UTC()
	payload, err := json.Marshal(jobRecord{Job: job, OwnerID: job.UserID})
	if err != nil {
		return nil, fmt.Errorf("marshal job: %w", err)
	}
	return payload, nil
}

// Save writes the job and publishes it to subscribers of its event channel.
func (s *JobStore) Save(ctx context.Context, job Job) error {
	payload, err := marshalJob(job)
	if err != nil {
		return err
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

// JobForIdempotencyKey returns the job id a key was claimed for, or false if the
// key is free.
func (s *JobStore) JobForIdempotencyKey(ctx context.Context, userID uuid.UUID, key string) (uuid.UUID, bool, error) {
	existing, err := s.client.Get(ctx, idempotencyKey(userID, key)).Result()
	if errors.Is(err, redis.Nil) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("read idempotency key: %w", err)
	}
	return parseJobID(existing)
}

// createJobScript claims the idempotency key and stores the job in one step, so
// a concurrent request that finds the key always finds the job behind it too.
// It returns the job id the key points to: the new one, or the first request's.
var createJobScript = redis.NewScript(`
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[3]) then
  redis.call('SET', KEYS[2], ARGV[2], 'PX', ARGV[3])
  return ARGV[1]
end
return redis.call('GET', KEYS[1])
`)

// CreateJob stores a new job under an Idempotency-Key. If the key was already
// claimed it stores nothing and returns the original job id with false, so a
// replayed POST returns that job instead of queueing a second generation.
func (s *JobStore) CreateJob(ctx context.Context, key string, job Job) (uuid.UUID, bool, error) {
	payload, err := marshalJob(job)
	if err != nil {
		return uuid.Nil, false, err
	}
	keys := []string{idempotencyKey(job.UserID, key), jobKey(job.ID)}
	got, err := createJobScript.Run(ctx, s.client, keys, job.ID.String(), payload, s.ttl.Milliseconds()).Text()
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("create job: %w", err)
	}
	id, _, err := parseJobID(got)
	if err != nil {
		return uuid.Nil, false, err
	}
	return id, id == job.ID, nil
}

func parseJobID(raw string) (uuid.UUID, bool, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("stored idempotency value is not a job id: %w", err)
	}
	return id, true, nil
}

// ReleaseIdempotencyKey drops the reservation so the user can retry immediately
// after a failure that never produced a result.
func (s *JobStore) ReleaseIdempotencyKey(ctx context.Context, userID uuid.UUID, key string) {
	if key == "" {
		return
	}
	_ = s.client.Del(ctx, idempotencyKey(userID, key)).Err()
}
