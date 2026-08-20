package rclone

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/tools/clientcmd"

	// k8s.io/mount-utils is the maintained home of what used to be
	// k8s.io/kubernetes/pkg/util/mount. Importing k8s.io/kubernetes as a
	// library is unsupported upstream and was pinning this module to the 2019
	// dependency tree.
	mount "k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"
)

// NOTE: never log a whole CSI request (e.g. klog.Infof("%+v", *req)). The
// request's VolumeContext carries `configData` - the rclone config, including
// OneDrive OAuth access/refresh tokens and crypt passwords - so a full dump
// writes live credentials into the node-plugin pod log. It also copies a
// sync.Mutex now that the CSI protos embed protoimpl.MessageState, which go vet
// flags. Log individual, non-secret fields instead.
type nodeServer struct {
	// Embedding the generated Unimplemented server is what makes this
	// forward-compatible: RPCs added to the CSI spec later return
	// Unimplemented instead of breaking the build. It replaces the embedded
	// csicommon.DefaultNodeServer.
	csi.UnimplementedNodeServer

	nodeID  string
	mounter *mount.SafeFormatAndMount
}

// NodeGetInfo reports this node's ID to the kubelet. Previously supplied by
// csicommon.DefaultNodeServer; the kubelet requires it, so it must be
// implemented explicitly now.
func (ns *nodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: ns.nodeID}, nil
}

type mountPoint struct {
	VolumeId  string
	MountPath string
}

// configBaseDir is the plugin-owned directory where per-volume rclone config
// files are written. It lives inside the plugin container (ephemeral), so the
// plaintext secrets it may contain never accumulate on the node's host disk.
const configBaseDir = "/var/lib/csi-rclone/configs"

// configPathForTarget returns a deterministic per-volume rclone config path
// derived from the target path. Because it is deterministic, NodeUnpublishVolume
// can recompute the exact same path and remove the file on teardown.
func configPathForTarget(targetPath string) string {
	// Clean the path (e.g. strip a trailing slash) so NodePublishVolume and
	// NodeUnpublishVolume hash to the same value even if a caller is inconsistent,
	// guaranteeing the config file is found and removed on teardown.
	sum := sha256.Sum256([]byte(filepath.Clean(targetPath)))
	return filepath.Join(configBaseDir, hex.EncodeToString(sum[:])+".conf")
}

