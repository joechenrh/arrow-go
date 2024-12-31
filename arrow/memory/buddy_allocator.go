// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !tinygo
// +build !tinygo

package memory

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

var (
	maxArenaCount    = 0       // maximum arena count
	arenaDefaultSize = 1 << 30 // size of each arena
)

// arenaPool is used to cache and reuse arenas
type arenaPool struct {
	arenas    chan *internalAllocator
	allocated int
	lock      sync.Mutex
}

func (ap *arenaPool) get() *internalAllocator {
	// First try to get cached arena
	select {
	case a := <-ap.arenas:
		return a
	default:
	}

	ap.lock.Lock()
	defer ap.lock.Unlock()

	// Create a new one and return
	if ap.allocated < maxArenaCount {
		ap.allocated++
		bd := &internalAllocator{}
		bd.init(arenaDefaultSize)
		return bd
	}

	// We can't create new arena, return nil
	return nil
}

func (ap *arenaPool) put(a *internalAllocator) {
	ap.arenas <- a
}

var pool = &arenaPool{
	allocated: 0,
	arenas:    make(chan *internalAllocator, 256),
}

// The smallest block size is 256KB
const leafSize = 256 << 10

func SetMaxMemoryUsage(size int) {
	maxArenaCount = size / arenaDefaultSize
}

// Convert slice to an uintptr. This value is used as key in map.
func UnsafeGetBlkAddr(slice []byte) uintptr {
	return uintptr(unsafe.Pointer(&slice[0]))
}

func roundUp(n, sz int) int {
	return (n + sz - 1) / sz * sz
}

// Compute block size at layer l
func BlkSize(l int) int {
	return (1 << l) * leafSize
}

// Compute the block index for offset at layer l
func BlkIndex(l, offset int) int {
	return offset / BlkSize(l)
}

// Compute the first block index at layer l after offset
func BlkIndexNext(l, offset int) int {
	blkSize := BlkSize(l)
	bi := offset / blkSize
	if offset%blkSize != 0 {
		bi++
	}
	return bi
}

// Convert a block index at layer l back into an offset
func BlkAddr(l, bi int) int {
	return bi * BlkSize(l)
}

// Return 1 if bit at position index in array is set to 1
func BitIsSet(arr []byte, index int) bool {
	b := int(arr[index/8])
	m := (1 << (index % 8))
	return (b & m) == m
}

// Set bit at position index in array to 1
func BitSet(arr []byte, index int) {
	b := int(arr[index/8])
	m := (1 << (index % 8))
	arr[index/8] = byte(b | m)
}

// Clear bit at position index in array
func BitClear(arr []byte, index int) {
	b := int(arr[index/8])
	m := (1 << (index % 8))
	arr[index/8] = byte(b & ^m)
}

// Return the first layer whose block size is larger than n
func FirstLayer(n int) int {
	l := 0
	for size := leafSize; size < n; size *= 2 {
		l++
	}
	return l
}

// The allocator has bufferInfo for each size k. Each bufferInfo has a free
// list, an array alloc to keep track which blocks have been
// allocated, and an split array to to keep track which blocks have
// been split.  The arrays are of type char (which is 1 byte), but the
// allocator uses 1 bit per block (thus, one char records the info of
// 8 blocks).
type bufferInfo struct {
	alloc       []byte
	split       []byte
	canAllocate []byte

	l       int
	nblk    int
	freeCnt int
}

func (binfo *bufferInfo) init(nblk, l int) {
	sz := roundUp(nblk, 8) / 8
	binfo.canAllocate = make([]byte, nblk)
	binfo.alloc = make([]byte, sz)
	binfo.split = make([]byte, sz)
	binfo.l = l
}

// Remove buffer at offset in this layer as non-allocatable.
func (binfo *bufferInfo) remove(offset int) {
	binfo.freeCnt--
	BitClear(binfo.canAllocate, BlkIndex(binfo.l, offset))
}

// Check whether there are available buffer in this layer.
func (binfo *bufferInfo) empty() bool {
	return binfo.freeCnt == 0
}

