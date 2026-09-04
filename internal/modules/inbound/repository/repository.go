package repository

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"gowms/internal/modules/inbound/model"
)

type Repository struct{}

func New() *Repository { return &Repository{} }

// ---------- 入库单 ----------

func (r *Repository) CreateOrder(tx *gorm.DB, order *model.ReceiptOrder, details []*model.ReceiptOrderDetail) error {
	return tx.Transaction(func(tx2 *gorm.DB) error {
		if err := tx2.Create(order).Error; err != nil {
			return err
		}
		for _, d := range details {
			d.OrderID = order.ID
		}
		return tx2.CreateInBatches(details, 100).Error
	})
}

// GetOrderForUpdate 事务内锁定入库单（乐观锁 version 配合状态推进）。
func (r *Repository) GetOrderForUpdate(tx *gorm.DB, id int64) (*model.ReceiptOrder, error) {
	var o model.ReceiptOrder
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&o, id).Error
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func (r *Repository) GetOrder(ctx context.Context, db *gorm.DB, id int64) (*model.ReceiptOrder, error) {
	var o model.ReceiptOrder
	if err := db.WithContext(ctx).First(&o, id).Error; err != nil {
		return nil, err
	}
	return &o, nil
}

// UpdateStatus 状态推进：WHERE status = from AND version = version 防并发跳变。
func (r *Repository) UpdateStatus(tx *gorm.DB, id int64, from, to model.OrderStatus) (int64, error) {
	res := tx.Model(&model.ReceiptOrder{}).
		Where("id = ? AND status = ?", id, from).
		Update("status", to)
	return res.RowsAffected, res.Error
}

// ReplaceDetails 仅 DRAFT 状态允许编辑明细（调用方先校验状态）。
func (r *Repository) ReplaceDetails(tx *gorm.DB, orderID int64, details []*model.ReceiptOrderDetail) error {
	if err := tx.Where("order_id = ?", orderID).Delete(&model.ReceiptOrderDetail{}).Error; err != nil {
		return err
	}
	for _, d := range details {
		d.OrderID = orderID
	}
	return tx.CreateInBatches(details, 100).Error
}

func (r *Repository) DeleteOrder(tx *gorm.DB, id int64) error {
	if err := tx.Delete(&model.ReceiptOrder{}, id).Error; err != nil {
		return err
	}
	return tx.Where("order_id = ?", id).Delete(&model.ReceiptOrderDetail{}).Error
}

func (r *Repository) ListDetails(tx *gorm.DB, orderID int64) ([]*model.ReceiptOrderDetail, error) {
	var list []*model.ReceiptOrderDetail
	err := tx.Where("order_id = ?", orderID).Order("id").Find(&list).Error
	return list, err
}

// GetDetailForUpdate 锁定明细行（收货累计校验）。
func (r *Repository) GetDetailForUpdate(tx *gorm.DB, detailID int64) (*model.ReceiptOrderDetail, error) {
	var d model.ReceiptOrderDetail
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&d, detailID).Error
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// UpdateDetailReceive 累加明细收货/残品数，并可落批次号。
func (r *Repository) UpdateDetailReceive(tx *gorm.DB, d *model.ReceiptOrderDetail) error {
	return tx.Model(&model.ReceiptOrderDetail{}).Where("id = ?", d.ID).Updates(map[string]any{
		"received_qty": d.ReceivedQty, "defective_qty": d.DefectiveQty, "batch_no": d.BatchNo,
	}).Error
}

// UpdateOrderReceive 累加主单收货/残品数并推进状态。
func (r *Repository) UpdateOrderReceive(tx *gorm.DB, o *model.ReceiptOrder) error {
	return tx.Model(&model.ReceiptOrder{}).Where("id = ? AND version = ?", o.ID, o.Version).Updates(map[string]any{
		"received_qty": o.ReceivedQty, "defective_qty": o.DefectiveQty,
		"status": o.Status, "version": o.Version + 1,
	}).Error
}

func (r *Repository) ListOrders(ctx context.Context, db *gorm.DB, warehouseID int64, status, keyword string, page, size int) ([]*model.ReceiptOrder, int64, error) {
	q := db.WithContext(ctx).Model(&model.ReceiptOrder{})
	if warehouseID > 0 {
		q = q.Where("warehouse_id = ?", warehouseID)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if keyword != "" {
		q = q.Where("order_no LIKE ?", "%"+keyword+"%")
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var list []*model.ReceiptOrder
	err := q.Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&list).Error
	return list, total, err
}

// ---------- 导入任务 ----------

func (r *Repository) CreateImportTask(ctx context.Context, db *gorm.DB, t *model.ImportTask) error {
	return db.WithContext(ctx).Create(t).Error
}

func (r *Repository) GetImportTask(ctx context.Context, db *gorm.DB, taskID string) (*model.ImportTask, error) {
	var t model.ImportTask
	if err := db.WithContext(ctx).Where("task_id = ?", taskID).First(&t).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

// CASImportStatus 乐观更新导入任务状态（CAS 防重复执行）。
func (r *Repository) CASImportStatus(db *gorm.DB, taskID string, from, to model.ImportTaskStatus) (int64, error) {
	res := db.Model(&model.ImportTask{}).
		Where("task_id = ? AND status = ?", taskID, from).
		Update("status", to)
	return res.RowsAffected, res.Error
}

// FinishImport 写入结果（仅 PROCESSING 时生效）。
func (r *Repository) FinishImport(db *gorm.DB, taskID string, status model.ImportTaskStatus, total, success, fail int, errMsg string) error {
	return db.Model(&model.ImportTask{}).
		Where("task_id = ? AND status = ?", taskID, model.ImportProcessing).
		Updates(map[string]any{
			"status": status, "total_rows": total, "success_rows": success, "fail_rows": fail, "error_msg": errMsg,
		}).Error
}

// TouchImport 刷新 updated_at（心跳，防止被悬挂补偿误判）。
func (r *Repository) TouchImport(db *gorm.DB, taskID string) error {
	return db.Model(&model.ImportTask{}).Where("task_id = ?", taskID).Update("updated_at", gorm.Expr("NOW()")).Error
}

// ListStaleImports 悬挂任务扫描：PENDING 超时 / PROCESSING 心跳超时。
func (r *Repository) ListStaleImports(ctx context.Context, db *gorm.DB, pendingBefore, processingBefore time.Time) ([]*model.ImportTask, error) {
	var list []*model.ImportTask
	err := db.WithContext(ctx).Model(&model.ImportTask{}).
		Where("(status = ? AND updated_at < ?) OR (status = ? AND updated_at < ?)",
			model.ImportPending, pendingBefore, model.ImportProcessing, processingBefore).
		Limit(10).Find(&list).Error
	return list, err
}

// ResetProcessingToPending 悬挂 PROCESSING 任务复位为 PENDING（CAS）。
func (r *Repository) ResetProcessingToPending(db *gorm.DB, taskID string) (int64, error) {
	return r.CASImportStatus(db, taskID, model.ImportProcessing, model.ImportPending)
}
