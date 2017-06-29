package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/peytonmcnully/docker-backup/backup"
)

const (
	defaultAddr  = "/var/run/docker.sock"
	defaultProto = "unix"
)

var (
	addr    = flag.String("addr", defaultAddr, "address to connect to")
	proto   = flag.String("proto", defaultProto, "protocol to use (unix, tcp)")
	metrics = flag.Bool("metrics", false, "print some metrics for prometheus consumption")
	direct  = flag.Bool("direct", false, "backup directly at data container")
)

func main() {
	flag.Parse()

	if flag.NArg() < 2 {
		log.Fatal("Syntax: store|restore filename [container-id]")
	}

	action := flag.Arg(0)
	filename := flag.Arg(1)

	begin := time.Now()
	switch action {
	case "store":
		if flag.NArg() < 3 {
			log.Fatal("Error: `store` requires a container-id")
		}
		containerId := flag.Arg(2)
		log.Printf("Storing %s's volume container as %s", containerId, filename)
		file, err := os.Create(filename)
		if err != nil {
			log.Fatal(err)
		}
		b := backup.NewBackup(*addr, *proto, file)
		n := uint(0)
		if *direct {
			n, err = b.VolumeContainerStore(containerId)
		} else {
			n, err = b.Store(containerId)
		}
		if err != nil {
			log.Fatal(err)
		}

		if *metrics {
			now := time.Now()
			fmt.Printf("duration_store{container=\"%s\"} %f %d\n", containerId, time.Since(begin).Seconds(), now.Unix())
			fmt.Printf("bytes_stored{container=\"%s\"} %d %d\n", containerId, n, now.Unix())
		}
	case "restore":
		log.Printf("Restoring %s", filename)
		file, err := os.Open(filename)
		if err != nil {
			log.Fatal(err)
		}
		b := backup.NewBackup(*addr, *proto, file)
		if err := b.Restore(); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatalf("Invalid action %s", action)
	}
}
