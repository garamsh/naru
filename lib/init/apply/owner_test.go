package apply_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/apply"
)

func TestApply_ANewOwnerOfAFileRewritesIt(t *testing.T) {
	r := newRig(t)

	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"files": []any{obj{"path": "/run/f1", "owner": obj{"uid": 6002, "gid": 6002}}}}), Mode: apply.NoReboot})

	require.NoError(t, err)
	assert.Contains(t, r.calls(), "file /run/f1", "the content and mode are the same, the owner is not")
}

func TestApply_AMetricsListenerNeedsAReboot(t *testing.T) {
	r := newRig(t)

	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"metrics": obj{"listen": "10.0.0.1"}}), Mode: apply.NoReboot})

	var invalid *apply.InvalidError
	require.ErrorAs(t, err, &invalid)
	assert.Equal(t, []string{"spec.metrics"}, invalid.Fields)
}