// Add buffer at offset in this layer as allocatable
func (binfo *bufferInfo) push(offset int) {
	binfo.freeCnt++
	BitSet(binfo.canAllocate, BlkIndex(binfo.l, offset))
}

// Get one free buffer in this layer
func (binfo *bufferInfo) pop() int {
	for bi := 0; bi < binfo.nblk; bi++ {
		if BitIsSet(binfo.canAllocate, bi) {
			BitClear(binfo.canAllocate, bi)
			binfo.freeCnt--
			return BlkAddr(binfo.l, bi)
		}
	}
	return -1
}

// buffer is represented as an offset.
type internalAllocator struct {
	buffer   []byte
	bufInfo  []bufferInfo
	nLayers  int
	maxLayer int

	allocated map[uintptr]int

	allocatedBytes atomic.Int64
	unavailable    int
	total          int
}

// Find the layer of the block at offset
func (b *internalAllocator) layer(offset int) int {
	for k := 0; k < b.maxLayer; k++ {
		if BitIsSet(b.bufInfo[k+1].split, BlkIndex(k+1, offset)) {
			return k
		}
	}
	return b.maxLayer
}

// Allocate nbytes, but malloc won't return anything smaller than LeafSize
func (b *internalAllocator) allocateInternal(nbytes int) []byte {
	// Find a free block >= nbytes, starting with lowest layer possible
	fl := FirstLayer(nbytes)
	l := fl
	for ; l < b.nLayers; l++ {
		if !b.bufInfo[l].empty() {
			break
		}
	}

	// No free blocks, allocation failed
	if l == b.nLayers {
		return nil
	}

	// Found a block, pop it and potentially split it.
	offset := b.bufInfo[l].pop()
	BitSet(b.bufInfo[l].alloc, BlkIndex(l, offset))
	for ; l > fl; l-- {
		// Get the buddy buffer
		qa := offset + BlkSize(l-1)
		// Split the block at layer l, mark it as splited.
		// Mark half of the block at l - 1 as allocated,
		// and put it into the free list at layer l-1.
		BitSet(b.bufInfo[l].split, BlkIndex(l, offset))
		BitSet(b.bufInfo[l-1].alloc, BlkIndex(l-1, offset))
		b.bufInfo[l-1].push(qa)
	}

	buf := b.buffer[offset : offset+BlkSize(l)]
	b.allocatedBytes.Add(int64(BlkSize(l)))
	b.allocated[UnsafeGetBlkAddr(buf)] = offset

	b.sanityCheck()

	return buf
}

// free memory marked by p, which was earlier allocated using Malloc
func (b *internalAllocator) freeInternal(bs []byte) {
	bs = bs[:1]
	addr := UnsafeGetBlkAddr(bs)
	offset, ok := b.allocated[addr]
	if !ok {
		return
	}

	l := b.layer(offset)

	b.allocatedBytes.Add(-int64(BlkSize(l)))
	delete(b.allocated, addr)

	// Start merge from layer l
	for ; l < b.maxLayer; l++ {
		// Find the buddy index at layer l
		bi := BlkIndex(l, offset)
		buddy := bi + 1
		if bi%2 != 0 {
			buddy = bi - 1
		}

		// Free p at layer l
		BitClear(b.bufInfo[l].alloc, bi)

		// If buddy is allocated, break the merge
		if BitIsSet(b.bufInfo[l].alloc, buddy) {
			break
		}

		// Buddy is free, merge with buddy and remove it from free list
		buddyOffset := BlkAddr(l, buddy)
		b.bufInfo[l].remove(buddyOffset)

		// Update offset to the merged buffer at layer l+1
		if buddy%2 == 0 {
			offset = buddyOffset
		}

		// At layer l+1, mark that the merged buddy pair isn't split anymore
		BitClear(b.bufInfo[l+1].split, BlkIndex(l+1, offset))
	}

	// Add the final merged buffer to free list.
	b.bufInfo[l].push(offset)

	b.sanityCheck()
}

func (b *internalAllocator) freeAll() {
	for _, offset := range b.allocated {
		b.freeInternal(b.buffer[offset:])
	}

	if len(b.allocated) != 0 || b.allocatedBytes.Load() != 0 {
		panic("freeAll error")
	}
}

