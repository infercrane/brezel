package buildinfo

import "testing"

func TestCurrentAlwaysHasIdentityFields(t *testing.T) {
	info := Current()
	if info.Version == "" || info.Revision == "" || info.BuiltAt == "" {
		t.Fatalf("incomplete build info: %+v", info)
	}
}
