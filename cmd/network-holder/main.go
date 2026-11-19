package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/thebsdbox/kube-vm-controller/internal/podnetwork"
)

func main() {
	check := flag.Bool("check", false, "Check the local network-holder socket.")
	socket := flag.String("socket", "/network/network.sock", "Per-VM attachment socket.")
	state := flag.String("state", "/state/network.json", "Captured CNI configuration.")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	id := podnetwork.Identity{VMUID: os.Getenv("VM_UID"), PodUID: os.Getenv("POD_UID"), IPAddress: os.Getenv("POD_IP")}
	if *check {
		if _, err := podnetwork.Status(ctx, *socket, id); err != nil {
			log.Fatal(err)
		}
		return
	}
	cfg, tap, err := podnetwork.Prepare(id, *state)
	if err != nil {
		log.Fatal(err)
	}
	defer tap.Close()
	if err := podnetwork.Serve(ctx, *socket, cfg, tap); err != nil {
		log.Fatal(err)
	}
}
