package tx

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"

	"gowms/internal/pkg/errcode"
	"gowms/internal/pkg/log"
)

const (
	// MaxOrderNoRetry 单号唯一索引冲突时的最大重试次数（snowflake 冲突概率极低，兜底用）。
	MaxOrderNoRetry = 3
	// MaxTxRetry 事务并发冲突（乐观锁版本冲突 / MySQL 死锁 1213）时的最大重试次数。
	MaxTxRetry = 3
)

// Manager 事务管理器：事务只在 Service 层通过它开启，Handler/Repository 不感知事务。
type Manager struct {
	db *gorm.DB
}

func New(db *gorm.DB) *Manager { return &Manager{db: db} }

// DB 返回非事务连接，用于查询。
func (m *Manager) DB() *gorm.DB { return m.db }

// Tx 在事务内执行 fn，panic 或返回 error 时回滚。
func (m *Manager) Tx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return m.db.WithContext(ctx).Transaction(fn)
}

// retryBackoff 冲突重试基础退避，第 i 次重试等待 (i+1)*retryBackoff（50ms、100ms、150ms...）。
const retryBackoff = 50 * time.Millisecond

// TxRetry 在事务内执行 fn；遇到并发冲突（乐观锁失败、MySQL 死锁 1213）自动用新事务重试。
// 每次重试都是全新事务，fn 内必须重新读取数据（不要依赖上一轮的内存状态）。
func (m *Manager) TxRetry(ctx context.Context, maxRetry int, fn func(tx *gorm.DB) error) error {
	var err error
	for i := 0; i < maxRetry; i++ {
		err = m.Tx(ctx, fn)
		if err == nil {
			return nil
		}
		if !IsRetryable(err) {
			return err
		}
		log.WithContext(ctx).Warn("tx conflict, retrying", "attempt", i+1, "max", maxRetry, "err", err.Error())
		if i == maxRetry-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i+1) * retryBackoff):
		}
	}
	return err
}

// IsRetryable 判断事务失败后是否可以安全重试：
//   - errcode.IsConflict：乐观锁版本冲突 / 行竞争（事务已回滚，无副作用）
//   - MySQL 死锁错误 1213：InnoDB 自动回滚整个事务，重试安全
//     锁等待超时 1205 不重试（可能是长事务持锁，重试只会加剧排队）。
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errcode.IsConflict(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "deadlock") || strings.Contains(msg, "error 1213")
}

// IsDuplicateErr 判断是否为唯一索引冲突错误（MySQL 1062 / "Duplicate entry"）。
// 用于单号/业务单号唯一索引兜底重试。
func IsDuplicateErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate")
}
