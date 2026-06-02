package ghdata

import (
	"testing"

	"github.com/wow-look-at-my/testify/assert"
)

func TestRollupState(t *testing.T) {
	assert.Equal(t, "", rollupState(nil))
	assert.Equal(t, "SUCCESS", rollupState([]string{"SUCCESS", "SUCCESS"}))
	assert.Equal(t, "PENDING", rollupState([]string{"SUCCESS", "PENDING"}))
	assert.Equal(t, "FAILURE", rollupState([]string{"SUCCESS", "PENDING", "FAILURE"}))
	assert.Equal(t, "FAILURE", rollupState([]string{"ERROR"}))
	assert.Equal(t, "", rollupState([]string{"WEIRD"}))
}
