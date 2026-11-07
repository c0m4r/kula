package storage

import (
	"fmt"

	"kula/internal/collector"
)

// The TCP extension carries TCP rates that the fixed scalar block has no room
// for: the fixed block cannot grow without breaking every stored record, so
// TCPStats.Retrans lives here, gated by flagHasTCPExt. It closes each variable
// block AFTER the disk identities, leaving every existing offset intact; the
// record-level MeanStats trailer still follows all Data/Min/Max blocks.
//
// Layout: version u8, then the version's fixed-size block.
//
//	v1 (4 bytes): retrans_ps float32
//
// Growing it means bumping tcpExtVersion and adding a branch in decodeTCPExt
// that reads the larger block; v1 records must keep decoding as they are.
const (
	tcpExtVersion = byte(1)
	tcpExtV1Size  = 4
)

func appendTCPExt(buf []byte, s *collector.Sample) []byte {
	buf = append(buf, tcpExtVersion)
	return appendF32(buf, s.Network.TCP.Retrans)
}

func decodeTCPExt(data []byte, s *collector.Sample) (int, error) {
	if len(data) < 1 {
		return 0, fmt.Errorf("variable block truncated at tcp extension version")
	}
	if data[0] != tcpExtVersion {
		return 1, fmt.Errorf("unsupported tcp extension version %d", data[0])
	}
	if len(data)-1 < tcpExtV1Size {
		return 1, fmt.Errorf("variable block truncated at tcp extension (need %d, have %d)", tcpExtV1Size, len(data)-1)
	}
	s.Network.TCP.Retrans = getF32(data[1:])
	return 1 + tcpExtV1Size, nil
}
