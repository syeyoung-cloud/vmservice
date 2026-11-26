package machines

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	uploadv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/upload/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/syeyoung-cloud/vmservice/internal/inspect"
	"github.com/syeyoung-cloud/vmservice/internal/store"
)

// Media: members' ISOs and disk images. The data is a DataVolume named after the medium's id (CDI
// imports it from an upload or a URL); whose it is and what it is lives in the store. Files are
// only ever parsed by the inspector Job ("vmservice inspect"), never here, except the first 64 KiB
// of an upload, which only tells its kind and disk size.

// MediaConfig is what media need from the deployment.
type MediaConfig struct {
	UploadProxy  string // CDI's upload proxy, e.g. https://cdi-uploadproxy.cdi.svc
	UploadCA     string // namespace/name of the ConfigMap with its CA ("ca.crt")
	InspectImage string // the image the inspector Jobs run (this binary)
	PullSecret   string // for InspectImage, "" for none
	DriversImage string // the virtio driver CD for Windows (a containerDisk)
}

const (
	LabelMedium  = "vmservice/medium" // on a medium's DataVolume
	LabelJob     = "vmservice/job"    // inspect | probe
	mediaPrefix  = "md-"
	probeTimeout = 90 * time.Second
	uploadReady  = 3 * time.Minute
	maxNameRunes = 63
)

var mediumID = regexp.MustCompile(`^md-[a-km-np-z2-9]{6}$`)

// Medium is an image as the API shows it.
type Medium struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Owner        string         `json:"owner"` // the owner's username
	Mine         bool           `json:"mine"`
	Public       bool           `json:"public"`
	CloudConfig  map[string]any `json:"cloudConfig"`
	Source       string         `json:"source"`
	Kind         string         `json:"kind"`
	Profile      string         `json:"profile"`
	SizeBytes    int64          `json:"sizeBytes"`
	VirtualBytes int64          `json:"virtualBytes"`
	State        string         `json:"state"`
	Progress     string         `json:"progress"`
	Detail       string         `json:"detail"`
	CreatedAt    time.Time      `json:"createdAt"`
}

// mediaRef is a medium resolved for making a machine from it.
type mediaRef struct {
	ID          string
	Name        string
	CloudConfig map[string]any
	PVC         string
	Kind        string
	Profile     string
	PVCBytes    int64 // the DataVolume's storage request: a machine's disk must be at least this
}

func newMediumID() string { return mediaPrefix + newID()[3:] }

// pvcBytes is the storage a medium's DataVolume asks for: the disk it holds, rounded up to MiB.
// (CDI adds its file-system overhead on top.)
func pvcBytes(r inspect.Result) int64 {
	n := max(r.VirtualBytes, r.SizeBytes)
	return (n + (1<<20 - 1)) &^ (1<<20 - 1)
}

func dvRequest(dv *cdiv1.DataVolume) int64 {
	if dv == nil || dv.Spec.Storage == nil {
		return 0
	}
	q := dv.Spec.Storage.Resources.Requests[corev1.ResourceStorage]
	return q.Value()
}

// mediumState is a medium's state from its DataVolume (the actual import) and whether the inspector
// has run.
func mediumState(dv *cdiv1.DataVolume, upload, inspected bool, created, now time.Time) (state, progress, detail string) {
	if dv == nil {
		if now.Sub(created) < missingGrace {
			return "Importing", "", ""
		}
		return "Failed", "", "Its data is missing; delete it."
	}
	if p := string(dv.Status.Progress); p != "" && p != "N/A" {
		progress = p
	}
	switch dv.Status.Phase {
	case cdiv1.Succeeded:
		if !inspected {
			return "Inspecting", "", ""
		}
		return "Ready", "", ""
	case cdiv1.Failed:
		for _, c := range dv.Status.Conditions {
			if c.Message != "" && c.Status == corev1.ConditionFalse {
				detail = c.Message
			}
		}
		return "Failed", "", detail
	case cdiv1.UploadReady:
		return "Uploading", progress, "" // CDI's upload server is waiting for the file
	}
	if upload && dv.Status.Phase != cdiv1.ImportInProgress {
		return "Preparing", "", "" // the upload server is starting: the browser waits before sending
	}
	return "Importing", progress, ""
}

