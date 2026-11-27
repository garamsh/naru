package mvmd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/encoding/protodelim"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// importRequestID is the request ID of every CreateDiskFromStream: the disk's ID is the key, so a
// repeat of an import is the same disk under the same request ID, and mvmd answers it as made.
const importRequestID = "naru-hostd-import"

func filesystemFromProto(f v1alpha2.Filesystem) (string, error) {
	switch f {
	case v1alpha2.Filesystem_FILESYSTEM_EXT4:
		return FilesystemExt4, nil
	case v1alpha2.Filesystem_FILESYSTEM_NONE:
		return FilesystemNone, nil
	}
	return "", fmt.Errorf("%w: the filesystem %s is neither ext4 nor none", ErrInvalid, f)
}

func filesystemToProto(f string) (v1alpha2.Filesystem, error) {
	switch f {
	case FilesystemExt4:
		return v1alpha2.Filesystem_FILESYSTEM_EXT4, nil
	case FilesystemNone:
		return v1alpha2.Filesystem_FILESYSTEM_NONE, nil
	}
	return v1alpha2.Filesystem_FILESYSTEM_UNSPECIFIED, fmt.Errorf("%w: the filesystem %q is neither ext4 nor none", ErrInvalid, f)
}

// partFromProto is the part a ReadDisk message carries; a message with none is an error.
func partFromProto(m *v1alpha2.ReadDiskResponse) (DiskPart, error) {
	switch p := m.GetPart().(type) {
	case *v1alpha2.ReadDiskResponse_Header:
		fs, err := filesystemFromProto(p.Header.GetFilesystem())
		if err != nil {
			return nil, err
		}
		return DiskHeader{SizeBytes: p.Header.GetSizeBytes(), Filesystem: fs}, nil
	case *v1alpha2.ReadDiskResponse_Extent:
		return DiskExtent{Offset: p.Extent.GetOffset(), Data: p.Extent.GetData()}, nil
	case *v1alpha2.ReadDiskResponse_Digest:
		return DiskDigest(p.Digest.GetSha256()), nil
	}
	return nil, errors.New("a disk stream's message carries no part")
}

// partToProto is the ReadDisk message of p; a nil part is an error.
func partToProto(p DiskPart) (*v1alpha2.ReadDiskResponse, error) {
	switch p := p.(type) {
	case DiskHeader:
		fs, err := filesystemToProto(p.Filesystem)
		if err != nil {
			return nil, err
		}
		return &v1alpha2.ReadDiskResponse{Part: &v1alpha2.ReadDiskResponse_Header{Header: &v1alpha2.DiskStreamHeader{SizeBytes: p.SizeBytes, Filesystem: fs}}}, nil
	case DiskExtent:
		return &v1alpha2.ReadDiskResponse{Part: &v1alpha2.ReadDiskResponse_Extent{Extent: &v1alpha2.DiskExtent{Offset: p.Offset, Data: p.Data}}}, nil
	case DiskDigest:
		return &v1alpha2.ReadDiskResponse{Part: &v1alpha2.ReadDiskResponse_Digest{Digest: &v1alpha2.DiskStreamDigest{Sha256: string(p)}}}, nil
	}
	return nil, errors.New("a disk stream's part is none of a header, an extent and a digest")
}

func (g *GRPC) ReadDisk(ctx context.Context, id string, sink func(DiskPart) error) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (struct{}, error) {
		stream, err := c.ReadDisk(ctx, &v1alpha2.ReadDiskRequest{DiskId: id})
		if err != nil {
			return struct{}{}, err
		}
		for {
			msg, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return struct{}{}, nil
			}
			if err != nil {
				return struct{}{}, err
			}
			part, err := partFromProto(msg)
			if err != nil {
				return struct{}{}, err
			}
			if err := sink(part); err != nil {
				return struct{}{}, sinkError{err}
			}
		}
	})
	var s sinkError
	if errors.As(err, &s) {
		return s.err
	}
	return err
}

// sinkError carries a sink's or a sender's own error past call's mapping of mvmd's.
type sinkError struct{ err error }

func (e sinkError) Error() string { return e.err.Error() }

func (g *GRPC) CreateDiskFromStream(ctx context.Context, id string, h DiskHeader, parts func(send func(DiskPart) error) error) error {
	fs, err := filesystemToProto(h.Filesystem)
	if err != nil {
		return err
	}
	// A stream that ends without its digest is one mvmd discards; cancelling it is how a sender's
	// failure ends it, since a close would read as the end of a stream with no digest.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err = call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.CreateDiskFromStreamResponse, error) {
		stream, err := c.CreateDiskFromStream(streamCtx)
		if err != nil {
			return nil, err
		}
		start := &v1alpha2.CreateDiskFromStreamRequest{Part: &v1alpha2.CreateDiskFromStreamRequest_Start{Start: &v1alpha2.CreateDiskFromStreamStart{
			DiskId: id, RequestId: importRequestID, SizeBytes: h.SizeBytes, Filesystem: fs,
		}}}
		// A send that fails with io.EOF means mvmd ended the stream: its status is the reason.
		ended := false
		send := func(m *v1alpha2.CreateDiskFromStreamRequest) error {
			if err := stream.Send(m); err != nil {
				ended = errors.Is(err, io.EOF)
				return err
			}
			return nil
		}
		err = send(start)
		if err == nil {
			err = parts(func(p DiskPart) error {
				switch p := p.(type) {
				case DiskExtent:
					return send(&v1alpha2.CreateDiskFromStreamRequest{Part: &v1alpha2.CreateDiskFromStreamRequest_Extent{Extent: &v1alpha2.DiskExtent{Offset: p.Offset, Data: p.Data}}})
				case DiskDigest:
					return send(&v1alpha2.CreateDiskFromStreamRequest{Part: &v1alpha2.CreateDiskFromStreamRequest_Digest{Digest: &v1alpha2.DiskStreamDigest{Sha256: string(p)}}})
				}
				return errors.New("a disk is made from extents and then a digest, after its header")
			})
		}
		switch {
		case err == nil, ended:
			return stream.CloseAndRecv()
		default:
			cancel()
			return nil, sinkError{err}
		}
	})
	var s sinkError
	if errors.As(err, &s) {
		return s.err
	}
	return err
}

// WriteDiskPart writes p to w as one length-delimited message of mvmd's ReadDisk stream: the form a
// disk's stream takes between hosts.
func WriteDiskPart(w io.Writer, p DiskPart) error {
	m, err := partToProto(p)
	if err != nil {
		return err
	}
	_, err = protodelim.MarshalTo(w, m)
	return err
}

// ReadDiskPart reads the next part WriteDiskPart wrote to r. The end of r between parts is io.EOF.
func ReadDiskPart(r protodelim.Reader) (DiskPart, error) {
	var m v1alpha2.ReadDiskResponse
	if err := protodelim.UnmarshalFrom(r, &m); err != nil {
		return nil, err
	}
	return partFromProto(&m)
}
