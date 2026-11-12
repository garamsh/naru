package judge_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/naru/tooling/qemu/internal/judge"
)

const (
	authorityLine = `time=2026-11-12T00:00:00Z level=INFO msg="authority written" name=gaon path=/run/gaon/ca.pem key=false`
	startLine     = `time=2026-11-12T00:00:01Z level=INFO msg=started service=mvmd pid=40`
)

func issuedLine(name string) string {
	return `time=2026-11-12T00:00:00Z level=INFO msg="certificate issued" name=` + name + ` authority=gaon usage=client path=/run/gaon/` + name + `.pem expires=2026-12-12T00:00:00Z`
}

func issuedLog(names ...string) []string {
	lines := []string{authorityLine}
	for _, n := range names {
		lines = append(lines, issuedLine(n))
	}
	return lines
}

func TestIssuance_EveryLineBeforeTheFirstServiceStartPasses(t *testing.T) {
	log := strings.Join(append(issuedLog(judge.GaonIssued...), startLine), "\n")

	assert.Empty(t, judge.Check("node-1", judge.Log{Text: log}, judge.Issuance(judge.GaonIssued...)))
}

func TestIssuance_EachMissingLineIsNamedAlone(t *testing.T) {
	for _, missing := range judge.GaonIssued {
		var names []string
		for _, n := range judge.GaonIssued {
			if n != missing {
				names = append(names, n)
			}
		}
		log := strings.Join(append(issuedLog(names...), startLine), "\n")

		bad := judge.Check("node-1", judge.Log{Text: log}, judge.Issuance(judge.GaonIssued...))

		if assert.Len(t, bad, 1, missing) {
			assert.Equal(t, "init issued "+missing, bad[0].Expect.What)
		}
	}
}

func TestIssuance_ALineAfterTheFirstServiceStartFails(t *testing.T) {
	log := strings.Join([]string{authorityLine, issuedLine("apiserver"), startLine, issuedLine("scheduler")}, "\n")

	bad := judge.Check("node-1", judge.Log{Text: log}, judge.Issuance("apiserver", "scheduler"))

	if assert.Len(t, bad, 1) {
		assert.Equal(t, "init issued scheduler", bad[0].Expect.What)
		assert.Contains(t, bad[0].String(), "before the first line")
	}
}

func TestIssuance_AFailedRenewalFailsTheNode(t *testing.T) {
	log := strings.Join(append(issuedLog("apiserver"), startLine, `level=ERROR msg="certificate renewal failed" name=apiserver`), "\n")

	bad := judge.Check("node-1", judge.Log{Text: log}, judge.Issuance("apiserver"))

	if assert.Len(t, bad, 1) {
		assert.Equal(t, "no renewal failed", bad[0].Expect.What)
	}
}

func TestIssuance_NoServiceStartMeansTheIssuingIsNotShownToPrecedeOne(t *testing.T) {
	bad := judge.Check("node-1", judge.Log{Text: strings.Join(issuedLog("apiserver"), "\n")}, judge.Issuance("apiserver"))

	assert.NotEmpty(t, bad)
}
