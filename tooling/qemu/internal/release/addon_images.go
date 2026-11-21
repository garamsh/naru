package release

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// addonImagesDir is where the initramfs carries the OCI archive of each cluster add-on's image (ADR 0083).
const addonImagesDir = "usr/lib/naru/addons/images/"

// AddonImage is a cluster add-on's image as the initramfs carries it: the add-on's name and the digest of
// the image's manifest, the one its manifest names the image by.
type AddonImage struct{ Name, Digest string }

// ReadAddonImages returns the add-on images in the members of the gzip-compressed newc cpio archives,
// sorted by name; the same name and digest in two archives is returned once, and the same name with two
// digests is an error.
func ReadAddonImages(archives []string) ([]AddonImage, error) {
	found := map[string]string{}
	for _, p := range archives {
		err := walkCpio(p, func(name string, data []byte) error {
			base, ok := strings.CutPrefix(strings.TrimPrefix(name, "/"), addonImagesDir)
			if !ok || path.Ext(base) != ".tar" {
				return nil
			}
			add := strings.TrimSuffix(base, ".tar")
			digest, err := ociManifestDigest(data)
			if err != nil {
				return fmt.Errorf("%s: %s: %w", p, name, err)
			}
			if prev, dup := found[add]; dup && prev != digest {
				return fmt.Errorf("%s: add-on %s has the images %s and %s", p, add, prev, digest)
			}
			found[add] = digest
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	out := make([]AddonImage, 0, len(found))
	for name, digest := range found {
		out = append(out, AddonImage{Name: name, Digest: digest})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ociManifestDigest returns the digest of the one manifest in the index.json of an OCI archive.
func ociManifestDigest(archive []byte) (string, error) {
	r := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := r.Next()
		if err == io.EOF {
			return "", errors.New("the archive has no index.json")
		}
		if err != nil {
			return "", err
		}
		if path.Clean(h.Name) != "index.json" {
			continue
		}
		var index struct {
			Manifests []struct {
				Digest string `json:"digest"`
			} `json:"manifests"`
		}
		if err := json.NewDecoder(r).Decode(&index); err != nil {
			return "", fmt.Errorf("index.json: %w", err)
		}
		if len(index.Manifests) != 1 || !strings.HasPrefix(index.Manifests[0].Digest, "sha256:") {
			return "", fmt.Errorf("index.json names %d manifests, and one sha256 digest is expected", len(index.Manifests))
		}
		return index.Manifests[0].Digest, nil
	}
}
