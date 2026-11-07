// checks_test.go keeps demo/verify.sql and the Go check list identical.
// Regenerate the file with `make verify-sql` (go test -run ... -update).
package invariants

import (
	"flag"
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite demo/verify.sql from the Go check list")

const verifySQLPath = "../../demo/verify.sql"

func TestVerifySQLMatchesTheGoChecks(t *testing.T) {
	want := RenderVerifySQL()
	if *update {
		require.NoError(t, os.WriteFile(verifySQLPath, []byte(want), 0o644))
	}
	got, err := os.ReadFile(verifySQLPath)
	require.NoError(t, err)
	assert.Equal(t, want, string(got),
		"demo/verify.sql is out of date with internal/invariants/checks.go: run `make verify-sql`")
}

func TestCheckNamesAreValidLowCardinalityLabels(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z][a-z_]*$`)
	seen := map[string]bool{}
	for _, c := range Checks {
		assert.Regexp(t, valid, c.Name)
		assert.False(t, seen[c.Name], "duplicate check name %q", c.Name)
		seen[c.Name] = true
	}
	assert.Len(t, Checks, 11)
}
