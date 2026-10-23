// Command portical manages UPnP port forwards for Docker containers and
// systemd units. Everything it does is in internal/cli; this is only the
// process around it.
package main

import (
	"context"
	"os"

	"github.com/danielbodart/portical/internal/cli"
)

func main() {
	os.Exit(cli.Main(context.Background(), &cli.Container{}))
}
