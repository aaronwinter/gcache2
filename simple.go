package gcache

import "time"

// SimpleCache has no clear priority for evict cache. It depends on key-value map order.
type SimpleCache struct {
	baseCache
	store map[interface{}]*simpleItem
}

func newSimpleCache(cb *CacheBuilder) *SimpleCache {
	c := &SimpleCache{}
	buildCache(&c.baseCache, cb)

	c.init()
	c.loadGroup.cache = c
	return c
}

func (c *SimpleCache) init() {
	if c.size <= 0 {
		c.store = make(map[interface{}]*simpleItem)
	} else {
		c.store = make(map[interface{}]*simpleItem, c.size)
	}
}

// Set a new key-value pair
func (c *SimpleCache) Set(key, value interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.set(key, value)
	return err
}

// Set a new key-value pair with an expiration time
func (c *SimpleCache) SetWithExpire(key, value interface{}, expiration time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, err := c.set(key, value)
	if err != nil {
		return err
	}

	t := c.clock.Now().Add(expiration)
	item.(*simpleItem).expiration = &t
	return nil
}

func (c *SimpleCache) set(key, value interface{}) (interface{}, error) {
	var err error
	if c.serializeFunc != nil {
		value, err = c.serializeFunc(key, value)
		if err != nil {
			return nil, err
		}
	}

	// Check for existing item
	item, ok := c.store[key]
	if ok {
		item.value = value
	} else {
		// Verify size not exceeded
		if (len(c.store) >= c.size) && c.size > 0 {
			c.evict(1)
		}
		item = &simpleItem{
			clock: c.clock,
			value: value,
		}
		c.store[key] = item
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

func (c *SimpleCache) unsafeGet(key interface{}, onLoad bool) (interface{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.get(key, onLoad)
}

// Get a value from cache pool using key if it exists.
// If it dose not exists key and has LoaderFunc,
// generate a value using `LoaderFunc` method returns value.
func (c *SimpleCache) Get(key interface{}) (interface{}, error) {
	c.mu.Lock()
	v, err := c.get(key, false)
	c.mu.Unlock()

	if err == KeyNotFoundError {
		return c.getWithLoader(key, true)
	}
	return v, err
}

// Get a value from cache pool using key if it exists.
// If it dose not exists key, returns KeyNotFoundError.
// And send a request which refresh value for specified key if cache object has LoaderFunc.
func (c *SimpleCache) GetIFPresent(key interface{}) (interface{}, error) {
	c.mu.Lock()
	v, err := c.get(key, false)
	c.mu.Unlock()
	if err == KeyNotFoundError {
		return c.getWithLoader(key, false)
	}
	return v, nil
}

func (c *SimpleCache) get(key interface{}, onLoad bool) (interface{}, error) {
	item, exists := c.store[key]
	if !exists {
		if !onLoad {
			c.stats.IncrMissCount()
		}
		return nil, KeyNotFoundError
	}

	if item.IsExpired(nil) {
		c.remove(key)
		return nil, KeyNotFoundError
	}

	v := item.value
	if !onLoad {
		c.stats.IncrHitCount()
	}

	if c.deserializeFunc != nil {
		return c.deserializeFunc(key, v)
	}
	return v, nil
}

func (c *SimpleCache) getWithLoader(key interface{}, isWait bool) (interface{}, error) {
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

func (c *SimpleCache) evict(count int) {
	now := c.clock.Now()
	current := 0
	for key, item := range c.store {
		if current >= count {
			return
		}
		if item.expiration == nil || now.After(*item.expiration) {
			defer c.remove(key)
			current++
		}
	}
}

// Removes the provided key from the cache.
func (c *SimpleCache) Remove(key interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.remove(key)
}

func (c *SimpleCache) remove(key interface{}) error {
	item, ok := c.store[key]
	if ok {
		delete(c.store, key)
		if c.evictedFunc != nil {
			c.evictedFunc(key, item.value)
		}
		return nil
	}
	return KeyNotFoundError
}

// Returns a slice of the keys in the cache.
func (c *SimpleCache) keys() []interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := make([]interface{}, len(c.store))
	var i = 0
	for k := range c.store {
		keys[i] = k
		i++
	}
	return keys
}

// Returns a slice of the keys in the cache.
func (c *SimpleCache) Keys() []interface{} {
	keys := []interface{}{}
	for _, k := range c.keys() {
		_, err := c.GetIFPresent(k)
		if err == nil {
			keys = append(keys, k)
		}
	}
	return keys
}

// Returns all key-value pairs in the cache.
func (c *SimpleCache) GetALL() map[interface{}]interface{} {
	m := make(map[interface{}]interface{})
	for _, k := range c.keys() {
		v, err := c.GetIFPresent(k)
		if err == nil {
			m[k] = v
		}
	}
	return m
}

// Returns the number of store in the cache.
func (c *SimpleCache) Len() int {
	return len(c.store)
}

// Completely clear the cache
func (c *SimpleCache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.purgeVisitorFunc != nil {
		for key, item := range c.store {
			c.purgeVisitorFunc(key, item.value)
		}
	}

	c.init()
}

type simpleItem struct {
	clock      Clock
	value      interface{}
	expiration *time.Time
}

// returns boolean value whether this item is expired or not.
func (si *simpleItem) IsExpired(now *time.Time) bool {
	if si.expiration == nil {
		return false
	}
	if now == nil {
		t := si.clock.Now()
		now = &t
	}
	return si.expiration.Before(*now)
}

func (c *SimpleCache) Debug() map[string][]int {
	d := make(map[string][]int)
	d["simple"] = []int{len(c.store)}
	return d
}
