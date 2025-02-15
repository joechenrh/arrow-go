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

package memory

const (
	alignment = 64
)

type BufferType string

const (
	// BufferCompressed means the buffer is used to store compressed data.
	BufferCompressed BufferType = "compressed"
	// BufferDictionary means the buffer is used to store uncompressed dictionary page.
	BufferDictionary BufferType = "dictpage"
	// BufferDataPage means the buffer is used to store uncompressed data page.
	BufferDataPage BufferType = "datapage"
	// BufferOthers means the buffer is used to for other usage, like byte reader.
	BufferOthers BufferType = "others"
)

type Allocator interface {
	// Allocate a buffer with given type.
	Allocate(size int, tp BufferType) []byte
	// Reallocate a buffer with given type.
	Reallocate(size int, b []byte, tp BufferType) []byte
	Free(b []byte)
}