func (s *Service) dataVolume(ctx context.Context, name string) *cdiv1.DataVolume {
	var dv cdiv1.DataVolume
	if s.read.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, &dv) != nil {
		return nil
	}
	return &dv
}

func (s *Service) toMedium(ctx context.Context, row store.Medium, a Actor, now time.Time) Medium {
	m := Medium{ID: row.ID, Name: row.Name, Owner: row.OwnerName, Mine: row.Owner == a.Owner, Public: row.Public,
		CloudConfig: row.CloudConfig, Source: row.Source, Kind: row.Kind, Profile: row.Profile,
		SizeBytes: row.SizeBytes, VirtualBytes: row.VirtualBytes, CreatedAt: row.CreatedAt.UTC()}
	if m.CloudConfig == nil {
		m.CloudConfig = map[string]any{}
	}
	m.State, m.Progress, m.Detail = mediumState(s.dataVolume(ctx, row.ID), row.Source == "upload", row.Profile != "", row.CreatedAt, now)
	return m
}

// visible says whether the actor may see (and make machines from) an image.
func visible(row store.Medium, a Actor) bool { return row.Public || a.may(row.Owner) }

// Media is the actor's images, then the public ones (admins: everyone's).
func (s *Service) Media(ctx context.Context, a Actor) ([]Medium, error) {
	var rows []store.Medium
	var err error
	if a.Admin {
		rows, err = s.store.AllMedia(ctx)
	} else {
		var mine, public []store.Medium
		if mine, err = s.store.Media(ctx, a.Owner); err == nil {
			public, err = s.store.PublicMedia(ctx)
		}
		rows = mine
		for _, p := range public {
			if p.Owner != a.Owner {
				rows = append(rows, p)
			}
		}
	}
	if err != nil {
		return nil, s.storeError("reading the images", err)
	}
	now := time.Now()
	out := make([]Medium, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.toMedium(ctx, r, a, now))
	}
	return out, nil
}

var errNoMedium = refuse(http.StatusNotFound, "No such medium.")

// mediumRow is an image the actor may see; edit says it must also be theirs to change (owner or
// admin). Anything else looks like no image.
func (s *Service) mediumRow(ctx context.Context, a Actor, id string, edit bool) (store.Medium, error) {
	if !mediumID.MatchString(id) {
		return store.Medium{}, errNoMedium
	}
	row, err := s.store.Medium(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (!visible(row, a) || edit && !a.may(row.Owner))) {
		return store.Medium{}, errNoMedium
	}
	if err != nil {
		return store.Medium{}, s.storeError("reading the image", err)
	}
	return row, nil
}

// GetMedium is an image the actor may see.
func (s *Service) GetMedium(ctx context.Context, a Actor, id string) (Medium, error) {
	row, err := s.mediumRow(ctx, a, id, false)
	if err != nil {
		return Medium{}, err
	}
	return s.toMedium(ctx, row, a, time.Now()), nil
}

// UpdateMedium changes an image's name, public flag or cloud-config extras (owner or admin).
func (s *Service) UpdateMedium(ctx context.Context, a Actor, id string, u store.MediumUpdate) (Medium, error) {
	if _, err := s.mediumRow(ctx, a, id, true); err != nil {
		return Medium{}, err
	}
	if u.Name != nil {
		name, err := checkMediumName(*u.Name)
		if err != nil {
			return Medium{}, err
		}
		u.Name = &name
	}
	for k := range u.CloudConfig {
		if slices.Contains(reservedCloudKeys, k) {
			return Medium{}, refuse(http.StatusBadRequest, "The cloud-config extras can't set %q: VMService sets it.", k)
		}
	}
	if err := s.store.UpdateMedium(ctx, id, u); err != nil {
		return Medium{}, s.storeError("saving the image", err)
	}
	row, err := s.store.Medium(ctx, id)
	if err != nil {
		return Medium{}, s.storeError("reading the image", err)
	}
	ch := change{owner: row.Owner, id: id, medium: true, public: row.Public || u.Public != nil} // unpublishing reaches everyone too
	s.hub.publish(ch)
	s.log.Info("image updated", "by", a.Username, "id", id)
	return s.toMedium(ctx, row, a, time.Now()), nil
}

