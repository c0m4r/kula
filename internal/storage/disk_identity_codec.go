package storage

import (
	"encoding/binary"
	"fmt"
	"math"

	"kula/internal/collector"
)

// Disk IDs extend each variable block AFTER PSU, leaving all existing offsets
// intact. The record-level MeanStats trailer still follows all Data/Min/Max.
// Layout: version u8, disk count u16, one length-prefixed ID per disk, in order,
// for the same disks the block stored (storedCount).
//
// An ID longer than its length can describe is never cut: it is stored empty,
// so that disk is tracked by its kernel name (DiskDevice.SeriesKey) rather than
// under a truncated identity, and the record is kept.
func appendDiskIDs(buf []byte, s *collector.Sample) []byte {
	devices := s.Disks.Devices[:storedCount(len(s.Disks.Devices))]
	buf = append(buf, 1)
	buf = appendUint16(buf, uint16(len(devices)))
	for _, disk := range devices {
		id := disk.ID
		if len(id) > math.MaxUint16 {
			id = ""
		}
		buf = appendUint16(buf, uint16(len(id)))
		buf = append(buf, id...)
	}
	return buf
}

func decodeDiskIDs(data []byte, s *collector.Sample) (int, error) {
	if len(data) < 3 {
		return 0, fmt.Errorf("truncated disk identities header")
	}
	if data[0] != 1 {
		return 1, fmt.Errorf("unsupported disk identities version %d", data[0])
	}
	count := int(binary.LittleEndian.Uint16(data[1:]))
	if count != len(s.Disks.Devices) {
		return 3, fmt.Errorf("disk identity count %d differs from disk count %d", count, len(s.Disks.Devices))
	}
	off := 3
	for i := range s.Disks.Devices {
		if len(data)-off < 2 {
			return off, fmt.Errorf("truncated disk id length")
		}
		n := int(binary.LittleEndian.Uint16(data[off:]))
		off += 2
		if n > len(data)-off {
			return off, fmt.Errorf("truncated disk id")
		}
		s.Disks.Devices[i].ID = string(data[off : off+n])
		off += n
	}
	return off, nil
}
