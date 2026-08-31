package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/cortex-go/cortex/internal/app"
)

const defaultListen = "127.0.0.1:7331"

var version = "0.1.0-dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "service":
			os.Exit(runService(os.Args[2:], version))
		case "serve":
			os.Args = append(os.Args[:1], os.Args[2:]...)
		}
	}
	listen := flag.String("listen", defaultListen, "HTTP listen address")
	root := flag.String("root", "", "workspace root (default: home directory)")
	data := flag.String("data", "", "Cortex data directory")
	trustProxy := flag.Bool("trust-proxy", false, "trust forwarding headers from a direct loopback reverse proxy")
	publicOrigin := flag.String("public-origin", "", "canonical external origin, for example https://cortex.example.com")
	flag.Parse()
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	if *root == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			*root = home
		} else {
			*root = cwd
		}
	}
	if *data == "" {
		if d, err := os.UserConfigDir(); err == nil {
			*data = filepath.Join(d, "cortex")
		} else {
			*data = filepath.Join(cwd, ".cortex")
		}
	}
	srv, err := app.New(app.Options{Listen: *listen, Root: *root, DataDir: *data, TrustProxy: *trustProxy, PublicOrigin: *publicOrigin})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()
	fmt.Printf("Cortex · http://%s\nWorkspace root · %s\n", *listen, srv.Root())
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
