package repository

import (
	"context"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"gowms/internal/modules/outbound/model"
)

type Repository struct{}

func New() *Repository { return &Repository{} }

// ---------- 出库单 ----------

func (r *Repository) CreateOrder(tx *gorm.DB, order *model.ShipmentOrder, details []*model.ShipmentOrderDetail) error {
	if err := tx.Create(order).Error; err != nil {
		return err
	}
	for _, d := range details {
		d.OrderID = order.ID
	}
	return tx.CreateInBatches(details, 100).Error
}

func (r *Repository) GetOrderForUpdate(tx *gorm.DB, id int64) (*model.ShipmentOrder, error) {
	var o model.ShipmentOrder
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&o, id).Error
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func (r *Repository) GetOrder(ctx context.Context, db *gorm.DB, id int64) (*model.ShipmentOrder, error) {
	var o model.ShipmentOrder
	if err := db.WithContext(ctx).First(&o, id).Error; err != nil {
		return nil, err
	}
	return &o, nil
}

func (r *Repository) GetOrderByBizNo(ctx context.Context, db *gorm.DB, bizNo string) (*model.ShipmentOrder, error) {
	var o model.ShipmentOrder
	err := db.WithContext(ctx).Where("biz_order_no = ?", bizNo).First(&o).Error
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// UpdateStatus 状态推进（乐观：WHERE status = from）。
func (r *Repository) UpdateStatus(tx *gorm.DB, id int64, from, to model.OrderStatus) (int64, error) {
	res := tx.Model(&model.ShipmentOrder{}).Where("id = ? AND status = ?", id, from).Update("status", to)
	return res.RowsAffected, res.Error
}

func (r *Repository) DeleteOrder(tx *gorm.DB, id int64) error {
	if err := tx.Delete(&model.ShipmentOrder{}, id).Error; err != nil {
		return err
	}
	return tx.Where("order_id = ?", id).Delete(&model.ShipmentOrderDetail{}).Error
}

// ---------- 明细 ----------

func (r *Repository) ListDetails(tx *gorm.DB, orderID int64) ([]*model.ShipmentOrderDetail, error) {
	var list []*model.ShipmentOrderDetail
	err := tx.Where("order_id = ?", orderID).Order("id").Find(&list).Error
	return list, err
}

// GetDetailForUpdate 锁定明细行。
func (r *Repository) GetDetailForUpdate(tx *gorm.DB, detailID int64) (*model.ShipmentOrderDetail, error) {
	var d model.ShipmentOrderDetail
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&d, detailID).Error
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// UpdateDetailAllocated 累加明细分配量。
func (r *Repository) UpdateDetailAllocated(tx *gorm.DB, d *model.ShipmentOrderDetail) error {
	return tx.Model(&model.ShipmentOrderDetail{}).Where("id = ?", d.ID).
		Update("allocated_qty", d.AllocatedQty).Error
}

// UpdateDetailPicked 累加明细拣货量。
func (r *Repository) UpdateDetailPicked(tx *gorm.DB, d *model.ShipmentOrderDetail) error {
	return tx.Model(&model.ShipmentOrderDetail{}).Where("id = ?", d.ID).
		Update("picked_qty", d.PickedQty).Error
}

// UpdateOrderProgress 累加主单分配/拣货量并推进状态（乐观锁）。
func (r *Repository) UpdateOrderProgress(tx *gorm.DB, o *model.ShipmentOrder) error {
	return tx.Model(&model.ShipmentOrder{}).Where("id = ? AND version = ?", o.ID, o.Version).Updates(map[string]any{
		"allocated_qty": o.AllocatedQty, "picked_qty": o.PickedQty,
		"status": o.Status, "version": o.Version + 1,
	}).Error
}

func (r *Repository) ListOrders(ctx context.Context, db *gorm.DB, warehouseID int64, status, keyword string, page, size int) ([]*model.ShipmentOrder, int64, error) {
	q := db.WithContext(ctx).Model(&model.ShipmentOrder{})
	if warehouseID > 0 {
		q = q.Where("warehouse_id = ?", warehouseID)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if keyword != "" {
		q = q.Where("order_no LIKE ? OR biz_order_no LIKE ?", "%"+keyword+"%", "%"+keyword+"%")
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var list []*model.ShipmentOrder
	err := q.Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&list).Error
	return list, total, err
}

// ---------- 分配行 ----------

func (r *Repository) CreateAllocations(tx *gorm.DB, list []*model.Allocation) error {
	return tx.CreateInBatches(list, 100).Error
}

func (r *Repository) ListAllocations(tx *gorm.DB, orderID int64) ([]*model.Allocation, error) {
	var list []*model.Allocation
	err := tx.Where("order_id = ?", orderID).Order("id").Find(&list).Error
	return list, err
}

func (r *Repository) GetAllocationForUpdate(tx *gorm.DB, id int64) (*model.Allocation, error) {
	var a model.Allocation
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&a, id).Error
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// UpdateAllocationPicked 更新分配行拣货量与状态。
func (r *Repository) UpdateAllocationPicked(tx *gorm.DB, a *model.Allocation) error {
	return tx.Model(&model.Allocation{}).Where("id = ? AND version = ?", a.ID, a.Version).Updates(map[string]any{
		"picked_qty": a.PickedQty, "status": a.Status, "version": a.Version + 1,
	}).Error
}

// CancelAllocationsByOrder 取消单据全部已分配未拣货行。
func (r *Repository) CancelAllocationsByOrder(tx *gorm.DB, orderID int64) (int64, error) {
	res := tx.Model(&model.Allocation{}).
		Where("order_id = ? AND status = ?", orderID, model.AllocAllocated).
		Update("status", model.AllocCancelled)
	return res.RowsAffected, res.Error
}
