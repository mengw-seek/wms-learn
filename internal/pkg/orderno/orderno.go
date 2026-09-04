package orderno

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Generator 单号生成器。
//
// 主方案：Redis Lua 原子执行 INCR + TTL 兜底（按天重置，TTL 48h）。
// 降级方案：Redis 不可用时切换本地 atomic 计数 + 时间戳。
// 兜底：调用方依赖数据库唯一索引，插入冲突时重新生成（重试逻辑在各业务 Service）。
type Generator struct {
	rdb      redis.UniversalClient
	fallback sync.Map // prefix -> *atomic.Int64
	lastDay  sync.Map // prefix -> string，用于降级时按天重置
}

// New 允许 rdb 为 nil（完全降级模式）。
func New(rdb redis.UniversalClient) *Generator {
	return &Generator{rdb: rdb}
}

// ttlSeconds 48 小时，覆盖跨天后仍能命中昨日 key 的场景。
const ttlSeconds = 48 * 3600

// luaScript 必须原子：INCR 后若进程崩溃、EXPIRE 未执行，key 永不过期会导致次日序号错乱。
// 因此每次 INCR 后检查 TTL，无 TTL 则补设。
var luaScript = redis.NewScript(`
local v = redis.call('INCR', KEYS[1])
if redis.call('TTL', KEYS[1]) < 0 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return v
`)

// Next 生成单号：{prefix}{yyyyMMdd}{6位序号}。
func (g *Generator) Next(ctx context.Context, prefix string) string {
	day := time.Now().Format("20060102")
	if g.rdb != nil {
		key := fmt.Sprintf("gowms:orderno:%s:%s", prefix, day)
		seq, err := luaScript.Run(ctx, g.rdb, []string{key}, ttlSeconds).Int64()
		if err == nil {
			return fmt.Sprintf("%s%s%06d", prefix, day, seq)
		}
		// Redis 故障 → 降级
	}
	return g.fallbackNext(prefix, day)
}

// fallbackNext 本地降级：时间戳 + 原子计数，跨天自动重置。
func (g *Generator) fallbackNext(prefix, day string) string {
	v, _ := g.fallback.LoadOrStore(prefix, &atomic.Int64{})
	counter := v.(*atomic.Int64)
	last, _ := g.lastDay.LoadOrStore(prefix, day)
	if last != day { // 跨天重置
		if g.lastDay.CompareAndSwap(prefix, last, day) {
			counter.Store(0)
		}
	}
	n := counter.Add(1)
	return fmt.Sprintf("%s%s%s%06d", prefix, day, time.Now().Format("150405"), n%1000000)
}
