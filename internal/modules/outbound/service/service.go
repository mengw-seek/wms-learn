package service

import (
	"context"
	"sort"

	"gorm.io/gorm"

	basicapi "gowms/internal/modules/basic/api"
	invapi "gowms/internal/modules/inventory/api"
	"gowms/internal/modules/outbound/dto"
	"gowms/internal/modules/outbound/model"
	"gowms/internal/modules/outbound/repository"
	sysmodel "gowms/internal/modules/system/model"
	taskapi "gowms/internal/modules/task/api"
	taskmodel "gowms/internal/modules/task/model"
	"gowms/internal/pkg/errcode"
	"gowms/internal/pkg/log"
	"gowms/internal/pkg/orderno"
	"gowms/internal/pkg/snowflake"
	"gowms/internal/pkg/tx"
)

type Service struct {
	repo    *repository.Repository
	tm      *tx.Manager
	no      *orderno.Generator
	basic   basicapi.BasicAPI
	inv     invapi.InventoryAPI
	taskAPI taskapi.TaskAPI
}

func New(repo *repository.Repository, tm *tx.Manager, no *orderno.Generator,
	basic basicapi.BasicAPI, inv invapi.InventoryAPI, taskAPI taskapi.TaskAPI) *Service {
	return &Service{repo: repo, tm: tm, no: no, basic: basic, inv: inv, taskAPI: taskAPI}
}

// ---------- 单据生命周期 ----------

// Create 创建出库单（CK 单号；biz_order_no 幂等键，唯一索引防重复创建）。
func (s *Service) Create(ctx context.Context, req *dto.CreateOrderReq, operator string) (*model.ShipmentOrder, error) {
	if err := s.basic.ValidateWarehouse(ctx, req.WarehouseID); err != nil {
		return nil, err
	}
	if _, err := s.repo.GetOrderByBizNo(ctx, s.tm.DB(), req.BizOrderNo); err == nil {
		return nil, errcode.BizOrderDuplicate
	}
	details, expected, err := s.buildDetails(ctx, req.Details)
	if err != nil {
		return nil, err
	}
	var order *model.ShipmentOrder
	for i := 0; i < tx.MaxOrderNoRetry; i++ {
		order = &model.ShipmentOrder{
			Base: sysmodel.Base{ID: snowflake.Next()}, OrderNo: s.no.Next(ctx, model.OrderNoPrefix),
			BizOrderNo: req.BizOrderNo, WarehouseID: req.WarehouseID,
			Status: model.OrderDraft, Remark: req.Remark, ExpectedQty: expected, CreatedBy: operator,
		}
		err = s.tm.Tx(ctx, func(tx *gorm.DB) error {
			return s.repo.CreateOrder(tx, order, details)
		})
		if err == nil {
			return order, nil
		}
		if !tx.IsDuplicateErr(err) {
			return nil, err
		}
		log.WithContext(ctx).Warn("order_no duplicated, retry", "order_no", order.OrderNo)
	}
	return nil, errcode.OrderNoDuplicate
}

func (s *Service) Delete(ctx context.Context, id int64) error {
	return s.tm.Tx(ctx, func(tx *gorm.DB) error {
		o, err := s.repo.GetOrderForUpdate(tx, id)
		if err != nil {
			return errcode.ShipOrderNotFound
		}
		if o.Status != model.OrderDraft {
			return errcode.ShipOrderStatusWrong
		}
		return s.repo.DeleteOrder(tx, id)
	})
}

func (s *Service) Submit(ctx context.Context, id int64) error {
	return s.tm.Tx(ctx, func(tx *gorm.DB) error {
		o, err := s.repo.GetOrderForUpdate(tx, id)
		if err != nil {
			return errcode.ShipOrderNotFound
		}
		if !model.CanTransit(o.Status, model.OrderSubmitted) {
			return errcode.ShipOrderStatusWrong
		}
		if n, err := s.repo.UpdateStatus(tx, id, o.Status, model.OrderSubmitted); err != nil {
			return err
		} else if n == 0 {
			return errcode.ShipOrderVersionBad
		}
		return nil
	})
}

