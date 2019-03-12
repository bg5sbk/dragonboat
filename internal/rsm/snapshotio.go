// Copyright 2017-2019 Lei Ni (nilei81@gmail.com)
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rsm

import (
	"bytes"
	"encoding/binary"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/lni/dragonboat/internal/settings"
	"github.com/lni/dragonboat/internal/utils/fileutil"
	pb "github.com/lni/dragonboat/raftpb"
)

type SnapshotVersion uint64

const (
	v1SnapshotVersion SnapshotVersion = 1
	v2SnapshotVersion SnapshotVersion = 2
	// current snapshot binary format version.
	currentSnapshotVersion SnapshotVersion = v1SnapshotVersion
	// SnapshotHeaderSize is the size of snapshot in number of bytes.
	SnapshotHeaderSize = settings.SnapshotHeaderSize
	// which checksum type to use.
	// CRC32IEEE and google's highway hash are supported
	defaultChecksumType = pb.CRC32IEEE
)

func newCRC32Hash() hash.Hash {
	return crc32.NewIEEE()
}

func getChecksum(t pb.ChecksumType) hash.Hash {
	if t == pb.CRC32IEEE {
		return newCRC32Hash()
	} else if t == pb.HIGHWAY {
		panic("highway hash not supported yet")
	} else {
		plog.Panicf("not supported checksum type %d", t)
	}
	panic("not suppose to reach here")
}

func getChecksumType() pb.ChecksumType {
	return defaultChecksumType
}

func getDefaultChecksum() hash.Hash {
	return getChecksum(getChecksumType())
}

func getVersionWriter(w io.Writer, v SnapshotVersion) IVWriter {
	if v == v1SnapshotVersion {
		return newV1Wrtier(w)
	} else if v == v2SnapshotVersion {
		return newV2Writer(w)
	} else {
		panic("unsupported v")
	}
}

func getVersionReader(r io.Reader, v SnapshotVersion) IVReader {
	if v == v1SnapshotVersion {
		return newV1Reader(r)
	} else if v == v2SnapshotVersion {
		return newV2Reader(r)
	} else {
		panic("unsupported v")
	}
}

func getVersionValidator(header pb.SnapshotHeader) IVValidator {
	v := (SnapshotVersion)(header.Version)
	if v == v1SnapshotVersion {
		return newV1Validator(header)
	} else if v == v2SnapshotVersion {
		h := getChecksum(header.ChecksumType)
		return newV2Validator(h)
	} else {
		panic("unsupported v")
	}
}

// SnapshotWriter is an io.Writer used to write snapshot file.
type SnapshotWriter struct {
	vw   IVWriter
	file *os.File
	fp   string
}

// NewSnapshotWriter creates a new snapshot writer instance.
func NewSnapshotWriter(fp string,
	version SnapshotVersion) (*SnapshotWriter, error) {
	f, err := os.OpenFile(fp,
		os.O_RDWR|os.O_CREATE|os.O_TRUNC, fileutil.DefaultFileMode)
	if err != nil {
		return nil, err
	}
	dummy := make([]byte, SnapshotHeaderSize)
	for i := uint64(0); i < SnapshotHeaderSize; i++ {
		dummy[i] = 0
	}
	if _, err := f.Write(dummy); err != nil {
		return nil, err
	}
	sw := &SnapshotWriter{
		vw:   getVersionWriter(f, version),
		file: f,
		fp:   fp,
	}
	return sw, nil
}

// Close closes the snapshot writer instance.
func (sw *SnapshotWriter) Close() error {
	if err := sw.file.Sync(); err != nil {
		return err
	}
	if err := fileutil.SyncDir(filepath.Dir(sw.fp)); err != nil {
		return err
	}
	return sw.file.Close()
}

// Write writes the specified data to the snapshot.
func (sw *SnapshotWriter) Write(data []byte) (int, error) {
	return sw.vw.Write(data)
}

func (sw *SnapshotWriter) Flush() error {
	return sw.vw.Flush()
}

// SaveHeader saves the snapshot header to the snapshot.
func (sw *SnapshotWriter) SaveHeader(smsz uint64, sz uint64) error {
	sh := pb.SnapshotHeader{
		SessionSize:     smsz,
		DataStoreSize:   sz,
		UnreliableTime:  uint64(time.Now().UnixNano()),
		PayloadChecksum: sw.vw.GetPayloadSum(),
		ChecksumType:    getChecksumType(),
		Version:         uint64(sw.vw.GetVersion()),
	}
	data, err := sh.Marshal()
	if err != nil {
		panic(err)
	}
	headerHash := getDefaultChecksum()
	if _, err := headerHash.Write(data); err != nil {
		panic(err)
	}
	headerChecksum := headerHash.Sum(nil)
	sh.HeaderChecksum = headerChecksum
	data, err = sh.Marshal()
	if err != nil {
		panic(err)
	}
	if uint64(len(data)) > SnapshotHeaderSize-8 {
		panic("snapshot header is too large")
	}
	if _, err = sw.file.Seek(0, 0); err != nil {
		panic(err)
	}
	lenbuf := make([]byte, 8)
	binary.LittleEndian.PutUint64(lenbuf, uint64(len(data)))
	if _, err := sw.file.Write(lenbuf); err != nil {
		return err
	}
	if _, err := sw.file.Write(data); err != nil {
		return err
	}
	return nil
}

