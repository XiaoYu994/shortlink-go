/*
Package memorystore 是内存版的链接存储，只用于测试和本地开发。
它用 map 加读写锁保存链接，短码由 idgen.GenerateCode 随机生成。
*/
package memorystore

import (
	"context"
	"fmt"
	"sync"
	"time"

	"shortlink/internal/idgen"
	"shortlink/internal/link"
)

// Store 是内存版的链接存储。
type Store struct {
	mu     sync.RWMutex
	nextID int64
	links  map[string]link.Link
}

// New 创建空的内存存储。
func New() *Store {
	return &Store{links: make(map[string]link.Link)}
}

/*
Create 分配 ID、生成随机短码、设置创建时间，然后把链接存进 map。
它直接修改传入的 l，调用方在返回后可以读到生成的 Code。
*/
func (s *Store) Create(ctx context.Context, l *link.Link) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	l.ID = s.nextID
	for {
		code, err := idgen.GenerateCode()
		if err != nil {
			return fmt.Errorf("generate code: %w", err)
		}
		// 短码是随机的，可能碰撞；map 里已存在就重新生成。
		if _, exists := s.links[code]; exists {
			continue
		}
		l.Code = code
		break
	}
	l.CreatedAt = time.Now().UTC()

	// map 的值是 Link 而不是指针：赋值和读取都会复制一份，调用方改不到共享数据。
	s.links[l.Code] = *l
	return nil
}

/*
GetByCode 按短码查找链接，找不到返回 link.ErrNotFound。
返回的 Link 是副本，调用方修改它不会影响存储里的数据。
*/
func (s *Store) GetByCode(ctx context.Context, code string) (*link.Link, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	l, ok := s.links[code]
	if !ok {
		return nil, link.ErrNotFound
	}
	return &l, nil
}
