package machines

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/GehirnInc/crypt/sha512_crypt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	"sigs.k8s.io/yaml"

	"github.com/syeyoung-cloud/vmservice/internal/config"
	"github.com/syeyoung-cloud/vmservice/internal/store"
)

// The labels and annotations the service puts on what it creates. Only the service writes to the
// namespace, so they are trusted.
const (
	LabelOwner     = "vmservice/owner"      // the member's stable id (auth.OwnerID), not their username
	LabelMachine   = "vmservice/machine"    // the machine id, also on the VM's pod
	AnnotationUser = "vmservice/owner-name" // the owner's username when the machine was made, for admins
)

// Machine is a member's VM as the API shows it.
type Machine struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Image           string    `json:"image"`
	ImageName       string    `json:"imageName"`
	DefaultUser     string    `json:"defaultUser"` // = User, for older clients
	User            string    `json:"user"`        // the account to sign in as ("" for Windows)
	Owner           string    `json:"owner"`       // the owner's username
	Size            string    `json:"size"`
	CPU             int       `json:"cpu"`
	MemoryMiB       int       `json:"memoryMiB"`
	DiskGiB         int       `json:"diskGiB"`
	Media           string    `json:"media,omitempty"`   // the medium it was made from
	Profile         string    `json:"profile,omitempty"` // its device profile (from media)
	InstallMedia    bool      `json:"installMedia"`      // its install CD-ROMs are still attached (Eject)
	Identity        string    `json:"identity"`          // the name of the identity it wears
	IdentityAccount string    `json:"identityAccount"`
	State           string    `json:"state"`
	Detail          string    `json:"detail"`
	CreatedAt       time.Time `json:"createdAt"`
}

// Request is what a member asks for in POST /api/vms, after checking.
type Request struct {
	Name string // the hostname, unique across everyone
	// OwnerName is the member's username: their account on the machine, and an annotation for
	// admins (the owner label is their stable id, which says nothing to a person).
	OwnerName string
	UID       *int // the account's uid, nil: the distro picks
	Size      config.Size
	DiskGiB   int
	// PasswordHash is the SHA-512 crypt hash of the default user's password; empty: none (the user
	// signs in with SSH keys only, and the console's login prompt can't be passed).
	PasswordHash string
	// AuthorizedKeys are the default user's SSH keys (authorized_keys lines); filled by Create.
	AuthorizedKeys []string
	// MediaID is the image it's made from; ProfileOverride replaces the image's detected profile.
	MediaID         string
	ProfileOverride string
	// Identity is the name of one of the member's identities to wear; "": a new one, named after
	// the machine.
	Identity     string
	identity     string    // its account, resolved by Create
	media        *mediaRef // resolved by Create
	cloudExtras  map[string]any
	profile      string // the profile it gets
	driversImage string
}

// Volumes of a machine made from an ISO, removed by Eject.
const (
	volumeInstall  = "install"  // the ISO, cloned per machine
	volumeDrivers  = "drivers"  // the virtio driver CD (Windows)
	volumeIdentity = "identity" // the identity it wears (identityVolume)
)

func installName(id string) string { return id + "-install" }

const idAlphabet = "abcdefghijkmnpqrstuvwxyz23456789" // no 0/o, 1/l

var idFormat = regexp.MustCompile(`^vm-[a-km-np-z2-9]{6}$`)

// newID is "vm-" and 6 random characters: names are the service's, display names the member's.
func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return "vm-" + string(b)
}

// HashPassword is the SHA-512 crypt hash cloud-init's chpasswd takes, with a random salt.
func HashPassword(password string) (string, error) {
	return sha512_crypt.New().Generate([]byte(password), nil)
}

func diskName(id string) string   { return id + "-root" }
func secretName(id string) string { return id + "-init" }