// publishMedium tells the owner's (and, for public images, everyone's) streams about a change.
func (s *Service) publishMedium(row store.Medium) {
	s.hub.publish(change{owner: row.Owner, id: row.ID, medium: true, public: row.Public})
}

// resolveMedium is a Ready image the actor may make a machine from.
func (s *Service) resolveMedium(ctx context.Context, a Actor, id string) (mediaRef, error) {
	row, err := s.mediumRow(ctx, a, id, false)
	if err != nil {
		return mediaRef{}, refuse(http.StatusBadRequest, "There is no image %q.", id)
	}
	dv := s.dataVolume(ctx, row.ID)
	if state, _, _ := mediumState(dv, row.Source == "upload", row.Profile != "", row.CreatedAt, time.Now()); state != "Ready" {
		return mediaRef{}, refuse(http.StatusConflict, "That image isn't ready.")
	}
	return mediaRef{ID: row.ID, Name: row.Name, CloudConfig: row.CloudConfig, PVC: row.ID, Kind: row.Kind, Profile: row.Profile, PVCBytes: dvRequest(dv)}, nil
}

func checkMediumName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > maxNameRunes || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", refuse(http.StatusBadRequest, "The name must be 1 to %d characters.", maxNameRunes)
	}
	return name, nil
}

func (s *Service) mediumDV(id, owner string, source cdiv1.DataVolumeSource, bytes int64) *cdiv1.DataVolume {
	storage := &cdiv1.StorageSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: *resource.NewQuantity(bytes, resource.BinarySI)}},
	}
	if s.Catalog.StorageClass != "" {
		storage.StorageClassName = &s.Catalog.StorageClass
	}
	return &cdiv1.DataVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: id, Namespace: s.Namespace,
			Labels:      map[string]string{LabelOwner: owner, LabelMedium: id},
			Annotations: map[string]string{"cdi.kubevirt.io/storage.bind.immediate.requested": "true"}, // nothing mounts it soon
		},
		Spec: cdiv1.DataVolumeSpec{Source: &source, ContentType: cdiv1.DataVolumeKubeVirt, Storage: storage},
	}
}

// addMedium inserts the row, then creates the DataVolume; a failed create gives the row back.
func (s *Service) addMedium(ctx context.Context, row store.Medium, source cdiv1.DataVolumeSource, r inspect.Result) (Medium, error) {
	for attempt := 0; ; attempt++ {
		err := s.store.AddMedium(ctx, row)
		if err == nil {
			break
		}
		if errors.Is(err, store.ErrExists) && attempt < 3 {
			row.ID = newMediumID()
			continue
		}
		return Medium{}, s.storeError("saving the medium", err)
	}
	if err := s.write.Create(ctx, s.mediumDV(row.ID, row.Owner, source, pvcBytes(r))); err != nil {
		s.store.DeleteMedium(context.WithoutCancel(ctx), row.ID)
		return Medium{}, s.kubeError("creating the medium", err)
	}
	s.log.Info("medium added", "owner", row.Owner, "id", row.ID, "source", row.Source, "kind", row.Kind, "bytes", row.SizeBytes)
	row.CreatedAt = time.Now()
	return s.toMedium(ctx, row, Actor{Owner: row.Owner}, time.Now()), nil
}

// AddUpload registers a medium the browser will upload: header is its first 64 KiB.
func (s *Service) AddUpload(ctx context.Context, a Actor, name string, size int64, header []byte) (Medium, error) {
	name, err := checkMediumName(name)
	if err != nil {
		return Medium{}, err
	}
	r, err := inspect.Header(header, size)
	if err != nil || size <= 0 {
		return Medium{}, refuse(http.StatusBadRequest, "That isn't an ISO or a disk image (qcow2, VMDK or raw).")
	}
	row := store.Medium{ID: newMediumID(), Owner: a.Owner, Name: name, Source: "upload", Kind: r.Kind, SizeBytes: size, VirtualBytes: pvcBytes(r)}
	return s.addMedium(ctx, row, cdiv1.DataVolumeSource{Upload: &cdiv1.DataVolumeSourceUpload{}}, r)
}

