package docker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/portical/internal/upnp"
)

// DefaultHelperImage is the image used for the throwaway relay container.
//
// It is Portical's own image running Portical's own `relay` command - there is
// no second image to build or publish. This is only the fallback for when we
// cannot work out what we are running as, which matters to anyone on a fork, a
// pinned tag or a locally built image.
const DefaultHelperImage = "danielbodart/portical:latest"

// RelayRequestTimeout bounds one request made through a helper container:
// creating and starting it, its own RelayTimeout, and reading its output.
//
// The TypeScript version had no bound here at all, so a Docker that stopped
// answering mid-relay held a pass, and so the daemon, forever.
const RelayRequestTimeout = time.Minute

// mountedFrom finds a container id in /proc/self/mountinfo.
var mountedFrom = regexp.MustCompile(`/(?:docker/)?containers/([0-9a-f]{64})/`)

// OwnImage is the image this process is running as, or "" if it cannot be
// worked out.
//
// A container cannot simply ask what image it came from, and Portical usually
// runs with host networking, so its hostname is the host's rather than its own
// container id. But Docker bind-mounts /etc/hostname and friends from
// /var/lib/docker/containers/<id>/, and those paths are visible in
// /proc/self/mountinfo whatever the network mode - so the id is recoverable,
// and the Engine API turns it into an image name. mountinfo is that file's
// contents.
func (c *Client) OwnImage(ctx context.Context, mountinfo []byte) string {
	match := mountedFrom.FindSubmatch(mountinfo)
	if match == nil {
		return ""
	}
	// Not in a container, or Docker declined to say. The default is a
	// perfectly good answer, so this is not worth reporting.
	image, err := c.ImageOf(ctx, string(match[1]))
	if err != nil {
		return ""
	}
	return image
}

// ViaContainer is a transport that makes its requests from inside
// container's network namespace.
//
// Because everything in Portical reaches the network through an
// http.RoundTripper, this is all it takes to speak to the gateway *as* a
// macvlan container: the UPnP client, reconcile and the daemon are unchanged
// and unaware.
//
// It works by starting a throwaway container from Portical's own image, joined
// to the target's network namespace, which performs the one request and prints
// the result. That is v1's trick, with an HTTP request relayed instead of a
// command line. It costs several Docker API calls and a container start per
// request, so it is used only for a forward whose target is the container's
// own address.
func ViaContainer(client *Client, container, image string) http.RoundTripper {
	return httpx.RoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		relayed, err := ToRelayedRequest(request)
		if request.Body != nil {
			request.Body.Close()
		}
		if err != nil {
			return nil, err
		}
		output, err := client.RunInNetworkOf(request.Context(), container, image, []string{"relay", EncodeRelay(relayed)})
		if err != nil {
			return nil, err
		}
		result, err := ParseRelayOutput(output)
		if err != nil {
			return nil, err
		}
		return &http.Response{
			Status:     fmt.Sprintf("%d %s", result.Status, http.StatusText(result.Status)),
			StatusCode: result.Status,
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			// SOAP faults arrive as HTTP 500 with the reason in the body, so
			// the body is passed on whatever the status.
			Header:        http.Header{"Content-Type": {`text/xml; charset="utf-8"`}},
			Body:          io.NopCloser(strings.NewReader(result.Body)),
			ContentLength: int64(len(result.Body)),
			Request:       request,
		}, nil
	})
}

// AsContainer is the gateway, asked from inside the network of whichever
// container is named - the daemon's Deps.AsContainer.
//
// image is Portical's own image; "" is DefaultHelperImage.
func AsContainer(client *Client, gateway *upnp.Client, image string) func(owner string) upnp.Gateway {
	if image == "" {
		image = DefaultHelperImage
	}
	return func(owner string) upnp.Gateway {
		relayed := &http.Client{Transport: ViaContainer(client, owner, image), Timeout: RelayRequestTimeout}
		return upnp.New(relayed, gateway.ControlURL, gateway.ServiceType)
	}
}