/*
 * Mark memory from [start, end), starting at layer 0, as allocated.
 *
 *              start(leftbi)                        end    rightBi
 *                    |                               |        |
 * |--------|---------|xxxxxxxx|xxxxxxxx|xxxxxxxx|xxxxxxxx|--------|--------|
 */
func (b *internalAllocator) markAllocated(start, end int) {
	for k := 0; k < b.nLayers; k++ {
		leftBi := BlkIndex(k, start)
		rightBi := BlkIndexNext(k, end)
		for bi := leftBi; bi < rightBi; bi++ {
			// if a block is allocated at size k, mark it as split too.
			BitSet(b.bufInfo[k].split, bi)
			BitSet(b.bufInfo[k].alloc, bi)
		}
	}
}

// Mark the range outside [start, end) as allocated
func (b *internalAllocator) markUnavailable(start, end int) int {
	heapSize := BlkSize(b.maxLayer)
	unavailableEnd := roundUp(heapSize-end, leafSize)
	unavailableStart := roundUp(start, leafSize)
	b.markAllocated(0, unavailableStart)
	b.markAllocated(heapSize-unavailableEnd, heapSize)
	return unavailableEnd + unavailableStart
}

// If a block is marked as allocated and its buddy is free, put the
// buddy on the free list at layer l.
func (b *internalAllocator) initFreePair(l, bi int) (free int) {
	buddy := bi + 1
	if bi%2 == 1 {
		buddy = bi - 1
	}

	// one of the pair is free
	if BitIsSet(b.bufInfo[l].alloc, bi) != BitIsSet(b.bufInfo[l].alloc, buddy) {
		free = BlkSize(l)
		if BitIsSet(b.bufInfo[l].alloc, bi) {
			b.bufInfo[l].push(BlkAddr(l, buddy))
		} else {
			b.bufInfo[l].push(BlkAddr(l, bi))
		}

	}
	return
}

/*
 * Initialize the free lists for each layer l.  For each layer l, there
 * are only two pairs that may have a buddy that should be on free list.
 *
 *                  start   leftBi           rightBi  end
 *                    |       |                 |      |
 * |xxxxxxxx|xxxxxxxx|x-------|--------|--------|------xx|xxxxxxxx|xxxxxxxx|
 */
func (b *internalAllocator) initFree(left, right int) int {
	free := 0

	for l := 0; l < b.maxLayer; l++ {
		nblk := 1 << (b.maxLayer - l)
		leftBi := BlkIndexNext(l, left)
		rightBi := BlkIndex(l, right)

		if leftBi < nblk {
			free += b.initFreePair(l, leftBi)
		}
		if rightBi > leftBi && (leftBi/2 != rightBi/2) && rightBi < nblk {
			free += b.initFreePair(l, rightBi)
		}
	}

	return free
}

// Initialize the buddy allocator, assert totalSize is the power of 2.
func (b *internalAllocator) init(totalSize int) {
	log2 := func(n int) int {
		k := 0
		for n > 1 {
			k++
			n = n >> 1
		}
		return k
	}

	// compute the number of sizes we need to manage totalSize
	b.buffer = make([]byte, totalSize)
	b.nLayers = log2(totalSize/leafSize) + 1
	if totalSize > BlkSize(b.nLayers-1) {
		b.nLayers++ // round up to the next power of 2
	}
	b.maxLayer = b.nLayers - 1
	b.bufInfo = make([]bufferInfo, b.nLayers)

	// Initialize free list and allocate the alloc array for each size l.
	// Also allocate the split array for each size l, l = 0 is not used.
	// since we will not split blocks of size l = 0, the smallest size.
	markedCount := 0
	for l := 0; l < b.nLayers; l++ {
		nblk := 1 << (b.maxLayer - l)
		sz := roundUp(nblk, 8) / 8
		b.bufInfo[l].canAllocate = b.buffer[markedCount : markedCount+sz]
		markedCount += sz
		b.bufInfo[l].alloc = b.buffer[markedCount : markedCount+sz]
		markedCount += sz
		b.bufInfo[l].split = b.buffer[markedCount : markedCount+sz]
		markedCount += sz
		b.bufInfo[l].l = l
		b.bufInfo[l].nblk = nblk
	}

	// Mark the memory in range [0, markedCount) and [totalSize, HeapSize) as allocated,
	// where HeapSize = BlkSize(maxLayer)
	unavailable := b.markUnavailable(markedCount, totalSize)
	// initialize free lists for each size k
	free := b.initFree(0, BlkSize(b.maxLayer)-unavailable)
	b.unavailable = unavailable
	b.total = BlkSize(b.maxLayer)

	// check if the amount that is free is what we expect
	if free != BlkSize(b.maxLayer)-unavailable {
		panic("Initialize allocator failed")
	}

	b.allocated = make(map[uintptr]int, totalSize/leafSize)

	b.sanityCheck()
}