// AddURL registers a medium CDI downloads from url, after the probe Job read its header.
func (s *Service) AddURL(ctx context.Context, a Actor, name, raw string) (Medium, error) {
	name, err := checkMediumName(name)
	if err != nil {
		return Medium{}, err
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return Medium{}, refuse(http.StatusBadRequest, "Give an http or https URL.")
	}
	r, err := s.probe(ctx, u.String())
	if err != nil {
		return Medium{}, err
	}
	row := store.Medium{ID: newMediumID(), Owner: a.Owner, Name: name, Source: "url", URL: u.String(), Kind: r.Kind, SizeBytes: r.SizeBytes, VirtualBytes: pvcBytes(r)}
	return s.addMedium(ctx, row, cdiv1.DataVolumeSource{HTTP: &cdiv1.DataVolumeSourceHTTP{URL: u.String()}}, r)
}

// Upload streams an upload's body to CDI's upload proxy, once the DataVolume is ready for it.
func (s *Service) Upload(ctx context.Context, a Actor, id string, body io.Reader, length int64) error {
	row, err := s.mediumRow(ctx, a, id, true)
	if err != nil {
		return err
	}
	if row.Source != "upload" {
		return refuse(http.StatusConflict, "That medium isn't waiting for a file.")
	}
	if length != row.SizeBytes {
		return refuse(http.StatusBadRequest, "The file is %d bytes; this medium was registered for %d.", length, row.SizeBytes)
	}
	if err := s.waitDataVolume(ctx, id, cdiv1.UploadReady); err != nil {
		return err
	}
	tr := &uploadv1.UploadTokenRequest{ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: s.Namespace}, Spec: uploadv1.UploadTokenRequestSpec{PvcName: id}}
	if err := s.write.Create(ctx, tr); err != nil || tr.Status.Token == "" {
		return s.kubeError("getting an upload token", err)
	}
	httpClient, err := s.uploadClient(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.MediaConfig.UploadProxy, "/")+"/v1beta1/upload", io.LimitReader(body, length))
	if err != nil {
		return err
	}
	req.ContentLength = length
	req.Header.Set("Authorization", "Bearer "+tr.Status.Token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := httpClient.Do(req)
	if err != nil {
		return refuse(http.StatusBadGateway, "The upload broke off.")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		s.log.Warn("upload proxy", "id", id, "status", resp.StatusCode, "body", strings.TrimSpace(string(msg)))
		return refuse(http.StatusBadGateway, "The cluster refused the upload.")
	}
	s.log.Info("image uploaded", "by", a.Username, "id", id, "bytes", length)
	return nil
}

func (s *Service) uploadClient(ctx context.Context) (*http.Client, error) {
	ns, name, _ := strings.Cut(s.MediaConfig.UploadCA, "/")
	var cm corev1.ConfigMap
	if err := s.write.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &cm); err != nil {
		return nil, s.kubeError("reading the upload proxy's CA", err)
	}
	pool := x509.NewCertPool()
	for _, v := range cm.Data {
		pool.AppendCertsFromPEM([]byte(v))
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}, nil
}

// waitDataVolume waits (a watch, not polling) until the DataVolume reaches phase.
func (s *Service) waitDataVolume(ctx context.Context, name string, phase cdiv1.DataVolumePhase) error {
	ctx, cancel := context.WithTimeout(ctx, uploadReady)
	defer cancel()
	w, err := s.watch.Watch(ctx, &cdiv1.DataVolumeList{}, client.InNamespace(s.Namespace), client.MatchingFields{"metadata.name": name})
	if err != nil {
		return s.kubeError("watching the medium", err)
	}
	defer w.Stop()
	for e := range w.ResultChan() {
		if dv, ok := e.Object.(*cdiv1.DataVolume); ok {
			switch dv.Status.Phase {
			case phase:
				return nil
			case cdiv1.Failed, cdiv1.Succeeded:
				return refuse(http.StatusConflict, "That medium isn't waiting for a file.")
			}
		}
	}
	return refuse(http.StatusGatewayTimeout, "The cluster wasn't ready for the upload in time; try again.")
}

