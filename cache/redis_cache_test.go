package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/contentsquare/chproxy/config"
)

// appendCtxTracker wraps a redis.UniversalClient and, on each Append, snapshots
// the cancellation state of every previously-seen context. This lets a test
// observe whether a per-iteration context is cancelled before the next
// iteration runs.
type appendCtxTracker struct {
	redis.UniversalClient
	mu        sync.Mutex
	seen      []context.Context
	snapshots [][]error
}

func (t *appendCtxTracker) Append(ctx context.Context, key, value string) *redis.IntCmd {
	t.mu.Lock()
	snap := make([]error, len(t.seen))
	for i, c := range t.seen {
		snap[i] = c.Err()
	}
	t.snapshots = append(t.snapshots, snap)
	t.seen = append(t.seen, ctx)
	t.mu.Unlock()
	return t.UniversalClient.Append(ctx, key, value)
}

func TestRedisCachePutCancelsPerChunkContextBetweenIterations(t *testing.T) {
	s := miniredis.RunT(t)
	base := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{s.Addr()}})
	tracker := &appendCtxTracker{UniversalClient: base}
	cache := newRedisCache(tracker, redisConf)
	defer cache.Close()

	// Two 2 MiB chunks → two Append iterations.
	payloadSize := 4 * 1024 * 1024
	payload := strings.Repeat("a", payloadSize)
	key := &Key{Query: []byte("SELECT defer_loop")}

	if _, err := cache.Put(strings.NewReader(payload), ContentMetadata{Length: int64(payloadSize)}, key); err != nil {
		t.Fatalf("put failed: %s", err)
	}

	if len(tracker.snapshots) < 2 {
		t.Fatalf("expected at least 2 Append calls, got %d", len(tracker.snapshots))
	}

	// At iteration i (i>0), every prior chunk's context must already be
	// cancelled. The buggy version defers cancel to Put's return, so prior
	// contexts are still live when later chunks Append.
	for i := 1; i < len(tracker.snapshots); i++ {
		for j, prevErr := range tracker.snapshots[i] {
			if prevErr == nil {
				t.Errorf("Append iteration %d: prior context from iteration %d not cancelled (defer-in-loop accumulates cancels until Put returns)", i, j)
			}
		}
	}
}

const cacheTTL = time.Duration(30 * time.Second)

var redisConf = config.Cache{
	Name: "foobar",
	Redis: config.RedisCacheConfig{
		Addresses: []string{"http://localhost:8080"},
	},
	Expire: config.Duration(cacheTTL),
}

func TestCacheSize(t *testing.T) {
	redisCache := getRedisCache(t)
	nbKeys := redisCache.nbOfKeys()
	if nbKeys > 0 {
		t.Fatalf("the cache should be empty")
	}

	cacheSize := redisCache.nbOfBytes()
	if cacheSize > 0 {
		t.Fatalf("the cache should be empty")
	}

	buffer := strings.NewReader("an object")

	if _, err := redisCache.Put(buffer, ContentMetadata{}, &Key{Query: []byte("SELECT 1")}); err != nil {
		t.Fatalf("failed to put it to cache: %s", err)
	}
	if _, err := redisCache.Put(buffer, ContentMetadata{}, &Key{Query: []byte("SELECT 2")}); err != nil {
		t.Fatalf("failed to put it to cache: %s", err)
	}
	if _, err := redisCache.Put(buffer, ContentMetadata{}, &Key{Query: []byte("SELECT 2")}); err != nil {
		t.Fatalf("failed to put it to cache: %s", err)
	}

	stats := redisCache.Stats()
	if stats.Items != 2 {
		t.Fatalf("cache should contain 2 items")
	}
	// because of the use of miniredis to simulate a real redis server
	// we can't check stats.Size because miniredis doesn't handle the memory usage of redis
}

func getRedisCacheAndServer(t *testing.T) (*redisCache, *miniredis.Miniredis) {
	s := miniredis.RunT(t)
	redisClient := redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs: []string{s.Addr()},
	})
	redisCache := newRedisCache(redisClient, redisConf)
	return redisCache, s
}
func getRedisCache(t *testing.T) *redisCache {
	redisCache, _ := getRedisCacheAndServer(t)
	return redisCache
}