func (ns *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	klog.Infof("NodePublishVolume: volumeId=%s targetPath=%s stagingTargetPath=%s readOnly=%t",
		req.GetVolumeId(), req.GetTargetPath(), req.GetStagingTargetPath(), req.GetReadonly())

	targetPath := req.GetTargetPath()

	notMnt, err := mount.New("").IsLikelyNotMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(targetPath, 0750); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			notMnt = true
		} else {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	if !notMnt {
		// testing original mount point, make sure the mount link is valid
		if _, err := os.ReadDir(targetPath); err == nil {
			klog.Infof("already mounted to target %s", targetPath)
			return &csi.NodePublishVolumeResponse{}, nil
		}
		// todo: mount link is invalid, now unmount and remount later (built-in functionality)
		klog.Warningf("ReadDir %s failed with %v, unmount this directory", targetPath, err)

		ns.mounter = &mount.SafeFormatAndMount{
			Interface: mount.New(""),
			Exec:      utilexec.New(),
		}

		if err := ns.mounter.Unmount(targetPath); err != nil {
			klog.Errorf("Unmount directory %s failed with %v", targetPath, err)
			return nil, err
		}
	}

	// STAGED PATH (normal since v1.7.0). NodeStageVolume has already created
	// exactly ONE rclone mount for this volume on this node; all we do per pod
	// is bind-mount it. This is what keeps the node to a single rclone process
	// (and therefore a single VFS cache) per volume, no matter how many pods
	// consume it.
	//
	// Why that matters: previously every pod got its own `rclone mount`, and
	// they all shared --cache-dir. Each process independently found the same
	// "Dirty" cache items and raced to upload them, so OneDrive returned
	// 409 resourceModified (eTag mismatch) and files were repeatedly truncated
	// to 0 bytes on the remote. Four pods on one node produced ~6500 409s and
	// upload retries reached try #50.
	if stagingTargetPath := req.GetStagingTargetPath(); stagingTargetPath != "" {
		// Repair a dead staging mount before binding to it.
		//
		// This is NOT redundant with the same check in NodeStageVolume. When the
		// node plugin restarts (a DaemonSet roll, or the postStart umount hook)
		// the rclone process dies and the staging path becomes a dead FUSE
		// endpoint. The kubelet still has the volume recorded as staged, so for
		// every new pod it calls NodePublishVolume ONLY - NodeStageVolume is
		// never called again. Without this, the bind fails with
		// "transport endpoint is not connected" on every retry and pods sit in
		// ContainerCreating forever. Verified in test/kind by rolling the
		// DaemonSet while pods were running.
		if err := ensureStaged(stagingTargetPath, req.GetVolumeContext(),
			req.GetVolumeCapability().GetMount().GetMountFlags()); err != nil {
			return nil, err
		}

		// Per-pod readOnly is applied HERE, on the bind mount, not on the
		// shared rclone mount underneath - the staged mount is shared by every
		// pod on this node, so it must stay writable for the pods that write.
		// The k8s mounter turns []string{"bind","ro"} into the required two
		// syscalls (bind, then remount,bind,ro); a single bind with "ro" would
		// silently stay read-write.
		options := []string{"bind"}
		if req.GetReadonly() {
			options = append(options, "ro")
		}

		klog.Infof("bind mounting staged volume %s -> %s (options=%v)", stagingTargetPath, targetPath, options)
		if err := mount.New("").Mount(stagingTargetPath, targetPath, "", options); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to bind mount %s to %s: %v", stagingTargetPath, targetPath, err)
		}

		return &csi.NodePublishVolumeResponse{}, nil
	}

	// LEGACY PATH: no staging target supplied, so mount rclone directly at the
	// pod's target path. Retained so the driver still works if it is ever run
	// without STAGE_UNSTAGE_VOLUME being honored by the kubelet.
	klog.Warningf("NodePublishVolume called without a staging target path for volume %s; "+
		"falling back to a direct per-pod rclone mount (one rclone process per pod)", req.GetVolumeId())

	// CSI mount flags (from PV/StorageClass mountOptions) and the readOnly
	// request are threaded through to the rclone command below. Previously
	// these were computed and then discarded (dead code).
	mountOptions := req.GetVolumeCapability().GetMount().GetMountFlags()
	readOnly := req.GetReadonly()

	remote, remotePath, configData, flags, e := extractFlags(req.GetVolumeContext())
	if e != nil {
		klog.Warningf("storage parameter error: %s", e)
		return nil, e
	}

	e = Mount(remote, remotePath, targetPath, configData, flags, mountOptions, readOnly)
	if e != nil {
		if os.IsPermission(e) {
			return nil, status.Error(codes.PermissionDenied, e.Error())
		}
		if strings.Contains(e.Error(), "invalid argument") {
			return nil, status.Error(codes.InvalidArgument, e.Error())
		}
		return nil, status.Error(codes.Internal, e.Error())
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

// ensureStaged guarantees stagingTargetPath is a live rclone mount, mounting or
// re-mounting it as needed. It is idempotent and safe to call on every publish.
//
// Called from both NodeStageVolume and NodePublishVolume on purpose: the kubelet
// records a volume as staged and will not re-stage it after the node plugin
// restarts, so publish is the only hook left that can repair a staging mount
// whose rclone process has died.
func ensureStaged(stagingTargetPath string, volumeContext map[string]string, mountOptions []string) error {
	// ORDER MATTERS: probe and clear a dead mount BEFORE any mkdir.
	// os.MkdirAll stats the path first, and on a dead FUSE endpoint that stat
	// returns ENOTCONN rather than "not a directory", so MkdirAll falls through
	// to mkdir(2) and fails with EEXIST ("file exists"). Doing the mkdir first
	// therefore makes a dead staging mount unrecoverable.
	mounted, healthy, err := mountState(stagingTargetPath)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if mounted && healthy {
		return nil
	}
	if mounted && !healthy {
		klog.Warningf("staging path %s is a dead mount, unmounting before re-staging", stagingTargetPath)
		if err := mount.New("").Unmount(stagingTargetPath); err != nil {
			return status.Errorf(codes.Internal, "failed to clear dead staging mount %s: %v", stagingTargetPath, err)
		}
	}

	// Safe now: the path is either absent or a plain directory.
	if err := os.MkdirAll(stagingTargetPath, 0750); err != nil {
		return status.Error(codes.Internal, err.Error())
	}

	remote, remotePath, configData, flags, e := extractFlags(volumeContext)
	if e != nil {
		klog.Warningf("storage parameter error: %s", e)
		return e
	}

	// readOnly is deliberately false: the staged mount is shared by every pod on
	// this node and must stay writable for the pods that write. Per-pod readOnly
	// is applied to the bind mount in NodePublishVolume.
	if e := Mount(remote, remotePath, stagingTargetPath, configData, flags, mountOptions, false); e != nil {
		if os.IsPermission(e) {
			return status.Error(codes.PermissionDenied, e.Error())
		}
		if strings.Contains(e.Error(), "invalid argument") {
			return status.Error(codes.InvalidArgument, e.Error())
		}
		return status.Error(codes.Internal, e.Error())
	}

	klog.Infof("staged rclone mount at %s", stagingTargetPath)
	return nil
}

// mountState reports whether path is currently a mount point, and whether that
// mount is actually usable. A dead FUSE mount (the rclone process behind it was
// killed) still looks like a mount point but every syscall on it fails with
// ENOTCONN, so "is it mounted" alone is not enough to decide anything.
func mountState(path string) (mounted bool, healthy bool, err error) {
	notMnt, err := mount.New("").IsLikelyNotMountPoint(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, false, nil
		}
		if mount.IsCorruptedMnt(err) {
			// Mounted, but the endpoint is dead.
			return true, false, nil
		}
		return false, false, err
	}
	if notMnt {
		return false, false, nil
	}
	if _, err := os.ReadDir(path); err != nil {
		klog.Warningf("mount point %s exists but is not readable (%v) - treating as dead", path, err)
		return true, false, nil
	}
	return true, true, nil
}

