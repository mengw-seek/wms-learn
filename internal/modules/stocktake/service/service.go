package service

import (
	"context"

	"gorm.io/gorm"

	"gowms/internal/modules/inventory/api"
	"gowms/internal/modules/stocktake/dto"
	"gowms/internal/modules/stocktake/model"
	"gowms/internal/modules/stocktake/repository"
	sysmodel "gowms/internal/modules/system/model"
	"gowms/internal/pkg/errcode"
	"gowms/internal/pkg/orderno"
	"gowms/internal/pkg/snowflake"
	"gowms/internal/pkg/tx"
)

type Service struct {
	repo *repository.Repository
	tm   *tx.Manager
	no   *orderno.Generator
	inv  api.InventoryAPI
}

func New(repo *repository.Repository, tm *tx.Manager, no *orderno.Generator, inv api.InventoryAPI) *Service {
	return &Service{repo: repo, tm: tm, no: no, inv: inv}
}

// Create 创建盘点单：按仓库/库位范围快照账面库存（book_qty）。
func (s *Service) Create(ctx context.Context, req *dto.CreateOrderReq, operator string) (*model.StocktakeOrder, error) {
	var order *model.StocktakeOrder
	err := s.tm.Tx(ctx, func(tx *gorm.DB) error {
		details, err := s.repo.SnapshotInventory(tx, req.WarehouseID, req.LocationID)
		if err != nil {
			return err
		}
		if len(details) == 0 {
			return errcode.StocktakeNoDetail
		}
		for _, d := range details {
			d.ID = snowflake.Next()
		}
		order = &model.StocktakeOrder{
			Base: sysmodel.Base{ID: snowflake.Next()}, OrderNo: s.no.Next(ctx, "PD"),
			WarehouseID: req.WarehouseID, LocationID: req.LocationID,
			LocationCode: req.LocationCode, Status: model.OrderDraft,
			Remark: req.Remark, CreatedBy: operator,
		}
		return s.repo.CreateOrder(tx, order, details)
	})
	if err != nil {
		return nil, err
	}
	return order, nil
}

// RecordActual 录入实盘数（仅 DRAFT）。
func (s *Service) RecordActual(ctx context.Context, orderID, detailID int64, actualQty int) error {
	return s.tm.Tx(ctx, func(tx *gorm.DB) error {
		o, err := s.repo.GetOrderForUpdate(tx, orderID)
		if err != nil {
			return errcode.StocktakeNotFound
		}
		if o.Status != model.OrderDraft {
			return errcode.StocktakeStatusWrong
		}
		d, err := s.repo.GetDetail(tx, detailID)
		if err != nil || d.OrderID != orderID {
			return errcode.StocktakeNotFound
		}
		return s.repo.UpdateDetailActual(tx, detailID, actualQty)
	})
}

// Approve 审核调整：以当前实时库存重算差异（行锁内），调用 inventory.Adjust 同事务写 ADJUST 流水。
func (s *Service) Approve(ctx context.Context, orderID int64, operator string) error {
	return s.tm.Tx(ctx, func(tx *gorm.DB) error {
		o, err := s.repo.GetOrderForUpdate(tx, orderID)
		if err != nil {
			return errcode.StocktakeNotFound
		}
		if o.Status != model.OrderDraft {
			return errcode.StocktakeStatusWrong
		}
		details, err := s.repo.ListDetails(tx, orderID)
		if err != nil {
			return err
		}
		anyCounted := false
		for _, d := range details {
			if d.ActualQty == nil || d.Adjusted {
				continue
			}
			anyCounted = true
			actual := *d.ActualQty
			// 以当前实时库存重算差异（快照后库存可能已变动）
			var current int
			if err := tx.Table("wms_inventory").Where("id = ?", d.InventoryID).Pluck("stock_quantity", &current).Error; err != nil {
				continue // 库存行已删除，跳过
			}
			diff := actual - current
			if diff != 0 {
				if err := s.inv.Adjust(ctx, tx, &api.AdjustReq{
					InventoryID: d.InventoryID, NewStock: actual,
					OrderNo: o.OrderNo, Operator: operator,
				}); err != nil {
					return err
				}
			}
			if err := s.repo.MarkAdjusted(tx, d.ID, diff); err != nil {
				return err
			}
		}
		if !anyCounted {
			return errcode.StocktakeNoDetail
		}
		if n, err := s.repo.UpdateStatus(tx, orderID, model.OrderDraft, model.OrderCompleted); err != nil {
			return err
		} else if n == 0 {
			return errcode.StocktakeStatusWrong
		}
		return nil
	})
}

func (s *Service) Cancel(ctx context.Context, orderID int64) error {
	return s.tm.Tx(ctx, func(tx *gorm.DB) error {
		if _, err := s.repo.GetOrderForUpdate(tx, orderID); err != nil {
			return errcode.StocktakeNotFound
		}
		if n, err := s.repo.UpdateStatus(tx, orderID, model.OrderDraft, model.OrderCancelled); err != nil {
			return err
		} else if n == 0 {
			return errcode.StocktakeStatusWrong
		}
		return nil
	})
}

type OrderDetail struct {
	Order   *model.StocktakeOrder    `json:"order"`
	Details []*model.StocktakeDetail `json:"details"`
}

func (s *Service) Get(ctx context.Context, id int64) (*OrderDetail, error) {
	o, err := s.repo.GetOrder(ctx, s.tm.DB(), id)
	if err != nil {
		return nil, errcode.StocktakeNotFound
	}
	details, err := s.repo.ListDetails(s.tm.DB(), id)
	if err != nil {
		return nil, err
	}
	return &OrderDetail{Order: o, Details: details}, nil
}

func (s *Service) List(ctx context.Context, q *dto.OrderQuery) ([]*model.StocktakeOrder, int64, error) {
	return s.repo.ListOrders(ctx, s.tm.DB(), q.WarehouseID, q.Status, q.Page, q.PageSize)
}
