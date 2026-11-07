package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/danielbodart/gokit/httpx"
)

// NewestAPI is the newest Engine API version this client asks for.
//
// The version is negotiated with the daemon, as Docker's own client does:
// the daemon's newest, capped at this, and never below the oldest the daemon
// still accepts. The cap keeps a newer Docker answering in a shape the
// structs here have been read against - every change Docker lists from v1.41,
// which they were written for, up to this one - so it is a ceiling, not a
// requirement: a Docker newer than it is spoken to at this version, and one
// whose oldest is newer still at that oldest. Nothing has to move here for a
// new Docker to work.
const NewestAPI = "1.56"

// apiVersion is the Engine API version to speak, negotiated with the daemon
// on first use and kept until a call says the daemon no longer accepts it.
//
// When Docker cannot say which versions it speaks, NewestAPI is spoken and
// the reason logged: a daemon that cannot answer /version cannot answer much
// else either, and once it can, a call it refuses for its version
// renegotiates.
func (c *Client) apiVersion(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.version != "" {
		return c.version, nil
	}
	offered, err := c.offered(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("Docker API /version failed: %w", err)
		}
		c.log.Error(fmt.Sprintf("Docker API /version failed, so speaking v%s until Docker says otherwise: %s", NewestAPI, err))
		c.version = NewestAPI
		return c.version, nil
	}
	c.version = offered.choose()
	c.log.Log(fmt.Sprintf("Speaking Docker Engine API v%s (Docker offers %s)", c.version, offered))
	return c.version, nil
}

// forget drops version, the one a call was refused for, so the next call
// negotiates afresh - unless another call has already done so.
func (c *Client) forget(version string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.version == version {
		c.version = ""
	}
}

// offer is the range of Engine API versions a daemon accepts.
type offer struct {
	Newest string `json:"ApiVersion"`
	Oldest string `json:"MinAPIVersion"`
}

func (o offer) String() string {
	if o.Oldest == "" {
		return "up to v" + o.Newest
	}
	return "v" + o.Oldest + " to v" + o.Newest
}

// choose is the version to speak: the daemon's newest, capped at NewestAPI,
// and never below its oldest.
func (o offer) choose() string {
	version := o.Newest
	if compareVersions(version, NewestAPI) > 0 {
		version = NewestAPI
	}
	if o.Oldest != "" && compareVersions(version, o.Oldest) < 0 {
		version = o.Oldest
	}
	return version
}

// offered asks the daemon which versions it speaks. /version is the one
// endpoint asked without a version, since it is how the version is found.
func (c *Client) offered(ctx context.Context) (offer, error) {
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/version", nil)
	if err != nil {
		return offer{}, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return offer{}, err
	}
	if err := httpx.Check(response); err != nil {
		return offer{}, err
	}
	defer drain(response)
	var found offer
	if err := json.NewDecoder(response.Body).Decode(&found); err != nil {
		return offer{}, err
	}
	if found.Newest == "" {
		return offer{}, errors.New("Docker did not say which API version it speaks")
	}
	return found, nil
}

// refusedVersion is the reason Docker gives for refusing a request for the
// version it asked in, whichever way it is out of range.
var refusedVersion = regexp.MustCompile(`client version \S+ is too (old|new)`)

// versionRefused says whether err is Docker refusing a call for the version
// it was asked in: a daemon upgraded or downgraded under a running client.
func versionRefused(err error) bool {
	var status *httpx.StatusError
	return errors.As(err, &status) && status.StatusCode == http.StatusBadRequest && refusedVersion.MatchString(status.Body)
}

// compareVersions orders two dotted versions by number, part by part, so
// 1.9 comes before 1.10. A part that is not a number counts as 0.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(as), len(bs)) {
		if diff := part(as, i) - part(bs, i); diff != 0 {
			if diff < 0 {
				return -1
			}
			return 1
		}
	}
	return 0
}

func part(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, _ := strconv.Atoi(parts[i])
	return n
}
