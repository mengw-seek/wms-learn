package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
	"gorm.io/gorm"

	basicapi "gowms/internal/modules/basic/api"
	basicmodel "gowms/internal/modules/basic/model"
	"gowms/internal/modules/inbound/dto"
	"gowms/internal/modules/inbound/model"
	"gowms/internal/modules/inbound/repository"
	invapi "gowms/internal/modules/inventory/api"
	sysmodel "gowms/internal/modules/system/model"
	taskapi "gowms/internal/modules/task/api"
	taskmodel "gowms/internal/modules/task/model"
	"gowms/internal/pkg/errcode"
	"gowms/internal/pkg/lock"
	"gowms/internal/pkg/log"
	"gowms/internal/pkg/orderno"
	"gowms/internal/pkg/snowflake"
	"gowms/internal/pkg/tx"
)

type Service struct {
	repo      *repository.Repository
	tm        *tx.Manager
	no        *orderno.Generator
	basic     basicapi.BasicAPI
	inv       invapi.InventoryAPI
	taskAPI   taskapi.TaskAPI
	uploadDir string
	locker    *lock.Locker // 补偿扫描分布式锁，多实例部署防重复补偿；nil 时跳过（单实例语义）
}

func New(repo *repository.Repository, tm *tx.Manager, no *orderno.Generator,
	basic basicapi.BasicAPI, inv invapi.InventoryAPI, taskAPI taskapi.TaskAPI, uploadDir string,
	locker *lock.Locker) *Service {
	return &Service{repo: repo, tm: tm, no: no, basic: basic, inv: inv, taskAPI: taskAPI, uploadDir: uploadDir, locker: locker}
}

func excelizeOpenFile(path string) (*excelize.File, error) {
	return excelize.OpenFile(path)
}

// ---------- 单据生命周期 ----------

// Create 创建入库单（RK 单号；唯一索引兜底 + 重新生成重试 3 次）。
func (s *Service) Create(ctx context.Context, req *dto.CreateOrderReq, operator string) (*model.ReceiptOrder, error) {
	if err := s.basic.ValidateWarehouse(ctx, req.WarehouseID); err != nil {
		return nil, err
	}
	details, expected, err := s.buildDetails(ctx, req.Details)
	if err != nil {
		return nil, err
	}
	var order *model.ReceiptOrder
	for i := 0; i < 3; i++ { // 单号冲突重试
		order = &model.ReceiptOrder{
			Base:        sysmodel.Base{ID: snowflake.Next()},
			OrderNo:     s.no.Next(ctx, "RK"),
			WarehouseID: req.WarehouseID, Status: model.OrderDraft,
			Source: "MANUAL", Remark: req.Remark, ExpectedQty: expected, CreatedBy: operator,
		}
		err = s.tm.Tx(ctx, func(tx *gorm.DB) error {
			return s.repo.CreateOrder(tx, order, details)
		})
		if err == nil {
			return order, nil
		}
		if !isDuplicateErr(err) {
			return nil, err
		}
		log.WithContext(ctx).Warn("order_no duplicated, retry", "order_no", order.OrderNo)
	}
	return nil, errcode.OrderNoDuplicate
}

// Update 仅 DRAFT 可编辑。
func (s *Service) Update(ctx context.Context, id int64, req *dto.CreateOrderReq) error {
	return s.tm.Tx(ctx, func(tx *gorm.DB) error {
		o, err := s.repo.GetOrderForUpdate(tx, id)
		if err != nil {
			return errcode.OrderNotFound
		}
		if o.Status != model.OrderDraft {
			return errcode.OrderStatusWrong
		}
		if err := s.basic.ValidateWarehouse(ctx, req.WarehouseID); err != nil {
			return err
		}
		details, expected, err := s.buildDetails(ctx, req.Details)
		if err != nil {
			return err
		}
		o.WarehouseID = req.WarehouseID
		o.Remark = req.Remark
		o.ExpectedQty = expected
		if err := tx.Model(o).Updates(map[string]any{
			"warehouse_id": o.WarehouseID, "remark": o.Remark, "expected_qty": o.ExpectedQty,
			"version": o.Version + 1,
		}).Error; err != nil {
			return err
		}
		return s.repo.ReplaceDetails(tx, id, details)
	})
}

