package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"time"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
)

const (
	// bootSetTimeout bounds adding a boot set: the upload of a kernel and an initramfs, which a
	// slow link makes longer than any other call of the API.
	bootSetTimeout = 15 * time.Minute
	// bootSetMaxBytes bounds the body of an add: the two files and the request part.
	bootSetMaxBytes = 1 << 30
	// bootSetRequestMax bounds the request part, which is a name and two digests.
	bootSetRequestMax = 4 << 10
)

var (
	bootSetName = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)
	sha256Hex   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// BootSetRequest is the first part, named "boot-set", of the body of POST /v1/boot-sets: the set's
// name and the SHA-256, in hex, of its kernel and its initramfs. The body is multipart/form-data
// whose parts are, in this order, "boot-set" (this as JSON), "kernel" and "initramfs".
type BootSetRequest struct {
	Name            string `json:"name"`
	KernelSHA256    string `json:"kernelSha256"`
	InitramfsSHA256 string `json:"initramfsSha256"`
}

// routeBootSets registers the routes of the boot sets.
func (s *Server) routeBootSets(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/boot-sets", s.addBootSet)
	mux.HandleFunc("DELETE /v1/boot-sets/{name}", s.removeBootSet)
}

// badBody is a body that is not the multipart shape of POST /v1/boot-sets.
type badBody string

func (b badBody) Error() string { return string(b) }

// lazyPart is the part of the body that must come next, named name, found when it is first read:
// the parts of a multipart body can only be taken in order, and the kernel is read before it.
type lazyPart struct {
	mr   *multipart.Reader
	name string
	part *multipart.Part
	err  error
}

func (p *lazyPart) Read(b []byte) (int, error) {
	if p.part == nil && p.err == nil {
		p.part, p.err = nextPart(p.mr, p.name)
	}
	if p.err != nil {
		return 0, p.err
	}
	return p.part.Read(b)
}

func nextPart(mr *multipart.Reader, name string) (*multipart.Part, error) {
	part, err := mr.NextPart()
	if errors.Is(err, io.EOF) {
		return nil, badBody("the body has no part named " + name)
	}
	if err != nil {
		return nil, err
	}
	if part.FormName() != name {
		return nil, badBody(fmt.Sprintf("the part %q is not the part named %q that comes here", part.FormName(), name))
	}
	return part, nil
}

// addBootSet adds a boot set to mvmd without a reboot: the kernel and the initramfs are streamed
// from the request to mvmd, which refuses a file that does not have the SHA-256 the request names.
// The same name and digests again is 200, and the same name with other digests is 409.
func (s *Server) addBootSet(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, bootSetMaxBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, hostapi.ErrorBody{Error: "the body is not multipart/form-data: " + err.Error()})
		return
	}
	req, err := readBootSetRequest(mr)
	if err != nil {
		s.failBody(w, err)
		return
	}
	if fields := validateBootSet(req); len(fields) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, hostapi.ErrorBody{Error: "the boot set is not valid", Fields: fields})
		return
	}
	kernel, err := nextPart(mr, "kernel")
	if err != nil {
		s.failBody(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), bootSetTimeout)
	defer cancel()
	set := mvmd.BootSet{Name: req.Name, KernelDigest: req.KernelSHA256, InitrdDigest: req.InitramfsSHA256}
	if err := s.deps.Runtime.AddBootSet(ctx, set, kernel, &lazyPart{mr: mr, name: "initramfs"}); err != nil {
		s.failBody(w, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func readBootSetRequest(mr *multipart.Reader) (BootSetRequest, error) {
	var req BootSetRequest
	part, err := nextPart(mr, "boot-set")
	if err != nil {
		return req, err
	}
	dec := json.NewDecoder(io.LimitReader(part, bootSetRequestMax))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		return req, badBody("the part boot-set is not one JSON object of the fields the API names")
	}
	return req, nil
}

// failBody answers the error of reading the body, or of mvmd.
func (s *Server) failBody(w http.ResponseWriter, err error) {
	var bad badBody
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &bad):
		writeJSON(w, http.StatusBadRequest, hostapi.ErrorBody{Error: bad.Error()})
	case errors.As(err, &tooBig):
		writeJSON(w, http.StatusRequestEntityTooLarge, hostapi.ErrorBody{Error: fmt.Sprintf("the body is larger than %d bytes", tooBig.Limit)})
	default:
		s.fail(w, err)
	}
}

func validateBootSet(req BootSetRequest) []string {
	var fields []string
	if !bootSetName.MatchString(req.Name) {
		fields = append(fields, "name")
	}
	if !sha256Hex.MatchString(req.KernelSHA256) {
		fields = append(fields, "kernelSha256")
	}
	if !sha256Hex.MatchString(req.InitramfsSHA256) {
		fields = append(fields, "initramfsSha256")
	}
	return fields
}

// removeBootSet removes a boot set added through the API. An absent one is 204 as well, and a set
// a microVM names, or one the node's configuration offers, is 409.
func (s *Server) removeBootSet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !bootSetName.MatchString(name) {
		writeJSON(w, http.StatusUnprocessableEntity, hostapi.ErrorBody{Error: "the name is not a boot set's", Fields: []string{"name"}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	if err := s.deps.Runtime.RemoveBootSet(ctx, name); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
