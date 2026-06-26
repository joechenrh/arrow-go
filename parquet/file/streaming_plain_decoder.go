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
	"encoding/binary"
	"errors"
	"io"
	"math"

	"github.com/apache/arrow-go/v18/internal/bitutils"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/internal/encoding"
)

type streamingPlainDecoder[T parquet.ColumnTypes] struct {
	r     io.Reader
	nvals int
	typ   parquet.Type
}

func (d *streamingPlainDecoder[T]) SetData(nvals int, _ []byte) error {
	d.nvals = nvals
	return nil
}

func (d *streamingPlainDecoder[T]) Encoding() parquet.Encoding { return parquet.Encodings.Plain }
func (d *streamingPlainDecoder[T]) ValuesLeft() int            { return d.nvals }
func (d *streamingPlainDecoder[T]) Type() parquet.Type         { return d.typ }

func (d *streamingPlainDecoder[T]) Discard(n int) (int, error) {
	n = min(n, d.nvals)
	nbytes := plainFixedWidthBytes[T](n)
	if _, err := io.CopyN(io.Discard, d.r, int64(nbytes)); err != nil {
		return 0, err
	}
	d.nvals -= n
	return n, nil
}

func (d *streamingPlainDecoder[T]) Decode(out []T) (int, error) {
	max := min(len(out), d.nvals)
	nbytes := plainFixedWidthBytes[T](max)
	buf := make([]byte, nbytes)
	if _, err := io.ReadFull(d.r, buf); err != nil {
		return 0, err
	}

	switch values := any(out[:max]).(type) {
	case []int32:
		for i := range values {
			values[i] = int32(binary.LittleEndian.Uint32(buf[i*4:]))
		}
	case []int64:
		for i := range values {
			values[i] = int64(binary.LittleEndian.Uint64(buf[i*8:]))
		}
	case []parquet.Int96:
		for i := range values {
			copy(values[i][:], buf[i*12:i*12+12])
		}
	case []float32:
		for i := range values {
			values[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
		}
	case []float64:
		for i := range values {
			values[i] = math.Float64frombits(binary.LittleEndian.Uint64(buf[i*8:]))
		}
	default:
		return 0, errors.New("parquet: unsupported streaming plain type")
	}

	d.nvals -= max
	return max, nil
}

func (d *streamingPlainDecoder[T]) DecodeSpaced(out []T, nullCount int, validBits []byte, validBitsOffset int64) (int, error) {
	toRead := len(out) - nullCount
	values, err := d.Decode(out[:toRead])
	if err != nil {
		return values, err
	}
	if values != toRead {
		return values, errors.New("parquet: number of values / definition levels read did not match")
	}
	return expandSpaced(out, nullCount, validBits, validBitsOffset), nil
}

type streamingPlainByteArrayDecoder struct {
	r     io.Reader
	nvals int
	buf   []byte
}

func (d *streamingPlainByteArrayDecoder) SetData(nvals int, _ []byte) error {
	d.nvals = nvals
	return nil
}

func (d *streamingPlainByteArrayDecoder) Encoding() parquet.Encoding { return parquet.Encodings.Plain }
func (d *streamingPlainByteArrayDecoder) ValuesLeft() int            { return d.nvals }
func (d *streamingPlainByteArrayDecoder) Type() parquet.Type         { return parquet.Types.ByteArray }

func (d *streamingPlainByteArrayDecoder) Discard(n int) (int, error) {
	n = min(n, d.nvals)
	var lenBuf [4]byte
	for i := 0; i < n; i++ {
		if _, err := io.ReadFull(d.r, lenBuf[:]); err != nil {
			return i, err
		}
		byteLen := int32(binary.LittleEndian.Uint32(lenBuf[:]))
		if byteLen < 0 {
			return i, errors.New("parquet: invalid BYTE_ARRAY value")
		}
		if _, err := io.CopyN(io.Discard, d.r, int64(byteLen)); err != nil {
			return i, err
		}
	}
	d.nvals -= n
	return n, nil
}

func (d *streamingPlainByteArrayDecoder) Decode(out []parquet.ByteArray) (int, error) {
	max := min(len(out), d.nvals)
	d.buf = d.buf[:0]

	var lenBuf [4]byte
	for i := 0; i < max; i++ {
		if _, err := io.ReadFull(d.r, lenBuf[:]); err != nil {
			return i, err
		}
		byteLen := int32(binary.LittleEndian.Uint32(lenBuf[:]))
		if byteLen < 0 {
			return i, errors.New("parquet: invalid BYTE_ARRAY value")
		}

		start := len(d.buf)
		d.buf = append(d.buf, make([]byte, int(byteLen))...)
		if _, err := io.ReadFull(d.r, d.buf[start:]); err != nil {
			return i, err
		}
		out[i] = d.buf[start:len(d.buf):len(d.buf)]
	}

	d.nvals -= max
	return max, nil
}

func (d *streamingPlainByteArrayDecoder) DecodeSpaced(out []parquet.ByteArray, nullCount int, validBits []byte, validBitsOffset int64) (int, error) {
	toRead := len(out) - nullCount
	values, err := d.Decode(out[:toRead])
	if err != nil {
		return values, err
	}
	if values != toRead {
		return values, errors.New("parquet: number of values / definition levels read did not match")
	}
	return expandSpaced(out, nullCount, validBits, validBitsOffset), nil
}

type streamingPlainFixedLenByteArrayDecoder struct {
	r       io.Reader
	nvals   int
	typeLen int
	buf     []byte
}

func (d *streamingPlainFixedLenByteArrayDecoder) SetData(nvals int, _ []byte) error {
	d.nvals = nvals
	return nil
}

func (d *streamingPlainFixedLenByteArrayDecoder) Encoding() parquet.Encoding {
	return parquet.Encodings.Plain
}
func (d *streamingPlainFixedLenByteArrayDecoder) ValuesLeft() int { return d.nvals }
func (d *streamingPlainFixedLenByteArrayDecoder) Type() parquet.Type {
	return parquet.Types.FixedLenByteArray
}

func (d *streamingPlainFixedLenByteArrayDecoder) Discard(n int) (int, error) {
	n = min(n, d.nvals)
	if _, err := io.CopyN(io.Discard, d.r, int64(n*d.typeLen)); err != nil {
		return 0, err
	}
	d.nvals -= n
	return n, nil
}

func (d *streamingPlainFixedLenByteArrayDecoder) Decode(out []parquet.FixedLenByteArray) (int, error) {
	max := min(len(out), d.nvals)
	d.buf = d.buf[:0]
	d.buf = append(d.buf, make([]byte, max*d.typeLen)...)
	if _, err := io.ReadFull(d.r, d.buf); err != nil {
		return 0, err
	}
	for i := 0; i < max; i++ {
		start := i * d.typeLen
		out[i] = d.buf[start : start+d.typeLen : start+d.typeLen]
	}
	d.nvals -= max
	return max, nil
}

func (d *streamingPlainFixedLenByteArrayDecoder) DecodeSpaced(out []parquet.FixedLenByteArray, nullCount int, validBits []byte, validBitsOffset int64) (int, error) {
	toRead := len(out) - nullCount
	values, err := d.Decode(out[:toRead])
	if err != nil {
		return values, err
	}
	if values != toRead {
		return values, errors.New("parquet: number of values / definition levels read did not match")
	}
	return expandSpaced(out, nullCount, validBits, validBitsOffset), nil
}

func plainFixedWidthBytes[T parquet.ColumnTypes](n int) int {
	var zero T
	switch any(zero).(type) {
	case int32, float32:
		return n * 4
	case int64, float64:
		return n * 8
	case parquet.Int96:
		return n * 12
	default:
		return 0
	}
}

func expandSpaced[T parquet.ColumnTypes](buffer []T, nullCount int, validBits []byte, validBitsOffset int64) int {
	numValues := len(buffer)
	idxDecode := int64(numValues - nullCount)
	if idxDecode == 0 {
		return numValues
	}

	rdr := bitutils.NewReverseSetBitRunReader(validBits, validBitsOffset, int64(numValues))
	for {
		run := rdr.NextRun()
		if run.Length == 0 {
			break
		}

		idxDecode -= run.Length
		copy(buffer[run.Pos:], buffer[idxDecode:int64(idxDecode)+run.Length])
	}

	return numValues
}

var (
	_ encoding.Int32Decoder             = (*streamingPlainDecoder[int32])(nil)
	_ encoding.Int64Decoder             = (*streamingPlainDecoder[int64])(nil)
	_ encoding.Int96Decoder             = (*streamingPlainDecoder[parquet.Int96])(nil)
	_ encoding.Float32Decoder           = (*streamingPlainDecoder[float32])(nil)
	_ encoding.Float64Decoder           = (*streamingPlainDecoder[float64])(nil)
	_ encoding.ByteArrayDecoder         = (*streamingPlainByteArrayDecoder)(nil)
	_ encoding.FixedLenByteArrayDecoder = (*streamingPlainFixedLenByteArrayDecoder)(nil)
)