// extractFlags extracts the flags from the given volumeContext
// Retturns: remote, remotePath, configData, flags, error
func extractFlags(volumeContext map[string]string) (string, string, string, map[string]string, error) {
	// Load default connection settings from secret
	var secret *v1.Secret

	if secretName, ok := volumeContext["secretName"]; ok {
		// Load the secret that the PV spec defines
		var e error
		secret, e = getSecret(secretName)
		if e != nil {
			// if the user explicitly requested a secret and there is an error fetching it, bail with an error
			return "", "", "", nil, e
		}
	} else {
		// use rclone-secret as the default secret if none was defined
		secret, _ = getSecret("rclone-secret")
	}

	// Empty argument list
	flags := make(map[string]string)

	// Secret values are default, gets merged and overriden by corresponding PV values
	if secret != nil && secret.Data != nil && len(secret.Data) > 0 {
		// Needs byte to string casting for map values
		for k, v := range secret.Data {
			flags[k] = string(v)
		}
	} else {
		klog.Infof("No csi-rclone connection defaults secret found.")
	}

	if len(volumeContext) > 0 {
		for k, v := range volumeContext {
			flags[k] = v
		}
	}

	if e := validateFlags(flags); e != nil {
		return "", "", "", flags, e
	}

	remote := flags["remote"]
	remotePath := flags["remotePath"]

	if remotePathSuffix, ok := flags["remotePathSuffix"]; ok {
		remotePath = remotePath + remotePathSuffix
		delete(flags, "remotePathSuffix")
	}

	configData := ""
	ok := false

	if configData, ok = flags["configData"]; ok {
		delete(flags, "configData")
	}

	delete(flags, "remote")
	delete(flags, "remotePath")
	delete(flags, "secretName")

	return remote, remotePath, configData, flags, nil
}

