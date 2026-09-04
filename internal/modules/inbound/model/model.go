package model

import (
	"gowms/internal/modules/system/model"
)

// OrderStatus 入库单状态机：DRAFT → SUBMITTED → APPROVED → RECEIVING → PUTAWAY → COMPLETED（可 CANCELLED）。
type OrderStatus string

const (
	OrderDraft     OrderStatus = "DRAFT"
	OrderSubmitted OrderStatus = "SUBMITTED"
	OrderApproved  OrderStatus = "APPROVED"
	OrderReceiving OrderStatus = "RECEIVING"
	OrderPutaway   OrderStatus = "PUTAWAY"
	OrderCompleted OrderStatus = "COMPLETED"
	OrderCancelled OrderStatus = "CANCELLED"
)

// StatusTransitions 状态转换表：只允许单向流转，非法流转返回错误。
var StatusTransitions = map[OrderStatus][]OrderStatus{
	OrderDraft:     {OrderSubmitted, OrderCancelled},
	OrderSubmitted: {OrderApproved, OrderCancelled},
	OrderApproved:  {OrderReceiving, OrderCancelled},
	OrderReceiving: {OrderPutaway},
	OrderPutaway:   {OrderCompleted},
}

type ReceiptOrder struct {
	model.Base
	model.Versioned
	OrderNo      string      `json:"order_no" gorm:"size:64;uniqueIndex;not null"`
	WarehouseID  int64       `json:"warehouse_id" gorm:"not null"`
	Status       OrderStatus `json:"status" gorm:"size:16;index;not null;default:'DRAFT'"`
	Source       string      `json:"source" gorm:"size:16;default:'MANUAL'"` // MANUAL / IMPORT
	Remark       string      `json:"remark" gorm:"size:255"`
	ExpectedQty  int         `json:"expected_qty" gorm:"not null;default:0"`
	ReceivedQty  int         `json:"received_qty" gorm:"not null;default:0"`
	DefectiveQty int         `json:"defective_qty" gorm:"not null;default:0"`
	CreatedBy    string      `json:"created_by" gorm:"size:64"`
}

func (ReceiptOrder) TableName() string { return "wms_receipt_order" }

type ReceiptOrderDetail struct {
	model.Base
	OrderID      int64  `json:"order_id" gorm:"index;not null"`
	SKUID        int64  `json:"sku_id" gorm:"column:sku_id;not null"`
	SKUCode      string `json:"sku_code" gorm:"size:64"`
	SKUName      string `json:"sku_name" gorm:"size:128"`
	ExpectedQty  int    `json:"expected_qty" gorm:"not null"`
	ReceivedQty  int    `json:"received_qty" gorm:"not null;default:0"`
	DefectiveQty int    `json:"defective_qty" gorm:"not null;default:0"`
	BatchNo      string `json:"batch_no" gorm:"size:64"` // 收货时录入
}

func (ReceiptOrderDetail) TableName() string { return "wms_receipt_order_detail" }

// ImportTaskStatus Excel 异步导入任务状态机。
type ImportTaskStatus string

const (
	ImportPending    ImportTaskStatus = "PENDING"
	ImportProcessing ImportTaskStatus = "PROCESSING"
	ImportCompleted  ImportTaskStatus = "COMPLETED"
	ImportFailed     ImportTaskStatus = "FAILED"
)

// ImportTask 异步导入任务：CAS 更新防重复执行，悬挂任务由定时补偿扫描重跑。
type ImportTask struct {
	model.Base
	TaskID      string           `json:"task_id" gorm:"size:64;uniqueIndex;not null"`
	Status      ImportTaskStatus `json:"status" gorm:"size:16;index;not null;default:'PENDING'"`
	FileName    string           `json:"file_name" gorm:"size:255"`
	FilePath    string           `json:"file_path" gorm:"size:255"`
	TotalRows   int              `json:"total_rows"`
	SuccessRows int              `json:"success_rows"`
	FailRows    int              `json:"fail_rows"`
	ErrorMsg    string           `json:"error_msg" gorm:"size:1024"`
}

func (ImportTask) TableName() string { return "wms_import_task" }
