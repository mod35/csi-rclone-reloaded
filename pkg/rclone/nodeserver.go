package rclone

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubernetes/pkg/util/mount"
	"k8s.io/kubernetes/pkg/volume/util"

	csicommon "github.com/kubernetes-csi/drivers/pkg/csi-common"
)

type nodeServer struct {
	*csicommon.DefaultNodeServer
	mounter *mount.SafeFormatAndMount
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
	klog.Infof("NodePublishVolume: called with args %+v", *req)

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
		if _, err := ioutil.ReadDir(targetPath); err == nil {
			klog.Infof("already mounted to target %s", targetPath)
			return &csi.NodePublishVolumeResponse{}, nil
		}
		// todo: mount link is invalid, now unmount and remount later (built-in functionality)
		klog.Warningf("ReadDir %s failed with %v, unmount this directory", targetPath, err)

		ns.mounter = &mount.SafeFormatAndMount{
			Interface: mount.New(""),
			Exec:      mount.NewOsExec(),
		}

		if err := ns.mounter.Unmount(targetPath); err != nil {
			klog.Errorf("Unmount directory %s failed with %v", targetPath, err)
			return nil, err
		}
	}

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

	klog.Infof("NodeUnPublishVolume: called with args %+v", *req)

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
		err = util.UnmountPath(req.GetTargetPath(), m)
		if err != nil {
			klog.Infof("Error while unmounting path: %s", err)
			// This will exit and fail the NodeUnpublishVolume making it to retry unmount on the next api schedule trigger.
			// Since we mount the volume with allow-non-empty now, we could skip this one too.
			return nil, status.Error(codes.Internal, err.Error())
		}

		klog.Infof("Volume %s unmounted successfully", req.VolumeId)
	}

	// Remove the per-volume rclone config file written during NodePublishVolume.
	// Cleanup happens here (rather than via a defer in Mount) because
	// `rclone mount --daemon` self-forks; deleting the config immediately after
	// mount would race the forked child re-reading it. By the time we unpublish,
	// the mount is being torn down, so removing the config is safe. This stops
	// the previous indefinite accumulation of plaintext-secret temp files.
	configFile := configPathForTarget(targetPath)
	if err := os.Remove(configFile); err != nil && !os.IsNotExist(err) {
		klog.Warningf("failed to remove rclone config file %s: %v", configFile, err)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	klog.Infof("NodeUnstageVolume: called with args %+v", *req)
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	klog.Infof("NodeStageVolume: called with args %+v", *req)
	return &csi.NodeStageVolumeResponse{}, nil
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
		Get(secretName, metav1.GetOptions{})

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
		if err := ioutil.WriteFile(configFile, []byte(configData), 0600); err != nil {
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