func TestRedisCacheAddGet(t *testing.T) {
	c := getRedisCache(t)
	defer func() {
		c.Close()
	}()
	cacheAddGetHelper(t, c)
}

func TestRedisCacheMiss(t *testing.T) {
	c := getRedisCache(t)
	cacheMissHelper(t, c)
}
func TestStringFromToByte(t *testing.T) {
	c := getRedisCache(t)
	b := c.encodeString("test")
	s, size, _ := c.decodeString(b)
	if s != "test" {
		t.Fatalf("got: %s, expected %s", s, "test")
	}
	if size != 8 {
		t.Fatalf("got: %d, expected %d", size, 8)
	}
}
func TestMetadataFromToByte(t *testing.T) {
	c := getRedisCache(t)

	expectedMetadata := &ContentMetadata{
		Length:   12,
		Type:     "json",
		Encoding: "gzip",
	}

	b := c.encodeMetadata(expectedMetadata)

	metadata, size, _ := c.decodeMetadata(b)
	if metadata.Encoding != expectedMetadata.Encoding {
		t.Fatalf("got: %s, expected %s", metadata.Encoding, expectedMetadata.Encoding)
	}
	if metadata.Type != expectedMetadata.Type {
		t.Fatalf("got: %s, expected %s", metadata.Type, expectedMetadata.Type)
	}
	if metadata.Length != expectedMetadata.Length {
		t.Fatalf("got: %d, expected %d", metadata.Length, expectedMetadata.Length)
	}
	if size != 24 {
		t.Fatalf("got: %d, expected %d", size, 24)
	}

}
func TestDecodingCorruptedMetadata(t *testing.T) {
	c := getRedisCache(t)

	// this test will make fetching the length of a payload fail
	_, _, err := c.decodeMetadata([]byte{})
	if (err == nil || !errors.Is(err, &RedisCacheCorruptionError{})) {
		t.Fatalf("expected a corruption error, err=%s", err)
	}

	// this test will make fetching a string in the metadata fail because it can't read the length of a metadata string
	_, _, err = c.decodeMetadata([]byte{0, 0, 0, 0, 1, 1, 1, 1})
	if (err == nil || !errors.Is(err, &RedisCacheCorruptionError{})) {
		t.Fatalf("expected a corruption error, err=%s", err)
	}
	// this test will make fetching a string in the metadata fail because the length of a metadata string doesn't match it's size
	_, _, err = c.decodeMetadata([]byte{0, 0, 0, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1})
	if (err == nil || !errors.Is(err, &RedisCacheCorruptionError{})) {
		t.Fatalf("expected a corruption error, err=%s", err)
	}

}

func TestKeyExpirationWhileFetchtingRedis(t *testing.T) {
	cache, redis := getRedisCacheAndServer(t)
	defer cache.Close()

	key := &Key{
		Query: []byte(fmt.Sprintf("SELECT test")),
	}
	payloadSize := 4 * 1024 * 1024
	exepctedValue := strings.Repeat("a", payloadSize)
	reader := strings.NewReader(exepctedValue)

	if _, err := cache.Put(reader, ContentMetadata{Encoding: "ce", Type: "ct", Length: int64(payloadSize)}, key); err != nil {
		t.Fatalf("failed to put it to cache: %s", err)
	}
	cachedData, err := cache.Get(key)
	if err != nil {
		t.Fatalf("failed to get data from redis cache: %s", err)
	}

	//simulating a cache expiration
	if !redis.Del(key.String()) {
		t.Fatalf("could not delete key")
	}
	_, err = io.ReadAll(cachedData.Data)
	_, isRedisCacheError := err.(*RedisCacheError)
	if err == nil || !isRedisCacheError {
		t.Fatalf("expecting an error of type RedisCacheError, err=%s", err)
	}
}

