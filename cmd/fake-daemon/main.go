// Command fake-daemon serves the kube-vm control protocol from memory so the
// controller can be exercised on hosts without KVM. It never boots a VM.
package main

import (
	"flag"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/thebsdbox/kube-vm-controller/internal/kubevm"
)

func main() {
	socket := flag.String("socket", kubevm.DefaultSocket, "control socket path")
	flag.Parse()

	if err := os.MkdirAll(filepath.Dir(*socket), 0o755); err != nil {
		log.Fatal(err)
	}
	_ = os.Remove(*socket)
	l, err := net.Listen("unix", *socket)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.Chmod(*socket, 0o666); err != nil {
		log.Fatal(err)
	}
	log.Printf("fake kube-vm daemon listening on %s", *socket)
	log.Fatal(kubevm.NewFake().Serve(l))
}
