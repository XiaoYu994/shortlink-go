package link

import (
	"context"
	"sync"
	"time"

	"shortlink/internal/idgen"
)

/*
MemoryStore 是内存版的链接存储，用 map 加读写锁保存链接。
它让服务在没有数据库时也能跑起来，同时给 httpapi 的测试提供实现。
*/
type MemoryStore struct {
	mu     sync.RWMutex
	nextID int64
	links  map[string]Link
}

// NewMemoryStore 创建空的内存存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{links: make(map[string]Link)}
}

/*
Create 分配 ID 和短码、设置创建时间，然后把链接存进 map。
它直接修改传入的 l，调用方在返回后可以读到生成的 Code。
*/
func (s *MemoryStore) Create(ctx context.Context, l *Link) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	l.ID = s.nextID
	l.Code = idgen.EncodeBase62(l.ID)
	l.CreatedAt = time.Now().UTC()

	// map 的值是 Link 而不是指针：赋值和读取都会复制一份，调用方改不到共享数据。
	s.links[l.Code] = *l
	return nil
}

/*
GetByCode 按短码查找链接，找不到返回 ErrNotFound。
返回的 Link 是副本，调用方修改它不会影响存储里的数据。
*/
func (s *MemoryStore) GetByCode(ctx context.Context, code string) (*Link, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	l, ok := s.links[code]
	if !ok {
		return nil, ErrNotFound
	}
	return &l, nil
}
