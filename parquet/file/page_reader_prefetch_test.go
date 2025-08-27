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

package file

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/internal/utils"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/stretchr/testify/assert"
)

// MockWorkerPool implements the WorkerPool interface for testing
type MockWorkerPool struct {
	tasks   chan func()
	workers int
	wg      sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewMockWorkerPool(workers int) *MockWorkerPool {
	ctx, cancel := context.WithCancel(context.Background())
	pool := &MockWorkerPool{
		tasks:   make(chan func(), 100),
		workers: workers,
		ctx:     ctx,
		cancel:  cancel,
	}
	
	// Start worker goroutines
	for i := 0; i < workers; i++ {
		pool.wg.Add(1)
		go pool.worker()
	}
	
	return pool
}

func (p *MockWorkerPool) worker() {
	defer p.wg.Done()
	for {
		select {
		case task := <-p.tasks:
			if task != nil {
				task()
			}
		case <-p.ctx.Done():
			return
		}
	}
}

func (p *MockWorkerPool) Submit(ctx context.Context, task func()) {
	select {
	case p.tasks <- task:
	case <-ctx.Done():
	case <-p.ctx.Done():
	}
}

func (p *MockWorkerPool) Close() {
	p.cancel()
	close(p.tasks)
	p.wg.Wait()
}

func TestSerializedPageReaderPrefetchInterface(t *testing.T) {
	mem := memory.NewCheckedAllocator(memory.DefaultAllocator)
	defer mem.AssertSize(t, 0)

	reader := &serializedPageReader{
		maxPageHeaderSize: defaultMaxPageHeaderSize,
		nrows:             0,
		mem:               mem,
	}

	// Initialize buffers without calling init method to avoid metadata requirements
	reader.decompressBuffer = memory.NewResizableBuffer(mem)
	reader.dataPageBuffer = memory.NewResizableBuffer(mem)
	reader.dictPageBuffer = memory.NewResizableBuffer(mem)
	
	defer reader.Close()

	// Test that SetWorkerPool works correctly
	t.Run("set_worker_pool", func(t *testing.T) {
		pool := NewMockWorkerPool(2)
		defer pool.Close()
		
		// Initially prefetch should be disabled
		assert.False(t, reader.prefetchEnabled)
		assert.Nil(t, reader.workerPool)
		assert.Nil(t, reader.prefetched)
		
		// Enable prefetch
		reader.SetWorkerPool(pool)
		assert.True(t, reader.prefetchEnabled)
		assert.NotNil(t, reader.workerPool)
		assert.NotNil(t, reader.prefetched)
		assert.NotNil(t, reader.prefetchCtx)
		assert.NotNil(t, reader.prefetchCancel)
		
		// Disable prefetch
		reader.SetWorkerPool(nil)
		assert.False(t, reader.prefetchEnabled)
		assert.Nil(t, reader.workerPool)
	})

	// Test prefetch data structure
	t.Run("prefetch_data", func(t *testing.T) {
		data := &prefetchData{
			valid: true,
		}
		assert.True(t, data.valid)
		assert.Nil(t, data.err)
		assert.Nil(t, data.header)
		assert.Nil(t, data.compressedData)
	})
}

func TestSerializedPageReaderPrefetchReset(t *testing.T) {
	mem := memory.NewCheckedAllocator(memory.DefaultAllocator)
	defer mem.AssertSize(t, 0)

	reader := &serializedPageReader{
		maxPageHeaderSize: defaultMaxPageHeaderSize,
		nrows:             0,
		mem:               mem,
	}

	// Initialize buffers without calling init method
	reader.decompressBuffer = memory.NewResizableBuffer(mem)
	reader.dataPageBuffer = memory.NewResizableBuffer(mem)
	reader.dictPageBuffer = memory.NewResizableBuffer(mem)
	
	defer reader.Close()

	pool := NewMockWorkerPool(1)
	defer pool.Close()
	
	// Enable prefetch
	reader.SetWorkerPool(pool)
	
	// Create a mock reader for reset
	mockReader := utils.NewBufferedReader(&mockReaderSeeker{}, 1024)
	
	// Reset should handle prefetch cleanup properly
	reader.Reset(mockReader, 0, compress.Codecs.Uncompressed, nil)
	
	// Prefetch should still be enabled after reset
	assert.True(t, reader.prefetchEnabled)
	assert.NotNil(t, reader.prefetched)
}

// mockReaderSeeker is a simple mock implementation
type mockReaderSeeker struct {
	data []byte
	pos  int
}

func (m *mockReaderSeeker) Read(p []byte) (n int, err error) {
	if m.pos >= len(m.data) {
		return 0, io.EOF
	}
	n = copy(p, m.data[m.pos:])
	m.pos += n
	return n, nil
}

func (m *mockReaderSeeker) ReadAt(p []byte, off int64) (n int, err error) {
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n = copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *mockReaderSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0: // relative to start
		m.pos = int(offset)
	case 1: // relative to current
		m.pos += int(offset)
	case 2: // relative to end
		m.pos = len(m.data) + int(offset)
	}
	return int64(m.pos), nil
}

func (m *mockReaderSeeker) Close() error {
	return nil
}