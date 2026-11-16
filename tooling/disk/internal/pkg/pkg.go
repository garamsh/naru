// Package pkg reads the packages image/pkgs builds: each is an OCI image layout in a tar, one image
// whose layers hold the files the package installs (ADR 0075). Unpack writes those files into a
// directory, and Digest names the image by its manifest digest, which two builds of one commit share.
// Only what BuildKit's OCI exporter writes is read: an index naming one manifest, whose layers are
// uncompressed or gzip-compressed tars.
package pkg

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	mediaIndex    = "application/vnd.oci.image.index.v1+json"
	mediaManifest = "application/vnd.oci.image.manifest.v1+json"
	layerTar      = "application/vnd.oci.image.layer.v1.tar"
	layerGzip     = "application/vnd.oci.image.layer.v1.tar+gzip"
	blobDir       = "blobs/"
)

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
}

type document struct {
	Manifests []descriptor `json:"manifests"`
	Layers    []descriptor `json:"layers"`
}

// Digest returns the digest of the manifest of the image in the OCI layout tar at file.
func Digest(file string) (string, error) {
	d, _, err := open(file)
	return d.Digest, err
}

// Unpack writes the files of the image in the OCI layout tar at file into dir, applying its layers in
// order, whiteouts included. dir is emptied first. Every operation goes through an os.Root on dir, so
// neither a ".." nor a symlink an earlier entry made can lead an entry outside it.
func Unpack(file, dir string) error {
	manifest, blobs, err := open(file)
	if err != nil {
		return err
	}
	// The layers are read from the tar in its own order and kept in files beside dir, then applied in
	// the manifest's order.
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	scratch, err := os.MkdirTemp(filepath.Dir(dir), ".unpack-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	var m document
	if err := json.Unmarshal(blobs[manifest.Digest], &m); err != nil {
		return fmt.Errorf("pkg: manifest of %s: %w", file, err)
	}
	want := map[string]string{}
	for i, l := range m.Layers {
		want[blobName(l.Digest)] = filepath.Join(scratch, fmt.Sprint(i))
	}
	if err := extractLayers(file, want); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for i, l := range m.Layers {
		if err := applyLayer(filepath.Join(scratch, fmt.Sprint(i)), l.MediaType, root); err != nil {
			return fmt.Errorf("pkg: layer %s of %s: %w", l.Digest, file, err)
		}
	}
	return nil
}

func blobName(digest string) string {
	return blobDir + strings.Replace(digest, ":", "/", 1)
}

// open reads index.json and every JSON blob that is small enough to be a manifest, and returns the
// descriptor of the one manifest the index names.
func open(file string) (descriptor, map[string][]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return descriptor{}, nil, err
	}
	defer f.Close()
	blobs := map[string][]byte{}
	var index []byte
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return descriptor{}, nil, fmt.Errorf("pkg: %s: %w", file, err)
		}
		name := path.Clean(h.Name)
		switch {
		case name == "index.json":
			if index, err = io.ReadAll(tr); err != nil {
				return descriptor{}, nil, err
			}
		case strings.HasPrefix(name, blobDir) && h.Size <= 1<<20:
			b, err := io.ReadAll(tr)
			if err != nil {
				return descriptor{}, nil, err
			}
			blobs[name] = b
		}
	}
	if index == nil {
		return descriptor{}, nil, fmt.Errorf("pkg: %s has no index.json, so it is not an OCI layout", file)
	}
	var idx document
	if err := json.Unmarshal(index, &idx); err != nil {
		return descriptor{}, nil, fmt.Errorf("pkg: index.json of %s: %w", file, err)
	}
	if len(idx.Manifests) != 1 || idx.Manifests[0].MediaType != mediaManifest {
		return descriptor{}, nil, fmt.Errorf("pkg: %s must name exactly one image manifest (%s) in index.json", file, mediaManifest)
	}
	d := idx.Manifests[0]
	if _, ok := blobs[blobName(d.Digest)]; !ok {
		return descriptor{}, nil, fmt.Errorf("pkg: %s has no blob for its manifest %s", file, d.Digest)
	}
	return d, mapKeys(blobs), nil
}

// mapKeys re-keys the blobs by digest.
func mapKeys(blobs map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(blobs))
	for name, b := range blobs {
		out[strings.Replace(strings.TrimPrefix(name, blobDir), "/", ":", 1)] = b
	}
	return out
}

// extractLayers copies the tar entries named by want (blob name to destination) into their files.
func extractLayers(file string, want map[string]string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	found := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("pkg: %s: %w", file, err)
		}
		dst, ok := want[path.Clean(h.Name)]
		if !ok {
			continue
		}
		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		found++
	}
	if found != len(want) {
		return fmt.Errorf("pkg: %s lacks a layer blob its manifest names", file)
	}
	return nil
}

// applyLayer unpacks one layer over dir.
func applyLayer(file, media string, root *os.Root) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	switch media {
	case layerTar:
	case layerGzip:
		z, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer z.Close()
		r = z
	default:
		return fmt.Errorf("unsupported layer type %s", media)
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := applyEntry(tr, h, root); err != nil {
			return fmt.Errorf("%s: %w", h.Name, err)
		}
	}
}

func applyEntry(tr *tar.Reader, h *tar.Header, root *os.Root) error {
	rel := path.Clean("/" + h.Name)[1:]
	if rel == "" {
		return nil
	}
	target := filepath.FromSlash(rel)
	parent := filepath.Dir(target)
	base := path.Base(rel)
	switch {
	case base == ".wh..wh..opq":
		// The directory's lower contents are hidden; its own entries follow in this layer.
		return emptyDir(root, parent)
	case strings.HasPrefix(base, ".wh."):
		return root.RemoveAll(filepath.Join(parent, strings.TrimPrefix(base, ".wh.")))
	}
	if err := root.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	switch h.Typeflag {
	case tar.TypeDir:
		if err := root.MkdirAll(target, 0o755); err != nil {
			return err
		}
		return root.Chmod(target, h.FileInfo().Mode().Perm())
	case tar.TypeReg:
		if err := root.RemoveAll(target); err != nil {
			return err
		}
		out, err := root.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, h.FileInfo().Mode().Perm())
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		return root.Chmod(target, h.FileInfo().Mode().Perm())
	case tar.TypeSymlink:
		if err := root.RemoveAll(target); err != nil {
			return err
		}
		return root.Symlink(h.Linkname, target)
	case tar.TypeLink:
		if err := root.RemoveAll(target); err != nil {
			return err
		}
		return root.Link(filepath.FromSlash(path.Clean("/" + h.Linkname)[1:]), target)
	default:
		return fmt.Errorf("entry type %q is not a file, directory or link", h.Typeflag)
	}
}

func emptyDir(root *os.Root, dir string) error {
	d, err := root.Open(dir)
	if errors.Is(err, os.ErrNotExist) {
		return root.MkdirAll(dir, 0o755)
	}
	if err != nil {
		return err
	}
	names, err := d.Readdirnames(-1)
	d.Close()
	if err != nil {
		return err
	}
	for _, n := range names {
		if err := root.RemoveAll(filepath.Join(dir, n)); err != nil {
			return err
		}
	}
	return nil
}