func TestCorruptedPayloadWhileFetchtingRedis(t *testing.T) {
	cache, redis := getRedisCacheAndServer(t)
	defer cache.Close()

	key := &Key{
		Query: []byte(fmt.Sprintf("SELECT test")),
	}
	payloadSize := 4 * 1024 * 1024
	exepctedValue := strings.Repeat("a", payloadSize)
	reader := strings.NewReader(exepctedValue)

	if _, err := cache.Put(reader, ContentMetadata{Encoding: "ce", Type: "ct", Length: int64(payloadSize)}, key); err != nil {
		t.Fatalf("failed to put it to cache: %s", err)
	}

	//simulating a data alteration
	redisData, err := redis.Get(key.String())
	if err != nil {
		t.Fatalf("item not inseted in redis")
	}

	err = redis.Set(key.String(), redisData[:payloadSize-100])
	if err != nil {
		t.Fatalf("could not alter data in redis")
	}
	//simulate a value almost expired
	redis.SetTTL(key.String(), 2*time.Second)

	_, err = cache.Get(key)
	_, isRedisCacheError := err.(*RedisCacheError)
	if err == nil || !isRedisCacheError {
		t.Fatalf("expecting an error of type RedisCacheError, err=%s", err)
	}

	//simulate a value that will not expire soon
	redis.SetTTL(key.String(), 500*time.Second)

	cachedData, err := cache.Get(key)
	if err != nil {
		t.Fatalf("failed to get data from redis cache: %s", err)
	}

	_, err = io.ReadAll(cachedData.Data)
	_, isRedisCacheError = err.(*RedisCacheError)
	if err == nil || !isRedisCacheError {
		t.Fatalf("expecting an error of type RedisCacheError, err=%s", err)
	}
}

func TestSmallTTLOnBigPayloadAreCacheWithFile(t *testing.T) {
	cache, redis := getRedisCacheAndServer(t)
	defer cache.Close()
	key := &Key{
		Query: []byte(fmt.Sprintf("SELECT test")),
	}
	payloadSize := 4 * 1024 * 1024
	expectedValue := strings.Repeat("a", payloadSize)
	reader := strings.NewReader(expectedValue)

	if _, err := cache.Put(reader, ContentMetadata{Encoding: "ce", Type: "ct", Length: int64(payloadSize)}, key); err != nil {
		t.Fatalf("failed to put it to cache: %s", err)
	}

	//simulate a value almost expired
	redis.SetTTL(key.String(), 2*time.Second)
	nbFileCacheBeforeGet, err := countFilesWithPrefix(os.TempDir(), redisTmpFilePrefix)
	if err != nil {
		t.Fatalf("could not read directory %s", err)
	}

	cachedData, err := cache.Get(key)
	if err != nil {
		t.Fatalf("expected cached to have the value")
	}
	nbFileCacheAfterGet, err := countFilesWithPrefix(os.TempDir(), redisTmpFilePrefix)
	if err != nil {
		t.Fatalf("could not read directory %s", err)
	}
	if nbFileCacheBeforeGet+1 != nbFileCacheAfterGet {
		t.Fatalf("expected a file to be stored by redisFileCache ")
	}

	cachedValue, err := io.ReadAll(cachedData.Data)
	if err != nil {
		t.Fatalf("could not read data from redisFileCache, err=%s", err)
	}
	if string(cachedValue) != expectedValue {
		t.Fatalf("got a value different than the expected one len(value)=%d vs len(expectedValue)=%d", len(string(cachedValue)), len(expectedValue))
	}
	cachedData.Data.Close()
	nbFileCacheAfterClose, err := countFilesWithPrefix(os.TempDir(), redisTmpFilePrefix)
	if err != nil {
		t.Fatalf("could not read directory %s", err)
	}

	if nbFileCacheBeforeGet != nbFileCacheAfterClose {
		t.Fatalf("expected the file stored by redisFileCache to be removed: nbFileCacheBeforeGet=%d | nbFileCacheAfterClose=%d", nbFileCacheBeforeGet, nbFileCacheAfterClose)
	}
}

func countFilesWithPrefix(dir, prefix string) (int, error) {
	count := 0
	files, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), prefix) {
			count++
		}
	}
	return count, nil
}
