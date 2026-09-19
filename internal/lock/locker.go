package lock

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrLockTimeout = errors.New("lock acquisition timeout")
)

// Locker 统一并发互斥锁抽象接口
type Locker interface {
	// Acquire 获取指定 Key 的锁，成功后返回释放回调函数
	Acquire(ctx context.Context, key string, ttl time.Duration) (unlock func(), err error)
}

// KeyedMutexLocker 细粒度进程内互斥锁实现（按 Key 独立锁定，避免粗粒度全局互斥造成的性能瓶颈）
type KeyedMutexLocker struct {
	mu    sync.Mutex
	locks map[string]*entry
}

type entry struct {
	ch      chan struct{}
	ref     int
	ownerID string
}

func NewKeyedMutexLocker() *KeyedMutexLocker {
	return &KeyedMutexLocker{
		locks: make(map[string]*entry),
	}
}

func (l *KeyedMutexLocker) Acquire(ctx context.Context, key string, ttl time.Duration) (func(), error) {
	l.mu.Lock()
	e, exists := l.locks[key]
	if !exists {
		e = &entry{
			ch:  make(chan struct{}, 1),
			ref: 0,
		}
		e.ch <- struct{}{} // 放入初始令牌
		l.locks[key] = e
	}
	e.ref++
	l.mu.Unlock()

	var timer *time.Timer
	var timeoutChan <-chan time.Time
	if ttl > 0 {
		timer = time.NewTimer(ttl)
		defer timer.Stop()
		timeoutChan = timer.C
	}

	select {
	case <-ctx.Done():
		l.releaseRef(key)
		return nil, ctx.Err()
	case <-timeoutChan:
		l.releaseRef(key)
		return nil, fmt.Errorf("%w for key [%s] after %v", ErrLockTimeout, key, ttl)
	case <-e.ch:
		// 成功取得令牌
	}

	var once sync.Once
	unlock := func() {
		once.Do(func() {
			e.ch <- struct{}{} // 归还令牌
			l.releaseRef(key)
		})
	}

	return unlock, nil
}

func (l *KeyedMutexLocker) releaseRef(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if e, ok := l.locks[key]; ok {
		e.ref--
		if e.ref <= 0 {
			delete(l.locks, key)
		}
	}
}
