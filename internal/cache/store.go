package cache

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"shortlink/internal/link"
)

const (
	// keyPrefix 给缓存 key 留命名空间，Redis 和其他应用共用时不会撞 key。
	keyPrefix = "shortlink:link:"
	// positiveTTL 是正常短链的缓存时间。
	positiveTTL = time.Hour
	// negativeTTL 比 positiveTTL 短得多：不存在的短码不该长期占内存，
	// 但短暂的负缓存能挡住用随机短码反复打数据库的请求。
	negativeTTL = time.Minute
)

/*
LinkStore 是装饰器需要的最小存储能力。接口由使用方定义，
这样 cache 不依赖 httpapi，也不依赖具体的 postgres 实现。
*/
type LinkStore interface {
	Create(ctx context.Context, l *link.Link) error
	GetByCode(ctx context.Context, code string) (*link.Link, error)
}

/*
Store 是带 Redis 读缓存的 LinkStore 装饰器。

缓存作用在 GetByCode 上。Create 成功后写穿正缓存，盖掉负缓存，
否则新短码会在 TTL 内继续 404。并发未命中用 singleflight 合并，同一个短码同时只回源一次。
Redis 故障按 fail-open 处理：读失败或写失败都回源数据库，不影响请求结果。
*/
type Store struct {
	next  LinkStore
	rdb   *redis.Client
	group singleflight.Group // 零值就能用
}

// New 包装 next，读写都走 rdb。rdb 只在这里当缓存用，不承担其他职责。
func New(next LinkStore, rdb *redis.Client) *Store {
	return &Store{next: next, rdb: rdb}
}

// Create 成功后写穿正缓存，盖掉负缓存。写失败不影响创建结果。
func (s *Store) Create(ctx context.Context, l *link.Link) error {
	if err := s.next.Create(ctx, l); err != nil {
		return err
	}
	s.writeCache(ctx, keyPrefix+l.Code, l)
	return nil
}

/*
GetByCode 先读缓存，未命中或 Redis 故障时回源数据库。

缓存命中直接返回，包括负缓存：value 为 JSON null，解出来的 Link 是 nil。
回源用 singleflight 按 key 合并：并发未命中只有一个请求查库，其余等它的结果。
singleflight 里的函数用第一个请求的 ctx；它被取消时，等待中的请求会一起拿到
context.Canceled。回源很快，暂时接受这个行为。
*/
func (s *Store) GetByCode(ctx context.Context, code string) (*link.Link, error) {
	key := keyPrefix + code

	if l, ok := s.readCache(ctx, key); ok {
		if l == nil {
			return nil, link.ErrNotFound // 负缓存命中
		}
		return l, nil
	}

	v, err, _ := s.group.Do(key, func() (any, error) {
		l, err := s.next.GetByCode(ctx, code)
		if err != nil && !errors.Is(err, link.ErrNotFound) {
			return nil, err // 真正的错误不写缓存，下次请求应该重试
		}
		s.writeCache(ctx, key, l)
		return l, err
	})

	l, _ := v.(*link.Link)
	return l, err
}

/*
readCache 返回缓存中的链接。ok 为 false 表示未命中或 Redis 故障，调用方回源。
value 为 JSON null 时 l 是 nil、err 是 nil，这是一条负缓存，表示短码不存在。

value 解析失败说明缓存里是旧格式或坏数据：删掉它，按未命中处理，让回源重新写入。
Redis 故障也按未命中处理，由调用方回源；降级次数等缓存指标那一步再补。
*/
func (s *Store) readCache(ctx context.Context, key string) (l *link.Link, ok bool) {
	data, err := s.rdb.Get(ctx, key).Bytes()
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &l); err != nil {
			_ = s.rdb.Del(ctx, key).Err()
			return nil, false
		}
		return l, true
	case errors.Is(err, redis.Nil):
		return nil, false
	default:
		return nil, false
	}
}

/*
writeCache 失败只影响下次命中。

负缓存用 SET NX，避免后写的「不存在」盖掉刚创建的正缓存。
正缓存用 SET，创建可以盖掉旧的负缓存。
*/
func (s *Store) writeCache(ctx context.Context, key string, l *link.Link) {
	if ctx.Err() != nil {
		return
	}

	data, err := json.Marshal(l)
	if err != nil {
		return
	}

	if l == nil {
		_ = s.rdb.SetNX(ctx, key, data, negativeTTL).Err()
		return
	}
	_ = s.rdb.Set(ctx, key, data, positiveTTL).Err()
}