// Approve 审核 + 分配：同一事务内完成审核通过即调用 inventory.Allocate，分配失败则审核回滚。
// 分配成功后生成拣货任务（按分配行，一分配行一任务），单据进入 PICKING。
func (s *Service) Approve(ctx context.Context, id int64, operator string) error {
	return s.tm.Tx(ctx, func(tx *gorm.DB) error {
		o, err := s.repo.GetOrderForUpdate(tx, id)
		if err != nil {
			return errcode.ShipOrderNotFound
		}
		if o.Status != model.OrderSubmitted {
			return errcode.ShipOrderStatusWrong
		}
		details, err := s.repo.ListDetails(tx, id)
		if err != nil {
			return err
		}
		// 按 SKU_ID 排序后再分配：所有事务库存行加锁顺序全局一致，
		// 避免并发审核交叉加锁（T1 持 A 等 B，T2 持 B 等 A）导致死锁
		sort.Slice(details, func(i, j int) bool { return details[i].SKUID < details[j].SKUID })

		// 逐明细 FIFO 分配（失败即整体回滚）
		allocations := make([]*model.Allocation, 0, len(details))
		for _, d := range details {
			result, err := s.inv.Allocate(ctx, tx, &invapi.AllocateReq{
				WarehouseID: o.WarehouseID, SKUID: d.SKUID,
				Quantity: d.ExpectedQty, OrderNo: o.OrderNo, Operator: operator,
			})
			if err != nil {
				return err // 可用不足：明确报错"需要N，实际可用M"，事务回滚
			}
			d.AllocatedQty = d.ExpectedQty
			if err := s.repo.UpdateDetailAllocated(tx, d); err != nil {
				return err
			}
			for _, row := range result.Rows {
				allocations = append(allocations, &model.Allocation{
					Base: sysmodel.Base{ID: snowflake.Next()}, OrderID: o.ID, DetailID: d.ID,
					InventoryID: row.InventoryID, SKUID: d.SKUID,
					LocationID: row.LocationID, LocationCode: row.LocationCode, BatchNo: row.BatchNo,
					AllocatedQty: row.Quantity, Status: model.AllocAllocated,
				})
			}
		}
		// 按储位编码排序：FIFO 分配结果不变（扣哪个批次多少个已定），
		// 只调整拣货顺序，让拣货员按储位字典序走一遍，优化拣货路径。
		sort.Slice(allocations, func(i, j int) bool {
			return allocations[i].LocationCode < allocations[j].LocationCode
		})
		if err := s.repo.CreateAllocations(tx, allocations); err != nil {
			return err
		}

		// 状态机校验 + 主单累加分配量，状态推进 SUBMITTED → PICKING（审核即分配）
		if !model.CanTransit(o.Status, model.OrderPicking) {
			return errcode.ShipOrderStatusWrong
		}
		o.AllocatedQty = o.ExpectedQty
		o.Status = model.OrderPicking
		if n, err := s.repo.UpdateOrderProgress(tx, o); err != nil {
			return err
		} else if n == 0 {
			return errcode.ShipOrderVersionBad
		}

		// 按分配行生成拣货任务
		tasks := make([]*taskapi.CreateTask, 0, len(allocations))
		for _, a := range allocations {
			tasks = append(tasks, &taskapi.CreateTask{
				TaskType: taskmodel.TaskPick, OrderID: o.ID, OrderNo: o.OrderNo,
				AllocationID: a.ID, SKUID: a.SKUID, WarehouseID: o.WarehouseID, TargetQty: a.AllocatedQty,
			})
		}
		return s.taskAPI.Create(ctx, tx, tasks)
	})
}

