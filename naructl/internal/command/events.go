package command

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/lib/orchapi"
)

// eventsDefaultLimit is how many of the newest events `naructl events` lists without --limit.
const eventsDefaultLimit = 50

func events(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	group := fs.String("group", "", "list only the events of this group and its members")
	limit := fs.Int("limit", eventsDefaultLimit, "how many of the newest events to list; 0 lists all")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 0); err != nil {
			return err
		}
		if *limit < 0 {
			return fmt.Errorf("--limit is %d: it is not negative", *limit)
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		all, err := cl.Events(ctx)
		if err != nil {
			return err
		}
		var shown []orchapi.Event
		for _, ev := range all {
			if *group == "" || ev.Subject == *group || ev.Detail["group"] == *group {
				shown = append(shown, ev)
			}
		}
		if *limit > 0 && len(shown) > *limit {
			shown = shown[len(shown)-*limit:]
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "TIME\tTYPE\tSUBJECT\tDETAIL")
		for _, ev := range shown {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", ev.Time.Format(time.RFC3339), ev.Type, ev.Subject, detailText(ev.Detail))
		}
		return tw.Flush()
	}
}

// detailText writes an event's detail as key=value pairs in key order, the message last.
func detailText(d map[string]string) string {
	keys := make([]string, 0, len(d))
	for k := range d {
		if k != "message" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(d))
	for _, k := range keys {
		parts = append(parts, k+"="+d[k])
	}
	if m, ok := d["message"]; ok {
		parts = append(parts, m)
	}
	return strings.Join(parts, " ")
}
