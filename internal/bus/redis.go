package bus

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// Redis is the production Bus implementation.
type Redis struct {
	client *redis.Client
}

func NewRedis(addr string) *Redis {
	return &Redis{client: redis.NewClient(&redis.Options{Addr: addr})}
}

func (r *Redis) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}

func (r *Redis) Close() error { return r.client.Close() }

func (r *Redis) Publish(ctx context.Context, channel string) error {
	return r.client.Publish(ctx, channel, "1").Err()
}

func (r *Redis) Subscribe(ctx context.Context, channels ...string) (<-chan struct{}, func()) {
	sub := r.client.Subscribe(ctx, channels...)
	out := make(chan struct{}, 1)
	go func() {
		for range sub.Channel() {
			select {
			case out <- struct{}{}:
			default:
			}
		}
	}()
	return out, func() { _ = sub.Close() }
}