// Cancel 取消：
// DRAFT/SUBMITTED 直接取消；已分配未拣货（APPROVED/PICKING 且 picked=0）释放库存；
// 已拣货/已发货禁止取消。
func (s *Service) Cancel(ctx context.Context, id int64, operator string) error {
	return s.tm.Tx(ctx, func(tx *gorm.DB) error {
		o, err := s.repo.GetOrderForUpdate(tx, id)
		if err != nil {
			return errcode.ShipOrderNotFound
		}
		// 状态机校验：终态 SHIPPED/CANCELLED 不在转换表中，直接拒绝取消
		if !model.CanTransit(o.Status, model.OrderCancelled) {
			return errcode.ShipOrderStatusWrong
		}
		switch o.Status {
		case model.OrderDraft, model.OrderSubmitted:
			if n, err := s.repo.UpdateStatus(tx, id, o.Status, model.OrderCancelled); err != nil {
				return err
			} else if n == 0 {
				return errcode.ShipOrderVersionBad
			}
			return s.taskAPI.CancelByOrder(ctx, tx, id)
		case model.OrderApproved, model.OrderPicking:
			if o.PickedQty > 0 {
				return errcode.ShipShippedForbidden
			}
		default:
			return errcode.ShipOrderStatusWrong
		}

		// 释放已锁定库存
		allocations, err := s.repo.ListAllocations(tx, id)
		if err != nil {
			return err
		}
		for _, a := range allocations {
			if a.Status != model.AllocAllocated {
				continue
			}
			if err := s.inv.Release(ctx, tx, &invapi.ReleaseReq{
				InventoryID: a.InventoryID, Quantity: a.AllocatedQty,
				OrderNo: o.OrderNo, Operator: operator,
			}); err != nil {
				return err
			}
		}
		if n, err := s.repo.CancelAllocationsByOrder(tx, id); err != nil {
			return err
		} else if n == 0 && len(allocations) > 0 {
			return errcode.AllocConflict
		}
		if n, err := s.repo.UpdateStatus(tx, id, o.Status, model.OrderCancelled); err != nil {
			return err
		} else if n == 0 {
			return errcode.ShipOrderVersionBad
		}
		return s.taskAPI.CancelByOrder(ctx, tx, id)
	})
}

// ---------- 拣货/发货 ----------