func (s *Service) Delete(ctx context.Context, id int64) error {
	return s.tm.Tx(ctx, func(tx *gorm.DB) error {
		o, err := s.repo.GetOrderForUpdate(tx, id)
		if err != nil {
			return errcode.OrderNotFound
		}
		if o.Status != model.OrderDraft {
			return errcode.OrderStatusWrong
		}
		return s.repo.DeleteOrder(tx, id)
	})
}

// Submit 提交：DRAFT → SUBMITTED。
func (s *Service) Submit(ctx context.Context, id int64) error {
	return s.transit(ctx, id, model.OrderDraft, model.OrderSubmitted)
}

// Approve 审核：SUBMITTED → APPROVED，并生成收货任务。
func (s *Service) Approve(ctx context.Context, id int64, operator string) error {
	return s.tm.Tx(ctx, func(tx *gorm.DB) error {
		o, err := s.repo.GetOrderForUpdate(tx, id)
		if err != nil {
			return errcode.OrderNotFound
		}
		if o.Status != model.OrderSubmitted {
			return errcode.OrderStatusWrong
		}
		details, err := s.repo.ListDetails(tx, id)
		if err != nil {
			return err
		}
		if n, err := s.repo.UpdateStatus(tx, id, model.OrderSubmitted, model.OrderApproved); err != nil || n == 0 {
			if err != nil {
				return err
			}
			return errcode.OrderVersionBad
		}
		tasks := make([]*taskapi.CreateTask, 0, len(details))
		for _, d := range details {
			tasks = append(tasks, &taskapi.CreateTask{
				TaskType: taskmodel.TaskReceive, OrderID: o.ID, OrderNo: o.OrderNo,
				DetailID: d.ID, SKUID: d.SKUID, WarehouseID: o.WarehouseID, TargetQty: d.ExpectedQty,
			})
		}
		_ = operator
		return s.taskAPI.Create(ctx, tx, tasks)
	})
}

// Cancel 取消：DRAFT/SUBMITTED/APPROVED 可取消，同时取消任务。
func (s *Service) Cancel(ctx context.Context, id int64) error {
	return s.tm.Tx(ctx, func(tx *gorm.DB) error {
		o, err := s.repo.GetOrderForUpdate(tx, id)
		if err != nil {
			return errcode.OrderNotFound
		}
		switch o.Status {
		case model.OrderDraft, model.OrderSubmitted, model.OrderApproved:
		default:
			return errcode.OrderStatusWrong
		}
		if n, err := s.repo.UpdateStatus(tx, id, o.Status, model.OrderCancelled); err != nil || n == 0 {
			if err != nil {
				return err
			}
			return errcode.OrderVersionBad
		}
		return s.taskAPI.CancelByOrder(ctx, tx, id)
	})
}

func (s *Service) transit(ctx context.Context, id int64, from, to model.OrderStatus) error {
	return s.tm.Tx(ctx, func(tx *gorm.DB) error {
		if _, err := s.repo.GetOrderForUpdate(tx, id); err != nil {
			return errcode.OrderNotFound
		}
		if n, err := s.repo.UpdateStatus(tx, id, from, to); err != nil || n == 0 {
			if err != nil {
				return err
			}
			return errcode.OrderStatusWrong
		}
		return nil
	})
}

// ---------- 收货 ----------

