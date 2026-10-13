package randexec_test

import "testing"

func TestOverlappingDirectoryMinimal(t *testing.T) {
	minimalOutputCase(t, "mkdir -p tree/nested; printf payload > tree/nested/file", []string{"tree", "tree/nested"})
}
