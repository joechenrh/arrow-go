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
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	parquetencoding "github.com/apache/arrow-go/v18/parquet/internal/encoding"
	format "github.com/apache/arrow-go/v18/parquet/internal/gen-go/parquet"
	"github.com/apache/arrow-go/v18/parquet/schema"
	"github.com/stretchr/testify/require"
)

func TestStreamingDataPageV1ReadsLevelsBeforePlainValues(t *testing.T) {
	sc := schema.NewSchema(schema.MustGroup(schema.NewGroupNode("schema", parquet.Repetitions.Required, schema.FieldList{
		schema.Must(schema.NewGroupNode("items", parquet.Repetitions.Repeated, schema.FieldList{
			schema.Must(schema.NewPrimitiveNode("body", parquet.Repetitions.Optional, parquet.Types.ByteArray, -1, -1)),
		}, -1)),
	}, -1)))
	descr := sc.Column(0)
	require.Greater(t, descr.MaxRepetitionLevel(), int16(0))
	require.Greater(t, descr.MaxDefinitionLevel(), int16(0))

	repLevels := []int16{0, 1, 0, 1}
	defLevels := []int16{
		descr.MaxDefinitionLevel(),
		descr.MaxDefinitionLevel(),
		descr.MaxDefinitionLevel(),
		descr.MaxDefinitionLevel(),
	}
	values := []parquet.ByteArray{
		[]byte("alpha"),
		[]byte("beta-beta"),
		[]byte("gamma"),
		[]byte("delta"),
	}

	var body bytes.Buffer
	writeLevelDataForTest(t, &body, parquet.Encodings.BitPacked, descr.MaxRepetitionLevel(), repLevels)
	writeLevelDataForTest(t, &body, parquet.Encodings.RLE, descr.MaxDefinitionLevel(), defLevels)
	writePlainByteArrayValuesForTest(t, &body, values)

	page := &DataPageV1{
		page: page{
			typ:      format.PageType_DATA_PAGE,
			nvals:    int32(len(repLevels)),
			encoding: format.Encoding_PLAIN,
		},
		defLvlEncoding:   format.Encoding_RLE,
		repLvlEncoding:   format.Encoding_BIT_PACKED,
		uncompressedSize: int32(body.Len()),
		payload: &pagePayloadReader{
			compressed:  &io.LimitedReader{R: bytes.NewReader(body.Bytes()), N: int64(body.Len())},
			compression: compress.Codecs.Uncompressed,
		},
		mem: memory.DefaultAllocator,
	}
	defer page.Release()

	rdr := columnChunkReader{descr: descr}
	require.NoError(t, rdr.initStreamingDataPageV1(page))

	gotRep := make([]int16, len(repLevels))
	nrep, _ := rdr.repetitionDecoder.Decode(gotRep)
	require.Equal(t, len(repLevels), nrep)
	require.Equal(t, repLevels, gotRep)

	gotDef := make([]int16, len(defLevels))
	ndef, valuesToRead := rdr.definitionDecoder.Decode(gotDef)
	require.Equal(t, len(defLevels), ndef)
	require.EqualValues(t, len(values), valuesToRead)
	require.Equal(t, defLevels, gotDef)

	gotValues := make([]parquet.ByteArray, len(values))
	nvalues, err := rdr.curDecoder.(parquetencoding.ByteArrayDecoder).Decode(gotValues)
	require.NoError(t, err)
	require.Equal(t, len(values), nvalues)
	require.Equal(t, values, gotValues)
}

func writeLevelDataForTest(t *testing.T, w *bytes.Buffer, enc parquet.Encoding, maxLevel int16, levels []int16) {
	buf := parquetencoding.NewBufferWriter(parquetencoding.LevelEncodingMaxBufferSize(enc, maxLevel, len(levels)), memory.DefaultAllocator)
	defer buf.Release()

	var levelEncoder parquetencoding.LevelEncoder
	levelEncoder.Init(enc, maxLevel, buf)
	_, err := levelEncoder.Encode(levels)
	require.NoError(t, err)

	nbytes := buf.Len()
	if enc == parquet.Encodings.RLE {
		nbytes = levelEncoder.Len()
		require.NoError(t, binary.Write(w, binary.LittleEndian, int32(nbytes)))
	}
	_, err = w.Write(buf.Bytes()[:nbytes])
	require.NoError(t, err)
}

func writePlainByteArrayValuesForTest(t *testing.T, w *bytes.Buffer, values []parquet.ByteArray) {
	var lenBuf [4]byte
	for _, value := range values {
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(value)))
		_, err := w.Write(lenBuf[:])
		require.NoError(t, err)
		_, err = w.Write(value)
		require.NoError(t, err)
	}
}