// DeleteMedium deletes one of the owner's media: its DataVolume, then its row. Machines made from
// it keep their own copies.
func (s *Service) DeleteMedium(ctx context.Context, a Actor, id string) error {
	row, err := s.mediumRow(ctx, a, id, true)
	if err != nil {
		return err
	}
	// Machines' disks are ZFS clones of the image: it can't go while any depends on it. A disk
	// image's machines always do; an ISO's only until they eject it (their install copy is the clone).
	users, err := s.store.MachinesFrom(ctx, id)
	if err != nil {
		return s.storeError("checking which machines use the image", err)
	}
	var mine []string
	blocking := 0
	for _, m := range users {
		if row.Kind == "iso" && s.dataVolume(ctx, installName(m.ID)) == nil {
			continue // ejected: nothing of the ISO left in it
		}
		blocking++
		if a.may(m.Owner) {
			mine = append(mine, m.Name)
		}
	}
	if blocking > 0 {
		msg := fmt.Sprintf("%d machine(s) still use this image", blocking)
		if len(mine) > 0 {
			msg += " (" + strings.Join(mine, ", ") + ")"
		}
		if row.Kind == "iso" {
			msg += "; eject it from them or delete them first."
		} else {
			msg += "; their disks are copies of it that share its data, so delete them first."
		}
		return refuse(http.StatusConflict, "%s", msg)
	}
	dv := &cdiv1.DataVolume{ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: s.Namespace}}
	if err := s.write.Delete(ctx, dv, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return s.kubeError("deleting the medium", err)
	}
	if err := s.store.DeleteMedium(ctx, id); err != nil {
		return s.storeError("deleting the medium", err)
	}
	s.publishMedium(row)
	s.log.Info("image deleted", "by", a.Username, "owner", row.Owner, "id", id)
	return nil
}

// --- the inspector Jobs ---------------------------------------------------------------------

// inspectorJob is a Job running "vmservice inspect args…" as a plain user with nothing but its
// arguments (and, for an inspection, the medium mounted read-only). Its network policy is picked by
// the app label: vmservice-inspect gets none at all, vmservice-probe only the internet.
func (s *Service) inspectorJob(name, app string, args []string, labels map[string]string, volume *corev1.Volume) *batchv1.Job {
	no, yes := false, true
	uid := int64(107) // CDI's disk.img belongs to 107 (qemu)
	deadline, ttl := int64(300), int32(600)
	backoff := int32(0)
	podLabels := map[string]string{"app": app}
	for k, v := range labels {
		podLabels[k] = v
	}
	container := corev1.Container{
		Name: "inspect", Image: s.MediaConfig.InspectImage, Args: append([]string{"inspect"}, args...),
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes,
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi"), corev1.ResourceCPU: resource.MustParse("500m")}},
	}
	pod := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: &no, EnableServiceLinks: &no,
		SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &yes, RunAsUser: &uid, RunAsGroup: &uid,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Containers: []corev1.Container{container},
	}
	if volume != nil {
		pod.Volumes = []corev1.Volume{*volume}
		pod.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: volume.Name, MountPath: "/disk", ReadOnly: true}}
	}
	if s.MediaConfig.PullSecret != "" {
		pod.ImagePullSecrets = []corev1.LocalObjectReference{{Name: s.MediaConfig.PullSecret}}
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{BackoffLimit: &backoff, ActiveDeadlineSeconds: &deadline, TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: podLabels}, Spec: pod}},
	}
}

