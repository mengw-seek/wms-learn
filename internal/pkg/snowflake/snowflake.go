package snowflake

import (
	"sync"
	"time"
)

// 简化雪花算法：41 位毫秒时间戳 + 10 位节点 + 12 位序列。
const (
	epoch     int64 = 1704038400000 // 2024-01-01 00:00:00 UTC
	nodeBits  uint  = 10
	seqBits   uint  = 12
	maxNode   int64 = -1 ^ (-1 << nodeBits)
	maxSeq    int64 = -1 ^ (-1 << seqBits)
	nodeShift       = seqBits
	tsShift         = nodeBits + seqBits
)

var (
	mu     sync.Mutex
	nodeID int64 = 1
	lastTS int64 = -1
	seq    int64 = 0
)

func Init(node int64) {
	if node > 0 && node <= maxNode {
		nodeID = node
	}
}

// Next 生成全局唯一递增 ID。
func Next() int64 {
	mu.Lock()
	defer mu.Unlock()
	now := time.Now().UnixMilli()
	if now == lastTS {
		seq = (seq + 1) & maxSeq
		if seq == 0 { // 当前毫秒序列耗尽，等待下一毫秒
			for now <= lastTS {
				now = time.Now().UnixMilli()
			}
		}
	} else {
		seq = 0
	}
	lastTS = now
	return ((now - epoch) << tsShift) | (nodeID << nodeShift) | seq
}