// SnapshotReader is an io.Reader for reading from snapshot files.
type SnapshotReader struct {
	r    IVReader
	file *os.File
}

// NewSnapshotReader creates a new snapshot reader instance.
func NewSnapshotReader(fp string) (*SnapshotReader, error) {
	f, err := os.OpenFile(fp, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	return &SnapshotReader{file: f}, nil
}

// Close closes the snapshot reader instance.
func (sr *SnapshotReader) Close() error {
	return sr.file.Close()
}

// GetHeader returns the snapshot header instance.
func (sr *SnapshotReader) GetHeader() (pb.SnapshotHeader, error) {
	empty := pb.SnapshotHeader{}
	lenbuf := make([]byte, 8)
	n, err := io.ReadFull(sr.file, lenbuf)
	if err != nil {
		return empty, err
	}
	if n != len(lenbuf) {
		return empty, io.ErrUnexpectedEOF
	}
	sz := binary.LittleEndian.Uint64(lenbuf)
	if sz > SnapshotHeaderSize-8 {
		panic("invalid snapshot header size")
	}
	data := make([]byte, sz)
	n, err = io.ReadFull(sr.file, data)
	if err != nil {
		return empty, err
	}
	if n != len(data) {
		return empty, io.ErrUnexpectedEOF
	}
	r := pb.SnapshotHeader{}
	if err := r.Unmarshal(data); err != nil {
		panic(err)
	}
	offset, err := sr.file.Seek(int64(SnapshotHeaderSize), 0)
	if err != nil {
		return empty, err
	}
	if uint64(offset) != SnapshotHeaderSize {
		return empty, io.ErrUnexpectedEOF
	}
	var reader io.Reader = sr.file
	if (SnapshotVersion)(r.Version) == v2SnapshotVersion {
		st, err := sr.file.Stat()
		if err != nil {
			return empty, err
		}
		fileSz := st.Size()
		payloadSz := fileSz - int64(SnapshotHeaderSize) - 16
		reader = io.LimitReader(reader, payloadSz)
	}
	sr.r = getVersionReader(reader, (SnapshotVersion)(r.Version))
	return r, nil
}

// Read reads up to len(data) bytes from the snapshot file.
func (sr *SnapshotReader) Read(data []byte) (int, error) {
	if sr.r == nil {
		panic("Read called before GetHeader")
	}
	return sr.r.Read(data)
}

// ValidatePayload validates whether the snapshot content matches the checksum
// recorded in the header.
func (sr *SnapshotReader) ValidatePayload(header pb.SnapshotHeader) {
	checksum := sr.r.Sum()
	if !bytes.Equal(checksum, header.PayloadChecksum) {
		panic("corrupted snapshot payload")
	}
}

// ValidateHeader validates whether the header matches the header checksum
// recorded in the header.
func (sr *SnapshotReader) ValidateHeader(header pb.SnapshotHeader) {
	checksum := header.HeaderChecksum
	header.HeaderChecksum = nil
	data, err := header.Marshal()
	if err != nil {
		panic(err)
	}
	headerHash := getChecksum(header.ChecksumType)
	if _, err := headerHash.Write(data); err != nil {
		panic(err)
	}
	headerChecksum := headerHash.Sum(nil)
	if !bytes.Equal(headerChecksum, checksum) {
		panic("corrupted snapshot header")
	}
}

// SnapshotValidator is the validator used to check incoming snapshot chunks.
type SnapshotValidator struct {
	v IVValidator
}

// NewSnapshotValidator creates and returns a new SnapshotValidator instance.
func NewSnapshotValidator() *SnapshotValidator {
	return &SnapshotValidator{}
}

// AddChunk adds a new snapshot chunk to the validator.
func (v *SnapshotValidator) AddChunk(data []byte, chunkID uint64) bool {
	if chunkID == 0 {
		header, ok := getHeaderFromFirstChunk(data)
		if !ok {
			return false
		}
		if v.v != nil {
			return false
		}
		v.v = getVersionValidator(header)
	} else {
		if v.v == nil {
			return false
		}
	}
	return v.v.AddChunk(data, chunkID)
}

// Validate validates the added chunks and return a boolean flag indicating
// whether the snapshot chunks are valid.
func (v *SnapshotValidator) Validate() bool {
	if v.v == nil {
		return false
	}
	return v.v.Validate()
}
