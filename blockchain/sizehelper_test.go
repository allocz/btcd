// Copyright (c) 2023 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.
package blockchain

import (
	"math"
	"os"
	"runtime"
	"testing"
	"unsafe"
)

// calculateEntries returns a number of entries that will make the map allocate
// the given total bytes.  The returned number is always the maximum number of
// entries that will allocate inside the given parameters.
func calculateEntries(totalBytes int, bucketSize int) int {
	// 48 is the number of bytes needed for the map header in a
	// 64 bit system. Refer to hmap in runtime/map.go in the go
	// standard library.
	totalBytes -= 48

	numBuckets := totalBytes / bucketSize
	B := uint8(math.Log2(float64(numBuckets)))
	if B == 0 {
		// For 0 buckets, the max is the bucket count.
		return bucketCnt
	}

	return int(loadFactorNum * (bucketShift(B) / loadFactorDen))
}

func TestCalculateEntries(t *testing.T) {
	for i := 0; i < 10_000_000; i++ {
		// It's not possible to calculate the exact amount of entries since
		// the map will only allocate for 2^N where N is the amount of buckets.
		//
		// So to see if the calculate entries function is working correctly,
		// we get the rough map size for i entries, then calculate the entries
		// for that map size.  If the size is the same, the function is correct.
		roughMapSize := calculateRoughMapSize(i, bucketSize)
		entries := calculateEntries(roughMapSize, bucketSize)
		gotRoughMapSize := calculateRoughMapSize(entries, bucketSize)

		if roughMapSize != gotRoughMapSize {
			t.Errorf("For hint of %d, expected %v, got %v\n",
				i, roughMapSize, gotRoughMapSize)
		}

		// Test that the entries returned are the maximum for the given map size.
		// If we increment the entries by one, we should get a bigger map.
		gotRoughMapSizeWrong := calculateRoughMapSize(entries+1, bucketSize)
		if roughMapSize == gotRoughMapSizeWrong {
			t.Errorf("For hint %d incremented by 1, expected %v, got %v\n",
				i, gotRoughMapSizeWrong*2, gotRoughMapSizeWrong)
		}

		minEntries := calculateMinEntries(roughMapSize, bucketSize)
		gotMinRoughMapSize := calculateRoughMapSize(minEntries, bucketSize)
		if roughMapSize != gotMinRoughMapSize {
			t.Errorf("For hint of %d, expected %v, got %v\n",
				i, roughMapSize, gotMinRoughMapSize)
		}

		// Can only test if they'll be half the size if the entries aren't 0.
		if minEntries > 0 {
			gotMinRoughMapSizeWrong := calculateRoughMapSize(minEntries-1, bucketSize)
			if gotMinRoughMapSize == gotMinRoughMapSizeWrong {
				t.Errorf("For hint %d decremented by 1, expected %v, got %v\n",
					i, gotRoughMapSizeWrong/2, gotRoughMapSizeWrong)
			}
		}
	}
}

// sampledAlloc uses sampling to perform allocations and return the first
// allocation size that reaches minSamples. We have to use sampling because the
// runtime introduces non determinism in respect with total bytes allocated
// between two points in code.
func sampledAlloc(count int, allocf func(count int) uint64) uint64 {
	const sampleLimit = 100000
	const minSamples = 100

	// We have to use sampling because the runtime introduces non
	// determinism in respect with total bytes allocated between
	// two points in code.
	samples := make(map[uint64]int)
	var lastSample uint64
	var reachedMinSamples bool
	for range sampleLimit {
		lastSample = allocf(count)
		samples[lastSample]++
		if samples[lastSample] >= minSamples {
			reachedMinSamples = true
			break
		}
	}

	if !reachedMinSamples {
		panic("not reached min samples")
	}

	return lastSample
}

// sliceSink is used to avoid compiler optimizations.
var sliceSink [][]byte

// TestRoundupsize asserts that the rounded up size maches the expectations for
// all size classes by comparing against runtime allocations. In case of
// breakages in the allocator behavior, this test should fail.
func TestRoundupsize(t *testing.T) {
	// For some reason the allocation sampling fails when make unit-race is
	// executed. But we don't use concurrency here, so is safe to not run
	// this test with race detection.
	if os.Getenv("GORACE") != "" {
		return
	}

	const n = 8

	// allocf allocates n byte slices of count capacity and returns the
	// total bytes allocated.
	allocF := func(count int) uint64 {
		var ms0, ms1 runtime.MemStats

		s := make([][]byte, n)
		runtime.ReadMemStats(&ms0)
		for i := range n {
			s[i] = make([]byte, count)
		}
		runtime.ReadMemStats(&ms1)
		sliceSink = s
		sliceSink = nil

		return ms1.TotalAlloc - ms0.TotalAlloc
	}

	// Allocations smaller than max size class size should align by the size
	// of the next class.
	for _, i := range class_to_size[1:] {
		want := sampledAlloc(int(i-1), allocF) / n
		got := uint64(roundupsize(uintptr(i - 1)))

		if want != got {
			t.Errorf("i %d; want %d, got %d", i, want, got)
		}
	}

	// Allocations greater than the last class should align by the page
	// size.
	lastClassSize := class_to_size[len(class_to_size)-1]

	want := alignUp(uintptr(lastClassSize+1), _PageSize)
	got := roundupsize(uintptr(lastClassSize + 1))
	real := sampledAlloc(int(lastClassSize+1), allocF) / n

	if want != got {
		t.Errorf("size %d; want %d got %d", lastClassSize+1, want, got)
	}

	if want != uintptr(real) {
		t.Errorf("size %d; want %d real %d", lastClassSize+1, want,
			real)
	}
}

// TestAvgEntrySize asserts that the avgEntrySize is being calculated correctly,
// accounting for the inneficiencies of the current implementation caused by
// allocating each entry with an allocation for the UtxoEntry and another for
// the scriptPubKey slice.
//
// This test remains true while we don't change the utxocache implementation to
// allocate in batch instead of at least two allocations for each added entry.
func TestAvgEntrySize(t *testing.T) {
	utxoEntryAlloc := roundupsize(unsafe.Sizeof(UtxoEntry{}))
	if baseEntrySize != utxoEntryAlloc {
		t.Errorf("bad base entry size; want %d got %d", baseEntrySize,
			utxoEntryAlloc)
	}

	const p2pkhPubScriptLen = 25
	pkhAlloc := roundupsize(p2pkhPubScriptLen)
	if pubKeyHashLen != pkhAlloc {
		t.Errorf("bad pubkeyHashLen; want %d got %d", pubKeyHashLen,
			pkhAlloc)
	}

	entryAlloc := roundupsize(utxoEntryAlloc + pkhAlloc)
	if avgEntrySize != entryAlloc {
		t.Errorf("bad avgEntrySize; want %d got %d", avgEntrySize,
			entryAlloc)
	}
}