// Pick 拣货执行：分次拣货，分配行拣满自动发货扣减 inventory.Ship。
// 全部分配行拣完 → 单据 SHIPPED。
// 并发拣货/死锁由 TxRetry 自动整事务重试；数量累加全部使用数据库原子递增。
func (s *Service) Pick(ctx context.Context, taskID int64, qty int, operator string) error {
	// 事务外只读不可变路由信息（OrderID/AllocationID/TaskType/TaskNo 建后不变）
	routing, err := s.taskAPI.Get(ctx, taskID)
	if err != nil {
		return errcode.TaskNotFound
	}
	if routing.TaskType != taskmodel.TaskPick {
		return errcode.TaskStatusWrong
	}
	return s.tm.TxRetry(ctx, tx.MaxTxRetry, func(tx *gorm.DB) error {
		// 锁顺序：主单 → 任务 → 分配行（与 Cancel 的 单据→分配行→任务 保持一致，降低死锁概率）
		o, err := s.repo.GetOrderForUpdate(tx, routing.OrderID)
		if err != nil {
			return errcode.ShipOrderNotFound
		}
		if o.Status != model.OrderPicking {
			return errcode.ShipOrderStatusWrong
		}
		// 事务内行锁读取任务：权威校验任务未被并发取消（AddProgress 内部也会锁读复核）
		t, err := s.taskAPI.GetForUpdate(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if t.Status != taskmodel.TaskCreated && t.Status != taskmodel.TaskInProgress {
			return errcode.TaskStatusWrong
		}
		a, err := s.repo.GetAllocationForUpdate(tx, routing.AllocationID)
		if err != nil {
			return errcode.ShipOrderNotFound
		}
		if a.Status != model.AllocAllocated {
			return errcode.TaskStatusWrong
		}
		// 推进拣货任务（内部行锁 + 校验数量不超剩余 + 任务状态机）
		if err := s.taskAPI.AddProgress(ctx, tx, taskID, qty, operator); err != nil {
			return err
		}
		// 分配行原子累加；拣满置 PICKED（行已锁，base+delta 即更新后值，用于决策）
		newAllocPicked := a.PickedQty + qty
		allocToStatus := model.AllocAllocated
		allocFullyPicked := newAllocPicked == a.AllocatedQty
		if allocFullyPicked {
			allocToStatus = model.AllocPicked
		}
		if n, err := s.repo.IncrAllocationPicked(tx, a, qty, allocToStatus); err != nil {
			return err
		} else if n == 0 {
			return errcode.AllocConflict
		}
		// 主单原子累加拣货量（不依赖内存对象回写）
		if n, err := s.repo.IncrOrderPicked(tx, o.ID, o.Version, qty); err != nil {
			return err
		} else if n == 0 {
			return errcode.ShipOrderVersionBad
		}
		// 明细原子累加
		if err := s.repo.IncrDetailPicked(tx, a.DetailID, qty); err != nil {
			return err
		}

		// 分配行拣满 → 发货扣减库存
		if allocFullyPicked {
			if err := s.inv.Ship(ctx, tx, &invapi.ShipReq{
				InventoryID: a.InventoryID, Quantity: a.AllocatedQty,
				OrderNo: o.OrderNo, TaskNo: t.TaskNo, Operator: operator,
			}); err != nil {
				return err
			}
		}
		// 主单全部拣完 → SHIPPED（行锁内 base+delta 决策，状态机校验 + CAS 兜底）
		newOrderPicked := o.PickedQty + qty
		if o.AllocatedQty-newOrderPicked == 0 {
			if !model.CanTransit(o.Status, model.OrderShipped) {
				return errcode.ShipOrderStatusWrong
			}
			if n, err := s.repo.UpdateStatus(tx, o.ID, model.OrderPicking, model.OrderShipped); err != nil {
				return err
			} else if n == 0 {
				return errcode.ShipOrderVersionBad
			}
		}
		return nil
	})
}

// ---------- 查询 ----------

type OrderDetail struct {
	Order       *model.ShipmentOrder         `json:"order"`
	Details     []*model.ShipmentOrderDetail `json:"details"`
	Allocations []*model.Allocation          `json:"allocations"`
	Tasks       []*taskmodel.Task            `json:"tasks"`
}

func (s *Service) Get(ctx context.Context, id int64) (*OrderDetail, error) {
	o, err := s.repo.GetOrder(ctx, s.tm.DB(), id)
	if err != nil {
		return nil, errcode.ShipOrderNotFound
	}
	details, err := s.repo.ListDetails(s.tm.DB(), id)
	if err != nil {
		return nil, err
	}
	allocations, err := s.repo.ListAllocations(s.tm.DB(), id)
	if err != nil {
		return nil, err
	}
	tasks, _, err := s.taskAPI.List(ctx, id, "", 1, taskapi.DetailTaskPageSize)
	if err != nil {
		return nil, err
	}
	return &OrderDetail{Order: o, Details: details, Allocations: allocations, Tasks: tasks}, nil
}

func (s *Service) List(ctx context.Context, q *dto.OrderQuery) ([]*model.ShipmentOrder, int64, error) {
	return s.repo.ListOrders(ctx, s.tm.DB(), q.WarehouseID, q.Status, q.Keyword, q.Page, q.PageSize)
}

// ---------- 内部 ----------

func (s *Service) buildDetails(ctx context.Context, items []dto.OrderDetailItem) ([]*model.ShipmentOrderDetail, int, error) {
	details := make([]*model.ShipmentOrderDetail, 0, len(items))
	expected := 0
	seen := map[int64]struct{}{}
	for _, it := range items {
		if _, dup := seen[it.SKUID]; dup {
			return nil, 0, errcode.ShipDetailDuplicateSKU
		}
		seen[it.SKUID] = struct{}{}
		sku, err := s.basic.GetSKU(ctx, it.SKUID)
		if err != nil {
			return nil, 0, err
		}
		details = append(details, &model.ShipmentOrderDetail{
			Base: sysmodel.Base{ID: snowflake.Next()}, SKUID: sku.ID, SKUCode: sku.Code, SKUName: sku.Name,
			ExpectedQty: it.ExpectedQty,
		})
		expected += it.ExpectedQty
	}
	return details, expected, nil
}
