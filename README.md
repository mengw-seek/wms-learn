# GoWMS — 轻量级仓储管理系统

![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)
![Vue](https://img.shields.io/badge/Vue-3.x-4FC08D?logo=vuedotjs&logoColor=white)
![TypeScript](https://img.shields.io/badge/TypeScript-5.x-3178C6?logo=typescript&logoColor=white)
![MySQL](https://img.shields.io/badge/MySQL-8.0.16+-4479A1?logo=mysql&logoColor=white)
![Redis](https://img.shields.io/badge/Redis-可选·自动降级-DC382D?logo=redis&logoColor=white)
![License](https://img.shields.io/badge/License-MIT-green)

个人了解了部分业务之后，用ai生成的代码，将部分代码逻辑看完了，但一些其他的，比如配置环境等还没有看


单体后端（Go / Gin / GORM / MySQL / Redis）+ 前端（Vue3 / TypeScript / Element Plus / Pinia）的**开箱即用 WMS**。
覆盖 **入库 → 库存 → 出库 → 盘点** 完整业务闭环，以"功能简洁、工程亮点完整"为设计目标——可直接二开为小型仓库的生产系统，也适合逐模块学习仓储领域建模与 Go 工程实践。

**了解它解决了什么**：库存并发不超卖（行锁 + 条件更新 + CHECK 三重防护，有测试证明）、每一件货的每一次变动都有流水可对账、单据状态机严格流转、审核即 FIFO 锁库、Excel 导入带断线补偿。

## 核心亮点

| 亮点 | 代码落点 |
| --- | --- |
| 三数量库存模型 `stock = available + allocated`，全量流水可对账 | `internal/modules/inventory` |
| 双重防超卖：`FOR UPDATE` 行锁 + `WHERE available >= N` 条件更新 + CHECK 约束兜底 | `inventory/repository` `AllocateQty` |
| FIFO 分配：按入库时间锁库，跨批次生成 `wms_allocation` 分配明细 | `inventory/service.Allocate` |
| 审核即分配、拣完即扣减；取消单据自动释放锁库 | `outbound/service` |
| 单据/任务状态机单向流转 + 乐观锁 version 防并发跳变 | 各模块 `model.StatusTransitions` |
| 事务边界收敛 Service 层，模块间仅经 api 接口通信（预留微服务拆分点） | `internal/pkg/tx`、`internal/app` |
| Redis Lua 单号生成器：降级本地模式 + 唯一索引重试兜底 | `internal/pkg/orderno` |
| Excel 异步导入：状态机 + CAS 抢占 + 悬挂任务 2 分钟扫描补偿 | `inbound/service` |
| 统一任务表 `wms_task`（收货/上架/拣货共用） | `internal/modules/task` |
| JWT 认证 + 路由级权限串 + 异步操作日志审计 | `internal/modules/system`、`pkg/middleware` |


## 快速开始

### 1. 依赖

- Go 1.22+、Node 20+、MySQL 8.0.16+、Redis（可选，不可用自动降级）
- Docker 用户直接：`make compose-up`（或 `docker compose -f deploy/docker-compose.yaml up -d`）；后端本体也可容器化：`docker build -t gowms .`
- Windows 本机服务：以管理员身份 `net start MySQL80 && net start Redis`

### 2. 启动后端

```bash
# 建库（如用本机 MySQL；Docker 方式已自动建库）
mysql -uroot -p < migrations/001_init.sql   # 或手动 CREATE DATABASE gowms
# 首次运行自动建表 + 写入种子管理员 admin/admin123
go run ./cmd/wms
```

### 3. 启动前端

```bash
cd web
npm install
npm run dev    # http://localhost:5173，已代理 /api → 127.0.0.1:8080
```

登录：`admin / admin123`，建议按 **仓库 → 批量生成库位 → 货品 → 入库单 → 出库单** 的顺序体验完整闭环。

## 目录结构

```
wms/
├── cmd/wms/            # 入口：配置加载 → DB/Redis 初始化 → 迁移 → HTTP 服务
├── configs/            # config.yaml
├── deploy/             # docker-compose (MySQL 8 + Redis 7)
├── migrations/         # 001_init.sql（人工审阅用，实际以 AutoMigrate 为准）
├── internal/
│   ├── app/            # 依赖组装（手动构造注入）+ 路由
│   ├── bootstrap/      # InitDB / InitRedis / Migrate / seed(admin)
│   ├── modules/
│   │   ├── system/     # 登录 JWT / 用户 / 角色 / 权限 / 操作日志
│   │   ├── basic/      # 仓库 / 库位(批量生成) / 货品(Redis 条码缓存)
│   │   ├── inventory/  # 三数量模型 + FIFO 分配 + 防超卖 + 流水（含 api/）
│   │   ├── task/       # 统一任务表 wms_task（含 api/）
│   │   ├── inbound/    # 入库单全流程 + Excel 异步导入
│   │   ├── outbound/   # 出库单：审核即分配 → 拣货 → 发货扣减
│   │   └── stocktake/  # 盘点：快照 → 实盘 → 审核调整
│   └── pkg/            # config/errcode/response/jwt/tx/snowflake/orderno/lock/middleware
├── web/                # Vue3 + TS + Vite + Element Plus + Pinia 前端
└── docs/               # 需求 / 架构 / 数据库 / API 文档
```

## 业务流程

**入库**：创建入库单(DRAFT) → 提交(SUBMITTED) → 审核(APPROVED，生成收货任务) → 收货(RECEIVING，可多次) → 上架(库存 Increase，任务完成) → 单据 COMPLETED

**出库**：创建(DRAFT，biz_order_no 幂等) → 提交(SUBMITTED) → **审核即分配**(PICKING，FIFO 锁库 available→allocated，生成拣货任务) → 拣货(可分次，分配行拣完自动发货扣减) → SHIPPED；未拣货可取消(释放锁库)

**盘点**：创建即快照(book_qty) → 录入实盘数 → 审核(行锁内重算差异，库存 Adjust，同事务写 ADJUST 流水)

> 完整状态机图与角色权限矩阵见 [需求规格说明书](docs/requirements.md)。


## 测试

```bash
# 并发防超卖 / FIFO / 发货释放不变量（需 MySQL 可用，连不上自动 SKIP）
go test ./internal/modules/inventory/service/ -v
# 可用 WMS_TEST_DSN 覆盖默认 DSN
```

关键用例：`TestConcurrentAllocateAntiOversell`（100 库存 vs 200 并发分配，要求恰好 100 成功 + 恒等式不破）、`TestAllocateFIFO`、`TestShipReleaseInvariant`。

## 常用 Make 命令

`make run` 启动后端 / `make build` 编译 / `make test` 全量测试 / `make compose-up|down`

## 工程化

- **CI**：推送到 GitHub 后自动运行 [.github/workflows/ci.yml](.github/workflows/ci.yml)——golangci-lint 检查、带 MySQL service 的集成测试（并发防超卖/FIFO/恒等式）、前端类型检查与构建；
- **Lint**：规则见 [.golangci.yml](.golangci.yml)（errcheck/govet/staticcheck/unused/ineffassign/misspell/gofmt）；
- **容器化**：根目录 [Dockerfile](Dockerfile) 多阶段构建后端镜像，时区默认 Asia/Shanghai。

## 配置

`configs/config.yaml`：MySQL DSN、Redis、JWT secret、上传目录、日志级别。

