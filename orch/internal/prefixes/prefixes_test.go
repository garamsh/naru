package prefixes_test

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/prefixes"
)

func p(s string) netip.Prefix { return netip.MustParsePrefix(s) }

func TestAllocateTakesTheFirstPrefixThatOverlapsNoClaim(t *testing.T) {
	space := p("10.0.0.0/16")
	got, err := prefixes.Allocate(space, 24, nil)
	require.NoError(t, err)
	require.Equal(t, p("10.0.0.0/24"), got)

	got, err = prefixes.Allocate(space, 24, []netip.Prefix{p("10.0.0.0/24"), p("10.0.1.0/24")})
	require.NoError(t, err)
	require.Equal(t, p("10.0.2.0/24"), got)
}

func TestAllocateSkipsAClaimOfAnotherLengthThatOverlaps(t *testing.T) {
	space := p("10.0.0.0/16")
	// A /28 inside the first /24 and a /20 over the next sixteen /24s.
	got, err := prefixes.Allocate(space, 24, []netip.Prefix{p("10.0.0.16/28"), p("10.0.16.0/20")})
	require.NoError(t, err)
	require.Equal(t, p("10.0.1.0/24"), got)

	// Control: a claim that overlaps nothing leaves the first prefix free.
	got, err = prefixes.Allocate(space, 24, []netip.Prefix{p("10.1.0.0/24")})
	require.NoError(t, err)
	require.Equal(t, p("10.0.0.0/24"), got)
}

func TestAllocateReportsAFullSpace(t *testing.T) {
	_, err := prefixes.Allocate(p("10.0.0.0/23"), 24, []netip.Prefix{p("10.0.0.0/24"), p("10.0.1.0/25")})
	require.ErrorIs(t, err, prefixes.ErrFull)
}

func TestAllocateRefusesALengthThatDoesNotFit(t *testing.T) {
	got, err := prefixes.Allocate(p("10.0.0.0/24"), 24, nil)
	require.NoError(t, err, "a space of the asked length is one prefix")
	require.Equal(t, p("10.0.0.0/24"), got)
	_, err = prefixes.Allocate(p("10.0.0.0/24"), 23, nil)
	require.Error(t, err)
	_, err = prefixes.Allocate(p("10.0.0.0/24"), 33, nil)
	require.Error(t, err)
}