// buildVM is the VirtualMachine for a request: run strategy Manual (it starts only when asked to,
// and a shutdown from inside the guest leaves it stopped), with its root disk cloned from the image's
// source PVC in the same namespace, cloud-init from the Secret secretName(id), the pod network
// (masquerade) and the default serial console and VNC display.
func buildVM(namespace, id, owner, storageClass string, r Request) *kubevirtv1.VirtualMachine {
	manual := kubevirtv1.RunStrategyManual
	labels := map[string]string{LabelOwner: owner, LabelMachine: id}
	memory := resource.MustParse(strconv.Itoa(r.Size.MemoryMiB) + "Mi")
	storage := &cdiv1.StorageSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceStorage: resource.MustParse(strconv.Itoa(r.DiskGiB) + "Gi"),
		}},
	}
	if storageClass != "" {
		storage.StorageClassName = &storageClass
	}
	vm := &kubevirtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name: id, Namespace: namespace, Labels: labels,
			Annotations: map[string]string{AnnotationUser: r.OwnerName}, // for admins; the row is the authority
		},
		Spec: kubevirtv1.VirtualMachineSpec{
			RunStrategy: &manual,
			DataVolumeTemplates: []kubevirtv1.DataVolumeTemplateSpec{{
				ObjectMeta: metav1.ObjectMeta{Name: diskName(id), Labels: labels},
				Spec: cdiv1.DataVolumeSpec{
					Source:  &cdiv1.DataVolumeSource{Blank: &cdiv1.DataVolumeBlankImage{}}, // applyMedia sets the real source
					Storage: storage,
				},
			}},
			Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{
						CPU:    &kubevirtv1.CPU{Cores: uint32(r.Size.CPU)},
						Memory: &kubevirtv1.Memory{Guest: &memory},
						Resources: kubevirtv1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceMemory: memory},
						},
						Devices: kubevirtv1.Devices{
							Disks: []kubevirtv1.Disk{
								// Everything on virtio-scsi: one controller, TRIM back to the pool, and Windows needs
								// just one driver (vioscsi). 24H2 Setup carries only the last "Load driver" package into
								// the new OS, so a virtio-blk disk (viostor) plus a virtio-scsi CD left it unbootable.
								{Name: "root", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: kubevirtv1.DiskBusSCSI}}},
								{Name: "cloudinit", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: kubevirtv1.DiskBusSCSI}}},
							},
							Interfaces: []kubevirtv1.Interface{{
								Name:                   "default",
								InterfaceBindingMethod: kubevirtv1.InterfaceBindingMethod{Masquerade: &kubevirtv1.InterfaceMasquerade{}},
							}},
						},
					},
					Networks: []kubevirtv1.Network{{Name: "default", NetworkSource: kubevirtv1.NetworkSource{Pod: &kubevirtv1.PodNetwork{}}}},
					Volumes: []kubevirtv1.Volume{
						{Name: "root", VolumeSource: kubevirtv1.VolumeSource{DataVolume: &kubevirtv1.DataVolumeSource{Name: diskName(id)}}},
						{Name: "cloudinit", VolumeSource: kubevirtv1.VolumeSource{CloudInitNoCloud: &kubevirtv1.CloudInitNoCloudSource{
							UserDataSecretRef: &corev1.LocalObjectReference{Name: secretName(id)},
						}}},
						identityVolume(r.identity),
					},
				},
			},
		},
	}
	applyMedia(vm, id, storage, r)
	return vm
}

// Keys VMService sets in a machine's #cloud-config itself; an image's extras can't touch them.
var reservedCloudKeys = []string{"hostname", "fqdn", "users", "user", "chpasswd", "password", "ssh_pwauth", "ssh_authorized_keys", "disable_root"}

// cloudConfig is the machine's #cloud-config: the image's extras, then what the service sets: the
// hostname, and the member's own account (their username, uid if set, SSH keys, password hash,
// passwordless sudo, bash) next to the image's default one. No SSH password logins.
func cloudConfig(hostname string, r Request) ([]byte, error) {
	doc := map[string]any{}
	for k, v := range r.cloudExtras {
		doc[k] = v
	}
	for _, k := range reservedCloudKeys {
		delete(doc, k)
	}
	doc["hostname"] = hostname
	doc["ssh_pwauth"] = false
	user := map[string]any{
		"name":        r.OwnerName,
		"shell":       "/bin/bash",
		"sudo":        "ALL=(ALL) NOPASSWD:ALL",
		"lock_passwd": r.PasswordHash == "",
	}
	if r.PasswordHash != "" {
		user["passwd"] = r.PasswordHash
	}
	if r.UID != nil {
		user["uid"] = *r.UID
	}
	if len(r.AuthorizedKeys) > 0 {
		user["ssh_authorized_keys"] = r.AuthorizedKeys
	}
	doc["users"] = []any{"default", user}
	body, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return append([]byte("#cloud-config\n"), body...), nil
}

// buildSecret is the cloud-init Secret, owned by the VM so it goes when the VM does.
// identityVolume has the launcher pod run as the identity's ServiceAccount. No disk refers to it,
// so the guest never sees the token; the metadata relay uses it (package metadata).
func identityVolume(account string) kubevirtv1.Volume {
	return kubevirtv1.Volume{Name: volumeIdentity, VolumeSource: kubevirtv1.VolumeSource{
		ServiceAccount: &kubevirtv1.ServiceAccountVolumeSource{ServiceAccountName: account},
	}}
}

func buildSecret(vm *kubevirtv1.VirtualMachine, userData []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretName(vm.Name), Namespace: vm.Namespace,
			Labels: map[string]string{LabelOwner: vm.Labels[LabelOwner], LabelMachine: vm.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kubevirtv1.SchemeGroupVersion.String(), Kind: "VirtualMachine", Name: vm.Name, UID: vm.UID,
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"userdata": userData},
	}
}

