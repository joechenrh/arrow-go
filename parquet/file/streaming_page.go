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
	"io"

	"github.com/apache/arrow-go/v18/parquet/compress"
)

type pagePayloadReader struct {
	compressed  *io.LimitedReader
	compression compress.Compression
	codec       compress.Codec

	openCloser io.Closer
}

func (p *pagePayloadReader) uncompressedReader() io.Reader {
	if p.compression == compress.Codecs.Uncompressed {
		return p.compressed
	}

	streamingCodec := p.codec.(compress.StreamingCodec)
	rdr := streamingCodec.NewReader(p.compressed)
	p.openCloser = rdr
	return rdr
}

func (p *pagePayloadReader) close() {
	if p.openCloser != nil {
		_ = p.openCloser.Close()
		p.openCloser = nil
	}
	if p.compressed != nil {
		_, _ = io.Copy(io.Discard, p.compressed)
	}
}
