package tx

import (
	"context"

	"gorm.io/gorm"
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
