package buffer

import "hash/fnv"

// Hash 将整型标识混合成 uint32，用作分片池（ShardedPool / ShardedFixedRingPool /
// ShardedClassPool）的 key。
//
// 分片路由取 key 的低 log2(shardCount) 位（key & mask），因此只需保证低位分布均匀即可，
// 无需抗碰撞。这里混合高低位是为了应对「低位变化慢、高位变化快」的 ID（如纳秒时间戳），
// 避免 key 集中在少数分片。
//
// 典型用法：
//
//	shardKey := buffer.Hash(uint64(connID))
//	buf := pool.Get(shardKey)
func Hash(v uint64) uint32 {
	return uint32(v ^ v>>32)
}

// HashString 对字符串做 FNV-1a 哈希，用作分片池的 key。
// 适用于非整型标识（如会话 token、UUID 等）。
//
// 典型用法：
//
//	shardKey := buffer.HashString(sessionToken)
//	buf := pool.Get(shardKey)
func HashString(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}
