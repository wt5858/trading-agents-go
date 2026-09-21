package repositories

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

const sessionPrefix = "ta:session:"

// SessionRepository stores login sessions in Redis.
//
// Besides the session itself it maintains a user -> sessions reverse index: deactivating
// an account has to revoke every live login at once, and without the index that would
// mean scanning the keyspace.
type SessionRepository struct {
	rdb *redis.Client
	ttl time.Duration
}

func NewSessionRepository(rdb *redis.Client, ttl time.Duration) *SessionRepository {
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	return &SessionRepository{rdb: rdb, ttl: ttl}
}

func (repo *SessionRepository) key(sessionID string) string { return sessionPrefix + sessionID }

func (repo *SessionRepository) userKey(userID uint64) string {
	return sessionPrefix + "user:" + strconv.FormatUint(userID, 10)
}

func (repo *SessionRepository) Save(ctx context.Context, sessionID string, userID uint64) error {
	pipe := repo.rdb.TxPipeline()
	pipe.Set(ctx, repo.key(sessionID), userID, repo.ttl)
	pipe.SAdd(ctx, repo.userKey(userID), sessionID)
	// The index outlives the session slightly; if it expired first we could no longer
	// bulk-revoke the sessions it points at.
	pipe.Expire(ctx, repo.userKey(userID), repo.ttl+time.Hour)
	if _, err := pipe.Exec(ctx); err != nil {
		return custom_errors.Internal("保存会话失败").Wrap(err)
	}
	return nil
}

func (repo *SessionRepository) Exists(ctx context.Context, sessionID string) (bool, error) {
	n, err := repo.rdb.Exists(ctx, repo.key(sessionID)).Result()
	if err != nil {
		return false, custom_errors.Internal("查询会话失败").Wrap(err)
	}
	return n > 0, nil
}

func (repo *SessionRepository) Revoke(ctx context.Context, sessionID string) error {
	// Read the owner first, otherwise the reverse index keeps a dangling member forever.
	uid, err := repo.rdb.Get(ctx, repo.key(sessionID)).Uint64()
	if err != nil && err != redis.Nil {
		return custom_errors.Internal("吊销会话失败").Wrap(err)
	}
	pipe := repo.rdb.TxPipeline()
	pipe.Del(ctx, repo.key(sessionID))
	if err == nil {
		pipe.SRem(ctx, repo.userKey(uid), sessionID)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return custom_errors.Internal("吊销会话失败").Wrap(err)
	}
	return nil
}

func (repo *SessionRepository) RevokeAllOfUser(ctx context.Context, userID uint64) error {
	ids, err := repo.rdb.SMembers(ctx, repo.userKey(userID)).Result()
	if err != nil {
		return custom_errors.Internal("查询用户会话失败").Wrap(err)
	}
	// One pipeline for the whole set rather than a DEL round-trip per session.
	pipe := repo.rdb.TxPipeline()
	for _, id := range ids {
		pipe.Del(ctx, repo.key(id))
	}
	pipe.Del(ctx, repo.userKey(userID))
	if _, err := pipe.Exec(ctx); err != nil {
		return custom_errors.Internal("吊销用户会话失败").Wrap(err)
	}
	return nil
}
