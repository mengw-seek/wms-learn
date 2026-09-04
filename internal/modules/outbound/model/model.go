package model

import (
	"gowms/internal/modules/system/model"
)

// OrderStatus 出库单状态机：DRAFT → SUBMITTED → APPROVED(即分配) → PICKING → SHIPPED（可 CANCELLED）。
type OrderStatus string

const (
	OrderDraft     OrderStatus = "DRAFT"
	OrderSubmitted OrderStatus = "SUBMITTED"
	OrderApproved  OrderStatus = "APPROVED"
	OrderPicking   OrderStatus = "PICKING"
	OrderShipped   OrderStatus = "SHIPPED"
	OrderCancelled OrderStatus = "CANCELLED"
)

// StatusTransitions 状态转换表。
var StatusTransitions = map[OrderStatus][]OrderStatus{
	OrderDraft:     {OrderSubmitted, OrderCancelled},
	OrderSubmitted: {OrderApproved, OrderCancelled},
	OrderApproved:  {OrderPicking, OrderCancelled},
	OrderPicking:   {OrderShipped, OrderCancelled}, // PICKING 且未拣货可取消（释放库存）
}

type ShipmentOrder struct {
	model.Base
	model.Versioned
	OrderNo      string      `json:"order_no" gorm:"size:64;uniqueIndex;not null"`
	BizOrderNo   string      `json:"biz_order_no" gorm:"size:64;uniqueIndex;not null"` // 幂等键：业务订单号
	WarehouseID  int64       `json:"warehouse_id" gorm:"not null"`
	Status       OrderStatus `json:"status" gorm:"size:16;index;not null;default:'DRAFT'"`
	Remark       string      `json:"remark" gorm:"size:255"`
	ExpectedQty  int         `json:"expected_qty" gorm:"not null;default:0"`
	AllocatedQty int         `json:"allocated_qty" gorm:"not null;default:0"`
	PickedQty    int         `json:"picked_qty" gorm:"not null;default:0"`
	CreatedBy    string      `json:"created_by" gorm:"size:64"`
}

func (ShipmentOrder) TableName() string { return "wms_shipment_order" }

type ShipmentOrderDetail struct {
	model.Base
	OrderID      int64  `json:"order_id" gorm:"index;not null"`
	SKUID        int64  `json:"sku_id" gorm:"column:sku_id;not null"`
	SKUCode      string `json:"sku_code" gorm:"size:64"`
	SKUName      string `json:"sku_name" gorm:"size:128"`
	ExpectedQty  int    `json:"expected_qty" gorm:"not null"`
	AllocatedQty int    `json:"allocated_qty" gorm:"not null;default:0"`
	PickedQty    int    `json:"picked_qty" gorm:"not null;default:0"`
}

func (ShipmentOrderDetail) TableName() string { return "wms_shipment_order_detail" }

// AllocationStatus 分配行状态。
type AllocationStatus string

const (
	AllocAllocated AllocationStatus = "ALLOCATED"
	AllocPicked    AllocationStatus = "PICKED"
	AllocCancelled AllocationStatus = "CANCELLED"
)

// Allocation 分配明细：FIFO 分配会跨批次/库位，一行出库明细对应 N 个分配行。
// 拣货任务、发货扣减、取消释放均以本表为准。
type Allocation struct {
	model.Base
	model.Versioned
	OrderID      int64            `json:"order_id" gorm:"index;not null"`
	DetailID     int64            `json:"detail_id" gorm:"index;not null"`
	InventoryID  int64            `json:"inventory_id" gorm:"index;not null"`
	SKUID        int64            `json:"sku_id" gorm:"column:sku_id;not null"`
	LocationID   int64            `json:"location_id" gorm:"not null"`
	LocationCode string           `json:"location_code" gorm:"size:64"`
	BatchNo      string           `json:"batch_no" gorm:"size:64"`
	AllocatedQty int              `json:"allocated_qty" gorm:"not null"`
	PickedQty    int              `json:"picked_qty" gorm:"not null;default:0"`
	Status       AllocationStatus `json:"status" gorm:"size:16;index;not null;default:'ALLOCATED'"`
}

func (Allocation) TableName() string { return "wms_allocation" }