// missingGrace is how long a machine's row may exist without its VM before that counts as an
// error: the row is written first and the VM a moment later (and the cache sees it later still).
const missingGrace = 2 * time.Minute

// toMachine is the API's view of a machine: its row, and its VM if it exists. disk is its root
// DataVolume, if known (for clone progress).
func toMachine(row store.Machine, vm *kubevirtv1.VirtualMachine, disk *cdiv1.DataVolume, now time.Time) Machine {
	m := Machine{
		ID: row.ID, Name: row.Name, Image: row.Media, ImageName: row.Image, Size: row.Size,
		CPU: row.CPU, MemoryMiB: row.MemoryMiB, DiskGiB: row.DiskGiB, CreatedAt: row.CreatedAt.UTC(),
		Media: row.Media, Profile: row.Profile, InstallMedia: hasInstallMedia(vm), Owner: row.OwnerName,
		Identity: row.IdentityName, IdentityAccount: row.Identity,
	}
	if row.Profile != "windows" {
		m.User, m.DefaultUser = row.OwnerName, row.OwnerName
	}
	switch {
	case vm != nil:
		m.State, m.Detail = state(vm, disk)
	case now.Sub(row.CreatedAt) < missingGrace:
		m.State = "Provisioning"
	default:
		m.State, m.Detail = "Error", "Its VM is missing; delete the machine."
	}
	return m
}

// state maps KubeVirt's printable status onto the API's few states.
func state(vm *kubevirtv1.VirtualMachine, disk *cdiv1.DataVolume) (string, string) {
	if vm.DeletionTimestamp != nil {
		return "Deleting", ""
	}
	s := vm.Status.PrintableStatus
	switch s {
	case kubevirtv1.VirtualMachineStatusStopped:
		return "Stopped", ""
	case kubevirtv1.VirtualMachineStatusProvisioning, kubevirtv1.VirtualMachineStatusWaitingForVolumeBinding, "":
		detail := ""
		if disk != nil && disk.Status.Progress != "" && disk.Status.Progress != "N/A" {
			detail = fmt.Sprintf("Copying the disk: %s", disk.Status.Progress)
		}
		return "Provisioning", detail
	case kubevirtv1.VirtualMachineStatusStarting, kubevirtv1.VirtualMachineStatusMigrating:
		return "Starting", ""
	case kubevirtv1.VirtualMachineStatusRunning:
		return "Running", ""
	case kubevirtv1.VirtualMachineStatusPaused:
		return "Running", "Paused"
	case kubevirtv1.VirtualMachineStatusStopping, kubevirtv1.VirtualMachineStatusTerminating:
		return "Stopping", ""
	}
	detail := string(s)
	for _, c := range vm.Status.Conditions {
		if c.Status == corev1.ConditionFalse && c.Type == kubevirtv1.VirtualMachineReady && c.Message != "" {
			detail += ": " + c.Message
			break
		}
		if c.Type == kubevirtv1.VirtualMachineFailure && c.Message != "" {
			detail += ": " + c.Message
			break
		}
	}
	return "Error", detail
}

