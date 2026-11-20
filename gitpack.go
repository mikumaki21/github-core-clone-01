// pkt-line codec and packfile encode/decode for the git smart-HTTP surface.
// No deltas are ever sent; deltas in a received pack (OFS_DELTA and
// REF_DELTA) are resolved, including thin-pack bases that live in the store
// rather than in the pack.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
)

const ZeroOID = "0000000000000000000000000000000000000000"

// PackObject is one object in a packfile.
type PackObject struct {
	Type GitObjectType
	Data []byte
}

var typeCode = map[GitObjectType]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]GitObjectType{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

// PktLineMax is the largest pkt-line including its 4-byte length header.
const PktLineMax = 65520

const sidebandMaxData = PktLineMax - 4 - 1

// ---------------------------------------------------------------------------------------------
// pkt-line
// ---------------------------------------------------------------------------------------------

type PktKind int

const (
	PktData PktKind = iota
	PktFlushKind
	PktDelimKind
	PktResponseEndKind
)

type Pkt struct {
	Kind    PktKind
	Payload []byte
}

// EncodePktLine: one data packet, 4 hex length bytes (payload + 4) then payload.
func EncodePktLine(payload []byte) []byte {
	if len(payload) > PktLineMax-4 {
		panic(fmt.Sprintf("pkt-line payload is %d bytes, over the maximum", len(payload)))
	}
	out := make([]byte, 4+len(payload))
	copy(out, []byte(fmt.Sprintf("%04x", len(payload)+4)))
	copy(out[4:], payload)
	return out
}

func EncodePktLineString(payload string) []byte { return EncodePktLine([]byte(payload)) }

func PktFlush() []byte       { return []byte("0000") }
func PktDelim() []byte       { return []byte("0001") }
func PktResponseEnd() []byte { return []byte("0002") }

var hex4Re = regexp.MustCompile(`^[0-9a-fA-F]{4}$`)

// DecodePackets decodes pkt-lines from a complete buffer. With stopAtFlush,
// decoding stops just after the first flush packet and consumed marks where
// it ended, so a caller can treat the remaining bytes as a raw packfile.
// Panics (recovered by the server as a 400) on malformed framing.
func DecodePackets(buf []byte, stopAtFlush bool) (packets []Pkt, consumed int) {
	off := 0
	for off < len(buf) {
		if off+4 > len(buf) {
			panic("truncated pkt-line")
		}
		head := string(buf[off : off+4])
		if !hex4Re.MatchString(head) {
			panic(fmt.Sprintf("invalid pkt-line length %q", head))
		}
		var length int
		fmt.Sscanf(head, "%x", &length)
		if length == 0 {
			packets = append(packets, Pkt{Kind: PktFlushKind})
			off += 4
			if stopAtFlush {
				break
			}
			continue
		}
		if length == 1 {
			packets = append(packets, Pkt{Kind: PktDelimKind})
			off += 4
			continue
		}
		if length == 2 {
			packets = append(packets, Pkt{Kind: PktResponseEndKind})
			off += 4
			continue
		}
		if length < 4 || length > PktLineMax {
			panic(fmt.Sprintf("invalid pkt-line length %d", length))
		}
		if off+length > len(buf) {
			panic("truncated pkt-line")
		}
		packets = append(packets, Pkt{Kind: PktData, Payload: buf[off+4 : off+length]})
		off += length
	}
	return packets, off
}

// Sideband frames bytes as sideband channel 1 (data) packets.
func Sideband(data []byte) []byte {
	if len(data) == 0 {
		return []byte{}
	}
	var out []byte
	for off := 0; off < len(data); off += sidebandMaxData {
		end := off + sidebandMaxData
		if end > len(data) {
			end = len(data)
		}
		chunk := make([]byte, 1+end-off)
		chunk[0] = 1
		copy(chunk[1:], data[off:end])
		out = append(out, EncodePktLine(chunk)...)
	}
	return out
}

// ---------------------------------------------------------------------------------------------
// packfile write (plain objects only, no deltas)
// ---------------------------------------------------------------------------------------------

func packObjectHeader(typeCode int, size int) []byte {
	var bytes_ []byte
	remaining := size
	first := (typeCode << 4) | (remaining & 0x0f)
	remaining = remaining / 16
	for remaining > 0 {
		bytes_ = append(bytes_, byte(first|0x80))
		first = remaining & 0x7f
		remaining = remaining / 128
	}
	bytes_ = append(bytes_, byte(first))
	return bytes_
}

// WritePack builds a packfile from plain objects. The trailer is the SHA-1 of all preceding bytes.
func WritePack(objects []PackObject) []byte {
	var parts [][]byte
	header := make([]byte, 12)
	copy(header, "PACK")
	header[4] = 0
	header[5] = 0
	header[6] = 0
	header[7] = 2
	n := len(objects)
	header[8] = byte(n >> 24)
	header[9] = byte(n >> 16)
	header[10] = byte(n >> 8)
	header[11] = byte(n)
	parts = append(parts, header)
	for _, object := range objects {
		parts = append(parts, packObjectHeader(typeCode[object.Type], len(object.Data)))
		var zbuf bytes.Buffer
		zw := zlib.NewWriter(&zbuf)
		zw.Write(object.Data)
		zw.Close()
		parts = append(parts, zbuf.Bytes())
	}
	body := bytes.Join(parts, nil)
	sum := sha1.Sum(body)
	return append(body, sum[:]...)
}

// ---------------------------------------------------------------------------------------------
// delta application
// ---------------------------------------------------------------------------------------------

// ApplyDelta applies a git delta (copy/insert instruction stream) to a base object's bytes.
func ApplyDelta(base, delta []byte) []byte {
	offset := 0
	readVarint := func() int {
		value := 0
		shift := uint(0)
		for {
			if offset >= len(delta) {
				panic("delta size varint is truncated")
			}
			b := delta[offset]
			offset++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
			if shift > 63 {
				panic("delta size varint is too long")
			}
		}
		return value
	}
	sourceSize := readVarint()
	targetSize := readVarint()
	if sourceSize != len(base) {
		panic(fmt.Sprintf("delta expects a %d-byte base, got %d", sourceSize, len(base)))
	}
	out := make([]byte, targetSize)
	written := 0
	for offset < len(delta) {
		op := delta[offset]
		offset++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					if offset >= len(delta) {
						panic("delta copy offset is truncated")
					}
					copyOffset += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					if offset >= len(delta) {
						panic("delta copy size is truncated")
					}
					copySize += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > len(base) {
				panic("delta copy is out of the base")
			}
			if written+copySize > targetSize {
				panic("delta writes past its target size")
			}
			copy(out[written:written+copySize], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := int(op & 0x7f)
			if offset+literal > len(delta) {
				panic("delta insert is truncated")
			}
			if written+literal > targetSize {
				panic("delta writes past its target size")
			}
			copy(out[written:written+literal], delta[offset:offset+literal])
			offset += literal
			written += literal
		} else {
			panic("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		panic(fmt.Sprintf("delta produced %d of %d bytes", written, targetSize))
	}
	return out
}

// ---------------------------------------------------------------------------------------------
// packfile read
// ---------------------------------------------------------------------------------------------

type ReadPackResult struct {
	Version       int
	DeclaredCount int
	Objects       map[string]PackObject
	DeltaCount    int
}

// inflateAt inflates one zlib stream at offset; returns its bytes and the offset just past the stream.
func inflateAt(buffer []byte, offset int) (data []byte, next int) {
	br := bytes.NewReader(buffer[offset:])
	zr, err := zlib.NewReader(br)
	if err != nil {
		panic(fmt.Sprintf("zlib stream at offset %d did not inflate: %s", offset, err))
	}
	data, err = io.ReadAll(zr)
	zr.Close()
	if err != nil {
		panic(fmt.Sprintf("zlib stream at offset %d did not inflate: %s", offset, err))
	}
	consumed := len(buffer[offset:]) - br.Len()
	if consumed <= 0 {
		panic(fmt.Sprintf("zlib stream at offset %d consumed no bytes", offset))
	}
	return data, offset + consumed
}

func readPackSize(buffer []byte, start int) (size int, next int, first int) {
	if start >= len(buffer) {
		panic("pack entry has no size header")
	}
	b := buffer[start]
	first = int(b)
	size = int(b & 0x0f)
	shift := uint(4)
	offset := start + 1
	for b&0x80 != 0 {
		if offset >= len(buffer) {
			panic("pack entry size varint is truncated")
		}
		b = buffer[offset]
		offset++
		size += int(b&0x7f) << shift
		shift += 7
		if shift > 63 {
			panic("pack entry size varint is too long")
		}
	}
	return size, offset, first
}

func readOfsBase(buffer []byte, start int) (baseOffset int, next int) {
	if start >= len(buffer) {
		panic("ofs-delta has no base offset")
	}
	b := buffer[start]
	offset := start + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if offset >= len(buffer) {
			panic("ofs-delta base offset is truncated")
		}
		b = buffer[offset]
		offset++
		value = ((value + 1) * 128) + int(b&0x7f)
	}
	return value, offset
}

func HashPackObject(typ GitObjectType, data []byte) string {
	return HashObject(typ, data)
}

// ReadPack reads a packfile (version 2 or 3) and resolves every object,
// including OFS_DELTA and REF_DELTA. A REF_DELTA base may come from the pack
// itself or from resolveBase (a thin pack's base, already held by the server).
// Every object's SHA-1 is recomputed from its resolved bytes. Panics on any
// corruption, including a bad trailer checksum.
func ReadPack(pack []byte, resolveBase func(oid string) *PackObject) ReadPackResult {
	if len(pack) < 32 {
		panic(fmt.Sprintf("pack is %d bytes, too short", len(pack)))
	}
	if string(pack[0:4]) != "PACK" {
		panic("pack does not start with PACK")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		panic(fmt.Sprintf("unsupported pack version %d", version))
	}
	declaredCount := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	trailer := hex.EncodeToString(pack[trailerStart:])
	sum := sha1.Sum(pack[:trailerStart])
	if trailer != hex.EncodeToString(sum[:]) {
		panic("pack trailer checksum mismatch")
	}

	type raw struct {
		offset       int
		typeCode     int
		size         int
		data         []byte
		baseOffset   int
		baseOID      string
		resolvedType GitObjectType
		resolved     []byte
		oid          string
		hasResolved  bool
	}

	var rawEntries []*raw
	byOffset := map[int]*raw{}
	offset := 12
	for index := 0; index < declaredCount; index++ {
		entryOffset := offset
		size, next, first := readPackSize(pack, offset)
		offset = next
		tc := (first >> 4) & 0x07
		entry := &raw{offset: entryOffset, typeCode: tc, size: size}
		if tc == 6 {
			base, next2 := readOfsBase(pack, offset)
			offset = next2
			if base <= 0 || base > entryOffset {
				panic(fmt.Sprintf("ofs-delta at %d names an invalid base offset %d", entryOffset, base))
			}
			entry.baseOffset = entryOffset - base
		} else if tc == 7 {
			if offset+20 > trailerStart {
				panic("ref-delta base id is truncated")
			}
			entry.baseOID = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		} else if _, ok := codeType[tc]; !ok {
			panic(fmt.Sprintf("pack entry %d has unknown type code %d", index, tc))
		}
		inflated, nextOff := inflateAt(pack[:trailerStart], offset)
		if len(inflated) != size {
			panic(fmt.Sprintf("pack entry at %d declares %d bytes but inflated to %d", entryOffset, size, len(inflated)))
		}
		entry.data = inflated
		offset = nextOff
		rawEntries = append(rawEntries, entry)
		byOffset[entryOffset] = entry
	}
	if offset != trailerStart {
		panic(fmt.Sprintf("pack has %d stray bytes before its trailer", trailerStart-offset))
	}

	objects := map[string]PackObject{}
	resolvedByOID := map[string]*raw{}
	resolving := map[int]bool{}

	var resolve func(entry *raw) (GitObjectType, []byte, string)
	resolve = func(entry *raw) (GitObjectType, []byte, string) {
		if entry.hasResolved {
			return entry.resolvedType, entry.resolved, entry.oid
		}
		if resolving[entry.offset] {
			panic(fmt.Sprintf("delta cycle at pack offset %d", entry.offset))
		}
		resolving[entry.offset] = true
		var typ GitObjectType
		var data []byte
		if entry.typeCode == 6 {
			baseEntry, ok := byOffset[entry.baseOffset]
			if !ok {
				panic(fmt.Sprintf("ofs-delta base at offset %d is not in the pack", entry.baseOffset))
			}
			baseType, baseData, _ := resolve(baseEntry)
			typ = baseType
			data = ApplyDelta(baseData, entry.data)
		} else if entry.typeCode == 7 {
			wanted := entry.baseOID
			if baseEntry, ok := resolvedByOID[wanted]; ok {
				baseType, baseData, _ := resolve(baseEntry)
				typ = baseType
				data = ApplyDelta(baseData, entry.data)
			} else {
				var external *PackObject
				if resolveBase != nil {
					external = resolveBase(wanted)
				}
				if external == nil {
					panic(fmt.Sprintf("ref-delta base %s is neither in the pack nor held by the server", wanted))
				}
				typ = external.Type
				data = ApplyDelta(external.Data, entry.data)
			}
		} else {
			typ = codeType[entry.typeCode]
			data = entry.data
		}
		oid := HashPackObject(typ, data)
		entry.resolved = data
		entry.resolvedType = typ
		entry.oid = oid
		entry.hasResolved = true
		resolvedByOID[oid] = entry
		delete(resolving, entry.offset)
		return typ, data, oid
	}

	deltaCount := 0
	for _, entry := range rawEntries {
		typ, data, oid := resolve(entry)
		if entry.typeCode == 6 || entry.typeCode == 7 {
			deltaCount++
		}
		if _, ok := objects[oid]; !ok {
			objects[oid] = PackObject{Type: typ, Data: data}
		}
	}
	return ReadPackResult{Version: version, DeclaredCount: declaredCount, Objects: objects, DeltaCount: deltaCount}
}