// Receive 分次收货：累计不超过预期数量，记录残品；首次收货录入批次号。
// 全部收完后同事务生成上架任务，单据流转 RECEIVING → PUTAWAY。
func (s *Service) Receive(ctx context.Context, orderID, detailID int64, req *dto.ReceiveReq, operator string) error {
	return s.tm.Tx(ctx, func(tx *gorm.DB) error {
		o, err := s.repo.GetOrderForUpdate(tx, orderID)
		if err != nil {
			return errcode.OrderNotFound
		}
		if o.Status != model.OrderApproved && o.Status != model.OrderReceiving {
			return errcode.OrderStatusWrong
		}
		d, err := s.repo.GetDetailForUpdate(tx, detailID)
		if err != nil {
			return errcode.OrderNotFound
		}
		if d.OrderID != orderID {
			return errcode.ParamError
		}
		remaining := d.ExpectedQty - d.ReceivedQty - d.DefectiveQty
		if req.Qty+req.DefectiveQty > remaining {
			return errcode.ReceiveQtyOver
		}
		// 批次号：首次收货必填并落库；后续保持一致
		if d.BatchNo == "" {
			if req.BatchNo == "" {
				return errcode.BatchNoRequired
			}
			d.BatchNo = req.BatchNo
		} else if req.BatchNo != "" && req.BatchNo != d.BatchNo {
			return errcode.New(40014, "同一明细的批次号必须与首次收货一致")
		}
		d.ReceivedQty += req.Qty
		d.DefectiveQty += req.DefectiveQty
		if err := s.repo.UpdateDetailReceive(tx, d); err != nil {
			return err
		}

		// 主单累计
		o.ReceivedQty += req.Qty
		o.DefectiveQty += req.DefectiveQty
		fullyReceived := true
		all, err := s.repo.ListDetails(tx, orderID)
		if err != nil {
			return err
		}
		for _, item := range all {
			if item.ReceivedQty+item.DefectiveQty < item.ExpectedQty {
				fullyReceived = false
				break
			}
		}
		if o.Status == model.OrderApproved {
			o.Status = model.OrderReceiving
		}
		if fullyReceived {
			o.Status = model.OrderPutaway
			// 收货完成 → 生成上架任务（残品不入库，上架量 = 已收 - 残品）
			tasks := make([]*taskapi.CreateTask, 0, len(all))
			for _, item := range all {
				putawayQty := item.ReceivedQty - item.DefectiveQty
				if putawayQty <= 0 {
					continue
				}
				tasks = append(tasks, &taskapi.CreateTask{
					TaskType: taskmodel.TaskPutaway, OrderID: o.ID, OrderNo: o.OrderNo,
					DetailID: item.ID, SKUID: item.SKUID, WarehouseID: o.WarehouseID, TargetQty: putawayQty,
				})
			}
			if len(tasks) > 0 {
				if err := s.taskAPI.Create(ctx, tx, tasks); err != nil {
					return err
				}
			}
		}
		return s.repo.UpdateOrderReceive(tx, o)
	})
}

// ---------- 上架 ----------

// Putaway 上架执行：指定库位 + 数量，调用 inventory.Increase 增加库存，库位标记占用。
// 全部上架任务完成后单据流转 COMPLETED。支持分多次上架。
func (s *Service) Putaway(ctx context.Context, taskID, locationID int64, qty int, operator string) error {
	if err := s.basic.ValidateLocation(ctx, locationID); err != nil {
		return err
	}
	return s.tm.Tx(ctx, func(tx *gorm.DB) error {
		t, err := s.taskAPI.Get(ctx, taskID)
		if err != nil {
			return errcode.TaskNotFound
		}
		if t.TaskType != taskmodel.TaskPutaway {
			return errcode.TaskStatusWrong
		}
		o, err := s.repo.GetOrderForUpdate(tx, t.OrderID)
		if err != nil {
			return errcode.OrderNotFound
		}
		if o.Status != model.OrderPutaway {
			return errcode.OrderStatusWrong
		}
		var detail *model.ReceiptOrderDetail
		details, err := s.repo.ListDetails(tx, o.ID)
		if err != nil {
			return err
		}
		for _, d := range details {
			if d.ID == t.DetailID {
				detail = d
				break
			}
		}
		if detail == nil || detail.BatchNo == "" {
			return errcode.BatchNoRequired
		}
		// 库存生效：上架时才增加库存
		if err := s.inv.Increase(ctx, tx, &invapi.IncreaseReq{
			WarehouseID: o.WarehouseID, LocationID: locationID, SKUID: t.SKUID,
			BatchNo: detail.BatchNo, Quantity: qty,
			OrderNo: o.OrderNo, TaskNo: t.TaskNo, Operator: operator,
		}); err != nil {
			return err
		}
		// 库位标记占用
		if err := s.basic.UpdateLocationStatusInTx(ctx, tx, locationID, basicmodel.LocationStatusOccupied); err != nil {
			return err
		}
		// 推进任务（含状态机与数量校验）
		if err := s.taskAPI.AddProgress(ctx, tx, taskID, qty, operator); err != nil {
			return err
		}
		// 全部上架任务完成 → 单据 COMPLETED
		unfinished, err := s.taskAPI.CountUnfinished(ctx, tx, o.ID, taskmodel.TaskPutaway)
		if err != nil {
			return err
		}
		if unfinished == 0 {
			if n, err := s.repo.UpdateStatus(tx, o.ID, model.OrderPutaway, model.OrderCompleted); err != nil {
				return err
			} else if n == 0 {
				return errcode.OrderVersionBad
			}
		}
		return nil
	})
}

// ---------- 查询 ----------