func (ns *nodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {

	klog.Infof("NodeUnpublishVolume: volumeId=%s targetPath=%s", req.GetVolumeId(), req.GetTargetPath())

	targetPath := req.GetTargetPath()
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume Target Path must be provided")
	}

	m := mount.New("")

	notMnt, err := m.IsLikelyNotMountPoint(targetPath)
	if err != nil && !mount.IsCorruptedMnt(err) {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if notMnt && !mount.IsCorruptedMnt(err) {
		klog.Infof("Volume not mounted")

	} else {
		err = mount.CleanupMountPoint(req.GetTargetPath(), m, true)
		if err != nil {
			klog.Infof("Error while unmounting path: %s", err)
			// This will exit and fail the NodeUnpublishVolume making it to retry unmount on the next api schedule trigger.
			// Since we mount the volume with allow-non-empty now, we could skip this one too.
			return nil, status.Error(codes.Internal, err.Error())
		}

		klog.Infof("Volume %s unmounted successfully", req.VolumeId)
	}

	// Remove the per-volume rclone config file, but ONLY for a legacy
	// (unstaged) publish, where the config is keyed on this pod's target path.
	//
	// In the staged path the config belongs to the shared mount and is keyed on
	// the STAGING path, so it must survive until NodeUnstageVolume - other pods
	// are still using that mount. This call is then a harmless no-op, because
	// no file exists at hash(targetPath).
	//
	// Cleanup happens on teardown (rather than via a defer in Mount) because
	// `rclone mount --daemon` self-forks; deleting the config immediately after
	// mount would race the forked child re-reading it. This stops the previous
	// indefinite accumulation of plaintext-secret temp files.
	configFile := configPathForTarget(targetPath)
	if err := os.Remove(configFile); err != nil && !os.IsNotExist(err) {
		klog.Warningf("failed to remove rclone config file %s: %v", configFile, err)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeGetCapabilities advertises STAGE_UNSTAGE_VOLUME.
//
// This override is REQUIRED and is not cosmetic: csicommon's DefaultNodeServer
// (drivers v1.0.2) reports only RPC_UNKNOWN, and that library has no
// AddNodeServiceCapabilities helper. Without this method shadowing the embedded
// default, the kubelet never calls NodeStageVolume at all and the driver
// silently falls back to one rclone mount per pod.
func (ns *nodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
					},
				},
			},
		},
	}, nil
}

// NodeStageVolume creates the single rclone mount for this volume on this node.
// Every pod that consumes the volume then gets a cheap bind mount of it in
// NodePublishVolume, so there is exactly one rclone process - and therefore one
// VFS cache and one uploader - per (volume, node).
func (ns *nodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	klog.Infof("NodeStageVolume: volumeId=%s stagingTargetPath=%s", req.GetVolumeId(), req.GetStagingTargetPath())

	stagingTargetPath := req.GetStagingTargetPath()
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume Staging Target Path must be provided")
	}
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume Volume ID must be provided")
	}

	// All of the mount/repair logic lives in ensureStaged, which NodePublishVolume
	// also calls - the kubelet will not re-stage after a plugin restart, so publish
	// has to be able to repair a dead staging mount too.
	//
	// Idempotent by construction: the kubelet re-issues NodeStageVolume on retry,
	// and every pod after the first arrives while the volume is already staged.
	if err := ensureStaged(stagingTargetPath, req.GetVolumeContext(),
		req.GetVolumeCapability().GetMount().GetMountFlags()); err != nil {
		return nil, err
	}

	klog.Infof("NodeStageVolume: staged volume %s at %s", req.GetVolumeId(), stagingTargetPath)
	return &csi.NodeStageVolumeResponse{}, nil
}

// NodeUnstageVolume tears down the shared rclone mount once the last pod on
// this node has been unpublished, and removes the per-volume rclone config.
func (ns *nodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	klog.Infof("NodeUnstageVolume: volumeId=%s stagingTargetPath=%s", req.GetVolumeId(), req.GetStagingTargetPath())

	stagingTargetPath := req.GetStagingTargetPath()
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeUnstageVolume Staging Target Path must be provided")
	}

	m := mount.New("")
	notMnt, err := m.IsLikelyNotMountPoint(stagingTargetPath)
	if err != nil && !mount.IsCorruptedMnt(err) && !os.IsNotExist(err) {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err == nil && notMnt {
		klog.Infof("NodeUnstageVolume: %s is not mounted", stagingTargetPath)
	} else if !os.IsNotExist(err) {
		// UnmountPath tolerates a corrupted (ENOTCONN) mount, which is exactly
		// the state a killed rclone process leaves behind.
		if err := mount.CleanupMountPoint(stagingTargetPath, m, true); err != nil {
			klog.Errorf("error unmounting staging path %s: %s", stagingTargetPath, err)
			return nil, status.Error(codes.Internal, err.Error())
		}
		klog.Infof("NodeUnstageVolume: unmounted %s", stagingTargetPath)
	}

	// The staged mount owns the config file (see Mount): it is keyed on the
	// staging path, so it is removed here rather than in NodeUnpublishVolume.
	configFile := configPathForTarget(stagingTargetPath)
	if err := os.Remove(configFile); err != nil && !os.IsNotExist(err) {
		klog.Warningf("failed to remove rclone config file %s: %v", configFile, err)
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func validateFlags(flags map[string]string) error {
	if _, ok := flags["remote"]; !ok {
		return status.Errorf(codes.InvalidArgument, "missing volume context value: remote")
	}
	if _, ok := flags["remotePath"]; !ok {
		return status.Errorf(codes.InvalidArgument, "missing volume context value: remotePath")
	}
	return nil
}

func getSecret(secretName string) (*v1.Secret, error) {
	clientset, e := GetK8sClient()
	if e != nil {
		return nil, status.Errorf(codes.Internal, "can not create kubernetes client: %s", e)
	}

	kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)

	namespace, _, err := kubeconfig.Namespace()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "can't get current namespace for secret %s: %v", secretName, err)
	}

	klog.Infof("Loading csi-rclone connection defaults from secret %s/%s", namespace, secretName)

	secret, e := clientset.CoreV1().
		Secrets(namespace).
		Get(context.TODO(), secretName, metav1.GetOptions{})

	if e != nil {
		return nil, status.Errorf(codes.Internal, "can't load csi-rclone settings from secret %s: %s", secretName, e)
	}

	return secret, nil
}

