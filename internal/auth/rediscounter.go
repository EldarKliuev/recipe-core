package auth

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisCounter adapts the Redis client to the counter interface used by
// RateLimiter, keeping the limiter itself trivially fakeable in tests.
type RedisCounter struct {
	client *redis.Client
}

func NewRedisCounter(client *redis.Client) *RedisCounter {
	return &RedisCounter{client: client}
}

func (c *RedisCounter) Incr(ctx context.Context, key string) *IntResult {
	v, err := c.client.Incr(ctx, key).Result()
	return &IntResult{Value: v, Err: err}
}

func (c *RedisCounter) Expire(ctx context.Context, key string, ttl time.Duration) error {
	return c.client.Expire(ctx, key, ttl).Err()
}