type OrderDetail struct {
	Order   *model.ReceiptOrder         `json:"order"`
	Details []*model.ReceiptOrderDetail `json:"details"`
	Tasks   []*taskmodel.Task           `json:"tasks"`
}

func (s *Service) Get(ctx context.Context, id int64) (*OrderDetail, error) {
	o, err := s.repo.GetOrder(ctx, s.tm.DB(), id)
	if err != nil {
		return nil, errcode.OrderNotFound
	}
	details, err := s.repo.ListDetails(s.tm.DB(), id)
	if err != nil {
		return nil, err
	}
	tasks, _, err := s.taskAPI.List(ctx, id, "", 1, 100)
	if err != nil {
		return nil, err
	}
	return &OrderDetail{Order: o, Details: details, Tasks: tasks}, nil
}

func (s *Service) List(ctx context.Context, q *dto.OrderQuery) ([]*model.ReceiptOrder, int64, error) {
	return s.repo.ListOrders(ctx, s.tm.DB(), q.WarehouseID, q.Status, q.Keyword, q.Page, q.PageSize)
}

// ---------- 内部 ----------

// buildDetails 校验 SKU 并填充编码名称快照。
func (s *Service) buildDetails(ctx context.Context, items []dto.OrderDetailItem) ([]*model.ReceiptOrderDetail, int, error) {
	details := make([]*model.ReceiptOrderDetail, 0, len(items))
	expected := 0
	seen := map[int64]struct{}{}
	for _, it := range items {
		if _, dup := seen[it.SKUID]; dup {
			return nil, 0, errcode.New(40015, "同一货品请合并为一行明细")
		}
		seen[it.SKUID] = struct{}{}
		sku, err := s.basic.GetSKU(ctx, it.SKUID)
		if err != nil {
			return nil, 0, err
		}
		details = append(details, &model.ReceiptOrderDetail{
			Base:  sysmodel.Base{ID: snowflake.Next()},
			SKUID: sku.ID, SKUCode: sku.Code, SKUName: sku.Name,
			ExpectedQty: it.ExpectedQty,
		})
		expected += it.ExpectedQty
	}
	return details, expected, nil
}

func isDuplicateErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate")
}

// ---------- Excel 异步导入 ----------

// Import 上传 Excel：秒返回 taskId，后台异步解析。
// 任务状态机 PENDING → PROCESSING → COMPLETED/FAILED，CAS 更新防重复执行。
func (s *Service) Import(ctx context.Context, fileName string, data []byte) (*dto.ImportResp, error) {
	if err := os.MkdirAll(s.uploadDir, 0o755); err != nil {
		return nil, err
	}
	taskID := fmt.Sprintf("IMP%d", snowflake.Next())
	path := filepath.Join(s.uploadDir, taskID+filepath.Ext(fileName))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return nil, err
	}
	t := &model.ImportTask{
		Base:   sysmodel.Base{ID: snowflake.Next()},
		TaskID: taskID, Status: model.ImportPending,
		FileName: fileName, FilePath: path,
	}
	if err := s.repo.CreateImportTask(ctx, s.tm.DB(), t); err != nil {
		return nil, err
	}
	go s.processImport(taskID) // 异步执行
	return &dto.ImportResp{TaskID: taskID}, nil
}

// GetImport 查询导入进度（前端轮询）。
func (s *Service) GetImport(ctx context.Context, taskID string) (*model.ImportTask, error) {
	t, err := s.repo.GetImportTask(ctx, s.tm.DB(), taskID)
	if err != nil {
		return nil, errcode.ImportTaskNotFound
	}
	return t, nil
}

// processImport 执行导入：CAS 抢占 → 解析 Excel 逐行建单 → 写结果。
func (s *Service) processImport(taskID string) {
	ctx := context.Background()
	n, err := s.repo.CASImportStatus(s.tm.DB(), taskID, model.ImportPending, model.ImportProcessing)
	if err != nil || n == 0 { // 已被其他 goroutine/节点抢占
		return
	}
	// 心跳：长任务定期刷新 updated_at，防止被悬挂补偿误判
	stopHeartbeat := make(chan struct{})
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				_ = s.repo.TouchImport(s.tm.DB(), taskID)
			case <-stopHeartbeat:
				return
			}
		}
	}()
	defer close(stopHeartbeat)

	t, err := s.repo.GetImportTask(ctx, s.tm.DB(), taskID)
	if err != nil {
		return
	}
	total, success, fail, errMsg := s.doImport(ctx, t)
	status := model.ImportCompleted
	if success == 0 && total > 0 {
		status = model.ImportFailed
	}
	if err := s.repo.FinishImport(s.tm.DB(), taskID, status, total, success, fail, errMsg); err != nil {
		log.L().Error("finish import failed", "task_id", taskID, "err", err)
	}
}

