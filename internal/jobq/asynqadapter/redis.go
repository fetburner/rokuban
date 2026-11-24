package asynqadapter

import (
	"errors"
	"net/url"

	"github.com/redis/go-redis/v9"

	"github.com/fetburner/rokuban/internal/jobq"
)

// NewFromRedisURL は Redis クライアントを所有する adapter を作る。
// 返す close は Stop の完了後に呼ぶ。停止中のクライアントは閉じずにエラーを返す。
func NewFromRedisURL(rawURL string, registry *jobq.Registry, queueFor QueueFunc, cfg Config) (*Adapter, func() error, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "redis" && u.Scheme != "rediss") || u.Hostname() == "" || u.Fragment != "" {
		return nil, nil, errors.New("expected a redis:// or rediss:// URL with a host")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		// Redis の URL には認証情報が入るため、パースエラーに元の値を含めない。
		return nil, nil, errors.New("invalid Redis database or connection options")
	}
	client := redis.NewClient(options)
	adapter, err := NewAdapter(client, registry, queueFor, cfg)
	if err != nil {
		return nil, nil, errors.Join(err, client.Close())
	}
	return adapter, func() error {
		adapter.mu.Lock()
		defer adapter.mu.Unlock()
		if adapter.run != nil {
			return errors.New("asynqadapter: Redis client cannot close before delivery stops")
		}
		return client.Close()
	}, nil
}
