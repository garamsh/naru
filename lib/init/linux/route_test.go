package linux

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
)

func TestHost_AddAndDeleteRoute(t *testing.T) {
	r := config.Route{To: netip.MustParsePrefix("10.200.2.0/24"), Via: netip.MustParseAddr("10.78.0.2")}

	t.Run("hands the destination and the next hop to the kernel", func(t *testing.T) {
		h, _ := testHost(t, nil)
		h.links = &fakeLinks{}

		require.NoError(t, h.AddRoute(r))
		require.NoError(t, h.DeleteRoute(r))

		assert.Equal(t, []string{"replace-route 10.200.2.0/24 10.78.0.2", "delete-route 10.200.2.0/24 10.78.0.2"}, h.links.(*fakeLinks).calls)
	})

	t.Run("names the route the kernel refuses", func(t *testing.T) {
		h, _ := testHost(t, nil)
		h.links = &fakeLinks{fail: "replace-route 10.200.2.0/24 10.78.0.2"}

		err := h.AddRoute(r)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "10.200.2.0/24 via 10.78.0.2")
	})
}