// jobResult reads what a finished Job's container said: an inspect.Result as JSON, or an error.
func (s *Service) jobResult(ctx context.Context, job *batchv1.Job) (inspect.Result, error) {
	var pods corev1.PodList
	if err := s.write.List(ctx, &pods, client.InNamespace(s.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return inspect.Result{}, err
	}
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			t := cs.State.Terminated
			if t == nil {
				continue
			}
			var out struct {
				inspect.Result
				Error string `json:"error"`
			}
			if json.Unmarshal([]byte(t.Message), &out) != nil {
				return inspect.Result{}, fmt.Errorf("the inspector said: %.200s", t.Message)
			}
			if out.Error != "" {
				return inspect.Result{}, errors.New(out.Error)
			}
			return out.Result, nil
		}
	}
	return inspect.Result{}, errors.New("the inspector left no result")
}

func jobFinished(j *batchv1.Job) bool { return j.Status.Succeeded > 0 || j.Status.Failed > 0 }

// probe runs a probe Job for url and waits (a watch) for its result.
func (s *Service) probe(ctx context.Context, u string) (inspect.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	name := "probe-" + newID()[3:]
	job := s.inspectorJob(name, "vmservice-probe", []string{"-url", u}, map[string]string{LabelJob: "probe"}, nil)
	if err := s.write.Create(ctx, job); err != nil {
		return inspect.Result{}, s.kubeError("starting the download check", err)
	}
	defer s.write.Delete(context.WithoutCancel(ctx), job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	w, err := s.watch.Watch(ctx, &batchv1.JobList{}, client.InNamespace(s.Namespace), client.MatchingFields{"metadata.name": name})
	if err != nil {
		return inspect.Result{}, s.kubeError("watching the download check", err)
	}
	defer w.Stop()
	for e := range w.ResultChan() {
		j, ok := e.Object.(*batchv1.Job)
		if !ok || e.Type == watch.Deleted || !jobFinished(j) {
			continue
		}
		r, err := s.jobResult(ctx, j)
		if err != nil {
			return inspect.Result{}, refuse(http.StatusBadRequest, "That URL can't be used: %v", err)
		}
		return r, nil
	}
	return inspect.Result{}, refuse(http.StatusGatewayTimeout, "Checking the URL took too long.")
}

// ensureInspected starts the inspector for a medium whose import finished and that wasn't inspected
// yet. Called on every DataVolume change; creating the Job twice is harmless (the name is fixed).
func (s *Service) ensureInspected(dv *cdiv1.DataVolume) {
	id := dv.Labels[LabelMedium]
	if dv.Status.Phase != cdiv1.Succeeded || !mediumID.MatchString(id) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	row, err := s.store.Medium(ctx, id)
	if err != nil || row.Profile != "" {
		return
	}
	volume := &corev1.Volume{Name: "disk", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: id, ReadOnly: true}}}
	job := s.inspectorJob("inspect-"+id, "vmservice-inspect", []string{"-file", "/disk/disk.img"}, map[string]string{LabelJob: "inspect", LabelMedium: id, LabelOwner: row.Owner}, volume)
	if err := s.write.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		s.log.Warn("starting the inspector", "id", id, "err", err)
	}
}

// inspected records a finished inspection and removes its Job. A failed inspection falls back to
// the profile for the kind (the header was already checked when the medium was added).
func (s *Service) inspected(j *batchv1.Job) {
	id := j.Labels[LabelMedium]
	if j.Labels[LabelJob] != "inspect" || !jobFinished(j) || !mediumID.MatchString(id) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	row, err := s.store.Medium(ctx, id)
	if err == nil && row.Profile == "" {
		r, ierr := s.jobResult(ctx, j)
		profile := r.Profile
		if ierr != nil || profile == "" {
			s.log.Warn("inspection failed; using the kind's default profile", "id", id, "err", ierr)
			profile = map[string]string{inspect.KindISO: inspect.ProfileUEFI, inspect.KindDisk: inspect.ProfileBIOS}[row.Kind]
		}
		if err := s.store.SetMediumProfile(ctx, id, profile, r.VirtualBytes); err != nil {
			s.log.Error("recording an inspection", "id", id, "err", err)
			return // the Job stays; the next change of it retries
		}
		s.log.Info("medium inspected", "id", id, "profile", profile, "label", r.Label)
		s.publishMedium(row)
	}
	s.write.Delete(ctx, j, client.PropagationPolicy(metav1.DeletePropagationBackground))
}
