package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
)

func microVMOf(t *testing.T, b []byte) hostapi.MicroVM {
	t.Helper()
	var v hostapi.MicroVM
	require.NoError(t, json.Unmarshal(b, &v), string(b))
	return v
}

func TestStopStart_StopsAndStartsAndEachRepeatIs200WithoutACallToMvmd(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, b := e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code, string(b))

	code, b = e.do(t, c, "POST", "/v1/microvms/web/stop", nil)
	require.Equal(t, 200, code, string(b))
	assert.Equal(t, "stopped", microVMOf(t, b).State)
	assert.Equal(t, mvmd.StateStopped, e.rt.machines["web"].State)
	assert.Equal(t, 1, e.rt.stops)

	code, b = e.do(t, c, "POST", "/v1/microvms/web/stop", nil)
	require.Equal(t, 200, code, string(b))
	assert.Equal(t, "stopped", microVMOf(t, b).State)
	assert.Equal(t, 1, e.rt.stops, "a stop of a stopped microVM does not reach mvmd")

	code, b = e.do(t, c, "POST", "/v1/microvms/web/start", nil)
	require.Equal(t, 200, code, string(b))
	assert.Equal(t, "running", microVMOf(t, b).State)
	assert.Equal(t, mvmd.StateRunning, e.rt.machines["web"].State)

	e.rt.calls = nil
	code, b = e.do(t, c, "POST", "/v1/microvms/web/start", nil)
	require.Equal(t, 200, code, string(b))
	assert.Equal(t, "running", microVMOf(t, b).State)
	assert.Empty(t, e.rt.calls, "a start of a running microVM does not reach mvmd")
}

func TestStopStart_AReaderIsRefusedAndTheProvisionerIsNot(t *testing.T) {
	e := start(t)
	prov, reader := e.as(t, ProvisionerOrganization), e.as(t, ReaderOrganization)
	code, _ := e.do(t, prov, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code)

	code, _ = e.do(t, reader, "POST", "/v1/microvms/web/stop", nil)
	assert.Equal(t, 403, code)
	assert.Equal(t, mvmd.StateRunning, e.rt.machines["web"].State, "the refused stop changed nothing")
	code, _ = e.do(t, prov, "POST", "/v1/microvms/web/stop", nil)
	require.Equal(t, 200, code, "the control: the provisioner's stop is accepted")

	code, _ = e.do(t, reader, "POST", "/v1/microvms/web/start", nil)
	assert.Equal(t, 403, code)
	assert.Equal(t, mvmd.StateStopped, e.rt.machines["web"].State, "the refused start changed nothing")
	code, _ = e.do(t, prov, "POST", "/v1/microvms/web/start", nil)
	assert.Equal(t, 200, code, "the control: the provisioner's start is accepted")
}

func TestStopStart_AnAbsentNameIs404(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	for _, verb := range []string{"stop", "start"} {
		code, _ := e.do(t, c, "POST", "/v1/microvms/web/"+verb, nil)
		assert.Equal(t, 404, code, verb)
	}
}

func TestStopStart_AStartMvmdRefusesInTheStateIs409(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code)
	m := e.rt.machines["web"]
	m.State = mvmd.StatePaused
	e.rt.machines["web"] = m
	e.rt.failAt["start web"] = mvmd.ErrPrecondition

	code, _ = e.do(t, c, "POST", "/v1/microvms/web/start", nil)

	assert.Equal(t, 409, code)
}

func TestRestartOnBoot_DefaultsToTrueForTheOSFormAndFalseForTheImageFormAndIsShown(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, b := e.do(t, c, "POST", "/v1/microvms", osBody())
	require.Equal(t, 201, code, string(b))
	assert.True(t, e.rt.machines["web"].RestartOnBoot, "the OS form's default")
	assert.True(t, microVMOf(t, b).RestartOnBoot)

	code, b = e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 409, code, "the control: the same name with the image form's body is another request")
	body := createBody()
	body["name"] = "img"
	code, b = e.do(t, c, "POST", "/v1/microvms", body)
	require.Equal(t, 201, code, string(b))
	assert.False(t, e.rt.machines["img"].RestartOnBoot, "the image form's default")
	assert.False(t, microVMOf(t, b).RestartOnBoot)
}

func TestRestartOnBoot_AChoiceOverridesTheDefaultInBothForms(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	os := osBody()
	os["restartOnBoot"] = false
	code, b := e.do(t, c, "POST", "/v1/microvms", os)
	require.Equal(t, 201, code, string(b))
	assert.False(t, e.rt.machines["web"].RestartOnBoot)

	img := createBody()
	img["name"] = "img"
	img["restartOnBoot"] = true
	code, b = e.do(t, c, "POST", "/v1/microvms", img)
	require.Equal(t, 201, code, string(b))
	assert.True(t, e.rt.machines["img"].RestartOnBoot)
}

func TestRestartOnBoot_ARepeatThatChangesItIs409AndAnEqualOneIs200(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", osBody())
	require.Equal(t, 201, code)

	code, _ = e.do(t, c, "POST", "/v1/microvms", osBody())
	assert.Equal(t, 200, code, "the control: the same body is the same microVM")
	other := osBody()
	other["restartOnBoot"] = false
	code, _ = e.do(t, c, "POST", "/v1/microvms", other)
	assert.Equal(t, 409, code)
}

func TestMicroVMs_AFailedRestartAfterTheHostsBootShowsMvmdsReasonInGet(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", osBody())
	require.Equal(t, 201, code)
	reason := "the restart after the host's boot changed failed: the boot set is not on this node"
	m := e.rt.machines["web"]
	m.State, m.Failure = mvmd.StateStopped, reason
	e.rt.machines["web"] = m

	code, b := e.do(t, c, "GET", "/v1/microvms/web", nil)

	require.Equal(t, 200, code)
	v := microVMOf(t, b)
	assert.Equal(t, "stopped", v.State)
	assert.Equal(t, reason, v.Failure)
}
