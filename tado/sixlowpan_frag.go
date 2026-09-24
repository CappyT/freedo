package tado

import "errors"

const (
	lowpanFragmentMaxPlain = 97
	lowpanFrag1HeaderLen   = 4
	lowpanFragNHeaderLen   = 5
	ipv6UDPHeaderLen       = 48
	// The valve rejects IPv6 datagrams larger than 0x500 before 6LoWPAN output.
	maxFragmentIPv6Datagram = 1280
)

// Fragment6LoWPANDatagram splits a link-local IPHC+UDP-NHC datagram into RFC 4944 bodies.
func Fragment6LoWPANDatagram(packet []byte, tag uint16) ([][]byte, error) {
	if len(packet) == 0 {
		return nil, errors.New("tado: cannot fragment an empty 6LoWPAN datagram")
	}
	if len(packet) < 9 || packet[0] != iphcLinkLocal0 || packet[1] != iphcLinkLocal1 || packet[2] != nhcUDPPortsIn {
		return nil, errors.New("tado: fragmentation requires a link-local IPHC datagram with an inline UDP checksum")
	}
	size := len(packet) + ipv6UDPHeaderLen - 9
	if size > maxFragmentIPv6Datagram {
		return nil, errors.New("tado: fragmented IPv6 datagram exceeds the valve 1280-byte MTU")
	}
	firstLen := lowpanFragmentMaxPlain - lowpanFrag1HeaderLen
	// FRAGN offsets are eight-byte units in the uncompressed datagram.
	for (ipv6UDPHeaderLen-9+firstLen)%8 != 0 {
		firstLen--
	}
	if len(packet) <= firstLen {
		return nil, errors.New("tado: datagram fits one Data frame and must not be fragmented")
	}

	frag1 := make([]byte, lowpanFrag1HeaderLen+firstLen)
	frag1[0] = 0xc0 | byte(size>>8)
	frag1[1] = byte(size)
	frag1[2] = byte(tag >> 8)
	frag1[3] = byte(tag)
	copy(frag1[lowpanFrag1HeaderLen:], packet[:firstLen])
	out := [][]byte{frag1}

	offset := ipv6UDPHeaderLen - 9 + firstLen
	for start := firstLen; start < len(packet); {
		remaining := len(packet) - start
		partLen := lowpanFragmentMaxPlain - lowpanFragNHeaderLen
		if remaining <= partLen {
			partLen = remaining
		} else {
			partLen &= ^7
		}
		frag := make([]byte, lowpanFragNHeaderLen+partLen)
		frag[0] = 0xe0 | byte(size>>8)
		frag[1] = byte(size)
		frag[2] = byte(tag >> 8)
		frag[3] = byte(tag)
		frag[4] = byte(offset / 8)
		copy(frag[lowpanFragNHeaderLen:], packet[start:start+partLen])
		out = append(out, frag)
		start += partLen
		offset += partLen
	}
	return out, nil
}