// applyMedia turns an image machine into one made from a medium: a disk image is cloned into the
// root disk; an ISO gets a blank root disk and is attached as a CD-ROM (a per-machine clone) to
// install from. The profile sets the firmware and devices; Windows also gets the virtio driver CD,
// Secure Boot with a TPM, Hyper-V enlightenments, and no cloud-init (Setup sets the password).
func applyMedia(vm *kubevirtv1.VirtualMachine, id string, storage *cdiv1.StorageSpec, r Request) {
	if r.media == nil {
		return
	}
	spec := &vm.Spec.Template.Spec
	root := &vm.Spec.DataVolumeTemplates[0].Spec
	switch r.media.Kind {
	case "disk":
		root.Source = &cdiv1.DataVolumeSource{PVC: &cdiv1.DataVolumeSourcePVC{Namespace: vm.Namespace, Name: r.media.PVC}}
	case "iso":
		root.Source = &cdiv1.DataVolumeSource{Blank: &cdiv1.DataVolumeBlankImage{}}
		iso := storage.DeepCopy()
		iso.Resources.Requests[corev1.ResourceStorage] = *resource.NewQuantity(r.media.PVCBytes, resource.BinarySI)
		vm.Spec.DataVolumeTemplates = append(vm.Spec.DataVolumeTemplates, kubevirtv1.DataVolumeTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Name: installName(id), Labels: vm.Spec.DataVolumeTemplates[0].Labels},
			Spec: cdiv1.DataVolumeSpec{
				Source:  &cdiv1.DataVolumeSource{PVC: &cdiv1.DataVolumeSourcePVC{Namespace: vm.Namespace, Name: r.media.PVC}},
				Storage: iso,
			},
		})
		one, two := uint(1), uint(2)
		spec.Domain.Devices.Disks[0].BootOrder = &two
		spec.Domain.Devices.Disks = append(spec.Domain.Devices.Disks, kubevirtv1.Disk{
			// virtio-scsi: the firmware reads it fast (an emulated SATA CD took minutes to load Windows'
			// boot.wim). Windows Setup then needs vioscsi from the driver CD, which stays on SATA so
			// Setup can read it without any driver; vioscsi also serves the root disk.
			Name: volumeInstall, BootOrder: &one,
			DiskDevice: kubevirtv1.DiskDevice{CDRom: &kubevirtv1.CDRomTarget{Bus: kubevirtv1.DiskBusSCSI}},
		})
		spec.Volumes = append(spec.Volumes, kubevirtv1.Volume{Name: volumeInstall,
			VolumeSource: kubevirtv1.VolumeSource{DataVolume: &kubevirtv1.DataVolumeSource{Name: installName(id)}}})
	}
	no, yes := false, true
	switch r.profile {
	case "uefi", "linux":
		spec.Domain.Firmware = &kubevirtv1.Firmware{Bootloader: &kubevirtv1.Bootloader{EFI: &kubevirtv1.EFI{SecureBoot: &no}}}
	case "windows":
		spec.Domain.Machine = &kubevirtv1.Machine{Type: "q35"}
		spec.Domain.Firmware = &kubevirtv1.Firmware{Bootloader: &kubevirtv1.Bootloader{EFI: &kubevirtv1.EFI{SecureBoot: &yes}}}
		on := &kubevirtv1.FeatureState{}
		spec.Domain.Features = &kubevirtv1.Features{
			ACPI: kubevirtv1.FeatureState{}, APIC: &kubevirtv1.FeatureAPIC{}, SMM: &kubevirtv1.FeatureState{},
			Hyperv: &kubevirtv1.FeatureHyperv{
				Relaxed: on, VAPIC: on, VPIndex: on, Runtime: on, SyNIC: on, Reset: on, Frequencies: on,
				Reenlightenment: on, IPI: on, Spinlocks: &kubevirtv1.FeatureSpinlocks{Retries: ptrUint32(8191)},
				SyNICTimer: &kubevirtv1.SyNICTimer{}, TLBFlush: &kubevirtv1.TLBFlush{},
			},
		}
		spec.Domain.Clock = &kubevirtv1.Clock{ClockOffset: kubevirtv1.ClockOffset{UTC: &kubevirtv1.ClockOffsetUTC{}},
			Timer: &kubevirtv1.Timer{HPET: &kubevirtv1.HPETTimer{Enabled: &no}, PIT: &kubevirtv1.PITTimer{TickPolicy: kubevirtv1.PITTickPolicyDelay},
				RTC: &kubevirtv1.RTCTimer{TickPolicy: kubevirtv1.RTCTickPolicyCatchup}, Hyperv: &kubevirtv1.HypervTimer{}}}
		spec.Domain.Devices.TPM = &kubevirtv1.TPMDevice{Persistent: &yes}
		spec.Domain.Devices.Inputs = []kubevirtv1.Input{{Name: "tablet", Type: kubevirtv1.InputTypeTablet, Bus: kubevirtv1.InputBusUSB}}
		// no cloud-init: drop its disk and volume
		spec.Domain.Devices.Disks = slices.DeleteFunc(spec.Domain.Devices.Disks, func(d kubevirtv1.Disk) bool { return d.Name == "cloudinit" })
		spec.Volumes = slices.DeleteFunc(spec.Volumes, func(v kubevirtv1.Volume) bool { return v.Name == "cloudinit" })
		if r.media.Kind == "iso" && r.driversImage != "" {
			spec.Domain.Devices.Disks = append(spec.Domain.Devices.Disks, kubevirtv1.Disk{Name: volumeDrivers,
				DiskDevice: kubevirtv1.DiskDevice{CDRom: &kubevirtv1.CDRomTarget{Bus: kubevirtv1.DiskBusSATA}}})
			spec.Volumes = append(spec.Volumes, kubevirtv1.Volume{Name: volumeDrivers,
				VolumeSource: kubevirtv1.VolumeSource{ContainerDisk: &kubevirtv1.ContainerDiskSource{Image: r.driversImage}}})
		}
	}
}

func ptrUint32(v uint32) *uint32 { return &v }

// hasInstallMedia says whether a machine still has its install CD-ROM attached.
func hasInstallMedia(vm *kubevirtv1.VirtualMachine) bool {
	if vm == nil || vm.Spec.Template == nil {
		return false
	}
	for _, v := range vm.Spec.Template.Spec.Volumes {
		if v.Name == volumeInstall {
			return true
		}
	}
	return false
}
