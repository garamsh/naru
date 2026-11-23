package extension_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/extension"
)

// fakeSource offers the image it was given for the name, or what err says.
type fakeSource struct {
	name    string
	image   string
	version string
	digest  string
	err     error
	asked   int
}

func (s *fakeSource) Name() string { return s.name }

// releasing is a source that counts how often it is told the stage is over.
type releasing struct {
	fakeSource
	released int
}

func (s *releasing) Release() error { s.released++; return nil }

func (s *fakeSource) Offer(name string) (extension.Offer, error) {
	s.asked++
	if s.err != nil {
		return extension.Offer{}, s.err
	}
	if s.image == "" {
		return extension.Offer{}, extension.ErrNoOffer
	}
	d := s.digest
	if d == "" {
		d = digestOf(s.image)
	}
	return extension.Offer{SHA256: d, Version: s.version, Body: io.NopCloser(strings.NewReader(s.image))}, nil
}

func TestManager_ProvideStoresTheImageOfAnUnpinnedEntryFromTheSourceAndMountTakesIt(t *testing.T) {
	n := newNode(t)
	img := manifest("naru-volumes", version)
	src := &fakeSource{name: "esp", image: img, version: version}

	n.mgr.Provide([]config.Extension{{Name: "naru-volumes"}}, []extension.Source{src})

	require.NoError(t, n.mgr.Mount([]config.Extension{{Name: "naru-volumes"}}))
	assert.Equal(t, img, n.m.mounted[n.root+"/naru-volumes"])
}

func TestManager_ProvideLeavesTheStoreAloneWhenItHoldsTheImageOfInitsVersion(t *testing.T) {
	n := newNode(t)
	n.put(t, manifest("naru-volumes", version))
	src := &fakeSource{name: "esp", image: manifest("naru-volumes", version), version: version}

	n.mgr.Provide([]config.Extension{{Name: "naru-volumes"}}, []extension.Source{src})

	assert.Zero(t, src.asked, "a source is not read when the store has the image")
}

func TestManager_ProvideNeverProvidesAPinnedEntry(t *testing.T) {
	n := newNode(t)
	img := manifest("naru-volumes", version)
	src := &fakeSource{name: "esp", image: img, version: version}

	n.mgr.Provide([]config.Extension{{Name: "naru-volumes", SHA256: digestOf(img)}}, []extension.Source{src})

	assert.Zero(t, src.asked)
	require.ErrorContains(t, n.mgr.Mount([]config.Extension{{Name: "naru-volumes", SHA256: digestOf(img)}}), "no image")
}

func TestManager_ProvideSkipsASourceThatFailsOrOffersAnotherVersionOrWrongBytesAndAsksTheNext(t *testing.T) {
	for name, bad := range map[string]*fakeSource{
		"a failing source":        {name: "bad", err: errors.New("read error")},
		"another version":         {name: "bad", image: manifest("naru-volumes", "v0.0.1"), version: "v0.0.1"},
		"a source with no image":  {name: "bad"},
		"a digest that is not it": {name: "bad", image: manifest("naru-volumes", version), version: version, digest: digestOf("other")},
		"another extension":       {name: "bad", image: manifest("naru-orch", version), version: version},
	} {
		t.Run(name, func(t *testing.T) {
			n := newNode(t)
			good := &fakeSource{name: "boot set", image: manifest("naru-volumes", version), version: version}

			n.mgr.Provide([]config.Extension{{Name: "naru-volumes"}}, []extension.Source{bad, good})

			assert.Equal(t, 1, bad.asked)
			require.NoError(t, n.mgr.Mount([]config.Extension{{Name: "naru-volumes"}}), "the next source's image is taken")
			list, err := n.mgr.List()
			require.NoError(t, err)
			assert.Len(t, list, 1, "a refused image is not kept")
		})
	}
}

func TestManager_ProvideWithNoSourceLeavesTheRefusalToMount(t *testing.T) {
	n := newNode(t)

	n.mgr.Provide([]config.Extension{{Name: "naru-volumes"}}, nil)

	require.ErrorContains(t, n.mgr.Mount([]config.Extension{{Name: "naru-volumes"}}), `no image of "naru-volumes" is stored`)
}

func TestManager_ProvideTellsASourceThatHoldsMemoryThatTheStageIsOverWhetherOrNotItWasUsed(t *testing.T) {
	for name, stored := range map[string]bool{"the image is staged from it": false, "the store already holds the image": true} {
		t.Run(name, func(t *testing.T) {
			n := newNode(t)
			if stored {
				n.put(t, manifest("naru-volumes", version))
			}
			src := &releasing{fakeSource: fakeSource{name: "initramfs", image: manifest("naru-volumes", version), version: version}}

			n.mgr.Provide([]config.Extension{{Name: "naru-volumes"}}, []extension.Source{src})

			assert.Equal(t, 1, src.released)
		})
	}
}
