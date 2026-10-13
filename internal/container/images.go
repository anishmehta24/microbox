package container

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/anishmehta24/microbox/internal/image"
)

// Pull implements `microbox pull IMAGE`.
func Pull(args []string) int {
	if len(args) != 1 {
		return usage("usage: microbox pull IMAGE[:TAG]")
	}
	if os.Geteuid() != 0 {
		return fail(errors.New("must run as root: unpacking layers keeps their owners, which needs root"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if _, err := image.Pull(ctx, args[0], os.Stdout); err != nil {
		return fail(err)
	}
	return 0
}

// Images implements `microbox images`: the pulled images, then the root
// filesystems under ImagesDir.
func Images(args []string) int {
	if len(args) > 0 {
		return usage("usage: microbox images")
	}
	imgs, err := image.List()
	if err != nil {
		return fail(err)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "IMAGE\tID\tLAYERS\tSIZE")
	for _, img := range imgs {
		fmt.Fprintf(w, "%s\t%s\t%d\t%.1f MB\n", img.Ref, strings.TrimPrefix(img.ID, "sha256:")[:12], len(img.Layers), float64(img.Size)/(1<<20))
	}
	dirs, err := os.ReadDir(ImagesDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fail(err)
	}
	for _, d := range dirs {
		if d.IsDir() {
			fmt.Fprintf(w, "%s\t(directory)\t\t\n", d.Name())
		}
	}
	w.Flush()
	return 0
}

// Rmi implements `microbox rmi IMAGE...`. It removes the images' records;
// microbox gc removes the layers no image or container uses any more.
func Rmi(args []string) int {
	if len(args) == 0 {
		return usage("usage: microbox rmi IMAGE...")
	}
	code := 0
	for _, ref := range args {
		if err := image.Remove(ref); err != nil {
			fmt.Fprintf(os.Stderr, "microbox: %v\n", err)
			code = 1
			continue
		}
		fmt.Println(ref)
	}
	return code
}

// removeUnusedLayers deletes the layers that neither an image nor an
// existing container uses. A container keeps the layers it was created
// from, even after rmi of its image.
func removeUnusedLayers() error {
	cs, err := list()
	if err != nil {
		return err
	}
	var inUse []string
	for _, c := range cs {
		inUse = append(inUse, c.Config.Rootfs.Lower...)
	}
	removed, err := image.RemoveUnusedLayers(inUse)
	for _, l := range removed {
		fmt.Printf("removed layer %s\n", filepath.Base(l))
	}
	return err
}
