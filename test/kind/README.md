# Disposable end-to-end test (kind)

Proves the NodeStageVolume behaviour added in v1.7.0 **without touching OneDrive**:
the PV points at rclone's `:local:` backend over a hostPath on the node, so there
are no credentials and nothing to clean up remotely.

The property being tested is the one that matters: **N pods sharing a volume on a
node must produce exactly ONE `rclone mount` process.** Before v1.7.0 each pod got
its own process, and because they all shared `--cache-dir` they raced to upload
the same dirty cache items, which OneDrive rejected with `409 resourceModified`.

## Run

```sh
docker build -t csi-rclone-reloaded:v1.7.0-local -f Dockerfile .
kind create cluster --config test/kind/kind-config.yaml
kind load docker-image csi-rclone-reloaded:v1.7.0-local --name csi-rclone-test
kubectl apply -f test/kind/driver.yaml
docker exec csi-rclone-test-worker sh -c 'mkdir -p /srv/testdata && echo hello-from-the-remote > /srv/testdata/probe.txt'
kubectl apply -f test/kind/workload.yaml -f test/kind/pods.yaml
```

## What to assert

```sh
POD=$(kubectl get pods -n kube-system -l app=csi-nodeplugin-rclone \
       --field-selector spec.nodeName=csi-rclone-test-worker -o name | head -1)

# 1. ONE rclone process for 4 pods  (the whole point)
kubectl exec -n kube-system ${POD#pod/} -c rclone -- sh -c 'ps aux | grep -c "[r]clone mount"'

# 2. one staged mount + one bind per pod
docker exec csi-rclone-test-worker sh -c 'grep fuse.rclone /proc/mounts'

# 3. stage happened once, publish bound per pod
kubectl logs -n kube-system ${POD#pod/} -c rclone | grep -E "NodeStageVolume|bind mounting"
```

Also worth exercising: a pod with `readOnly: true` must fail writes
(`Read-only file system`) while other pods keep writing — readOnly is applied to
the per-pod BIND, never to the shared mount.

**Teardown is asynchronous.** After the last consumer goes, kubelet takes up to
~60-90s to call NodeUnstageVolume. Do not conclude it leaked before then; when it
fires, the rclone process count drops to 0, `/proc/mounts` has no fuse.rclone
entries, and /var/lib/csi-rclone/configs is empty.

```sh
kind delete cluster --name csi-rclone-test
```

## Also test: recovery from a node-plugin roll

This is the scenario that broke production, and it found two real bugs.

```sh
kubectl rollout restart ds/csi-nodeplugin-rclone -n kube-system   # kills the rclone processes
kubectl rollout status  ds/csi-nodeplugin-rclone -n kube-system
kubectl scale deploy consumer --replicas=4                        # new pods must still mount
```

Expected: new pods reach Running. The driver logs
`staging path ... is a dead mount, unmounting before re-staging`, then
`staged rclone mount at ...`.

**Pods that were already running keep a dead bind** (`Socket not connected`) and
must be recreated - their bind was taken against the old mount instance, and
re-mounting the source does not repair existing binds. `kubectl rollout restart`
on the consumer fixes those. That residual gap is what the rclone-mount-healer
CronJob covers in production.