func (b *internalAllocator) sanityCheck() {
	free := 0
	for _, binfo := range b.bufInfo {
		blkSize := BlkSize(binfo.l)
		for bi := 0; bi < binfo.nblk; bi++ {
			if BitIsSet(binfo.canAllocate, bi) {
				free += blkSize
			}
		}
	}

	alloc := 0
	for _, offset := range b.allocated {
		alloc += BlkSize(b.layer(offset))
	}
	if alloc != int(b.allocatedBytes.Load()) {
		panic("Sanity check failed")
	}

	if free+int(b.allocatedBytes.Load())+b.unavailable != b.total {
		panic("Sanity check failed")
	}
}

type BuddyAllocator struct {
	arenas    []*internalAllocator
	allocated map[uintptr]int
	lock      sync.Mutex

	allocatedOutside    atomic.Int64
	allocatedOutsideNum atomic.Int64
}

func (b *BuddyAllocator) Init(_ int) {
	b.allocated = make(map[uintptr]int, maxArenaCount)

	go func() {
		tick := time.NewTicker(5 * time.Second)
		for range tick.C {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)

			fmt.Printf("[BuddyAllocator] Inside the allocator: %d MiB(%d blocks), outside the allocator: %d MiB(%d blocks)\n",
				int(b.Allocated())/1024/1024, len(b.allocated),
				int(b.allocatedOutsideNum.Load()), int(b.allocatedOutside.Load())/1024/1024,
			)
		}
	}()
}

func (b *BuddyAllocator) Allocate(size int) []byte {
	b.lock.Lock()
	defer b.lock.Unlock()

	for i, arena := range b.arenas {
		buf := arena.allocateInternal(size)
		if buf != nil {
			b.allocated[UnsafeGetBlkAddr(buf)] = i
			return buf
		}
	}

	if arena := pool.get(); arena != nil {
		b.arenas = append(b.arenas, arena)
		buf := arena.allocateInternal(size)
		b.allocated[UnsafeGetBlkAddr(buf)] = len(b.arenas) - 1
		return buf
	}

	b.allocatedOutside.Add(int64(size))
	b.allocatedOutsideNum.Add(1)
	return make([]byte, size)
}

func (b *BuddyAllocator) Free(bs []byte) {
	b.lock.Lock()
	defer b.lock.Unlock()

	if bs == nil || cap(bs) == 0 {
		return
	}
	bs = bs[:1]
	addr := UnsafeGetBlkAddr(bs)
	arenaID, ok := b.allocated[addr]
	if !ok {
		return
	}

	b.arenas[arenaID].freeInternal(bs)
	delete(b.allocated, addr)
}

func (b *BuddyAllocator) Reallocate(size int, bs []byte) []byte {
	b.Free(bs)
	return b.Allocate(size)
}

func (b *BuddyAllocator) Allocated() int64 {
	b.lock.Lock()
	defer b.lock.Unlock()

	allocatedBytes := 0
	for _, arena := range b.arenas {
		allocatedBytes += int(arena.allocatedBytes.Load())
	}
	return int64(allocatedBytes)
}

// Close return the allocated memory to the pool
func (b *BuddyAllocator) Close() {
	for _, arena := range b.arenas {
		arena.sanityCheck()
		arena.freeAll()
		arena.sanityCheck()
		pool.put(arena)
	}
}
