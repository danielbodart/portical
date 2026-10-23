package version

import "testing"

// What `go run .` and `go test` see, with no -ldflags to say otherwise.
func TestAnUnstampedBuildSaysItIsDevelopment(t *testing.T) {
	if Version != "development" {
		t.Errorf("got %q", Version)
	}
}
