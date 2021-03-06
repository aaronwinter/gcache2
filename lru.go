package gcache

import (
	"container/list"
	"time"
)

type LRUCache struct {
	baseCache
	store     map[interface{}]*list.Element
	evictList *list.List
}

type lruItem struct {
	clock      Clock
	key        interface{}
	value      interface{}
	expiration *time.Time
}

func newLRUCache(cb *CacheBuilder) *LRUCache {
	c := &LRUCache{}
	buildCache(&c.baseCache, cb)

	c.init()
	c.loadGroup.cache = c
	return c
}

func (c *LRUCache) init() {
	c.evictList = list.New()
	c.store = make(map[interface{}]*list.Element, c.capacity+1)
}

func (c *LRUCache) set(key, value interface{}) (interface{}, error) {
	var err error
	if c.serializeFunc != nil {
		value, err = c.serializeFunc(key, value)
		if err != nil {
			return nil, err
		}
	}

	var item *lruItem
	if it, ok := c.store[key]; ok {
		c.evictList.MoveToFront(it)
		item = it.Value.(*lruItem)
		item.value = value
	} else {
		if c.evictList.Len() >= c.capacity {
			c.evict(1)
		}
		item = &lruItem{
			clock: c.clock,
			key:   key,
			value: value,
		}
		c.store[key] = c.evictList.PushFront(item)
	}

	if c.expiration != nil {
		t := c.clock.Now().Add(*c.expiration)
		item.expiration = &t
	}

	if c.addedFunc != nil {
		c.addedFunc(key, value)
	}

	return item, nil
}

func (c *LRUCache) Set(key, value interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	_, err := c.set(key, value)
	return err
}

func (c *LRUCache) SetWithExpire(key, value interface{}, expiration time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	item, err := c.set(key, value)
	if err != nil {
		return err
	}

	t := c.clock.Now().Add(expiration)
	item.(*lruItem).expiration = &t
	return nil
}

func (c *LRUCache) get(key interface{}, onLoad bool) (interface{}, error) {
	entry, exists := c.store[key]

	if !exists {
		if !onLoad {
			c.stats.IncrMissCount()
		}
		return nil, KeyNotFoundError
	}

	item := entry.Value.(*lruItem)
	if item.isExpired(nil) {
		c.removeElement(entry)
		if !onLoad {
			c.stats.IncrMissCount()
		}

		return nil, KeyNotFoundError
	}

	c.evictList.MoveToFront(entry)
	if !onLoad {
		c.stats.IncrHitCount()
	}

	if c.deserializeFunc != nil {
		return c.deserializeFunc(key, item.value)
	}

	return item.value, nil
}

func (c *LRUCache) getWithLoader(key interface{}, isWait bool) (interface{}, error) {
	if c.loaderExpireFunc == nil {
		return nil, KeyNotFoundError
	}

	value, _, err := c.load(key, func(v interface{}, expiration *time.Duration, e error) (interface{}, error) {
		if e != nil {
			return nil, e
		}

		err := c.Set(key, v)
		if err != nil {
			return nil, err
		}

		return v, nil
	}, isWait)
	if err != nil {
		return nil, err
	}
	return value, nil
}

func (c *LRUCache) Get(key interface{}) (interface{}, error) {
	c.mu.Lock()
	v, err := c.get(key, false)
	c.mu.Unlock()

	if err == KeyNotFoundError {
		return c.getWithLoader(key, true)
	}
	return v, err
}

func (c *LRUCache) GetIFPresent(key interface{}) (interface{}, error) {
	c.mu.Lock()
	v, err := c.get(key, false)
	c.mu.Unlock()

	if err == KeyNotFoundError {
		return c.getWithLoader(key, false)
	}
	return v, err
}

func (c *LRUCache) GetALL() map[interface{}]interface{} {
	c.mu.Lock()
	allKeys := c.keys()
	c.mu.Unlock()

	m := make(map[interface{}]interface{})
	for _, k := range allKeys {
		v, err := c.GetIFPresent(k)
		if err == nil {
			m[k] = v
		}
	}
	return m
}

func (c *LRUCache) remove(key interface{}) error {
	if ent, ok := c.store[key]; ok {
		c.removeElement(ent)
		return nil
	}
	return KeyNotFoundError
}

func (c *LRUCache) Remove(key interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.remove(key)
}

func (c *LRUCache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.purgeVisitorFunc != nil {
		for key, item := range c.store {
			it := item.Value.(*lruItem)
			v := it.value
			c.purgeVisitorFunc(key, v)
		}
	}

	c.init()
}
func (c *LRUCache) keys() []interface{} {
	keys := make([]interface{}, len(c.store))
	var i = 0
	for k := range c.store {
		keys[i] = k
		i++
	}
	return keys
}

func (c *LRUCache) Keys() []interface{} {
	c.mu.Lock()
	allKeys := c.keys()
	c.mu.Unlock()

	keys := []interface{}{}
	for _, k := range allKeys {
		_, err := c.GetIFPresent(k)
		if err == nil {
			keys = append(keys, k)
		}
	}
	return keys
}

func (c *LRUCache) Len() int {
	return len(c.store)
}

func (c *LRUCache) evict(count int) {
	for i := 0; i < count; i++ {
		ent := c.evictList.Back()
		if ent == nil {
			return
		} else {
			c.removeElement(ent)
		}
	}
}

func (c *LRUCache) removeElement(e *list.Element) {
	c.evictList.Remove(e)
	entry := e.Value.(*lruItem)
	delete(c.store, entry.key)
	if c.evictedFunc != nil {
		entry := e.Value.(*lruItem)
		c.evictedFunc(entry.key, entry.value)
	}
}

func (it *lruItem) isExpired(now *time.Time) bool {
	if it.expiration == nil {
		return false
	}
	if now == nil {
		t := it.clock.Now()
		now = &t
	}
	return it.expiration.Before(*now)
}

func (c *LRUCache) Debug() map[string][]int {
	d := make(map[string][]int)
	d["lru"] = []int{len(c.store), c.evictList.Len()}
	return d
}

func (c *LRUCache) unsafeGet(key interface{}, onLoad bool) (interface{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.get(key, onLoad)
}
