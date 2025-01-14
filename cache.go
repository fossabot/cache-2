package cache

import (
	"hash/crc32"
	"sync"
	"time"
)

// node to store cache item
type node struct {
	p, n *node
	k    string
	v    interface{}
}

// a data structure that is efficient to insert/fetch/delete cache items [both O(1) time complexity]
type cache struct {
	cap  int
	hmap map[interface{}]*node
	head *node // not use pointer-to-pointer here,
	tail *node // coz it's trade-off for performance
}

// create a new lru cache object
func create(cap int) *cache {
	return &cache{cap, make(map[interface{}]*node, cap), nil, nil}
}

// put a cache item into lru cache
func (c *cache) put(k string, v interface{}) {
	if e, ok := c.hmap[k]; ok {
		e.v = v
		c._refresh(e)
		return
	}

	if c.cap <= 0 {
		return
	} else if len(c.hmap) >= c.cap {
		// transfer the tail item as the new item, then refresh
		delete(c.hmap, c.tail.k)
		c.tail.k, c.tail.v = k, v // reuse to reduce gc
		c.hmap[k] = c.tail
		c._refresh(c.tail)
		return
	}

	e := &node{nil, c.head, k, v}
	c.hmap[k] = e
	if len(c.hmap) != 1 {
		c.head.p = e
	} else {
		c.tail = e
	}
	c.head = e
}

// get value of key from lru cache with result
func (c *cache) get(k string) (interface{}, bool) {
	if e, ok := c.hmap[k]; ok {
		c._refresh(e)
		return e.v, ok
	}
	return nil, false
}

// delete item by key from lru cache
func (c *cache) del(k string) (interface{}, bool) {
	if e, ok := c.hmap[k]; ok {
		delete(c.hmap, k)
		c._remove(e)
		return e.v, true
	}
	return nil, false
}

// calls f sequentially for each key and value present in the lru cache
func (c *cache) foreach(f func(k string, v interface{}) bool) {
	for i := c.head; i != nil; i = i.n {
		if !f(i.k, i.v) {
			break
		}
	}
}

// inplace update
func (c *cache) update(k string, f func(v *interface{})) {
	if e, ok := c.hmap[k]; ok {
		f(&e.v)
		c._refresh(e)
	}
}

// length of lru cache
func (c *cache) length() int {
	return len(c.hmap)
}

// capacity of lru cache
func (c *cache) capacity() int {
	return c.cap
}

func (c *cache) _refresh(e *node) {
	if e.p == nil { // head node
		return
	}
	e.p.n = e.n
	if e.n == nil { // tail node
		c.tail = e.p
	} else {
		e.n.p = e.p
	}
	e.p, e.n, c.head.p, c.head = nil, c.head, e, e
}

func (c *cache) _remove(e *node) {
	if e.p == nil { // head node
		c.head = e.n
	} else {
		e.p.n = e.n
	}
	if e.n == nil { // tail node
		c.tail = e.p
	} else {
		e.n.p = e.p
	}
}

// hashCode hashes a string to a unique hashcode.
func hashCode(s string) int {
	return int(crc32.ChecksumIEEE([]byte(s)))
}

// Cache - concurrent cache structure
type Cache struct {
	locks  []sync.Mutex
	insts  [][2]*cache // level-0 for normal LRU, level-1 for LFU-2
	mask   int
	expire time.Duration
}

// the wrapper is necessary because of node reuse otherwise it's not threadsafe
type wrapper struct {
	v  interface{}
	ts int64 // nano timestamp
}

func nextPowOf2(cap int) int {
	if cap <= 1 {
		return 1
	}
	if cap&(cap-1) == 0 {
		return cap
	}
	cap |= cap >> 1
	cap |= cap >> 2
	cap |= cap >> 4
	cap |= cap >> 8
	cap |= cap >> 16
	return cap + 1
}

// NewLRUCache - create lru cache
// `bucketCnt` is buckets that shard items to reduce lock racing
// `capPerBkt` is length of each bucket
// can store `capPerBkt * bucketCnt` count of element in Cache at most
// `expire` is expiration that item alive (and we only use lazy eviction here)
func NewLRUCache(bucketCnt int, capPerBkt int, expire time.Duration) *Cache {
	size := nextPowOf2(bucketCnt)
	c := &Cache{make([]sync.Mutex, size), make([][2]*cache, size), size - 1, expire}
	for i := range c.insts {
		c.insts[i][0] = create(capPerBkt)
	}
	return c
}

// LFU - add lfu support (especially lfu-2 that when item visited twice it moves to upper-level-cache)
// `capPerBkt` is length of each lfu bucket
// can store extra `capPerBkt * bucketCnt` count of element in Cache at most
func (c *Cache) LFU(capPerBkt int) *Cache {
	for i := range c.insts {
		c.insts[i][1] = create(capPerBkt)
	}
	return c
}

// Put - put a item into cache
func (c *Cache) Put(key string, val interface{}) {
	idx := hashCode(key) & c.mask
	c.locks[idx].Lock()
	c.insts[idx][0].put(key, &wrapper{val, time.Now().UnixNano()})
	c.locks[idx].Unlock()
}

// internal sub function that get item at specific level
func (c *Cache) get(key string, idx, level int) (interface{}, bool) {
	if v, b := c.insts[idx][level].get(key); b {
		if time.Since(time.Unix(0, v.(*wrapper).ts)) > c.expire {
			// we don't need to remove the expired item here
			// removal is also ok that control the memory usage before the cache is full, but will cause GC thrashing
			// c.insts[idx][level].del(key)
			return v, false
		}
		return v, b
	}
	return nil, false
}

// Get - get value of key from cache with result
// if the item is expired, maybe you can also get the former item even if it returns `false`
func (c *Cache) Get(key string) (v interface{}, b bool) {
	idx := hashCode(key) & c.mask
	c.locks[idx].Lock()
	if c.insts[idx][1] == nil { // (if lfu mode not support, loss is little)
		// normal lru mode
		v, b = c.get(key, idx, 0)
	} else {
		// lfu-2 mode
		v, b = c.insts[idx][0].del(key)
		if !b {
			// re-find in level-1
			v, b = c.get(key, idx, 1)
		} else {
			// find in level-0, move to level-1
			c.insts[idx][1].put(key, v.(*wrapper))
		}
	}
	if !b {
		c.locks[idx].Unlock()
		return nil, false
	}
	c.locks[idx].Unlock()
	return v.(*wrapper).v, b
}

// Del - delete item by key from cache
func (c *Cache) Del(key string) {
	idx := hashCode(key) & c.mask
	c.locks[idx].Lock()
	c.insts[idx][0].del(key)
	if c.insts[idx][1] != nil { // (if lfu mode not support, loss is little)
		c.insts[idx][1].del(key)
	}
	c.locks[idx].Unlock()
}
