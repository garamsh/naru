package modcheck_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/importcheck/internal/modcheck"
)

func check(t *testing.T, fixture string) modcheck.Result {
	t.Helper()
	res, err := modcheck.Check(filepath.Join("testdata", fixture))
	require.NoError(t, err)
	return res
}

// Both modules of the clean tree require x/sys at one version, and the module a requires b, a
// module of the workspace, which is not compared.
func TestCheck_ModulesAtOneVersionOfEachDependencyPass(t *testing.T) {
	res := check(t, "clean")
	assert.Empty(t, res.Skews)
	assert.Empty(t, res.WrongGo)
	assert.Equal(t, 2, res.Modules)
	assert.Equal(t, 1, res.Dependencies)
}

func TestCheck_ADependencyAtTwoVersionsFails(t *testing.T) {
	res := check(t, "skew")
	require.Len(t, res.Skews, 1)
	assert.Equal(t, "golang.org/x/sys", res.Skews[0].Path)
	assert.Equal(t, map[string][]string{"v0.49.0": {"a"}, "v0.48.0": {"b"}}, res.Skews[0].Modules)
	assert.Contains(t, res.Skews[0].String(), "golang.org/x/sys is required at v0.48.0 by [b]; v0.49.0 by [a];")
}

func TestCheck_AGoDirectiveOtherThanGoWorksFails(t *testing.T) {
	res := check(t, "wronggo")
	assert.Equal(t, []string{"b"}, res.WrongGo)
	assert.Empty(t, res.Skews)
}

func TestCheck_RootWithNoGoWorkFails(t *testing.T) {
	_, err := modcheck.Check(filepath.Join("testdata", "none"))
	assert.Error(t, err)
}
