package command

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// hostDrain is host drain or host undrain: it asks orch to stop placing on the host and move its
// members off it, or to place on it again. verb is "drain" or "undrain".
func hostDrain(verb string, args []string, stdout, stderr io.Writer) error {
	o := newOrchFlags("host "+verb, stderr)
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("host " + verb + " needs the host ID first")
	}
	id := args[0]
	if _, err := o.parse(args[1:], 0); err != nil {
		return err
	}
	_, cl, err := o.connect()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	act := cl.Drain
	if verb == "undrain" {
		act = cl.Undrain
	}
	h, err := act(ctx, id)
	if err != nil {
		return err
	}
	if h.Draining {
		_, _ = fmt.Fprintf(stdout, "naructl: host %s is draining\n", h.ID)
	} else {
		_, _ = fmt.Fprintf(stdout, "naructl: host %s is not draining\n", h.ID)
	}
	return nil
}