// Mount routine.
func Mount(remote string, remotePath string, targetPath string, configData string, flags map[string]string, mountOptions []string, readOnly bool) error {
	mountCmd := "rclone"
	mountArgs := []string{}

	// Defaults applied only when the user has not overridden them (see below).
	// Note: cache-info-age / cache-chunk-clean-interval belonged to rclone's
	// deprecated "cache" backend and are no-ops on a VFS mount, and a 5s
	// dir-cache-time hammered OneDrive and widened the mkdir race behind 409s;
	// all three were removed so dir-cache-time falls through to rclone's sane
	// 5m native default.
	defaultFlags := map[string]string{}
	defaultFlags["vfs-cache-mode"] = "writes"
	defaultFlags["allow-non-empty"] = "true"
	defaultFlags["allow-other"] = "true"

	remoteWithPath := fmt.Sprintf(":%s:%s", remote, remotePath)

	if strings.Contains(configData, "["+remote+"]") {
		remoteWithPath = fmt.Sprintf("%s:%s", remote, remotePath)
		klog.Infof("remote %s found in configData, remoteWithPath set to %s", remote, remoteWithPath)
	}

	// rclone mount remote:path /path/to/mountpoint [flags]
	mountArgs = append(
		mountArgs,
		"mount",
		remoteWithPath,
		targetPath,
		"--daemon",
	)

	// If a custom configData is defined, write it to a deterministic per-volume
	// config file and run rclone with --config <file>.
	//
	// We intentionally do NOT defer os.Remove here: `rclone mount --daemon`
	// self-forks, so removing the file immediately would race the forked child
	// re-reading it. Instead the file lives for the whole mount lifetime and is
	// removed in NodeUnpublishVolume. The path is deterministic (hash of the
	// target path) so unpublish can find and delete exactly this file, which
	// fixes the previous indefinite leak of plaintext-secret temp files.
	if configData != "" {

		if err := os.MkdirAll(configBaseDir, 0700); err != nil {
			return err
		}

		configFile := configPathForTarget(targetPath)
		if err := os.WriteFile(configFile, []byte(configData), 0600); err != nil {
			return err
		}

		mountArgs = append(mountArgs, "--config", configFile)
	}

	// Add default flags
	for k, v := range defaultFlags {
		// Exclude overriden flags
		if _, ok := flags[k]; !ok {
			mountArgs = append(mountArgs, fmt.Sprintf("--%s=%s", k, v))
		}
	}

	// Add user supplied flags
	for k, v := range flags {
		mountArgs = append(mountArgs, fmt.Sprintf("--%s=%s", k, v))
	}

	// Honor the CSI readOnly request.
	if readOnly {
		mountArgs = append(mountArgs, "--read-only")
	}

	// Honor CSI mountOptions (PV/StorageClass mountFlags) as rclone flags,
	// prefixing "--" when the caller supplied a bare flag name.
	for _, opt := range mountOptions {
		opt = strings.TrimSpace(opt)
		if opt == "" {
			continue
		}
		if !strings.HasPrefix(opt, "-") {
			opt = "--" + opt
		}
		mountArgs = append(mountArgs, opt)
	}

	// create target, os.Mkdirall is noop if it exists
	err := os.MkdirAll(targetPath, 0750)
	if err != nil {
		return err
	}

	klog.Infof("executing mount command cmd=%s, remote=%s, targetpath=%s", mountCmd, remoteWithPath, targetPath)

	out, err := exec.Command(mountCmd, mountArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mounting failed: %v cmd: '%s' remote: '%s' targetpath: %s output: %q",
			err, mountCmd, remoteWithPath, targetPath, string(out))
	}

	return nil
}
