package importcheck_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/importcheck/internal/importcheck"
)

func check(t *testing.T, fixture string) importcheck.Result {
	t.Helper()
	res, err := importcheck.Check(filepath.Join("testdata", fixture))
	require.NoError(t, err)
	return res
}

// The clean tree holds every import a unit may make: lib/, its own packages, another module of its
// own unit (tooling/qemu imports tooling/disk) and external modules. The cases below differ from it by
// one import, so a failure there is that import's.
func TestCheck_CleanTreePasses(t *testing.T) {
	res := check(t, "clean")
	assert.Empty(t, res.Violations)
	assert.Equal(t, 7, res.Modules)
	assert.Equal(t, 9, res.Files)
}

func TestCheck_UnitImportingAnotherUnitsInternalPackageFails(t *testing.T) {
	res := check(t, "crossunit")
	require.Len(t, res.Violations, 1)
	v := res.Violations[0]
	assert.Equal(t, "naructl/naructl.go", v.File)
	assert.Equal(t, "fixture.test/crossunit/host/internal/api", v.Import)
	assert.Equal(t, "naructl", v.FromUnit)
	assert.Equal(t, "host", v.ToUnit)
	assert.Contains(t, v.String(), "unit naructl/ imports fixture.test/crossunit/host/internal/api")
}

func TestCheck_LibImportingAUnitFails(t *testing.T) {
	res := check(t, "libtounit")
	require.Len(t, res.Violations, 1)
	v := res.Violations[0]
	assert.Equal(t, "lib/shared/shared.go", v.File)
	assert.Equal(t, "lib", v.FromUnit)
	assert.Equal(t, "node", v.ToUnit)
	assert.Contains(t, v.String(), "lib/ imports no unit")
}

func TestCheck_ReadsATaggedTestFile(t *testing.T) {
	res := check(t, "testfile")
	require.Len(t, res.Violations, 1)
	assert.Equal(t, "host/host_qemu_test.go", res.Violations[0].File)
}

// A path that begins with another module's path but not at an element boundary is not in that module.
func TestCheck_PathSharingOnlyAPrefixWithAModuleIsExternal(t *testing.T) {
	res := check(t, "prefix")
	assert.Empty(t, res.Violations)
}

func TestCheck_RootWithNoModuleFails(t *testing.T) {
	_, err := importcheck.Check(filepath.Join("testdata", "empty"))
	assert.ErrorContains(t, err, "no go.mod")
}
