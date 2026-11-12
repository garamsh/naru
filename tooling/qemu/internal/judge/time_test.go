package judge_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/naru/tooling/qemu/internal/judge"
)

const timeLog = `time=2026-11-13T00:00:01.000Z level=INFO msg="time synchronised" sync.server=10.0.2.2:5123 sync.offset=48h0m0s sync.delay=1ms sync.correction=step
time=2026-11-13T00:00:01.100Z level=INFO msg="authority written" name=ca
time=2026-11-13T00:00:01.200Z level=INFO msg=started service=mvmd
`

func TestTimeSynced_AStepBeforeTheAuthorityPasses(t *testing.T) {
	assert.Empty(t, judge.Check("node-1", judge.Log{Text: timeLog}, judge.TimeSynced("10.0.2.2:5123")))
}

func TestTimeSynced_EachDeviationIsNamedAlone(t *testing.T) {
	const ok = "10.0.2.2:5123"
	for name, tc := range map[string]struct{ log, server, failing string }{
		"another server":    {timeLog, "10.0.2.2:9", "init stepped the clock from 10.0.2.2:9"},
		"a slew":            {"msg=\"time synchronised\" sync.server=10.0.2.2:5123 sync.correction=slew\nmsg=\"authority written\"\n", ok, "init stepped the clock"},
		"after the issuing": {"msg=\"authority written\"\nmsg=\"time synchronised\" sync.server=10.0.2.2:5123 sync.correction=step\n", ok, "init stepped the clock"},
		"no time line":      {"msg=\"authority written\"\n", ok, "init stepped the clock"},
		"not synchronised":  {timeLog + "msg=\"time is not synchronised: the configuration has no spec.time\"\n", ok, "init did not log that time is not synchronised"},
		"a failed attempt":  {timeLog + "level=WARN msg=\"time not synchronised; will try again\"\n", ok, "init did not warn"},
	} {
		t.Run(name, func(t *testing.T) {
			failures := judge.Check("node-1", judge.Log{Text: tc.log}, judge.TimeSynced(tc.server))

			if assert.Len(t, failures, 1) {
				assert.Contains(t, failures[0].String(), tc.failing)
			}
		})
	}
}
