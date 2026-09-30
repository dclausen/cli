package redact

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// memSpanCache is an in-memory OPFSpanCache for tests.
type memSpanCache struct {
	entries map[string]map[string][]Span
	stores  int
}

func newMemSpanCache() *memSpanCache {
	return &memSpanCache{entries: make(map[string]map[string][]Span)}
}

func (c *memSpanCache) LoadOPFSpans(key string) (map[string][]Span, bool) {
	e, ok := c.entries[key]
	return e, ok
}

func (c *memSpanCache) StoreOPFSpans(key string, spans map[string][]Span) error {
	c.stores++
	c.entries[key] = spans
	return nil
}

func cacheTestBlobs() []NamedBlob {
	return []NamedBlob{
		{Name: "0/full.jsonl", ID: "blob-a", Content: []byte(`{"content":"Alice met Bob"}` + "\n" + `{"content":"Charlie sat down"}`)},
		{Name: "0/metadata.json", ID: "blob-b", Content: []byte(`{"summary":"Alice walked home","id":"x_1"}`)},
		{Name: "0/note.txt", ID: "blob-c", Content: []byte("Frank reviewed the diff")},
	}
}

var cacheTestCats = map[string]bool{"private_person": true}

// Scanning into the cache and applying from it must produce exactly what the
// one-pass batch produces: the split moves the model call, not the result.
func TestScanThenApplyMatchesOnePassBatch(t *testing.T) {
	fake := &fakeRuntime{spans: []Span{{Start: 0, End: 5, Label: "private_person"}}}
	configureFakeOPF(t, fake, cacheTestCats)
	want, err := BatchBytesWithPrivacyFilter(context.Background(), cacheTestBlobs())
	if err != nil {
		t.Fatalf("BatchBytesWithPrivacyFilter: %v", err)
	}

	cache := newMemSpanCache()
	if err := ScanBlobsWithPrivacyFilter(context.Background(), cacheTestBlobs(), cache); err != nil {
		t.Fatalf("ScanBlobsWithPrivacyFilter: %v", err)
	}
	got, err := ApplyCachedPrivacyFilter(cacheTestBlobs(), cache)
	if err != nil {
		t.Fatalf("ApplyCachedPrivacyFilter: %v", err)
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("blob %d: cached apply = %q, one-pass = %q", i, got[i], want[i])
		}
	}
	for key, entry := range cache.entries {
		for leafKey := range entry {
			if strings.Contains(key+leafKey, "Alice") {
				t.Fatalf("cache keys must not contain leaf text")
			}
		}
	}
}

func TestApplyCachedPrivacyFilter_PendingWithoutScan(t *testing.T) {
	configureFakeOPF(t, &fakeRuntime{}, cacheTestCats)
	if _, err := ApplyCachedPrivacyFilter(cacheTestBlobs(), newMemSpanCache()); !errors.Is(err, ErrOPFScanPending) {
		t.Fatalf("err = %v, want ErrOPFScanPending", err)
	}
}

func TestApplyCachedPrivacyFilter_PendingWithoutBlobID(t *testing.T) {
	configureFakeOPF(t, &fakeRuntime{}, cacheTestCats)
	cache := newMemSpanCache()
	blobs := cacheTestBlobs()
	if err := ScanBlobsWithPrivacyFilter(context.Background(), blobs, cache); err != nil {
		t.Fatal(err)
	}
	blobs[1].ID = ""
	if _, err := ApplyCachedPrivacyFilter(blobs, cache); !errors.Is(err, ErrOPFScanPending) {
		t.Fatalf("err = %v, want ErrOPFScanPending for a blob that cannot be keyed", err)
	}
}

// A second scan of already-cached blobs must not call the model.
func TestScanBlobsWithPrivacyFilter_SkipsCachedBlobs(t *testing.T) {
	fake := &fakeRuntime{}
	configureFakeOPF(t, fake, cacheTestCats)
	cache := newMemSpanCache()
	for range 2 {
		if err := ScanBlobsWithPrivacyFilter(context.Background(), cacheTestBlobs(), cache); err != nil {
			t.Fatal(err)
		}
	}
	if fake.batchCalls != 1 {
		t.Fatalf("batchCalls = %d, want 1", fake.batchCalls)
	}
	if cache.stores != 3 {
		t.Fatalf("stores = %d, want one per blob", cache.stores)
	}
}

// Results are only valid for the categories they were computed with.
func TestApplyCachedPrivacyFilter_CategoryChangeRequiresRescan(t *testing.T) {
	configureFakeOPF(t, &fakeRuntime{}, cacheTestCats)
	cache := newMemSpanCache()
	if err := ScanBlobsWithPrivacyFilter(context.Background(), cacheTestBlobs(), cache); err != nil {
		t.Fatal(err)
	}
	configureFakeOPF(t, &fakeRuntime{}, map[string]bool{"private_person": true, "private_email": true})
	if _, err := ApplyCachedPrivacyFilter(cacheTestBlobs(), cache); !errors.Is(err, ErrOPFScanPending) {
		t.Fatalf("err = %v, want ErrOPFScanPending after enabling a category", err)
	}
}

// An entry that does not cover every prose leaf of its blob is not a complete
// scan and must not be applied.
func TestApplyCachedPrivacyFilter_IncompleteEntryIsPending(t *testing.T) {
	configureFakeOPF(t, &fakeRuntime{}, cacheTestCats)
	cache := newMemSpanCache()
	if err := ScanBlobsWithPrivacyFilter(context.Background(), cacheTestBlobs(), cache); err != nil {
		t.Fatal(err)
	}
	for _, entry := range cache.entries {
		for leafKey := range entry {
			delete(entry, leafKey)
			break
		}
	}
	if _, err := ApplyCachedPrivacyFilter(cacheTestBlobs(), cache); !errors.Is(err, ErrOPFScanPending) {
		t.Fatalf("err = %v, want ErrOPFScanPending for a partial entry", err)
	}
}

func TestOPFSpanCache_NoCategoriesFailsClosed(t *testing.T) {
	configureFakeOPF(t, &fakeRuntime{}, map[string]bool{})
	cache := newMemSpanCache()
	if err := ScanBlobsWithPrivacyFilter(context.Background(), cacheTestBlobs(), cache); !errors.Is(err, ErrOPFNoEnabledCategories) {
		t.Fatalf("scan err = %v, want ErrOPFNoEnabledCategories", err)
	}
	if _, err := ApplyCachedPrivacyFilter(cacheTestBlobs(), cache); !errors.Is(err, ErrOPFNoEnabledCategories) {
		t.Fatalf("apply err = %v, want ErrOPFNoEnabledCategories", err)
	}
}

// A failed model call stores nothing, so a later apply cannot mistake the
// failure for a clean scan.
func TestScanBlobsWithPrivacyFilter_RuntimeFailureStoresNothing(t *testing.T) {
	configureFakeOPF(t, &fakeRuntime{err: errors.New("opf crashed")}, cacheTestCats)
	cache := newMemSpanCache()
	if err := ScanBlobsWithPrivacyFilter(context.Background(), cacheTestBlobs(), cache); err == nil {
		t.Fatal("want an error from a failing runtime")
	}
	if cache.stores != 0 {
		t.Fatalf("stores = %d, want 0 after a failed scan", cache.stores)
	}
	if err := ScanBlobsWithPrivacyFilter(context.Background(), cacheTestBlobs(), cache); !errors.Is(err, ErrOPFUnavailable) {
		t.Fatalf("second scan err = %v, want ErrOPFUnavailable once the breaker tripped", err)
	}
}
