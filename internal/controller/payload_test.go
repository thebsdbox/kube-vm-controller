package controller

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vmv1 "github.com/thebsdbox/kube-vm-controller/api/v1alpha1"
)

func ptr[T any](v T) *T { return &v }

func testVM() *vmv1.VirtualMachine {
	return &vmv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Namespace: "my.ns", Name: "web server", UID: "11111111-2222-3333-4444-555555555555"},
		Spec: vmv1.VirtualMachineSpec{
			CPUs: 2, MemoryMiB: 2048,
			Boot: &vmv1.BootSpec{Kernel: "bzImage", Cmdline: "console=hvc0"},
		},
	}
}

func TestVMID(t *testing.T) {
	vm := testVM()
	id := VMID(vm)
	if id != "my.ns-web-server-"+id[len(id)-8:] || len(id) != len("my.ns-web-server-")+8 {
		t.Fatalf("unexpected id %q", id)
	}
	vm.Name = strings.Repeat("a", 100)
	if id := VMID(vm); len(id) != maxVMIDPrefix+1+8 {
		t.Fatalf("id %q not truncated to %d chars", id, maxVMIDPrefix+9)
	}
	other := testVM()
	other.UID = "other"
	if VMID(testVM()) == VMID(other) {
		t.Fatal("recreated object must get a different id")
	}
}

func TestLaunchHash(t *testing.T) {
	a, b := testVM(), testVM()
	if LaunchHash(&a.Spec) != LaunchHash(&b.Spec) {
		t.Fatal("identical specs must hash equally")
	}
	b.Spec.Idle = &vmv1.IdleSpec{PauseAfter: &metav1.Duration{Duration: time.Minute}}
	b.Spec.RunStrategy = vmv1.RunStrategyOnce
	b.Spec.Paused = true
	b.Spec.Provisioning = &vmv1.ProvisioningSpec{}
	if LaunchHash(&a.Spec) != LaunchHash(&b.Spec) {
		t.Fatal("idle, runStrategy, paused and provisioning must not affect the launch hash")
	}
	b.Spec.MemoryMiB = 4096
	if LaunchHash(&a.Spec) == LaunchHash(&b.Spec) {
		t.Fatal("memory change must alter the launch hash")
	}
}

func TestBuildStartPayload(t *testing.T) {
	vm := testVM()
	vm.Spec.Boot.Initrd = "initrd.img"
	vm.Spec.MemoryBalancing = &vmv1.MemoryBalancing{MinMiB: ptr(int32(512))}
	vm.Spec.Disks = []vmv1.Disk{
		{BaseDisk: "ubuntu"},
		{Path: "/data/a.img", ReadOnly: true},
		{Path: "/data/b.img", Overlay: "/data/b.ovl"},
		{Memory: ptr(resource.MustParse("1Mi"))},
		{URL: "https://example.com/x.img"},
	}
	vm.Spec.Network = []vmv1.NetworkInterface{
		{Type: vmv1.NetworkTypeNAT},
		{Type: vmv1.NetworkTypeTAP, TapName: "tap9", MAC: "aa:bb:cc:dd:ee:ff", Switch: true},
	}
	vm.Spec.Vsock = &vmv1.VsockSpec{Port: 1024, CID: ptr(int64(5))}
	vm.Spec.IOUring = &vmv1.IOUringSpec{QueueDepth: 32}
	vm.Spec.Idle = &vmv1.IdleSpec{PauseAfter: &metav1.Duration{Duration: time.Minute}, WakeOnNetwork: ptr(false)}

	p := BuildStartPayload(vm, nil)
	if p["uid"] != VMID(vm) || p["mem"] != int32(2048) || p["cpu"] != int32(2) {
		t.Fatalf("core fields wrong: %v", p)
	}
	if p["base_disk"] != "ubuntu" {
		t.Fatalf("base_disk = %v", p["base_disk"])
	}
	wantBlocks := []string{"/data/a.img:ro", "cow:/data/b.img?overlay=/data/b.ovl", "mem:1048576", "https://example.com/x.img"}
	got := p["block"].([]string)
	if strings.Join(got, ",") != strings.Join(wantBlocks, ",") {
		t.Fatalf("blocks = %v, want %v", got, wantBlocks)
	}
	nics := p["nics"].([]map[string]any)
	if nics[0]["nat"] != true || nics[1]["nat"] != false || nics[1]["tap"] != "tap9" || nics[1]["switch"] != true {
		t.Fatalf("nics = %v", nics)
	}
	if p["memory_balancing"] != true || p["memory_min"] != int32(512) {
		t.Fatalf("balancing = %v %v", p["memory_balancing"], p["memory_min"])
	}
	if p["vsock"] != int64(1024) || p["vsock-cid"] != int64(5) || p["io_uring_block"] != true || p["io_uring_queue_depth"] != int32(32) {
		t.Fatalf("vsock/io_uring wrong: %v", p)
	}
	if p["activity_timeout"] != "1m0s" || p["activity_disk_timeout"] != "0s" || p["wake_on_network"] != false {
		t.Fatalf("idle wrong: %v", p)
	}
	if _, ok := p["provision"]; ok {
		t.Fatal("provision must be absent without provisioning")
	}
}

func TestResolveProvisioning(t *testing.T) {
	vm := testVM()
	vm.Spec.Provisioning = &vmv1.ProvisioningSpec{
		Files: []vmv1.ProvisionFile{
			{Path: "/etc/a", Content: ptr("hello"), Mode: "0644", UID: ptr(int32(1000))},
			{Path: "/etc/b", SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "k"}},
			{Path: "/etc/c", ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "cm"}, Key: "k"}},
			{Path: "/var/dir", Directory: true},
		},
		SSH: &vmv1.SSHAccess{User: "ubuntu"},
	}
	r := fake.NewClientBuilder().WithObjects(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "my.ns", Name: "s"}, Data: map[string][]byte{"k": []byte("secret")}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "my.ns", Name: "cm"}, Data: map[string]string{"k": "config"}},
	).Build()

	pp, err := ResolveProvisioning(context.Background(), r, vm)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"hello", "secret", "config"}
	for i, w := range want {
		raw, _ := base64.StdEncoding.DecodeString(pp.Files[i]["data"].(string))
		if string(raw) != w {
			t.Fatalf("file %d = %q, want %q", i, raw, w)
		}
	}
	if pp.Files[0]["mode"] != "0644" || pp.Files[0]["uid"] != int32(1000) {
		t.Fatalf("metadata lost: %v", pp.Files[0])
	}
	if pp.Files[3]["directory"] != true || pp.Files[3]["data"] != nil {
		t.Fatalf("directory entry wrong: %v", pp.Files[3])
	}
	if pp.SSH["user"] != "ubuntu" {
		t.Fatalf("ssh = %v", pp.SSH)
	}
	if p := BuildStartPayload(vm, pp); p["provision"] == nil || p["ssh_access"] == nil {
		t.Fatalf("provisioning missing from payload: %v", p)
	}

	vm.Spec.Provisioning.Files[1].SecretKeyRef.Key = "missing"
	if _, err := ResolveProvisioning(context.Background(), r, vm); err == nil {
		t.Fatal("missing secret key must be an error")
	}
	vm.Spec.Provisioning.Files[1].SecretKeyRef.Optional = ptr(true)
	if _, err := ResolveProvisioning(context.Background(), r, vm); err != nil {
		t.Fatalf("optional missing key: %v", err)
	}
}
