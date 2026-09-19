package lock

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestKeyedMutexLocker_Basic(t *testing.T) {
	locker := NewKeyedMutexLocker()
	ctx := context.Background()

	unlock, err := locker.Acquire(ctx, "res1", time.Second)
	assert.NoError(t, err)
	assert.NotNil(t, unlock)

	// 在持有锁的情况下，另一个协程尝试获取相同的锁，应阻塞直到释放
	acquiredSecond := atomic.Bool{}
	go func() {
		u2, err2 := locker.Acquire(ctx, "res1", time.Second)
		if err2 == nil {
			acquiredSecond.Store(true)
			u2()
		}
	}()

	time.Sleep(50 * time.Millisecond)
	assert.False(t, acquiredSecond.Load(), "相同 key 在释放前不应被再次获取")

	unlock()

	time.Sleep(50 * time.Millisecond)
	assert.True(t, acquiredSecond.Load(), "释放后另一个协程应当能成功获取锁")
}

func TestKeyedMutexLocker_DifferentKeys(t *testing.T) {
	locker := NewKeyedMutexLocker()
	ctx := context.Background()

	u1, err1 := locker.Acquire(ctx, "keyA", time.Second)
	assert.NoError(t, err1)
	defer u1()

	// 不同的 key 应该立即成功，不受影响
	u2, err2 := locker.Acquire(ctx, "keyB", time.Second)
	assert.NoError(t, err2)
	u2()
}

func TestKeyedMutexLocker_Timeout(t *testing.T) {
	locker := NewKeyedMutexLocker()
	ctx := context.Background()

	u1, err1 := locker.Acquire(ctx, "timeout_key", time.Second)
	assert.NoError(t, err1)
	defer u1()

	// 短超时应返回超时错误
	_, err2 := locker.Acquire(ctx, "timeout_key", 50*time.Millisecond)
	assert.Error(t, err2)
	assert.ErrorIs(t, err2, ErrLockTimeout)
}

func TestKeyedMutexLocker_ConcurrentSafety(t *testing.T) {
	locker := NewKeyedMutexLocker()
	ctx := context.Background()

	counter := 0
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := locker.Acquire(ctx, "shared_counter", 3*time.Second)
			if assert.NoError(t, err) {
				counter++
				time.Sleep(1 * time.Millisecond)
				unlock()
			}
		}()
	}

	wg.Wait()
	assert.Equal(t, 50, counter)
}
