package storage

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Record checksums (recordKindChecksummed).
//
// Kula does not fsync every write, so after a power loss or kernel crash the
// tier header can reach the disk while part of a record does not: a page of it
// then reads back as zeros, or as the bytes an older ring pass left there. The
// positional codec cannot tell such a record from a real one when the damage
// misses the bytes it validates, and restart recovery replays exactly these
// last records into the rollup tiers. A CRC-32 trailer makes the damage
// detectable, and readers skip the record like any other undecodable one.
//
// Layout of a checksummed record, after its 4-byte length prefix:
//
//	[0x03][encoded sample][crc32 uint32 LE]
//
// The CRC-32 is IEEE, the polynomial Python's zlib.crc32 computes too (the
// tier inspector verifies it), over the length prefix followed by every
// payload byte before the trailer. Covering the prefix ties the trailer to
// the record's extent. Records written before checksums (recordKindBinary,
// legacy JSON and legacy binary) still decode, unverified.

// recordChecksumSize is the size of the CRC-32 trailer.
const recordChecksumSize = 4

// errRecordChecksum reports a checksummed record whose bytes do not match
// its trailer: torn by an unclean shutdown, or otherwise corrupt.
var errRecordChecksum = errors.New("record checksum mismatch")

// recordChecksum returns the CRC-32 of a checksummed payload (no length
// prefix, trailer included): its length as the prefix encodes it, then every
// payload byte before the trailer.
func recordChecksum(payload []byte) uint32 {
	var prefix [4]byte
	binary.LittleEndian.PutUint32(prefix[:], uint32(len(payload)))
	crc := crc32.ChecksumIEEE(prefix[:])
	return crc32.Update(crc, crc32.IEEETable, payload[:len(payload)-recordChecksumSize])
}

// sealRecord writes the trailer of a checksummed payload whose last
// recordChecksumSize bytes are reserved for it.
func sealRecord(payload []byte) {
	binary.LittleEndian.PutUint32(payload[len(payload)-recordChecksumSize:], recordChecksum(payload))
}

// checkedRecordBody verifies a checksummed payload and returns the encoded
// sample between its kind byte and its trailer.
func checkedRecordBody(payload []byte) ([]byte, error) {
	if len(payload) < 1+recordChecksumSize {
		return nil, fmt.Errorf("checksummed record too short: %d bytes", len(payload))
	}
	end := len(payload) - recordChecksumSize
	if binary.LittleEndian.Uint32(payload[end:]) != recordChecksum(payload) {
		return nil, errRecordChecksum
	}
	return payload[1:end], nil
}

// peekedRecordIntact reports whether the checksummed record at br's position,
// recordLen bytes including its length prefix, matches its trailer. A record
// larger than br's buffer cannot be peeked whole; it passes on its header, as
// every record did before checksums.
func peekedRecordIntact(br *bufio.Reader, recordLen int64) bool {
	if recordLen > int64(br.Size()) {
		return true
	}
	record, err := br.Peek(int(recordLen))
	if err != nil {
		return false
	}
	_, err = checkedRecordBody(record[4:])
	return err == nil
}
