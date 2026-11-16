package dhcp4_test

import (
	"encoding/hex"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/dhcp4"
)

// The vectors below were written by an implementation other than this package's, so that a fault
// of the codec is not repeated in the bytes it is tested with. insomniacslk/dhcp
// v0.0.0-20261014062924-c6bb46ad3f3a (github.com/insomniacslk/dhcp/dhcpv4) produced vectorDiscover
// with NewDiscovery (broadcast, transaction ID deadbeef, MAC 02:00:00:00:00:09, requested options 1,
// 3 and 6) and vectorOfferLongDNS with NewReplyFromRequest on it (an OFFER of 10.0.2.15 with
// 70 name servers 10.1.0.1 to 10.1.0.70), then ToBytes. Its encoder splits the 280 bytes of
// name servers into instances of 255 and 25 bytes (RFC 3396). The program that wrote them is not
// kept: running those calls again writes the same bytes. vectorOverload is not from that library,
// which does not read option 52; it is built by hand from RFC 2131 section 4.1 and RFC 3396.
const (
	vectorDiscover = "" +
		"01010600deadbeef000080000000000000000000000000000000000002000000" +
		"0009000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"00000000000000000000000063825363350101370401030f06ff000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"000000000000000000000000"

	vectorOfferLongDNS = "" +
		"02010600deadbeef00008000000000000a00020f000000000000000002000000" +
		"0009000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"000000000000000000000000638253630104ffffff0003040a00020206ff0a01" +
		"00010a0100020a0100030a0100040a0100050a0100060a0100070a0100080a01" +
		"00090a01000a0a01000b0a01000c0a01000d0a01000e0a01000f0a0100100a01" +
		"00110a0100120a0100130a0100140a0100150a0100160a0100170a0100180a01" +
		"00190a01001a0a01001b0a01001c0a01001d0a01001e0a01001f0a0100200a01" +
		"00210a0100220a0100230a0100240a0100250a0100260a0100270a0100280a01" +
		"00290a01002a0a01002b0a01002c0a01002d0a01002e0a01002f0a0100300a01" +
		"00310a0100320a0100330a0100340a0100350a0100360a0100370a0100380a01" +
		"00390a01003a0a01003b0a01003c0a01003d0a01003e0a01003f0a0100061940" +
		"0a0100410a0100420a0100430a0100440a0100450a010046330400000e103501" +
		"0236040a000202ff"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

// overloaded is an OFFER of 10.0.2.15 to vectorMAC with the given options, file and sname fields.
func overloaded(options, file, sname []byte) []byte {
	b := make([]byte, 236)
	b[0], b[1], b[2] = 2, 1, 6
	copy(b[4:], []byte{0, 0, 0, 1})
	copy(b[16:], []byte{10, 0, 2, 15})
	copy(b[28:], vectorMAC)
	copy(b[44:], sname)
	copy(b[108:], file)
	b = append(b, 0x63, 0x82, 0x53, 0x63)
	return append(b, options...)
}

// vectorOverload is an OFFER whose options field holds the message type, a server identifier and
// option 52 with the given value, whose file field holds the lease time and the subnet mask, and
// whose sname field holds a router and a name server. The options field has no router: an
// implementation that ignores option 52 finds none.
func vectorOverload(overload byte) []byte {
	return overloaded(
		[]byte{53, 1, 2, 52, 1, overload, 54, 4, 10, 0, 2, 2, 255},
		[]byte{51, 4, 0, 0, 0x0e, 0x10, 1, 4, 255, 255, 255, 0, 255},
		[]byte{3, 4, 10, 0, 2, 2, 6, 4, 10, 0, 2, 3, 255})
}

var vectorMAC = net.HardwareAddr{2, 0, 0, 0, 0, 9}

func TestDecode_ReadsADiscoverAnotherImplementationWrote(t *testing.T) {
	m, err := dhcp4.Decode(unhex(t, vectorDiscover))

	require.NoError(t, err)
	assert.False(t, m.Reply)
	assert.Equal(t, uint32(0xdeadbeef), m.XID)
	assert.True(t, m.Broadcast)
	assert.Equal(t, vectorMAC, m.CHAddr)
	assert.Equal(t, dhcp4.Discover, m.Type())
	assert.Equal(t, []byte{1, 3, 15, 6}, m.Options[dhcp4.OptParameterList])
}

func TestEncode_WritesTheDiscoverTheOtherImplementationWrote(t *testing.T) {
	m := dhcp4.Message{XID: 0xdeadbeef, Broadcast: true, CHAddr: vectorMAC, Options: map[byte][]byte{
		dhcp4.OptMessageType:   {byte(dhcp4.Discover)},
		dhcp4.OptParameterList: {1, 3, 15, 6},
	}}

	assert.Equal(t, unhex(t, vectorDiscover), m.Encode())
}

func TestDecode_JoinsTheInstancesOfAnOptionThatRFC3396SplitsAcrossTheLimit(t *testing.T) {
	m, err := dhcp4.Decode(unhex(t, vectorOfferLongDNS))

	require.NoError(t, err)
	assert.Equal(t, dhcp4.Offer, m.Type())
	assert.Equal(t, netip.MustParseAddr("10.0.2.15"), m.YIAddr)
	dns := m.Options[dhcp4.OptDNS]
	require.Len(t, dns, 280, "both instances, 255 and 25 bytes")
	for i := range 70 {
		assert.Equal(t, []byte{10, 1, 0, byte(i + 1)}, dns[4*i:4*i+4], "name server %d, in order", i+1)
	}
	assert.Equal(t, []byte{255, 255, 255, 0}, m.Options[dhcp4.OptSubnetMask], "an option of one instance is unchanged")
}

func TestEncode_SplitsAnOptionOfMoreThan255BytesAsTheOtherImplementationDoes(t *testing.T) {
	var dns []byte
	for i := 1; i <= 70; i++ {
		dns = append(dns, 10, 1, 0, byte(i))
	}
	m := dhcp4.Message{Reply: true, XID: 0xdeadbeef, Broadcast: true, YIAddr: netip.MustParseAddr("10.0.2.15"), CHAddr: vectorMAC, Options: map[byte][]byte{
		dhcp4.OptMessageType: {byte(dhcp4.Offer)},
		dhcp4.OptSubnetMask:  {255, 255, 255, 0},
		dhcp4.OptRouter:      {10, 0, 2, 2},
		dhcp4.OptDNS:         dns,
		dhcp4.OptLeaseTime:   {0, 0, 0x0e, 0x10},
		dhcp4.OptServerID:    {10, 0, 2, 2},
	}}

	got, want := m.Encode(), unhex(t, vectorOfferLongDNS)

	assert.Equal(t, want[240:], got[240:], "the options, from the magic cookie on, are the same bytes")
	back, err := dhcp4.Decode(got)
	require.NoError(t, err)
	assert.Equal(t, dns, back.Options[dhcp4.OptDNS])
}

func TestDecode_ReadsTheOptionsAMessageOverloadsIntoItsFileAndSnameFields(t *testing.T) {
	tests := []struct {
		name     string
		overload byte
		router   bool
		lease    bool
		dns      bool
	}{
		{"the file field", 1, false, true, false},
		{"the sname field", 2, true, false, true},
		{"both fields", 3, true, true, true},
		// The control: with no overload the two fields are not options, whatever they hold.
		{"none", 0, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := dhcp4.Decode(vectorOverload(tt.overload))

			require.NoError(t, err)
			assert.Equal(t, dhcp4.Offer, m.Type(), "the options field is read in every case")
			assert.Equal(t, []byte{10, 0, 2, 2}, m.Options[dhcp4.OptServerID])
			assert.Equal(t, tt.router, m.Options[dhcp4.OptRouter] != nil, "router, in the sname field")
			assert.Equal(t, tt.dns, m.Options[dhcp4.OptDNS] != nil, "name server, in the sname field")
			assert.Equal(t, tt.lease, m.Options[dhcp4.OptLeaseTime] != nil, "lease time, in the file field")
			assert.Equal(t, tt.lease, m.Options[dhcp4.OptSubnetMask] != nil, "subnet mask, in the file field")
		})
	}
}

func TestDecode_JoinsAnOptionThatBeginsInTheOptionsFieldAndContinuesInTheFileField(t *testing.T) {
	b := overloaded(
		[]byte{53, 1, 2, 6, 4, 10, 0, 2, 3, 52, 1, 1, 255},
		[]byte{6, 4, 10, 0, 2, 4, 255},
		nil)

	m, err := dhcp4.Decode(b)

	require.NoError(t, err)
	assert.Equal(t, []byte{10, 0, 2, 3, 10, 0, 2, 4}, m.Options[dhcp4.OptDNS], "the options field first, then the file field")
}

func TestDecode_RefusesAnOptionCutShortInAnOverloadedField(t *testing.T) {
	cut := overloaded([]byte{53, 1, 2, 52, 1, 1, 255}, []byte{6, 200, 10, 0, 2, 4}, nil)
	whole := overloaded([]byte{53, 1, 2, 52, 1, 1, 255}, []byte{6, 4, 10, 0, 2, 4, 255}, nil)

	_, err := dhcp4.Decode(cut)
	_, control := dhcp4.Decode(whole)

	assert.ErrorContains(t, err, "cut short")
	assert.NoError(t, control)
}
