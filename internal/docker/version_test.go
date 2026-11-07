package docker

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/gokit/logx/logtest"
)

// versioned is the Engine API's versioning in front of a fake Docker: GET
// /version says which versions it speaks, and a request in any other is
// refused the way Docker refuses it.
type versioned struct {
	mu             sync.Mutex
	oldest, newest string
	// /version fails, as on a daemon that is still starting.
	broken bool
	// Every /version asked, and every other request's version.
	negotiations int
	asked        []string
	next         http.Handler
}

func (v *versioned) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	oldest, newest, broken := v.oldest, v.newest, v.broken
	if r.URL.Path == "/version" {
		v.negotiations++
	}
	v.mu.Unlock()

	if r.URL.Path == "/version" {
		if broken {
			refuse(w, http.StatusInternalServerError, "starting up")
			return
		}
		answer(w, http.StatusOK, map[string]string{"Version": "29.0.0", "ApiVersion": newest, "MinAPIVersion": oldest})
		return
	}
	version, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v"), "/")
	v.mu.Lock()
	v.asked = append(v.asked, version)
	v.mu.Unlock()
	switch {
	case compareVersions(version, oldest) < 0:
		refuse(w, http.StatusBadRequest, fmt.Sprintf("client version %s is too old. Minimum supported API version is %s, please upgrade your client to a newer version", version, oldest))
	case compareVersions(version, newest) > 0:
		refuse(w, http.StatusBadRequest, fmt.Sprintf("client version %s is too new. Maximum supported API version is %s", version, newest))
	default:
		v.next.ServeHTTP(w, r)
	}
}

// speaks makes this Docker speak oldest to newest from now on, as an upgrade
// or downgrade under a running client would.
func (v *versioned) speaks(oldest, newest string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.oldest, v.newest = oldest, newest
}

func (v *versioned) fail(broken bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.broken = broken
}

func (v *versioned) versions() (negotiations int, asked []string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.negotiations, slices.Clone(v.asked)
}

func loggedClient(docker *fakeDocker) (*Client, *logtest.Logger) {
	log := &logtest.Logger{}
	return NewClient(httpx.StreamingHandlerTransport(docker), log), log
}

func list(t *testing.T, client *Client) {
	t.Helper()
	if _, err := client.Containers(ctx, ForwardLabel); err != nil {
		t.Fatal(err)
	}
}

func TestSpeaksTheNewestVersionADockerThatHasDroppedOldOnesOffers(t *testing.T) {
	docker := newFakeDocker()
	docker.api.speaks("1.44", "1.52")
	client, log := loggedClient(docker)

	list(t, client)

	if _, asked := docker.api.versions(); !slices.Equal(asked, []string{"1.52", "1.52"}) {
		t.Errorf("asked in %v", asked)
	}
	if entries := log.Entries(); len(entries) != 1 || entries[0].Text != "Speaking Docker Engine API v1.52 (Docker offers v1.44 to v1.52)" {
		t.Errorf("logged %v", entries)
	}
}

func TestSpeaksNoNewerThanItWasWrittenFor(t *testing.T) {
	docker := newFakeDocker()
	docker.api.speaks("1.44", "1.99")

	list(t, docker.client())

	if _, asked := docker.api.versions(); asked[0] != NewestAPI {
		t.Errorf("asked in %v", asked)
	}
}

func TestSpeaksTheOldestADockerAcceptsWhenThatIsNewerThanItWasWrittenFor(t *testing.T) {
	docker := newFakeDocker()
	docker.api.speaks("1.98", "1.99")

	list(t, docker.client())

	if _, asked := docker.api.versions(); asked[0] != "1.98" {
		t.Errorf("asked in %v", asked)
	}
}

func TestSpeaksAnOldDockersNewestVersion(t *testing.T) {
	docker := newFakeDocker()
	// Docker 24.
	docker.api.speaks("1.12", "1.43")

	list(t, docker.client())

	if _, asked := docker.api.versions(); asked[0] != "1.43" {
		t.Errorf("asked in %v", asked)
	}
}

func TestNegotiatesOnceAndKeepsTheVersion(t *testing.T) {
	docker := newFakeDocker()
	client := docker.client()

	list(t, client)
	list(t, client)

	if negotiations, asked := docker.api.versions(); negotiations != 1 || len(asked) != 4 {
		t.Errorf("negotiated %d times, asked %v", negotiations, asked)
	}
}

func TestSpeaksTheNewestItWasWrittenForAndSaysWhyWhenDockerCannotSayWhatItSpeaks(t *testing.T) {
	docker := newFakeDocker()
	docker.api.fail(true)
	client, log := loggedClient(docker)

	list(t, client)

	if _, asked := docker.api.versions(); asked[0] != NewestAPI {
		t.Errorf("asked in %v", asked)
	}
	entries := log.Entries()
	if len(entries) != 1 || !entries[0].Error ||
		!strings.Contains(entries[0].Text, "Docker API /version failed, so speaking v"+NewestAPI+" until Docker says otherwise") ||
		!strings.Contains(entries[0].Text, "starting up") {
		t.Errorf("logged %v", entries)
	}
}

func TestRenegotiatesWhenDockerIsUpgradedPastTheVersionSpoken(t *testing.T) {
	docker := newFakeDocker()
	docker.api.speaks("1.24", "1.43")
	client := docker.client()
	list(t, client)

	// Docker 29.0, which refused anything before 1.44.
	docker.api.speaks("1.44", "1.52")
	list(t, client)

	negotiations, asked := docker.api.versions()
	if negotiations != 2 || !slices.Equal(asked, []string{"1.43", "1.43", "1.43", "1.52", "1.52"}) {
		t.Errorf("negotiated %d times, asked in %v", negotiations, asked)
	}
}

func TestRenegotiatesWhenTheVersionSpokenWithoutAskingIsTooNew(t *testing.T) {
	docker := newFakeDocker()
	docker.api.speaks("1.24", "1.43")
	docker.api.fail(true)
	client := docker.client()
	// Refused for its version, and asked again in the same, since Docker
	// still cannot say what it speaks.
	if _, err := client.Containers(ctx, ForwardLabel); err == nil {
		t.Fatal("expected Docker to refuse it")
	}
	docker.api.fail(false)

	list(t, client)

	if _, asked := docker.api.versions(); !slices.Equal(asked, []string{NewestAPI, NewestAPI, NewestAPI, "1.43", "1.43"}) {
		t.Errorf("asked in %v", asked)
	}
}

func TestAsksAgainOnlyOnceWhenDockerStillRefusesTheVersion(t *testing.T) {
	docker := newFakeDocker()
	// Says it speaks 1.52 but refuses it, as nothing real does.
	docker.api.speaks("1.53", "1.52")
	client := docker.client()

	_, err := client.Containers(ctx, ForwardLabel)

	if err == nil || !strings.Contains(err.Error(), "client version 1.53 is too new") {
		t.Errorf("got %v", err)
	}
	if negotiations, asked := docker.api.versions(); negotiations != 2 || len(asked) != 2 {
		t.Errorf("negotiated %d times, asked in %v", negotiations, asked)
	}
}

func TestComparesVersionsByNumberPartByPart(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.9", "1.10", -1},
		{"1.44", "1.44", 0},
		{"1.52", "1.44", 1},
		{"2.0", "1.99", 1},
		{"1.44", "1.44.0", 0},
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