// doImport 逐行解析：列 = 仓库编码 | 货品编码 | 预期数量 | 备注，每行一张入库单。
func (s *Service) doImport(ctx context.Context, t *model.ImportTask) (total, success, fail int, errMsg string) {
	f, err := excelizeOpenFile(t.FilePath)
	if err != nil {
		return 0, 0, 1, "打开文件失败: " + err.Error()
	}
	defer f.Close()
	rows, err := f.GetRows(f.GetSheetName(0))
	if err != nil || len(rows) < 2 {
		return 0, 0, 1, errcode.ImportTemplateHeader.Msg
	}
	header := rows[0]
	if len(header) < 3 || header[0] != "仓库编码" || header[1] != "货品编码" || header[2] != "预期数量" {
		return 0, 0, 1, errcode.ImportTemplateHeader.Msg
	}
	var failMsgs []string
	for i, row := range rows[1:] {
		total++
		if len(row) < 3 {
			fail++
			failMsgs = append(failMsgs, fmt.Sprintf("第%d行: 列数不足", i+2))
			continue
		}
		var expectedQty int
		if _, err := fmt.Sscanf(strings.TrimSpace(row[2]), "%d", &expectedQty); err != nil || expectedQty <= 0 {
			fail++
			failMsgs = append(failMsgs, fmt.Sprintf("第%d行: 预期数量非法", i+2))
			continue
		}
		wh, err := s.basic.GetWarehouseByCode(ctx, strings.TrimSpace(row[0]))
		if err != nil {
			fail++
			failMsgs = append(failMsgs, fmt.Sprintf("第%d行: %v", i+2, err))
			continue
		}
		sku, err := s.basic.GetSKUByCode(ctx, strings.TrimSpace(row[1]))
		if err != nil {
			fail++
			failMsgs = append(failMsgs, fmt.Sprintf("第%d行: %v", i+2, err))
			continue
		}
		remark := ""
		if len(row) > 3 {
			remark = row[3]
		}
		_, err = s.Create(ctx, &dto.CreateOrderReq{
			WarehouseID: wh.ID, Remark: remark,
			Details: []dto.OrderDetailItem{{SKUID: sku.ID, ExpectedQty: expectedQty}},
		}, "import")
		if err != nil {
			fail++
			failMsgs = append(failMsgs, fmt.Sprintf("第%d行: %v", i+2, err))
			continue
		}
		success++
	}
	if len(failMsgs) > 0 {
		errMsg = strings.Join(failMsgs, "; ")
		if len(errMsg) > 1000 {
			errMsg = errMsg[:1000]
		}
	}
	return total, success, fail, errMsg
}

// StartCompensator 悬挂任务补偿：每 2 分钟扫描超时任务，CAS 抢占后重新执行。
func (s *Service) StartCompensator(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.compensateOnce()
			}
		}
	}()
}

func (s *Service) compensateOnce() {
	ctx := context.Background()
	// 多实例部署时用分布式锁防止重复补偿；Redis 故障降级为直接执行（单实例语义）。
	if s.locker != nil {
		release, ok, err := s.locker.Lock(ctx, "wms:import:compensate", 5*time.Minute)
		if err != nil {
			log.L().Warn("compensate lock unavailable, run in standalone mode", "err", err)
		} else if !ok {
			return // 其他实例正在补偿
		} else {
			defer release()
		}
	}
	now := time.Now()
	stale, err := s.repo.ListStaleImports(ctx, s.tm.DB(), now.Add(-2*time.Minute), now.Add(-5*time.Minute))
	if err != nil {
		log.L().Error("scan stale imports failed", "err", err)
		return
	}
	for _, t := range stale {
		if t.Status == model.ImportProcessing { // 心跳超时的 PROCESSING：复位后重跑
			n, err := s.repo.ResetProcessingToPending(s.tm.DB(), t.TaskID)
			if err != nil || n == 0 {
				continue
			}
			log.L().Warn("stale processing import reset", "task_id", t.TaskID)
		}
		// PENDING：CAS 抢占后重跑
		go s.processImport(t.TaskID)
	}
}
